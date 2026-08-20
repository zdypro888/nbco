package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zdypro888/nbco/workerproto"
)

const (
	pollInterval      = 10 * time.Second // 无活时的轮询间隔
	taskTimeout       = 2 * time.Hour    // 单任务总时限（卡死另有 waitOpts.Stuck 检测）
	heartbeatInterval = 1 * time.Minute  // 活跃执行租约续期；不依赖 CLI 是否产生输出
	ackRetryInterval  = 2 * time.Second  // 首次交付确认遇到不确定网络结果时幂等重试
	progressInterval  = 60 * time.Second // 屏幕快照回传间隔（有变化才发）
	progressTail      = 25               // 每次快照回传的屏幕行数
	maxStalledTurns   = 3                // 同一停滞根因连续出现才判卡；真实进展不限制轮数
	taskIORelDir      = ".nbco-task/current"
)

var errAgentNoProgress = errors.New("agent 连续多个交互回合没有产生可见进展")

// 默认完成哨兵：测试与兼容辅助使用。真实 worker 任务会用带 nonce 的唯一哨兵，
// 避免任务描述里提前注入固定标记后被 parseCompletion 误认成完成输出。
const (
	markSummary   = "[[NBCO_SUMMARY]]"
	markLessons   = "[[NBCO_LESSONS]]"
	markNeedInput = "[[NBCO_NEED_INPUT]]"
	markEnd       = "[[NBCO_END]]"
)

type completionMarks struct {
	Summary   string
	Lessons   string
	NeedInput string
	End       string
}

var defaultCompletionMarks = completionMarks{
	Summary: markSummary, Lessons: markLessons, NeedInput: markNeedInput, End: markEnd,
}

func newCompletionMarks() completionMarks {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		nonce := fmt.Sprint(time.Now().UnixNano())
		return completionMarks{
			Summary:   "[[NBCO_SUMMARY:" + nonce + "]]",
			Lessons:   "[[NBCO_LESSONS:" + nonce + "]]",
			NeedInput: "[[NBCO_NEED_INPUT:" + nonce + "]]",
			End:       "[[NBCO_END:" + nonce + "]]",
		}
	}
	nonce := hex.EncodeToString(b[:])
	return completionMarks{
		Summary:   "[[NBCO_SUMMARY:" + nonce + "]]",
		Lessons:   "[[NBCO_LESSONS:" + nonce + "]]",
		NeedInput: "[[NBCO_NEED_INPUT:" + nonce + "]]",
		End:       "[[NBCO_END:" + nonce + "]]",
	}
}

// Worker 轮询+实时唤醒的执行循环。
type Worker struct {
	cfg    Config
	client *Client
	wait   waitOpts
	link   *wsLink // 实时通道（增强件，nil 或断线时轮询兜底）

	mu        sync.Mutex
	curTask   int64
	curCancel context.CancelFunc
	curKilled bool // 当前任务被服务端取消
}

func newWorker(cfg Config) *Worker {
	wait := defaultWaitOpts()
	if p := strings.TrimSpace(cfg.BusyPattern); p != "" {
		if re, err := regexp.Compile(p); err != nil {
			log.Printf("busy_pattern %q 非法，沿用默认: %v", p, err)
		} else {
			wait.Busy = re // 自定义 harness 的「工作中」状态行
			wait.BusyStable = 2 * time.Minute
		}
	}
	return &Worker{
		cfg:    cfg,
		client: newClient(cfg.Server, cfg.Token),
		wait:   wait,
		link:   newWSLink(cfg.Server, cfg.Token),
	}
}

// Loop 持续领活、执行，直到 ctx 取消。实时通道并行维护：唤醒让空闲等待立即
// 结束去认领；取消终止正在执行的任务。
func (w *Worker) Loop(ctx context.Context) {
	go w.link.run(ctx)
	go w.watchCancel(ctx)
	w.reportCapabilities(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		task, knowledge, history, err := w.client.Next(ctx, w.cfg.Engine)
		if err != nil {
			log.Printf("领取任务失败: %v", err)
			w.waitForWork(ctx)
			continue
		}
		if task == nil {
			w.waitForWork(ctx)
			continue
		}
		if err := w.acknowledgeRun(ctx, task); err != nil {
			log.Printf("确认任务 #%d 交付失败，等待服务端回收租约: %v", task.ID, err)
			w.waitForWork(ctx)
			continue
		}
		log.Printf("领到任务 #%d：%s", task.ID, task.Title)
		w.execute(ctx, task, knowledge, history)
	}
}

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	w.reportCapabilities(ctx)
	task, knowledge, history, err := w.client.Next(ctx, w.cfg.Engine)
	if err != nil {
		return false, err
	}
	if task == nil {
		return false, nil
	}
	if err := w.acknowledgeRun(ctx, task); err != nil {
		return false, fmt.Errorf("确认任务 #%d 交付: %w", task.ID, err)
	}
	log.Printf("领到任务 #%d：%s", task.ID, task.Title)
	w.execute(ctx, task, knowledge, history)
	return true, nil
}

func (w *Worker) acknowledgeRun(ctx context.Context, run *Run) error {
	if run == nil || run.ID <= 0 || strings.TrimSpace(run.ClaimID) == "" {
		return errors.New("任务租约无效")
	}
	for {
		ackCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := w.client.Heartbeat(ackCtx, run.ID, run.ClaimID)
		cancel()
		if err == nil || errors.Is(err, errWorkerLeaseLost) {
			return err
		}
		timer := time.NewTimer(ackRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (w *Worker) reportCapabilities(ctx context.Context) {
	ctxReport, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	report := collectCapabilities(w.cfg)
	if err := w.client.RegisterCapabilities(ctxReport, report); err != nil {
		log.Printf("worker 能力上报失败（不影响接活）: %v", err)
		return
	}
	log.Printf("worker 能力已上报：engine=%s caps=%s", report.Engine, strings.Join(report.Capabilities, ","))
}

// waitForWork 空闲等待：轮询间隔到点，或实时唤醒提前结束。
func (w *Worker) waitForWork(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(pollInterval):
	case <-w.link.wake:
		log.Printf("收到实时唤醒，立即领取任务")
	}
}

// watchCancel 消费实时取消：命中当前任务就终止其执行上下文。
func (w *Worker) watchCancel(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-w.link.cancel:
			w.cancelCurrentRun(id, "收到服务端实时取消")
		}
	}
}

func (w *Worker) cancelCurrentRun(id int64, reason string) bool {
	w.mu.Lock()
	if w.curTask != id || w.curCancel == nil {
		w.mu.Unlock()
		return false
	}
	w.curKilled = true
	cancel := w.curCancel
	w.mu.Unlock()
	cancel()
	log.Printf("执行 #%d 已失去租约，正在终止（%s）", id, reason)
	return true
}

func (w *Worker) registerRun(id int64, cancel context.CancelFunc) {
	w.mu.Lock()
	w.curTask, w.curCancel, w.curKilled = id, cancel, false
	w.mu.Unlock()
}

func (w *Worker) unregisterRun() (killed bool) {
	w.mu.Lock()
	killed = w.curKilled
	w.curTask, w.curCancel, w.curKilled = 0, nil, false
	w.mu.Unlock()
	return killed
}

// killed 查当前任务是否被服务端取消（只读，不清状态；清理交给 defer unregisterRun）。
func (w *Worker) killed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.curKilled
}

// execute 在 PTY 里驱动交互式 CLI 完成任务：warmup → 粘贴任务 → 等回复结束 →
// 解析收尾（不合格就补提醒）→ 提交验收。进度以屏幕快照周期回传。
func (w *Worker) execute(ctx context.Context, task *Run, knowledge, history []string) {
	dir, err := w.workDir(task)
	if err != nil {
		w.failTask(ctx, task, "创建工作目录失败: "+err.Error(), task.Session, "")
		return
	}
	marks := newCompletionMarks()
	prompt := buildPromptWithMarks(task, knowledge, history, marks)

	// registerRun 覆盖整个「下载附件 → 跑 CLI → 上传产物」生命周期，且这三段都
	// 走 runCtx：服务端 cancel 能中断在途的大文件收发，全程受 taskTimeout 约束。
	runCtx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	w.registerRun(task.ID, cancel)
	defer w.unregisterRun()
	go w.keepRunLease(runCtx, task)

	if err := w.prepareFiles(runCtx, task, dir); err != nil {
		if w.killed() {
			log.Printf("执行 #%d 在下载输入时被取消", task.ID)
			return
		}
		w.failTask(ctx, task, "下载任务附件失败: "+err.Error(), task.Session, dir)
		return
	}
	if strings.TrimSpace(task.Command) != "" {
		w.executeCommand(ctx, runCtx, task, dir)
		return
	}
	if w.cfg.Engine == engineBuiltin {
		w.executeBuiltin(ctx, runCtx, task, knowledge, history, dir)
		return
	}

	sessionStartedAt := time.Now()
	invocation := w.cliInvocationFor(task.Session, dir)
	task.Session.EngineRuntimeFingerprint = invocation.RuntimeFingerprint
	persistNewSession := invocation.ResumeRef == ""
	if persistNewSession {
		// Do not accidentally submit a stale ref if the fresh CLI fails before
		// its new native session metadata becomes visible.
		task.Session.EngineSessionRef = ""
	}
	if invocation.ResumeRef != "" {
		log.Printf("worker 会话 #%d scope=%s 恢复 %s 原生会话 %s", task.Session.ID, task.Session.ScopeKey, w.cfg.Engine, invocation.ResumeRef)
	}
	sess, err := startSession(runCtx, dir, w.cfg.Bin, invocation.Args...)
	if err != nil {
		if w.killed() {
			return
		}
		w.failTask(ctx, task, "启动 "+w.cfg.Bin+" 失败: "+err.Error(), task.Session, dir)
		return
	}
	warmup(runCtx, sess.Screen, sess.Write)
	if invocation.ResumeRef != "" && !sess.Alive() {
		log.Printf("恢复 %s 原生会话 %s 失败，改用新交互会话", w.cfg.Engine, invocation.ResumeRef)
		sess.Kill()
		sessionStartedAt = time.Now()
		persistNewSession = true
		sess, err = startSession(runCtx, dir, w.cfg.Bin, w.cliArgs()...)
		if err != nil {
			if w.killed() {
				return
			}
			w.failTask(ctx, task, "恢复会话失败，重新启动 "+w.cfg.Bin+" 也失败: "+err.Error(), task.Session, dir)
			return
		}
		warmup(runCtx, sess.Screen, sess.Write)
	}
	defer sess.Kill()
	if persistNewSession {
		go w.persistNativeSession(runCtx, task, dir, sessionStartedAt)
	}

	// 周期回传屏幕快照当进度（有变化才发）。
	stopProg := make(chan struct{})
	defer close(stopProg)
	go w.relayProgress(ctx, task.ID, task.ClaimID, sess, stopProg)

	supervisor := newAgentSupervisor(w, task, knowledge, history, dir)
	screen, werr := sess.submitAndWait(runCtx, prompt, w.wait)
	turns := 1
	var summary, lessons, question string
	var ok, needsInput bool
	submitTurn := func(continuation string) (string, error) {
		turns++
		log.Printf("任务 #%d 尚未收尾，继续自主执行（交互回合 %d）", task.ID, turns)
		return sess.submitAndWait(runCtx, continuation, w.wait)
	}
	reviewFailures := map[string]int{}
	for {
		screen, summary, lessons, question, ok, needsInput, werr = driveAgentTurnsWithJudge(
			screen, werr, marks, submitTurn,
			func(current string) (agentTurnAssessment, error) {
				assessment, err := supervisor.assessTurn(runCtx, current)
				if err == nil && assessment.Evaluated && assessment.stalled() {
					w.report(ctx, task.ID, task.ClaimID, "🧭 执行监督发现没有形成新进展："+assessment.Reason)
				}
				return assessment, err
			},
		)
		if !ok || needsInput || werr != nil {
			break
		}

		assessment, reviewErr := supervisor.reviewCompletion(runCtx, summary, screen)
		if reviewErr != nil {
			ok = false
			werr = fmt.Errorf("独立完成评估不可用: %w", reviewErr)
			break
		}
		if assessment.passed() {
			break
		}

		reviewFailures[assessment.Signature]++
		w.report(ctx, task.ID, task.ClaimID, "🔎 独立完成评估要求返工："+assessment.Reason)
		if reviewFailures[assessment.Signature] >= maxRepeatedReviewFailures {
			ok = false
			werr = fmt.Errorf("独立评估连续指出同一未解决问题（%s）: %s", assessment.Signature, assessment.Reason)
			break
		}
		// 返工必须换一组完成 nonce。否则终端仍保留上一版完成块时，解析器会在
		// 新回复尚未收尾前重新捞到旧块，把旧交付再次送审。
		marks = newCompletionMarks()
		screen, werr = submitTurn(agentRevisionWithMarks(marks, assessment))
	}

	// 被服务端取消（任务已删或改派）：报告后直接退出，不上传不提交。
	if w.killed() {
		w.report(ctx, task.ID, task.ClaimID, "⛔ 任务被服务端取消，已终止执行。")
		log.Printf("任务 #%d 已按服务端指令取消", task.ID)
		return
	}
	if needsInput && (!ok || strings.LastIndex(screen, marks.NeedInput) > strings.LastIndex(screen, marks.Summary)) {
		submitSession := task.Session
		if ref := w.detectEngineSessionRef(dir, sessionStartedAt); ref != "" {
			submitSession.EngineSessionRef = ref
		}
		if err := w.client.RequestInput(ctx, task.ID, task.ClaimID, question, submitSession, dir); err != nil {
			log.Printf("任务 #%d 请求补充信息失败: %v", task.ID, err)
			w.failTask(ctx, task, "请求补充信息失败: "+err.Error(), submitSession, dir)
			return
		}
		log.Printf("任务 #%d 已暂停，等待分配者补充信息", task.ID)
		return
	}
	if !ok {
		note := "Agent 未按协议返回完成或补充信息标记"
		if werr != nil {
			note = "Agent 执行中断: " + werr.Error()
		}
		if tail := tailLines(screen, 12); tail != "" {
			note += "；最后屏幕：\n" + tail
		}
		submitSession := task.Session
		if ref := w.detectEngineSessionRef(dir, sessionStartedAt); ref != "" {
			submitSession.EngineSessionRef = ref
		}
		w.failTask(ctx, task, note, submitSession, dir)
		return
	}
	summary = w.appendArtifactReport(runCtx, task, dir, summary)
	if w.killed() {
		log.Printf("执行 #%d 在上传产物时被取消", task.ID)
		return
	}
	submitSession := task.Session
	if ref := w.detectEngineSessionRef(dir, sessionStartedAt); ref != "" {
		submitSession.EngineSessionRef = ref
	}
	if err := w.client.Submit(ctx, task.ID, task.ClaimID, summary, lessons, submitSession, dir,
		SubmissionResult{Outcome: workerproto.OutcomeSucceeded}); err != nil {
		log.Printf("提交任务 #%d 失败: %v", task.ID, err)
		w.failTask(ctx, task, "提交任务结果失败: "+err.Error(), submitSession, dir)
		w.handoffDeferredRestart()
		return
	}
	log.Printf("任务 #%d 已提交结果", task.ID)
	w.handoffDeferredRestart()
}

// driveAgentTurns retains the deterministic screen-fingerprint fallback used
// by tests and callers without a semantic supervisor. Production CLI execution
// uses driveAgentTurnsWithJudge so changed prose or file churn is not mistaken
// for verified progress; the outer task context remains the wall-clock bound.
func driveAgentTurns(initial string, initialErr error, marks completionMarks, submit func(string) (string, error)) (
	screen, summary, lessons, question string, ok, needsInput bool, err error,
) {
	return driveAgentTurnsWithJudge(initial, initialErr, marks, submit, nil)
}

func driveAgentTurnsWithJudge(initial string, initialErr error, marks completionMarks, submit func(string) (string, error),
	judge func(string) (agentTurnAssessment, error),
) (screen, summary, lessons, question string, ok, needsInput bool, err error) {
	screen, err = initial, initialErr
	continuation := agentContinuationWithMarks(marks)
	lastProgress := agentTurnFingerprint(screen, continuation)
	stalled := 0
	lastStallSignature := ""
	for {
		summary, lessons, ok = parseCompletionWithMarks(screen, marks)
		question, needsInput = parseInputRequestWithMarks(screen, marks)
		if ok || needsInput || err != nil {
			return
		}

		nextPrompt := continuation
		judgeFallback := judge == nil
		if judge != nil {
			assessment, judgeErr := judge(screen)
			judgeFallback = judgeErr != nil
			if judgeErr == nil && assessment.Evaluated {
				switch {
				case assessment.stalled():
					if assessment.Signature == lastStallSignature {
						stalled++
					} else {
						lastStallSignature = assessment.Signature
						stalled = 1
					}
					if stalled >= maxStalledTurns {
						err = fmt.Errorf("%w: %s", errAgentNoProgress, assessment.Reason)
						return
					}
					nextPrompt = agentRecoveryContinuationWithMarks(marks, assessment)
				case assessment.progressing():
					stalled = 0
					lastStallSignature = ""
				}
			}
		}

		next, submitErr := submit(nextPrompt)
		fingerprint := agentTurnFingerprint(next, continuation)
		if judgeFallback {
			if fingerprint == lastProgress {
				stalled++
			} else {
				stalled = 0
				lastProgress = fingerprint
			}
		}
		screen, err = next, submitErr
		if err == nil && judgeFallback && stalled >= maxStalledTurns {
			err = errAgentNoProgress
		}
	}
}

func agentTurnFingerprint(screen, continuation string) string {
	normalized := strings.Join(strings.Fields(denoise(screen)), " ")
	prompt := strings.Join(strings.Fields(denoise(continuation)), " ")
	if prompt != "" {
		if i := strings.LastIndex(normalized, prompt); i >= 0 {
			// 只观察本轮续跑提示之后的内容。终端保留多少条旧提示、是否滚屏，
			// 都不应让 Worker 自己发送的文本成为新进展。
			normalized = normalized[i+len(prompt):]
		} else {
			normalized = strings.Join(strings.Fields(denoise(tailLines(screen, 32))), " ")
		}
	}
	// Codex 在用户输入前绘制该提示符。续跑提示被移除后若只剩提示符，
	// 它也不能成为“进展”；真实 Agent 输出使用另一套项目符号。
	normalized = strings.ReplaceAll(normalized, "›", " ")
	return strings.Join(strings.Fields(normalized), " ")
}

func (w *Worker) keepRunLease(ctx context.Context, run *Run) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			heartbeatCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := w.client.Heartbeat(heartbeatCtx, run.ID, run.ClaimID)
			cancel()
			if errors.Is(err, errWorkerLeaseLost) {
				w.cancelCurrentRun(run.ID, "续租被服务端拒绝")
				return
			}
			if err != nil && ctx.Err() == nil {
				log.Printf("执行 #%d 续租失败（下一周期重试）: %v", run.ID, err)
			}
		}
	}
}

func (w *Worker) executeCommand(ctx, runCtx context.Context, task *Run, dir string) {
	command := strings.TrimSpace(task.Command)
	if err := writeCommandScript(dir, command); err != nil {
		w.report(ctx, task.ID, task.ClaimID, "⚠️ 保存命令记录失败: "+err.Error())
	}
	mode := "pipe"
	run := runCommandExec
	if task.CommandPTY {
		mode = "pty"
		run = runCommandPTY
	}
	w.report(ctx, task.ID, task.ClaimID, "🖥 开始执行命令（"+mode+"）：\n"+command)
	res, err := run(runCtx, dir, command, func(chunk string) {
		if strings.TrimSpace(chunk) != "" {
			w.report(ctx, task.ID, task.ClaimID, "🖥 命令输出：\n"+chunk)
		}
	})
	if w.killed() {
		w.report(ctx, task.ID, task.ClaimID, "⛔ 任务被服务端取消，命令已终止。")
		log.Printf("命令任务 #%d 已按服务端指令取消", task.ID)
		return
	}
	summary := commandSummary(command, mode, res, err)
	if err != nil {
		// A normal non-zero command exit is represented by res.ExitCode with a nil
		// error and remains a reviewable result. Context expiry, process startup,
		// and PTY infrastructure failures are execution failures: release the claim
		// through the server's durable retry policy instead of marking work done.
		w.failTask(ctx, task, "命令执行基础设施失败: "+summary, task.Session, dir)
		return
	}
	summary = w.appendArtifactReport(runCtx, task, dir, summary)
	if w.killed() {
		log.Printf("命令执行 #%d 在上传产物时被取消", task.ID)
		return
	}
	outcome := workerproto.OutcomeSucceeded
	if res.ExitCode != 0 {
		outcome = workerproto.OutcomeFailed
	}
	// Deterministic commands produce execution evidence, not reusable lessons.
	// Learning remains reserved for an agent's explicit, task-derived findings.
	if err := w.client.Submit(ctx, task.ID, task.ClaimID, summary, "", task.Session, dir,
		SubmissionResult{Outcome: outcome, ExitCode: &res.ExitCode}); err != nil {
		log.Printf("提交命令任务 #%d 失败: %v", task.ID, err)
		w.failTask(ctx, task, "提交命令任务结果失败: "+err.Error(), task.Session, dir)
		w.handoffDeferredRestart()
		return
	}
	log.Printf("命令任务 #%d 已提交结果（exit=%d）", task.ID, res.ExitCode)
	w.handoffDeferredRestart()
}

func (w *Worker) failTask(ctx context.Context, task *Run, cause string, session SessionInfo, workdir string) {
	if task == nil || strings.TrimSpace(task.ClaimID) == "" {
		return
	}
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := w.client.Fail(failCtx, task.ID, task.ClaimID, cause, session, workdir); err != nil {
		// A conflict commonly means a previous request reached the server but its
		// response was lost. The claim lease remains the final recovery fallback.
		log.Printf("记录任务 #%d 执行失败状态未确认: %v", task.ID, err)
		return
	}
	log.Printf("任务 #%d 执行失败，已交由服务端退避重试", task.ID)
}

func (w *Worker) handoffDeferredRestart() {
	if scheduled, err := scheduleDeferredWorkerRestart(); err != nil {
		log.Printf("延迟重启 worker 交接失败，将保留标记供后续重试: %v", err)
	} else if scheduled {
		log.Printf("任务结果已提交，worker 服务将在后台重启以加载新版本")
	}
}

func (w *Worker) persistNativeSession(ctx context.Context, task *Run, dir string, since time.Time) {
	if task == nil || task.Session.ID <= 0 || strings.TrimSpace(task.ClaimID) == "" {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ref := w.detectEngineSessionRef(dir, since); ref != "" {
			session := task.Session
			session.EngineSessionRef = ref
			persistCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := w.client.UpdateSession(persistCtx, task.ID, task.ClaimID, session, dir)
			cancel()
			if err == nil {
				log.Printf("任务 #%d 已提前保存 %s 原生会话 %s", task.ID, w.cfg.Engine, ref)
				return
			}
			if ctx.Err() == nil {
				log.Printf("任务 #%d 提前保存原生会话失败，将重试: %v", task.ID, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) appendArtifactReport(ctx context.Context, task *Run, dir, summary string) string {
	uploaded, failed, rejected, uerr := w.uploadArtifacts(ctx, task.ID, task.ClaimID, filepath.Join(dir, taskArtifactRelDir()))
	if uerr != nil {
		w.report(ctx, task.ID, task.ClaimID, "⚠️ 遍历产物目录出错: "+uerr.Error())
	}
	if len(uploaded) > 0 {
		summary += "\n\n已上传产物：\n- " + strings.Join(uploaded, "\n- ")
	}
	if len(failed) > 0 {
		// 任务提交后 claim 失效、无法再补传产物。必须在验收报告里显式点名失败的
		// 产物，否则「按完成提交但交付物缺失」会被静默吞掉。
		warn := "⚠️ 以下产物上传失败，交付不完整，请打回要求重新提交：\n- " + strings.Join(failed, "\n- ")
		w.report(ctx, task.ID, task.ClaimID, warn)
		summary += "\n\n" + warn
	}
	if len(rejected) > 0 {
		// 软链接/硬链接/非常规文件：安全策略拒绝上传，如实告知（也是异常信号）。
		warn := "⚠️ 以下产物因安全策略被拒（软链接/硬链接/非常规文件，不上传）：\n- " + strings.Join(rejected, "\n- ")
		w.report(ctx, task.ID, task.ClaimID, warn)
		summary += "\n\n" + warn
	}
	return summary
}

// relayProgress 周期采样渲染屏幕，尾部内容有变化就回传一段快照。
func (w *Worker) relayProgress(ctx context.Context, taskID int64, claimID string, sess *cliSession, stop <-chan struct{}) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	var last string
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cur := tailLines(sess.Screen(), progressTail)
		if cur == "" || cur == last {
			continue
		}
		last = cur
		w.report(ctx, taskID, claimID, "🖥 屏幕快照：\n"+cur)
	}
}

// cliArgs 按引擎拼交互式命令行。严禁使用 claude -p / codex exec 等非交互入口；
// worker 必须像人一样在 PTY 里操作 CLI，只把任务文本写进终端。
// 自定义引擎（swarm harness 等）用 cfg.Args 指定启动参数，覆盖内置默认。
func (w *Worker) cliArgs() []string {
	if len(w.cfg.Args) > 0 {
		return w.cfg.Args
	}
	switch w.cfg.Engine {
	case "codex":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	default: // claude
		return []string{"--dangerously-skip-permissions"}
	}
}

func (w *Worker) workDir(task *Run) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if task != nil && strings.TrimSpace(task.Session.ScopeKey) != "" {
		if dir := w.configuredWorkspace(task.Session); dir != "" {
			return dir, os.MkdirAll(dir, 0o700)
		}
		if dir := strings.TrimSpace(task.Session.Workdir); dir != "" {
			return dir, os.MkdirAll(dir, 0o700)
		}
		dir := filepath.Join(home, "nbco-work", "sessions", safeScopePath(task.Session.Engine), safeScopePath(task.Session.ScopeKey))
		return dir, os.MkdirAll(dir, 0o700)
	}
	taskID := int64(0)
	claimID := ""
	if task != nil {
		taskID = task.ID
		claimID = task.ClaimID
	}
	claim := safeFileName(claimID)
	if claim == "" {
		claim = "no-claim"
	}
	dir := filepath.Join(home, "nbco-work", fmt.Sprintf("task-%d", taskID), "claim-"+claim)
	return dir, os.MkdirAll(dir, 0o700)
}

func (w *Worker) configuredWorkspace(session SessionInfo) string {
	if len(w.cfg.SessionWorkspaces) == 0 {
		return ""
	}
	for _, key := range []string{session.ScopeKey, session.ScopeType, session.Title} {
		if dir := strings.TrimSpace(w.cfg.SessionWorkspaces[key]); dir != "" {
			return dir
		}
	}
	return ""
}

func safeScopePath(v string) string {
	original := strings.TrimSpace(v)
	v = original
	v = strings.NewReplacer(":", "-", "/", "-", "\\", "-", " ", "-").Replace(v)
	v = safeFileName(v)
	if v == "" {
		if original == "" {
			return "default"
		}
		sum := sha256.Sum256([]byte(original))
		return "default-" + hex.EncodeToString(sum[:5])
	}
	if v != original {
		sum := sha256.Sum256([]byte(original))
		v += "-" + hex.EncodeToString(sum[:5])
	}
	return v
}

func taskAttachmentRelDir(kind string) string {
	if kind == "previous_artifact" {
		return filepath.ToSlash(filepath.Join(taskIORelDir, "previous_artifacts"))
	}
	return filepath.ToSlash(filepath.Join(taskIORelDir, "attachments"))
}

func taskArtifactRelDir() string {
	return filepath.ToSlash(filepath.Join(taskIORelDir, "artifacts"))
}

func (w *Worker) prepareFiles(ctx context.Context, task *Run, dir string) error {
	ioDir := filepath.Join(dir, taskIORelDir)
	if err := os.RemoveAll(ioDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, taskAttachmentRelDir("attachment")), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, taskAttachmentRelDir("previous_artifact")), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, taskArtifactRelDir()), 0o700); err != nil {
		return err
	}
	if len(task.Attachments) == 0 {
		return nil
	}
	for i := range task.Attachments {
		a := &task.Attachments[i]
		name := attachmentFileName(*a)
		subdir := taskAttachmentRelDir(a.Kind)
		dst := filepath.Join(dir, subdir, name)
		if err := w.client.DownloadFile(ctx, a.DownloadURL, dst); err != nil {
			return fmt.Errorf("%s: %w", a.OriginalName, err)
		}
		if a.SHA256 != "" {
			ok, err := verifySHA256(dst, a.SHA256)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s sha256 不匹配", a.OriginalName)
			}
		}
		a.LocalPath = filepath.ToSlash(filepath.Join(subdir, name))
	}
	return nil
}

func attachmentFileName(a Attachment) string {
	name := safeFileName(a.OriginalName)
	if name == "" {
		name = fmt.Sprintf("file-%d", a.ID)
	}
	return fmt.Sprintf("%d-%s", a.ID, name)
}

// uploadArtifacts 上传 dir 下的产物文件。单个失败不中断其余（避免「第 N 个失败
// → 后面的全被跳过 → 静默丢交付物」），分别返回成功、失败与因安全策略被拒的清单。
func (w *Worker) uploadArtifacts(ctx context.Context, taskID int64, claimID, dir string) (uploaded, failed, rejected []string, err error) {
	entries, err := artifactEntries(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, path := range entries {
		rel, _ := filepath.Rel(dir, path)
		relSlash := filepath.ToSlash(rel)
		// 安全打开：O_NOFOLLOW 拒最终路径段为软链接、fstat 校验常规文件且 nlink==1
		// （挡软链接、硬链接、FIFO、设备）。注意这是【纵深加固而非安全边界】：worker
		// 与其模型 CLI 同为一个机器账号、CLI 带 --dangerously-skip-permissions 有完整
		// shell，模型只要 `cp ~/.nbco-worker.json .nbco-task/current/artifacts/x`（普通文件）就能外泄，
		// 且中间目录段被换成软链接的 TOCTOU 仍可绕过（O_NOFOLLOW 只管最后一段）。
		// 真正的边界需要把 worker CLI 沙箱化（低权限 uid / 容器）。此处只挡手滑与
		// naive 外泄，把上传限定为本轮 artifacts 里的常规文件。
		f, oerr := openArtifactFile(path)
		if oerr != nil {
			log.Printf("任务 #%d 拒绝上传产物 %s: %v", taskID, relSlash, oerr)
			rejected = append(rejected, relSlash)
			continue
		}
		upErr := w.client.UploadArtifact(ctx, taskID, claimID, relSlash, f)
		_ = f.Close()
		if upErr != nil {
			log.Printf("任务 #%d 产物 %s 上传失败: %v", taskID, relSlash, upErr)
			failed = append(failed, relSlash)
			continue
		}
		uploaded = append(uploaded, relSlash)
	}
	return uploaded, failed, rejected, nil
}

// artifactEntries 枚举 dir 下的候选产物文件名（跳过目录、软链接、.tmp、点文件）。
// 真正的安全校验在 openArtifactFile（打开时的 fd）上做，这里只是廉价预筛。
func artifactEntries(dir string) ([]string, error) {
	if _, err := os.Stat(dir); errorsIsNotExist(err) {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

var unsafeFileNameRe = regexp.MustCompile(`[\x00-\x1f/\\:*?"<>|]+`)

func safeFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafeFileNameRe.ReplaceAllString(name, "_")
	name = strings.Trim(name, ". ")
	if len(name) > 160 {
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		if len(ext) > 20 {
			ext = ""
		}
		name = truncateUTF8Bytes(stem, 160-len(ext)) + ext
	}
	return name
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.ValidString(s[:maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func verifySHA256(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want), nil
}

func (w *Worker) report(ctx context.Context, taskID int64, claimID, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	if err := w.client.Progress(ctx, taskID, claimID, content); err != nil {
		log.Printf("回传进度失败: %v", err)
	}
}

// buildPrompt 把任务、历史经验与过程记录拼成给 CLI 的指令，并要求结构化收尾。
func buildPrompt(task *Run, knowledge, history []string) string {
	return buildPromptWithMarks(task, knowledge, history, defaultCompletionMarks)
}

// taskBrief 任务正文（标题/目标/描述/验收/过程记录/经验/附件清单）：
// PTY CLI 提示与内置智能体共用同一份信息组装。
func taskBrief(task *Run, knowledge, history []string) string {
	var b strings.Builder
	if task.Session.ScopeKey != "" {
		title := strings.TrimSpace(task.Session.Title)
		if title == "" {
			title = task.Session.ScopeKey
		}
		fmt.Fprintf(&b, "Worker 主题会话：%s（%s，%s）\n", title, task.Session.ScopeType, task.Session.ScopeKey)
		b.WriteString("当前工作目录是这个主题会话的持久 workspace；同一主题的后续任务会复用这里的代码、资料和上下文痕迹。\n")
		b.WriteString("每轮任务输入/输出只使用 .nbco-task/current/，不要删除或污染其他主题无关内容。\n")
		if strings.TrimSpace(task.Session.Summary) != "" {
			fmt.Fprintf(&b, "上次会话摘要：%s\n", task.Session.Summary)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "任务：%s\n", task.Title)
	if task.Goal != "" {
		fmt.Fprintf(&b, "目标（为什么做）：%s\n", task.Goal)
	}
	if task.Description != "" {
		fmt.Fprintf(&b, "要做什么：%s\n", task.Description)
	}
	if task.Acceptance != "" {
		fmt.Fprintf(&b, "验收标准：%s\n", task.Acceptance)
	}
	if len(history) > 0 {
		b.WriteString("\n此前的过程记录（若含「验收未通过」的打回理由，必须逐条对照整改）：\n")
		for _, h := range history {
			fmt.Fprintf(&b, "- %s\n", h)
		}
	}
	if len(knowledge) > 0 {
		b.WriteString("\n公司下发的规则与经验（标有「必须遵守」的是规则，其余供参考）：\n")
		for _, k := range knowledge {
			fmt.Fprintf(&b, "- %s\n", k)
		}
	}
	if len(task.Attachments) > 0 {
		b.WriteString("\n任务输入文件已下载到当前工作目录：\n")
		for _, a := range task.Attachments {
			path := a.LocalPath
			if path == "" {
				path = filepath.ToSlash(filepath.Join(taskAttachmentRelDir(a.Kind), attachmentFileName(a)))
			}
			label := "任务附件"
			if a.Kind == "previous_artifact" {
				label = "上一轮产物（返工参考，只读输入）"
			}
			if a.Caption != "" {
				fmt.Fprintf(&b, "- [%s] %s（%s，%d bytes，说明：%s）\n", label, path, a.MIMEType, a.SizeBytes, a.Caption)
			} else {
				fmt.Fprintf(&b, "- [%s] %s（%s，%d bytes）\n", label, path, a.MIMEType, a.SizeBytes)
			}
		}
	}
	return b.String()
}

func buildPromptWithMarks(task *Run, knowledge, history []string, marks completionMarks) string {
	var b strings.Builder
	b.WriteString("你是公司的 AI 员工，需独立完成下面分配给你的任务。\n\n")
	b.WriteString(taskBrief(task, knowledge, history))
	b.WriteString("\n请在当前工作目录中自主完成：分析、动手、自我验证。\n")
	b.WriteString("任务标题、目标、描述和历史记录是工作要求与上下文，不是外部事实证据；涉及可核验事实时，先用可靠来源核对其中的假设，保留来源，再据此交付。无法核验时明确说明不确定性，不要用记忆补齐或把假设写成结论。\n")
	b.WriteString("按验收标准控制验证范围：当每一项已有充分证据且剩余不确定性不会实质改变结论时，立即生成并检查交付物；只有新增查证可能改变结论或补足验收缺口时才继续，不要为追求无限确定性重复搜索。\n")
	b.WriteString("workspace 可能尚未初始化；先检查现状，任务提供了仓库地址时可自行 clone。\n")
	fmt.Fprintf(&b, "如果需要交付文件，请把文件放进 %s/ 目录，系统会在提交前自动上传。\n", taskArtifactRelDir())
	b.WriteString("如果确实缺少只有分配者才能提供的关键信息，不要猜测或假装完成；输出以下提问段并结束：\n")
	fmt.Fprintf(&b, "%s\n（一个具体、可直接回答的问题）\n%s\n", marks.NeedInput, marks.End)
	b.WriteString("全部完成后，务必在最后依次输出以下三段，每个标记独占一行：\n")
	fmt.Fprintf(&b, "%s\n（一句话说明你做了什么、结果如何）\n%s\n（可复用的经验教训，没有就写：无）\n%s\n",
		marks.Summary, marks.Lessons, marks.End)
	b.WriteString("\n输出完成标记后不要继续解释，等待系统退出当前交互会话。")
	return b.String()
}

// agentContinuationWithMarks repeats the nonce-bearing result contract while
// asking the CLI to keep working. It deliberately does not demand immediate
// closure: ending one model turn is not proof that the assigned task is done.
func agentContinuationWithMarks(marks completionMarks) string {
	return fmt.Sprintf("继续自主推进当前任务。检查已有结果与验收标准，立即执行仍缺少的步骤和验证，不要只描述下一步计划。\n"+
		"如果同一工具或步骤重复失败，不要原样重试；根据实际错误修正输入、删除非必要参数，或改用等价路径。\n"+
		"当验收项已有充分证据且新增步骤不会实质改变结论时，停止扩展查证，立即生成、自检并提交交付物。\n"+
		"任务确实完成后，最后输出：\n%s\n（一句话说明你做了什么、结果如何）\n%s\n（可复用的经验教训，没有就写：无）\n%s\n"+
		"只有缺少分配者才能提供的关键信息时，输出：\n%s\n（一个具体、可直接回答的问题）\n%s",
		marks.Summary, marks.Lessons, marks.End, marks.NeedInput, marks.End)
}

func agentRecoveryContinuationWithMarks(marks completionMarks, assessment agentTurnAssessment) string {
	return fmt.Sprintf("独立执行监督判断当前没有形成新的可验证进展。\n问题：%s\n改进方向：%s\n"+
		"不要为原做法辩解，也不要原样重复失败步骤；先检查真实状态和错误输出，再采用能改变结果的路径。\n%s",
		strings.TrimSpace(assessment.Reason), strings.TrimSpace(assessment.Guidance), agentContinuationWithMarks(marks))
}

func agentRevisionWithMarks(marks completionMarks, assessment agentTurnAssessment) string {
	return fmt.Sprintf("独立完成评估未通过，本次结果不能提交。\n未满足项：%s\n返工要求：%s\n"+
		"请直接检查并修正实际交付物，重新验证全部验收标准；不要只改总结或解释。完成后重新输出本轮完成标记。\n%s",
		strings.TrimSpace(assessment.Reason), strings.TrimSpace(assessment.Guidance), agentContinuationWithMarks(marks))
}

// parseCompletion 从渲染屏幕上解析收尾三段。粘贴的任务原文可能被 TUI 回显
// （其中也含哨兵与占位说明），因此从最后一个候选块往前找，跳过回显的指令块。
func parseCompletion(out string) (summary, lessons string, ok bool) {
	return parseCompletionWithMarks(out, defaultCompletionMarks)
}

func parseCompletionWithMarks(out string, marks completionMarks) (summary, lessons string, ok bool) {
	for si := strings.LastIndex(out, marks.Summary); si >= 0; si = strings.LastIndex(out[:si], marks.Summary) {
		rest := out[si+len(marks.Summary):]
		ei := strings.Index(rest, marks.End)
		if ei < 0 {
			continue // 块未收完（比如只回显了一半）
		}
		block := rest[:ei]
		var s, l string
		if li := strings.Index(block, marks.Lessons); li >= 0 {
			s = strings.TrimSpace(block[:li])
			l = strings.TrimSpace(block[li+len(marks.Lessons):])
		} else {
			s = strings.TrimSpace(block)
		}
		if s == "" || isPromptEcho(s) {
			continue
		}
		if l == "无" || l == "None" || isPromptEcho(l) {
			l = ""
		}
		// 屏幕折行会给文本掺进换行和成串的补位空格，归一成单空格。
		return strings.Join(strings.Fields(s), " "), strings.Join(strings.Fields(l), " "), true
	}
	return "", "", false
}

func parseInputRequestWithMarks(out string, marks completionMarks) (question string, ok bool) {
	for si := strings.LastIndex(out, marks.NeedInput); si >= 0; si = strings.LastIndex(out[:si], marks.NeedInput) {
		rest := out[si+len(marks.NeedInput):]
		ei := strings.Index(rest, marks.End)
		if ei < 0 {
			continue
		}
		question = strings.TrimSpace(rest[:ei])
		if question == "" || isInputPromptEcho(question) {
			continue
		}
		return strings.Join(strings.Fields(question), " "), true
	}
	return "", false
}

// isPromptEcho 是否是任务指令里的占位说明（回显）。屏幕换行/空格可能把文字
// 折断，先压掉空白再比对。
func isPromptEcho(s string) bool {
	flat := strings.NewReplacer("\n", "", " ", "").Replace(s)
	return strings.Contains(flat, "一句话说明你做了什么") ||
		strings.Contains(flat, "可复用的经验教训")
}

func isInputPromptEcho(s string) bool {
	flat := strings.NewReplacer("\n", "", " ", "").Replace(s)
	return strings.Contains(flat, "一个具体、可直接回答的问题")
}

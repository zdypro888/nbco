package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
)

const (
	pollInterval     = 10 * time.Second // 无活时的轮询间隔
	taskTimeout      = 2 * time.Hour    // 单任务总时限（卡死另有 waitOpts.Stuck 检测）
	progressInterval = 60 * time.Second // 屏幕快照回传间隔（有变化才发）
	progressTail     = 25               // 每次快照回传的屏幕行数
	maxNudges        = 2                // 未按格式收尾时的补提醒次数
	taskIORelDir     = ".nbco-task/current"
)

// 默认完成哨兵：测试与兼容辅助使用。真实 worker 任务会用带 nonce 的唯一哨兵，
// 避免任务描述里提前注入固定标记后被 parseCompletion 误认成完成输出。
const (
	markSummary = "<<<SUMMARY>>>"
	markLessons = "<<<LESSONS>>>"
	markEnd     = "<<<END>>>"
)

type completionMarks struct {
	Summary string
	Lessons string
	End     string
}

var defaultCompletionMarks = completionMarks{Summary: markSummary, Lessons: markLessons, End: markEnd}

func newCompletionMarks() completionMarks {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		nonce := fmt.Sprint(time.Now().UnixNano())
		return completionMarks{
			Summary: "<<<SUMMARY:" + nonce + ">>>",
			Lessons: "<<<LESSONS:" + nonce + ">>>",
			End:     "<<<END:" + nonce + ">>>",
		}
	}
	nonce := hex.EncodeToString(b[:])
	return completionMarks{
		Summary: "<<<SUMMARY:" + nonce + ">>>",
		Lessons: "<<<LESSONS:" + nonce + ">>>",
		End:     "<<<END:" + nonce + ">>>",
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
	log.Printf("领到任务 #%d：%s", task.ID, task.Title)
	w.execute(ctx, task, knowledge, history)
	return true, nil
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
			w.mu.Lock()
			if w.curTask == id && w.curCancel != nil {
				w.curKilled = true
				w.curCancel()
				log.Printf("任务 #%d 被服务端取消，正在终止", id)
			}
			w.mu.Unlock()
		}
	}
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
func (w *Worker) execute(ctx context.Context, task *Task, knowledge, history []string) {
	dir, err := w.workDir(task)
	if err != nil {
		w.report(ctx, task.ID, task.ClaimID, "创建工作目录失败: "+err.Error())
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

	if err := w.prepareFiles(runCtx, task, dir); err != nil {
		w.report(ctx, task.ID, task.ClaimID, "下载任务附件失败: "+err.Error())
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
	invocation := w.cliInvocationFor(task.Session)
	if invocation.ResumeRef != "" {
		log.Printf("worker 会话 #%d scope=%s 恢复 %s 原生会话 %s", task.Session.ID, task.Session.ScopeKey, w.cfg.Engine, invocation.ResumeRef)
	}
	sess, err := startSession(runCtx, dir, w.cfg.Bin, invocation.Args...)
	if err != nil {
		w.report(ctx, task.ID, task.ClaimID, "启动 "+w.cfg.Bin+" 失败: "+err.Error())
		return
	}
	warmup(runCtx, sess.Screen, sess.Write)
	if invocation.ResumeRef != "" && !sess.Alive() {
		log.Printf("恢复 %s 原生会话 %s 失败，改用新交互会话", w.cfg.Engine, invocation.ResumeRef)
		sess.Kill()
		sessionStartedAt = time.Now()
		sess, err = startSession(runCtx, dir, w.cfg.Bin, w.cliArgs()...)
		if err != nil {
			w.report(ctx, task.ID, task.ClaimID, "恢复会话失败，重新启动 "+w.cfg.Bin+" 也失败: "+err.Error())
			return
		}
		warmup(runCtx, sess.Screen, sess.Write)
	}
	defer sess.Kill()

	// 周期回传屏幕快照当进度（有变化才发）。
	stopProg := make(chan struct{})
	defer close(stopProg)
	go w.relayProgress(ctx, task.ID, task.ClaimID, sess, stopProg)

	screen, werr := sess.submitAndWait(runCtx, prompt, w.wait)
	summary, lessons, ok := parseCompletionWithMarks(screen, marks)
	// 长任务可能触发 CLI 上下文压缩，把开头的收尾要求挤掉——用简短提醒补一轮。
	for n := 0; !ok && werr == nil && n < maxNudges; n++ {
		log.Printf("任务 #%d 未按格式收尾，补提醒（%d/%d）", task.ID, n+1, maxNudges)
		screen, werr = sess.submitAndWait(runCtx, completionNudge, w.wait)
		summary, lessons, ok = parseCompletionWithMarks(screen, marks)
	}

	// 被服务端取消（任务已删或改派）：报告后直接退出，不上传不提交。
	if w.killed() {
		w.report(ctx, task.ID, task.ClaimID, "⛔ 任务被服务端取消，已终止执行。")
		log.Printf("任务 #%d 已按服务端指令取消", task.ID)
		return
	}
	if !ok {
		note := "任务执行结束（未按格式收尾）"
		if werr != nil {
			note = "任务执行中断（" + werr.Error() + "）"
		}
		summary = note + "，最后屏幕：\n" + tailLines(screen, 12)
	}
	summary = w.appendArtifactReport(runCtx, task, dir, summary)
	submitSession := task.Session
	if ref := w.detectEngineSessionRef(dir, sessionStartedAt); ref != "" {
		submitSession.EngineSessionRef = ref
	}
	if err := w.client.Submit(ctx, task.ID, task.ClaimID, summary, lessons, submitSession, dir); err != nil {
		log.Printf("提交任务 #%d 失败: %v", task.ID, err)
		return
	}
	log.Printf("任务 #%d 已提交验收", task.ID)
}

func (w *Worker) executeCommand(ctx, runCtx context.Context, task *Task, dir string) {
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
	summary = w.appendArtifactReport(runCtx, task, dir, summary)
	if err := w.client.Submit(ctx, task.ID, task.ClaimID, summary, "命令任务默认使用 stdout/stderr pipe 执行；需要终端交互时可显式启用 PTY；产物仍通过 "+taskArtifactRelDir()+"/ 回传。", task.Session, dir); err != nil {
		log.Printf("提交命令任务 #%d 失败: %v", task.ID, err)
		return
	}
	log.Printf("命令任务 #%d 已提交验收（exit=%d）", task.ID, res.ExitCode)
}

func (w *Worker) appendArtifactReport(ctx context.Context, task *Task, dir, summary string) string {
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

func (w *Worker) workDir(task *Task) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if task != nil && strings.TrimSpace(task.Session.ScopeKey) != "" {
		if dir := w.configuredWorkspace(task.Session); dir != "" {
			return dir, os.MkdirAll(dir, 0o755)
		}
		if dir := strings.TrimSpace(task.Session.Workdir); dir != "" {
			return dir, os.MkdirAll(dir, 0o755)
		}
		dir := filepath.Join(home, "nbco-work", "sessions", safeScopePath(task.Session.Engine), safeScopePath(task.Session.ScopeKey))
		return dir, os.MkdirAll(dir, 0o755)
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
	return dir, os.MkdirAll(dir, 0o755)
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
	v = strings.TrimSpace(v)
	v = strings.NewReplacer(":", "-", "/", "-", "\\", "-", " ", "-").Replace(v)
	v = safeFileName(v)
	if v == "" {
		return "default"
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

func (w *Worker) prepareFiles(ctx context.Context, task *Task, dir string) error {
	ioDir := filepath.Join(dir, taskIORelDir)
	if err := os.RemoveAll(ioDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, taskAttachmentRelDir("attachment")), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, taskAttachmentRelDir("previous_artifact")), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, taskArtifactRelDir()), 0o755); err != nil {
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
		upErr := w.client.UploadArtifact(ctx, taskID, claimID, filepath.Base(path), f)
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
		name = strings.TrimSuffix(name, ext)
		if len(ext) > 20 {
			ext = ""
		}
		if len(name) > 160-len(ext) {
			name = name[:160-len(ext)]
		}
		name += ext
	}
	return name
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
func buildPrompt(task *Task, knowledge, history []string) string {
	return buildPromptWithMarks(task, knowledge, history, defaultCompletionMarks)
}

// taskBrief 任务正文（标题/目标/描述/验收/过程记录/经验/附件清单）：
// PTY CLI 提示与内置智能体共用同一份信息组装。
func taskBrief(task *Task, knowledge, history []string) string {
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

func buildPromptWithMarks(task *Task, knowledge, history []string, marks completionMarks) string {
	var b strings.Builder
	b.WriteString("你是公司的 AI 员工，需独立完成下面分配给你的任务。\n\n")
	b.WriteString(taskBrief(task, knowledge, history))
	b.WriteString("\n请在当前工作目录中自主完成：分析、动手、自我验证。\n")
	fmt.Fprintf(&b, "如果需要交付文件，请把文件放进 %s/ 目录，系统会在提交前自动上传。\n", taskArtifactRelDir())
	b.WriteString("全部完成后，务必在最后依次输出以下三段，每个标记独占一行：\n")
	fmt.Fprintf(&b, "%s\n（一句话说明你做了什么、结果如何）\n%s\n（可复用的经验教训，没有就写：无）\n%s\n",
		marks.Summary, marks.Lessons, marks.End)
	b.WriteString("\n输出完成标记后不要继续解释，等待系统退出当前交互会话。")
	return b.String()
}

// completionNudge 收尾补提醒。刻意不写哨兵原文，避免它的回显被误认成收尾输出。
const completionNudge = "请现在收尾：按任务开头的要求，依次单独成行输出 SUMMARY、LESSONS、END 三个尖括号标记段（一句话总结；可复用经验，无则写：无）。"

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

// isPromptEcho 是否是任务指令里的占位说明（回显）。屏幕换行/空格可能把文字
// 折断，先压掉空白再比对。
func isPromptEcho(s string) bool {
	flat := strings.NewReplacer("\n", "", " ", "").Replace(s)
	return strings.Contains(flat, "一句话说明你做了什么") ||
		strings.Contains(flat, "可复用的经验教训")
}

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
)

// 完成哨兵：prompt 要求 CLI 收尾时依次输出这三段。
const (
	markSummary = "<<<SUMMARY>>>"
	markLessons = "<<<LESSONS>>>"
	markEnd     = "<<<END>>>"
)

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
	return &Worker{
		cfg:    cfg,
		client: newClient(cfg.Server, cfg.Token),
		wait:   defaultWaitOpts(),
		link:   newWSLink(cfg.Server, cfg.Token),
	}
}

// Loop 持续领活、执行，直到 ctx 取消。实时通道并行维护：唤醒让空闲等待立即
// 结束去认领；取消终止正在执行的任务。
func (w *Worker) Loop(ctx context.Context) {
	go w.link.run(ctx)
	go w.watchCancel(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		task, knowledge, history, err := w.client.Next(ctx)
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

// execute 在 PTY 里驱动交互式 CLI 完成任务：warmup → 粘贴任务 → 等回复结束 →
// 解析收尾（不合格就补提醒）→ 提交验收。进度以屏幕快照周期回传。
func (w *Worker) execute(ctx context.Context, task *Task, knowledge, history []string) {
	dir, err := w.workDir(task.ID)
	if err != nil {
		w.report(ctx, task.ID, task.ClaimID, "创建工作目录失败: "+err.Error())
		return
	}
	if err := w.prepareFiles(ctx, task, dir); err != nil {
		w.report(ctx, task.ID, task.ClaimID, "下载任务附件失败: "+err.Error())
		return
	}
	prompt := buildPrompt(task, knowledge, history)

	runCtx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	w.registerRun(task.ID, cancel)
	defer w.unregisterRun() // 兜底清理；正常路径在提交前已消费（幂等）
	sess, err := startSession(runCtx, dir, w.cfg.Bin, w.cliArgs()...)
	if err != nil {
		w.report(ctx, task.ID, task.ClaimID, "启动 "+w.cfg.Bin+" 失败: "+err.Error())
		return
	}
	defer sess.Kill()

	// 周期回传屏幕快照当进度（有变化才发）。
	stopProg := make(chan struct{})
	defer close(stopProg)
	go w.relayProgress(ctx, task.ID, task.ClaimID, sess, stopProg)

	warmup(runCtx, sess.Screen, sess.Write)

	screen, werr := sess.submitAndWait(runCtx, prompt, w.wait)
	summary, lessons, ok := parseCompletion(screen)
	// 长任务可能触发 CLI 上下文压缩，把开头的收尾要求挤掉——用简短提醒补一轮。
	for n := 0; !ok && werr == nil && n < maxNudges; n++ {
		log.Printf("任务 #%d 未按格式收尾，补提醒（%d/%d）", task.ID, n+1, maxNudges)
		screen, werr = sess.submitAndWait(runCtx, completionNudge, w.wait)
		summary, lessons, ok = parseCompletion(screen)
	}

	// 被服务端取消（任务已删或改派）：报告后直接退出，不提交。
	if w.unregisterRun() {
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
	uploaded, uerr := w.uploadArtifacts(ctx, task.ID, task.ClaimID, filepath.Join(dir, "artifacts"))
	if uerr != nil {
		w.report(ctx, task.ID, task.ClaimID, "⚠️ 上传产物失败: "+uerr.Error())
	}
	if len(uploaded) > 0 {
		summary += "\n\n已上传产物：\n- " + strings.Join(uploaded, "\n- ")
	}
	if err := w.client.Submit(ctx, task.ID, task.ClaimID, summary, lessons); err != nil {
		log.Printf("提交任务 #%d 失败: %v", task.ID, err)
		return
	}
	log.Printf("任务 #%d 已提交验收", task.ID)
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
func (w *Worker) cliArgs() []string {
	switch w.cfg.Engine {
	case "codex":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	default: // claude
		return []string{"--dangerously-skip-permissions"}
	}
}

func (w *Worker) workDir(taskID int64) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "nbco-work", fmt.Sprintf("task-%d", taskID))
	return dir, os.MkdirAll(dir, 0o755)
}

func (w *Worker) prepareFiles(ctx context.Context, task *Task, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "attachments"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		return err
	}
	if len(task.Attachments) == 0 {
		return nil
	}
	for i := range task.Attachments {
		a := &task.Attachments[i]
		name := safeFileName(a.OriginalName)
		if name == "" {
			name = fmt.Sprintf("file-%d", a.ID)
		}
		name = fmt.Sprintf("%d-%s", a.ID, name)
		dst := filepath.Join(dir, "attachments", name)
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
		a.LocalPath = filepath.ToSlash(filepath.Join("attachments", name))
	}
	return nil
}

func (w *Worker) uploadArtifacts(ctx context.Context, taskID int64, claimID, dir string) ([]string, error) {
	var uploaded []string
	if _, err := os.Stat(dir); errorsIsNotExist(err) {
		return nil, nil
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".tmp") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if err := w.client.UploadArtifact(ctx, taskID, claimID, path); err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		uploaded = append(uploaded, filepath.ToSlash(rel))
		return nil
	})
	return uploaded, err
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
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	return strings.EqualFold(hex.EncodeToString(sum[:]), want), nil
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
	var b strings.Builder
	b.WriteString("你是公司的 AI 员工，需独立完成下面分配给你的任务。\n\n")
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
		b.WriteString("\n公司相关经验（供参考，可能有用）：\n")
		for _, k := range knowledge {
			fmt.Fprintf(&b, "- %s\n", k)
		}
	}
	if len(task.Attachments) > 0 {
		b.WriteString("\n任务附件已下载到当前工作目录的 attachments/：\n")
		for _, a := range task.Attachments {
			path := a.LocalPath
			if path == "" {
				path = filepath.ToSlash(filepath.Join("attachments", safeFileName(a.OriginalName)))
			}
			fmt.Fprintf(&b, "- %s（%s，%d bytes）\n", path, a.MIMEType, a.SizeBytes)
		}
	}
	b.WriteString("\n请在当前工作目录中自主完成：分析、动手、自我验证。")
	b.WriteString("如果需要交付文件，请把文件放进 artifacts/ 目录，系统会在提交前自动上传。\n")
	b.WriteString("全部完成后，务必在最后依次输出以下三段，每个标记独占一行：\n")
	fmt.Fprintf(&b, "%s\n（一句话说明你做了什么、结果如何）\n%s\n（可复用的经验教训，没有就写：无）\n%s\n",
		markSummary, markLessons, markEnd)
	b.WriteString("\n输出完成标记后不要继续解释，等待系统退出当前交互会话。")
	return b.String()
}

// completionNudge 收尾补提醒。刻意不写哨兵原文，避免它的回显被误认成收尾输出。
const completionNudge = "请现在收尾：按任务开头的要求，依次单独成行输出 SUMMARY、LESSONS、END 三个尖括号标记段（一句话总结；可复用经验，无则写：无）。"

// parseCompletion 从渲染屏幕上解析收尾三段。粘贴的任务原文可能被 TUI 回显
// （其中也含哨兵与占位说明），因此从最后一个候选块往前找，跳过回显的指令块。
func parseCompletion(out string) (summary, lessons string, ok bool) {
	for si := strings.LastIndex(out, markSummary); si >= 0; si = strings.LastIndex(out[:si], markSummary) {
		rest := out[si+len(markSummary):]
		ei := strings.Index(rest, markEnd)
		if ei < 0 {
			continue // 块未收完（比如只回显了一半）
		}
		block := rest[:ei]
		var s, l string
		if li := strings.Index(block, markLessons); li >= 0 {
			s = strings.TrimSpace(block[:li])
			l = strings.TrimSpace(block[li+len(markLessons):])
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

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/creack/pty"
)

const (
	pollInterval = 10 * time.Second // 无活时的轮询间隔
	taskTimeout  = 30 * time.Minute // 单任务总时限
	flushBytes   = 1500             // 进度攒够这么多字节回传一次
	ansiEscape   = "\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07]*\x07|\x1b[@-Z\\-_]"
)

var ansiRe = regexp.MustCompile(ansiEscape)

// 完成哨兵：prompt 要求 CLI 收尾时依次输出这三段。
const (
	markSummary = "<<<SUMMARY>>>"
	markLessons = "<<<LESSONS>>>"
	markEnd     = "<<<END>>>"
)

// Worker 轮询执行循环。
type Worker struct {
	cfg    Config
	client *Client
}

// Loop 持续领活、执行，直到 ctx 取消。
func (w *Worker) Loop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		task, knowledge, err := w.client.Next(ctx)
		if err != nil {
			log.Printf("领取任务失败: %v", err)
			w.sleep(ctx, pollInterval)
			continue
		}
		if task == nil {
			w.sleep(ctx, pollInterval)
			continue
		}
		log.Printf("领到任务 #%d：%s", task.ID, task.Title)
		w.execute(ctx, task, knowledge)
	}
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// execute 用 PTY 驱动 CLI 完成任务，输出实时回传，退出后提交验收。
func (w *Worker) execute(ctx context.Context, task *Task, knowledge []string) {
	dir, err := w.workDir(task.ID)
	if err != nil {
		w.report(ctx, task.ID, "创建工作目录失败: "+err.Error())
		return
	}
	prompt := buildPrompt(task, knowledge)

	runCtx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, w.cfg.Bin, w.cliArgs(prompt)...)
	cmd.Dir = dir
	ptmx, err := pty.Start(cmd)
	if err != nil {
		w.report(ctx, task.ID, "启动 "+w.cfg.Bin+" 失败: "+err.Error())
		return
	}
	defer func() { _ = ptmx.Close() }()

	// 读 PTY 输出：累积全文（供解析哨兵）+ 节流回传进度。
	var full strings.Builder
	pending := &strings.Builder{}
	buf := make([]byte, 4096)
	for {
		n, readErr := ptmx.Read(buf)
		if n > 0 {
			clean := stripANSI(string(buf[:n]))
			full.WriteString(clean)
			pending.WriteString(clean)
			if pending.Len() >= flushBytes {
				w.report(ctx, task.ID, pending.String())
				pending.Reset()
			}
		}
		if readErr != nil {
			break // 进程退出（EOF）或读错误
		}
	}
	_ = cmd.Wait()
	if pending.Len() > 0 {
		w.report(ctx, task.ID, pending.String())
	}

	summary, lessons := parseTail(full.String())
	if summary == "" {
		summary = "任务执行结束（未解析到明确结论，请查看进度）。"
	}
	if err := w.client.Submit(ctx, task.ID, summary, lessons); err != nil {
		log.Printf("提交任务 #%d 失败: %v", task.ID, err)
		return
	}
	log.Printf("任务 #%d 已提交验收", task.ID)
}

// cliArgs 按引擎拼命令行。安全按放宽处理：让 CLI 自主执行、不卡在权限询问。
func (w *Worker) cliArgs(prompt string) []string {
	switch w.cfg.Engine {
	case "codex":
		return []string{"exec", "--dangerously-bypass-approvals-and-sandbox", prompt}
	default: // claude
		return []string{"-p", prompt, "--dangerously-skip-permissions"}
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

func (w *Worker) report(ctx context.Context, taskID int64, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	if err := w.client.Progress(ctx, taskID, content); err != nil {
		log.Printf("回传进度失败: %v", err)
	}
}

// buildPrompt 把任务与历史经验拼成给 CLI 的指令，并要求结构化收尾。
func buildPrompt(task *Task, knowledge []string) string {
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
	if len(knowledge) > 0 {
		b.WriteString("\n公司相关经验（供参考，可能有用）：\n")
		for _, k := range knowledge {
			fmt.Fprintf(&b, "- %s\n", k)
		}
	}
	b.WriteString("\n请在当前工作目录中自主完成：分析、动手、自我验证。")
	b.WriteString("全部完成后，务必在最后依次输出以下三段，每个标记独占一行：\n")
	fmt.Fprintf(&b, "%s\n（一句话说明你做了什么、结果如何）\n%s\n（可复用的经验教训，没有就写：无）\n%s\n",
		markSummary, markLessons, markEnd)
	return b.String()
}

// parseTail 从完整输出里解析收尾的 summary / lessons 段。
func parseTail(out string) (summary, lessons string) {
	si := strings.LastIndex(out, markSummary)
	if si < 0 {
		return "", ""
	}
	li := strings.LastIndex(out, markLessons)
	ei := strings.LastIndex(out, markEnd)
	if li < 0 || li < si {
		return strings.TrimSpace(out[si+len(markSummary):]), ""
	}
	summary = strings.TrimSpace(out[si+len(markSummary) : li])
	tail := out[li+len(markLessons):]
	if ei > li {
		tail = out[li+len(markLessons) : ei]
	}
	lessons = strings.TrimSpace(tail)
	if lessons == "无" || lessons == "None" {
		lessons = ""
	}
	return summary, lessons
}

// stripANSI 去掉终端转义序列，让回传的进度可读。
func stripANSI(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

var _ = io.Discard

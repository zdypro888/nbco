package main

// PTY 会话层：把 claude/codex 的交互式 TUI 跑在本进程的伪终端下，
// 原始字节流喂给内存 vt10x 终端仿真器，一切检测（就绪、忙碌、完成）
// 都读渲染后的屏幕，而不是在原始流上扒 ANSI。
// 手法来自用户已验证的 github.com/zdypro888/aibridge（internal/agent + bridge/driver）。

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

const (
	termCols = 160
	termRows = 48

	// submitEnterDelay：prompt 写入与回车之间的停顿。TUI 对突发输入有 paste
	// 防抖——连着回车一次性写入会被整体当成粘贴缓冲住，永远不会提交；
	// 停顿后单独发 \r 才被当成真实按键。
	submitEnterDelay = 250 * time.Millisecond
)

// waitOpts 控制一轮回复的完成检测（屏幕稳定性启发式）。
type waitOpts struct {
	Poll   time.Duration // 屏幕采样间隔
	Stable time.Duration // 屏幕开始变化后，连续静止多久算完成
	Settle time.Duration // 提交后等待屏幕开始变化的上限
	// Stuck 是卡死检测而非时长上限：只有「无忙碌标记且屏幕无变化」持续
	// 这么久才触发。正在干活的 CLI 永远不会被它掐掉。
	Stuck time.Duration
	// Busy 匹配 TUI 的「工作中」状态行；匹配期间无论屏幕多久没变都不算完成
	// （深度思考/慢工具调用时屏幕会静止）。
	Busy *regexp.Regexp
}

// busyRe：claude 与 codex 工作中都渲染 "esc to interrupt"，空闲时消失。
var busyRe = regexp.MustCompile(`(?i)esc to interrupt`)

func defaultWaitOpts() waitOpts {
	return waitOpts{
		Poll:   500 * time.Millisecond,
		Stable: 5 * time.Second,
		Settle: 30 * time.Second,
		Stuck:  10 * time.Minute,
		Busy:   busyRe,
	}
}

// cliSession 一个跑在 PTY 上的交互式 CLI。
type cliSession struct {
	cmd  *exec.Cmd
	ptmx *os.File

	mu     sync.Mutex
	vt     vt10x.Terminal
	closed bool

	waitOnce sync.Once
}

// startSession 在 dir 下启动交互式 CLI 并开始把输出喂给屏幕仿真器。
func startSession(ctx context.Context, dir, bin string, args ...string) (*cliSession, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: termCols, Rows: termRows})
	if err != nil {
		return nil, err
	}
	s := &cliSession{cmd: cmd, ptmx: ptmx, vt: vt10x.New(vt10x.WithSize(termCols, termRows))}
	go s.readLoop()
	return s, nil
}

func (s *cliSession) readLoop() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.mu.Lock()
			_, _ = s.vt.Write(buf[:n])
			s.mu.Unlock()
		}
		if err != nil {
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			s.waitOnce.Do(func() { _ = s.cmd.Wait() })
			return
		}
	}
}

// Screen 当前渲染后的屏幕纯文本。
func (s *cliSession) Screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vt == nil {
		return ""
	}
	return s.vt.String()
}

// Alive CLI 进程是否还活着。
func (s *cliSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

func (s *cliSession) Write(p []byte) error {
	_, err := s.ptmx.Write(p)
	return err
}

// Kill 结束会话（任务级别不需要优雅退出，直接收掉进程与 PTY）。
func (s *cliSession) Kill() {
	_ = s.ptmx.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.waitOnce.Do(func() { _ = s.cmd.Wait() })
}

// 启动期对话框：
// bypassPromptRe——claude 在新目录以 --dangerously-skip-permissions 启动时的
// Bypass Permissions 确认，默认选中「No, exit」，必须发 2 选 Yes（worker 的任务
// 目录每次都是新的，这个对话框必然出现）。
// trustPromptRe——通用 trust/onboarding 确认，回车走默认即可。
var (
	bypassPromptRe = regexp.MustCompile(`(?i)bypass permissions mode`)
	trustPromptRe  = regexp.MustCompile(`(?i)trust|press enter to continue|yes, (continue|proceed)`)
)

// warmup 等 TUI 启动完毕，并自动应答启动期对话框。尽力而为：超时就继续。
func warmup(ctx context.Context, screen func() string, write func([]byte) error) {
	deadline := time.Now().Add(25 * time.Second)
	var stableSince, lastAnswer time.Time
	answer := func(keys string) {
		_ = write([]byte(keys))
		lastAnswer = time.Now()
		stableSince = time.Time{}
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		cur := screen()
		if strings.TrimSpace(cur) == "" {
			continue
		}
		switch {
		case bypassPromptRe.MatchString(cur):
			// 数字键直接选中确认；若只是高亮则补的 \r 完成确认（多余的空回车无害）。
			answer("2\r")
			continue
		case trustPromptRe.MatchString(cur):
			answer("\r")
			continue
		}
		if stableSince.IsZero() {
			stableSince = time.Now()
		}
		if time.Since(stableSince) >= 1500*time.Millisecond && time.Since(lastAnswer) >= time.Second {
			return
		}
	}
}

// writePromptTwoStep 两步投递：bracketed paste 包住多行任务原文（防止文本里的
// 换行被当成提交），停顿后单独发 \r（防 paste 防抖吞掉提交）。
func writePromptTwoStep(write func([]byte) error, prompt string, delay time.Duration) error {
	if err := write([]byte("\x1b[200~" + prompt + "\x1b[201~")); err != nil {
		return err
	}
	time.Sleep(delay)
	return write([]byte("\r"))
}

// submitAndWait 提交一段输入并阻塞到这轮回复结束，返回最终屏幕。
// baseline 在提交前采集：WaitIdle 靠「屏幕先偏离 baseline、再持续静止」判定完成。
func (s *cliSession) submitAndWait(ctx context.Context, text string, o waitOpts) (string, error) {
	baseline := s.Screen()
	if err := writePromptTwoStep(s.Write, text, submitEnterDelay); err != nil {
		return "", err
	}
	return waitIdle(ctx, o, baseline, s.Screen)
}

// errStuck 长时间毫无动静（非工作状态）。
type errStuck struct{}

func (errStuck) Error() string { return "CLI 长时间无活动，疑似卡死" }

// waitIdle 采样渲染屏幕直到这轮回复看起来结束了：屏幕先「动起来」（回复开始
// 渲染），随后连续 Stable 不变。Busy 匹配期间视为仍在干活，永不判闲、永不判卡。
func waitIdle(ctx context.Context, o waitOpts, baseline string, screen func() string) (string, error) {
	start := time.Now()
	ticker := time.NewTicker(o.Poll)
	defer ticker.Stop()

	moved := false
	last := baseline
	var lastChange time.Time
	lastActivity := start
	settleDeadline := start.Add(o.Settle)

	for {
		select {
		case <-ctx.Done():
			return screen(), ctx.Err()
		case <-ticker.C:
		}

		cur := screen()
		busy := o.Busy != nil && o.Busy.MatchString(cur)
		if busy || cur != last {
			lastActivity = time.Now()
		}
		if o.Stuck > 0 && time.Since(lastActivity) > o.Stuck {
			return cur, errStuck{}
		}

		if !moved {
			if cur != baseline || busy {
				moved = true
				last = cur
				lastChange = time.Now()
			} else if time.Now().After(settleDeadline) && time.Since(lastActivity) >= o.Stable {
				return cur, errStuck{} // 提交后迟迟没有任何反应
			}
			continue
		}

		if busy {
			last = cur
			lastChange = time.Now()
			continue
		}
		if cur != last {
			last = cur
			lastChange = time.Now()
			continue
		}
		if time.Since(lastChange) >= o.Stable {
			return cur, nil
		}
	}
}

// tailLines 屏幕最后 n 行有效内容（去掉空行与纯装饰行）。
func tailLines(s string, n int) string {
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimRight(ln, " ")
		if strings.TrimSpace(ln) == "" || isDecorationLine(ln) {
			continue
		}
		lines = append(lines, ln)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// isDecorationLine 纯边框/提示符/分隔线（TUI 输入框等），进度回传时过滤掉。
func isDecorationLine(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '─', '│', '╭', '╮', '╰', '╯', '┌', '┐', '└', '┘', '═', '║', '>', '·', '…', '⏵', '⏸', '✳', '✽':
		default:
			return false
		}
	}
	return true
}

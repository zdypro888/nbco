package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCompletion(t *testing.T) {
	out := "干活的一堆输出……\n" +
		markSummary + "\n新建了登录页，本地验证通过\n" +
		markLessons + "\n登录页要覆盖空态和错误态\n" +
		markEnd + "\n"
	summary, lessons, ok := parseCompletion(out)
	if !ok || summary != "新建了登录页，本地验证通过" {
		t.Errorf("summary = %q ok=%v", summary, ok)
	}
	if lessons != "登录页要覆盖空态和错误态" {
		t.Errorf("lessons = %q", lessons)
	}
}

func TestParseCompletionNoLessons(t *testing.T) {
	out := markSummary + "\n改完了\n" + markLessons + "\n无\n" + markEnd
	summary, lessons, ok := parseCompletion(out)
	if !ok || summary != "改完了" || lessons != "" {
		t.Errorf("summary=%q lessons=%q ok=%v（无/None 应归空）", summary, lessons, ok)
	}
}

func TestParseCompletionMissing(t *testing.T) {
	if s, l, ok := parseCompletion("完全没有哨兵的输出"); ok || s != "" || l != "" {
		t.Errorf("无哨兵应返回 !ok: %q %q %v", s, l, ok)
	}
}

func TestParseCompletionSkipsPromptEcho(t *testing.T) {
	echo := buildPrompt(&Task{Title: "测试"}, nil)
	// 只有回显（含占位说明）：不算收尾。
	if s, _, ok := parseCompletion(echo); ok {
		t.Fatalf("只看到 prompt 回显不应判定完成: %q", s)
	}
	// 回显在前、真输出在后：取真输出。
	full := echo + "\n……真正干活……\n" +
		markSummary + "\n真结论\n" + markLessons + "\n真经验\n" + markEnd + "\n"
	s, l, ok := parseCompletion(full)
	if !ok || s != "真结论" || l != "真经验" {
		t.Errorf("应跳过回显取真输出: %q %q %v", s, l, ok)
	}
}

func TestParseCompletionEchoWrapped(t *testing.T) {
	// TUI 窄屏可能把回显的占位说明折行，仍应识别为回显。
	echo := markSummary + "\n（一句话说明你做\n了什么、结果如何）\n" + markLessons +
		"\n（可复用的经验\n教训，没有就写：无）\n" + markEnd
	if s, _, ok := parseCompletion(echo); ok {
		t.Fatalf("折行回显不应判定完成: %q", s)
	}
}

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt(
		&Task{Title: "写登录页", Goal: "让用户能登录", Description: "实现表单", Acceptance: "能提交"},
		[]string{"经验A：先看规范"},
	)
	for _, want := range []string{"写登录页", "让用户能登录", "实现表单", "能提交", "经验A：先看规范", markSummary, markEnd} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺 %q", want)
		}
	}
}

func TestCompletionNudgeHasNoSentinel(t *testing.T) {
	for _, m := range []string{markSummary, markLessons, markEnd} {
		if strings.Contains(completionNudge, m) {
			t.Errorf("补提醒不能含哨兵原文（回显会被误判成收尾）: %q", m)
		}
	}
}

func TestCliArgs(t *testing.T) {
	claude := (&Worker{cfg: Config{Engine: "claude"}}).cliArgs()
	for _, arg := range claude {
		if arg == "-p" || arg == "--print" {
			t.Errorf("claude args = %v", claude)
		}
	}
	codex := (&Worker{cfg: Config{Engine: "codex"}}).cliArgs()
	if len(codex) > 0 && codex[0] == "exec" {
		t.Errorf("codex args = %v", codex)
	}
}

// recordWriter 记录每次 Write 的内容。
type recordWriter struct {
	mu     sync.Mutex
	writes []string
}

func (r *recordWriter) write(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, string(p))
	return nil
}

func TestWritePromptTwoStep(t *testing.T) {
	var r recordWriter
	if err := writePromptTwoStep(r.write, "第一行\n第二行", 0); err != nil {
		t.Fatal(err)
	}
	if len(r.writes) != 2 {
		t.Fatalf("应分两次写入（文本、回车），got %d 次", len(r.writes))
	}
	if !strings.HasPrefix(r.writes[0], "\x1b[200~") || !strings.HasSuffix(r.writes[0], "\x1b[201~") {
		t.Errorf("第一段应是 bracketed paste: %q", r.writes[0])
	}
	if strings.Contains(r.writes[0], "\r") {
		t.Errorf("文本段不能带回车（会被 paste 防抖吞掉）: %q", r.writes[0])
	}
	if r.writes[1] != "\r" {
		t.Errorf("第二段应是单独回车: %q", r.writes[1])
	}
}

func TestWaitIdleMovedThenStable(t *testing.T) {
	frames := []string{"boot", "working", "working", "result", "result", "result", "result"}
	i := 0
	screen := func() string {
		if i < len(frames)-1 {
			i++
		}
		return frames[i]
	}
	o := waitOpts{Poll: 10 * time.Millisecond, Stable: 50 * time.Millisecond,
		Settle: time.Second, Stuck: time.Second, Busy: busyRe}
	got, err := waitIdle(context.Background(), o, "boot", screen)
	if err != nil || got != "result" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestWaitIdleBusyHoldsOpen(t *testing.T) {
	calls := 0
	screen := func() string {
		calls++
		if calls < 12 {
			return "thinking… esc to interrupt" // 屏幕静止但仍在干活
		}
		return "final answer"
	}
	o := waitOpts{Poll: 10 * time.Millisecond, Stable: 40 * time.Millisecond,
		Settle: time.Second, Stuck: time.Second, Busy: busyRe}
	got, err := waitIdle(context.Background(), o, "boot", screen)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "final answer") {
		t.Fatalf("忙碌期间被误判完成: %q", got)
	}
}

func TestWaitIdleStuck(t *testing.T) {
	o := waitOpts{Poll: 10 * time.Millisecond, Stable: 20 * time.Millisecond,
		Settle: 30 * time.Millisecond, Stuck: 50 * time.Millisecond, Busy: busyRe}
	_, err := waitIdle(context.Background(), o, "boot", func() string { return "boot" })
	if err == nil {
		t.Fatal("毫无动静应返回卡死错误")
	}
}

func TestWarmupAcceptsTrust(t *testing.T) {
	var mu sync.Mutex
	accepted := false
	screen := func() string {
		mu.Lock()
		defer mu.Unlock()
		if accepted {
			return "✻ Welcome!"
		}
		return "Do you trust the files in this folder?"
	}
	write := func(p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if string(p) == "\r" {
			accepted = true
		}
		return nil
	}
	done := make(chan struct{})
	go func() { warmup(context.Background(), screen, write); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("warmup 未在期限内结束")
	}
	if !accepted {
		t.Fatal("未自动确认 trust 提示")
	}
}

func TestWarmupAcceptsBypassDialog(t *testing.T) {
	var mu sync.Mutex
	var sent string
	screen := func() string {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(sent, "2") {
			return "✻ Welcome!"
		}
		return "WARNING: Claude Code running in Bypass Permissions mode\n❯ 1. No, exit\n  2. Yes, I accept"
	}
	write := func(p []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sent += string(p)
		return nil
	}
	done := make(chan struct{})
	go func() { warmup(context.Background(), screen, write); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("warmup 未在期限内结束")
	}
	if !strings.Contains(sent, "2") {
		t.Fatalf("未选择 Yes（发送记录 %q）", sent)
	}
	if strings.HasPrefix(sent, "\r") {
		t.Fatalf("bypass 对话框默认是 No, exit，直接回车会退出 CLI（发送记录 %q）", sent)
	}
}

// TestSessionEndToEnd 用一个假 TUI 脚本走真实 PTY 路径：
// 启动 → warmup → bracketed paste 投递 → 回显 + busy 状态 → 清掉 busy 输出收尾 →
// waitIdle 判定完成 → 跳过回显解析出真结论。
func TestSessionEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过 PTY 集成测试")
	}
	// stty -echo：真 TUI 都是 raw 模式自绘输入；关掉 tty 回显后脚本才能像真
	// TUI 一样完全控制屏幕（否则我们补发的回车会错位脚本的光标操作）。
	script := `
stty -echo
printf 'mock-tui ready\n'
IFS= read -r line
printf 'thinking... esc to interrupt\n'
sleep 1.2
printf '\033[2J\033[H'
printf '<<<SUMMARY>>>\n建好了 hello.txt\n<<<LESSONS>>>\n无\n<<<END>>>\n'
sleep 60
`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := startSession(ctx, t.TempDir(), "sh", "-c", script)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Kill()

	warmup(ctx, sess.Screen, sess.Write)

	o := waitOpts{Poll: 50 * time.Millisecond, Stable: 800 * time.Millisecond,
		Settle: 5 * time.Second, Stuck: 10 * time.Second, Busy: busyRe}
	screen, err := sess.submitAndWait(ctx, buildPrompt(&Task{ID: 1, Title: "建文件"}, nil), o)
	if err != nil {
		t.Fatalf("waitIdle: %v\n屏幕：\n%s", err, screen)
	}
	summary, lessons, ok := parseCompletion(screen)
	if !ok {
		t.Fatalf("未解析到收尾。屏幕：\n%s", screen)
	}
	if summary != "建好了 hello.txt" || lessons != "" {
		t.Fatalf("summary=%q lessons=%q", summary, lessons)
	}
}

// TestSmokeClaude 真·claude 冒烟：NBCO_SMOKE_CLAUDE=/path/to/claude 时启用。
// 在临时目录里让 claude 建一个文件并按格式收尾，验证 PTY 交互全链路。
func TestSmokeClaude(t *testing.T) {
	bin := os.Getenv("NBCO_SMOKE_CLAUDE")
	if bin == "" {
		t.Skip("设置 NBCO_SMOKE_CLAUDE=/path/to/claude 以运行真 CLI 冒烟")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sess, err := startSession(ctx, dir, bin, "--dangerously-skip-permissions")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Kill()

	warmup(ctx, sess.Screen, sess.Write)

	task := &Task{ID: 999, Title: "创建问候文件",
		Description: "在当前工作目录创建 hello.txt，内容为一行：你好nbco", Acceptance: "hello.txt 存在且内容正确"}
	screen, err := sess.submitAndWait(ctx, buildPrompt(task, nil), defaultWaitOpts())
	if err != nil {
		t.Fatalf("waitIdle: %v\n屏幕尾部：\n%s", err, tailLines(screen, 30))
	}
	summary, _, ok := parseCompletion(screen)
	if !ok {
		t.Fatalf("未解析到收尾。屏幕尾部：\n%s", tailLines(screen, 40))
	}
	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("claude 没建出 hello.txt: %v\nsummary=%q", err, summary)
	}
	if !strings.Contains(string(data), "你好nbco") {
		t.Fatalf("hello.txt 内容不对: %q", string(data))
	}
	t.Logf("冒烟通过。summary=%q", summary)
}

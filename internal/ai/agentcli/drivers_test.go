package agentcli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/internal/ai"
)

// setTempCache 把 workDir（os.UserCacheDir）重定向到测试临时目录。
func setTempCache(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)                                   // darwin
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache")) // linux
}

// argAfter 取 args 中 flag 的下一个值。
func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// --- claude ---

func TestClaudeDriverPlanArgs(t *testing.T) {
	setTempCache(t)
	d := &ClaudeDriver{Cmd: "claude", Model: "opus"}
	req := &ai.TurnRequest{System: "系统提示", UserText: "用户输入", EngineSession: "sess-9"}
	plan, err := d.Plan(req, "http://x/mcp/cli/tok")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Bin != "claude" {
		t.Errorf("Bin = %q", plan.Bin)
	}
	if v, ok := argAfter(plan.Args, "--model"); !ok || v != "opus" {
		t.Errorf("--model = %q ok=%v", v, ok)
	}
	if v, ok := argAfter(plan.Args, "--resume"); !ok || v != "sess-9" {
		t.Errorf("--resume = %q ok=%v", v, ok)
	}
	if v, ok := argAfter(plan.Args, "--system-prompt"); !ok || v != "系统提示" {
		t.Errorf("--system-prompt = %q ok=%v", v, ok)
	}
	// MCP 回连配置内联注入。
	raw, ok := argAfter(plan.Args, "--mcp-config")
	if !ok {
		t.Fatal("缺 --mcp-config")
	}
	var mcpCfg struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &mcpCfg); err != nil {
		t.Fatalf("--mcp-config 不是合法 JSON: %v", err)
	}
	if srv := mcpCfg.MCPServers["nbco"]; srv.Type != "http" || srv.URL != "http://x/mcp/cli/tok" {
		t.Errorf("nbco server = %+v", srv)
	}
	// 用户输入走 stdin，不应出现在命令行。
	if slices.Contains(plan.Args, "用户输入") {
		t.Error("UserText 不应进命令行")
	}
}

func TestClaudeDriverPlanNoResume(t *testing.T) {
	setTempCache(t)
	d := &ClaudeDriver{Cmd: "claude"}
	plan, err := d.Plan(&ai.TurnRequest{}, "http://x/mcp/cli/tok")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(plan.Args, "--resume") || slices.Contains(plan.Args, "--model") {
		t.Errorf("新会话且未配模型时不应有 --resume/--model: %v", plan.Args)
	}
}

func newClaudePlan(t *testing.T) *Plan {
	t.Helper()
	setTempCache(t)
	plan, err := (&ClaudeDriver{Cmd: "claude"}).Plan(&ai.TurnRequest{}, "http://x/mcp/cli/tok")
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestClaudeDriverParseSuccess(t *testing.T) {
	plan := newClaudePlan(t)
	for _, line := range []string{
		`{"type":"system","subtype":"init"}`,
		`不是 JSON 的杂音行`,
		`{"type":"assistant","message":{}}`,
		`{"type":"result","subtype":"success","result":"  你好  ","session_id":"s1","usage":{"input_tokens":10,"output_tokens":5}}`,
	} {
		plan.Consume([]byte(line))
	}
	res, err := plan.Finish(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "你好" || res.EngineSession != "s1" {
		t.Errorf("res = %+v", res)
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v", res.Usage)
	}
}

func TestClaudeDriverParseErrorResult(t *testing.T) {
	plan := newClaudePlan(t)
	plan.Consume([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"boom"}`))
	_, err := plan.Finish(nil, "")
	if err == nil || !strings.Contains(err.Error(), "error_max_turns") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("错误应带 subtype 与 result, got %v", err)
	}
}

func TestClaudeDriverParseNoResult(t *testing.T) {
	plan := newClaudePlan(t)
	if _, err := plan.Finish(nil, "标准错误内容"); err == nil || !strings.Contains(err.Error(), "未输出 result") {
		t.Errorf("无 result 事件应报错, got %v", err)
	}
	plan = newClaudePlan(t)
	if _, err := plan.Finish(errors.New("exit 1"), "标准错误内容"); err == nil || !strings.Contains(err.Error(), "标准错误内容") {
		t.Errorf("进程失败应带 stderr, got %v", err)
	}
}

// --- codex ---

func TestCodexDriverPlan(t *testing.T) {
	setTempCache(t)
	// 预置上一轮残留的输出文件，Plan 必须清掉（不可作为本轮兜底）。
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, "nbco", "codex", "s42")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "last-message.txt")
	if err := os.WriteFile(stale, []byte("上轮残留"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := &CodexDriver{Cmd: "codex", Model: "o4"}
	req := &ai.TurnRequest{SessionID: "42", System: "系统提示", EngineSession: "th-1"}
	plan, err := d.Plan(req, "http://x/mcp/cli/tok")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dir != dir {
		t.Errorf("Dir = %q, want %q", plan.Dir, dir)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("残留的 last-message.txt 应被清除")
	}
	// system prompt 落到会话独占目录的 AGENTS.md。
	if got, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")); err != nil || string(got) != "系统提示" {
		t.Errorf("AGENTS.md = %q, err=%v", got, err)
	}
	if v, ok := argAfter(plan.Args, "-m"); !ok || v != "o4" {
		t.Errorf("-m = %q ok=%v", v, ok)
	}
	if v, ok := argAfter(plan.Args, "resume"); !ok || v != "th-1" {
		t.Errorf("resume = %q ok=%v", v, ok)
	}
	if plan.Args[len(plan.Args)-1] != "-" {
		t.Errorf("最后一个参数应是 stdin 占位 -: %v", plan.Args)
	}
	if !slices.Contains(plan.Args, `mcp_servers.nbco.url="http://x/mcp/cli/tok"`) {
		t.Errorf("MCP 回连地址未注入: %v", plan.Args)
	}
}

func newCodexPlan(t *testing.T) *Plan {
	t.Helper()
	setTempCache(t)
	plan, err := (&CodexDriver{Cmd: "codex"}).Plan(&ai.TurnRequest{SessionID: "7", System: "s"}, "http://x/mcp/cli/tok")
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestCodexDriverParse(t *testing.T) {
	plan := newCodexPlan(t)
	for _, line := range []string{
		`{"type":"thread.started","thread_id":"th9"}`,
		`{"type":"item.completed","item":{"type":"reasoning","text":"应忽略"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"草稿"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"最终答复"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":3}}`,
		`{"type":"turn.completed","usage":{"input_tokens":2,"output_tokens":1}}`,
	} {
		plan.Consume([]byte(line))
	}
	res, err := plan.Finish(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// 无 -o 文件时以 JSONL 里最后一条 agent_message 兜底；usage 跨 turn 累计。
	if res.Text != "最终答复" || res.EngineSession != "th9" {
		t.Errorf("res = %+v", res)
	}
	if res.Usage.InputTokens != 9 || res.Usage.OutputTokens != 4 {
		t.Errorf("Usage = %+v", res.Usage)
	}
}

func TestCodexDriverOutputFileWins(t *testing.T) {
	plan := newCodexPlan(t)
	plan.Consume([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"JSONL 答复"}}`))
	// 模拟 CLI 写出 --output-last-message 文件：以它为准。
	if err := os.WriteFile(filepath.Join(plan.Dir, "last-message.txt"), []byte(" 文件答复 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := plan.Finish(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "文件答复" {
		t.Errorf("Text = %q, -o 文件应优先", res.Text)
	}
}

func TestCodexDriverErrors(t *testing.T) {
	plan := newCodexPlan(t)
	plan.Consume([]byte(`{"type":"error","message":"额度耗尽"}`))
	if _, err := plan.Finish(errors.New("exit 1"), "stderr!"); err == nil || !strings.Contains(err.Error(), "额度耗尽") {
		t.Errorf("进程失败应优先带 error 事件内容, got %v", err)
	}

	plan = newCodexPlan(t)
	if _, err := plan.Finish(nil, "空跑"); err == nil || !strings.Contains(err.Error(), "未输出答复") {
		t.Errorf("无任何答复应报错, got %v", err)
	}
}

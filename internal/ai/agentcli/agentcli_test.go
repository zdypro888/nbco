package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/internal/ai"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.register("t1", []ai.Tool{{Name: "a"}})
	r.register("t2", []ai.Tool{{Name: "b"}})

	tools, ok := r.lookup("t1")
	if !ok || len(tools) != 1 || tools[0].Name != "a" {
		t.Errorf("t1 应查到工具 a, got %v ok=%v", tools, ok)
	}
	if _, ok := r.lookup("nope"); ok {
		t.Error("未注册 token 不应命中")
	}
	r.unregister("t1")
	if _, ok := r.lookup("t1"); ok {
		t.Error("注销后不应命中")
	}
	if _, ok := r.lookup("t2"); !ok {
		t.Error("注销 t1 不应影响 t2")
	}
}

// fakeDriver 用 sh 造一轮可控的子进程调用。
type fakeDriver struct {
	script  string // sh -c 脚本；用户输入经 stdin 到达
	mcpURL  string // Plan 收到的回连地址
	consume func(line []byte)
	finish  func(waitErr error, stderr string) (*ai.TurnResult, error)
}

func (d *fakeDriver) Name() string { return "fake" }

func (d *fakeDriver) Plan(req *ai.TurnRequest, mcpURL string) (*Plan, error) {
	d.mcpURL = mcpURL
	return &Plan{Bin: "sh", Args: []string{"-c", d.script}, Consume: d.consume, Finish: d.finish}, nil
}

func TestEngineRunTurn(t *testing.T) {
	registry := NewRegistry()
	driver := &fakeDriver{script: "cat"} // 原样回显 stdin
	eng := NewEngine(driver, registry, "http://127.0.0.1:1/mcp/cli/")

	var lines []string
	driver.consume = func(line []byte) {
		lines = append(lines, string(line))
		// 模拟 CLI 经 MCP 回连调工具：进程运行期间 token 必须已注册且工具已包轨迹层。
		token := path.Base(driver.mcpURL)
		tools, ok := registry.lookup(token)
		if !ok || len(tools) != 1 {
			t.Errorf("运行期 token 应已注册, ok=%v n=%d", ok, len(tools))
			return
		}
		if out, err := tools[0].Handler(context.Background(), json.RawMessage(`{"x":1}`)); err != nil || out != "结果文本" {
			t.Errorf("工具调用 = (%q, %v)", out, err)
		}
	}
	driver.finish = func(waitErr error, stderr string) (*ai.TurnResult, error) {
		if waitErr != nil {
			return nil, waitErr
		}
		return &ai.TurnResult{Text: strings.Join(lines, "|"), EngineSession: "es1"}, nil
	}

	var events []ai.Step
	req := &ai.TurnRequest{
		UserText: "line1\n  line2  \n\n",
		Tools: []ai.Tool{{
			Name:    "demo",
			Handler: func(context.Context, json.RawMessage) (string, error) { return "结果文本", nil },
		}},
		OnEvent: func(s ai.Step) { events = append(events, s) },
	}
	res, err := eng.RunTurn(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// stdin 管线：空行被跳过、空白被裁剪。
	if res.Text != "line1|line2" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.EngineSession != "es1" {
		t.Errorf("EngineSession = %q", res.EngineSession)
	}
	// 回连地址 = base + "/" + token。
	if !strings.HasPrefix(driver.mcpURL, "http://127.0.0.1:1/mcp/cli/") || len(path.Base(driver.mcpURL)) != 48 {
		t.Errorf("mcpURL = %q", driver.mcpURL)
	}
	// 工具轨迹（每行回显各调一次）与实时事件都应记录。
	if len(res.Steps) != 2 || res.Steps[0].ToolName != "demo" || res.Steps[0].Result != "结果文本" {
		t.Errorf("Steps = %+v", res.Steps)
	}
	if len(events) != 2 {
		t.Errorf("OnEvent 次数 = %d", len(events))
	}
	// 轮次结束 token 必须注销。
	if _, ok := registry.lookup(path.Base(driver.mcpURL)); ok {
		t.Error("轮次结束后 token 应已注销")
	}
}

func TestEngineRunTurnProcessError(t *testing.T) {
	registry := NewRegistry()
	driver := &fakeDriver{script: "echo oops >&2; exit 3", consume: func([]byte) {}}
	driver.finish = func(waitErr error, stderr string) (*ai.TurnResult, error) {
		if waitErr == nil {
			return nil, fmt.Errorf("应有 waitErr")
		}
		return nil, fmt.Errorf("CLI 失败: %w; stderr: %s", waitErr, stderr)
	}
	eng := NewEngine(driver, registry, "http://127.0.0.1:1/mcp/cli")
	_, err := eng.RunTurn(context.Background(), &ai.TurnRequest{UserText: "x"})
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("错误应带 stderr 内容, got %v", err)
	}
}

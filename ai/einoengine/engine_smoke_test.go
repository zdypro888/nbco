package einoengine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
)

// 真 exo chat 冒烟：设 NBCO_SMOKE_CHAT=1 时用 nbco.json 的 ai 配置跑一轮，验证
// 升级 eino 后中枢引擎（含 tool 循环链路）仍能出结果。默认跳过。
func TestSmokeRealChat(t *testing.T) {
	if os.Getenv("NBCO_SMOKE_CHAT") == "" {
		t.Skip("设 NBCO_SMOKE_CHAT=1 + NBCO_SMOKE_* 跑真端点 chat 冒烟")
	}
	cfg := config.AIConfig{
		Engine:   "eino",
		Provider: os.Getenv("NBCO_SMOKE_PROVIDER"),
		APIKey:   os.Getenv("NBCO_SMOKE_KEY"),
		BaseURL:  os.Getenv("NBCO_SMOKE_BASE"),
		Model:    os.Getenv("NBCO_SMOKE_MODEL"),
	}
	eng, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var deltas int
	res, err := eng.RunTurn(context.Background(), &ai.TurnRequest{
		Mode:     ai.TurnModeOneShot,
		System:   "你是测试助手，简短回答。",
		UserText: "用一句话介绍你自己",
		OnDelta:  func(string) { deltas++ },
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if strings.TrimSpace(res.Text) == "" {
		t.Fatal("回复为空")
	}
	if deltas < 2 {
		t.Errorf("流式增量块数=%d（<2，可能没走流式）", deltas)
	}
	t.Logf("chat 流式冒烟通过：%d 块增量，最终 %q", deltas, res.Text)
}

// 真端点工具循环冒烟：验证当前模型能完成 Eino 原生的
// tool_search -> deferred tool -> final response，不产生任何外部副作用。
func TestSmokeRealToolLoop(t *testing.T) {
	if os.Getenv("NBCO_SMOKE_TOOL") == "" {
		t.Skip("设 NBCO_SMOKE_TOOL=1 + NBCO_SMOKE_* 跑真端点工具循环")
	}
	cfg := config.AIConfig{
		Engine:   "eino",
		Provider: os.Getenv("NBCO_SMOKE_PROVIDER"),
		APIKey:   os.Getenv("NBCO_SMOKE_KEY"),
		BaseURL:  os.Getenv("NBCO_SMOKE_BASE"),
		Model:    os.Getenv("NBCO_SMOKE_MODEL"),
	}
	engine, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode:     ai.TurnModeDeep,
		System:   "你是工具循环测试助手。用户要求验证时，必须先搜索并调用对应工具，再根据真实结果简短回答。",
		UserText: "请使用当前可用的无副作用验证能力，把值 EINO_OK 送入测试探针并告诉我真实结果。",
		Tools: []ai.Tool{{
			Name:        "echo_probe",
			Description: "无副作用地回显 value，用于验证 agent 工具调用链。",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"value": map[string]any{"type": "string"}},
				"required":   []string{"value"},
			},
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				calls.Add(1)
				return `{"ok":true,"echo":"EINO_OK"}`, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("echo_probe calls=%d steps=%+v", calls.Load(), result.Steps)
	}
	found := false
	for _, step := range result.Steps {
		if step.ToolName == "echo_probe" && strings.Contains(step.Result, "EINO_OK") {
			found = true
		}
	}
	if !found || strings.TrimSpace(result.Text) == "" {
		t.Fatalf("tool loop incomplete: text=%q steps=%+v", result.Text, result.Steps)
	}
	t.Logf("real Eino tool loop passed: %q", result.Text)
}

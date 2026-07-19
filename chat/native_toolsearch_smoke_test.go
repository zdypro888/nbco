package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/ai/einoengine"
	"github.com/zdypro888/nbco/config"
)

func TestSmokeRealNativeToolSearch(t *testing.T) {
	if os.Getenv("NBCO_SMOKE_NATIVE_SEARCH") == "" {
		t.Skip("set NBCO_SMOKE_NATIVE_SEARCH=1 and NBCO_SMOKE_* to run real native tool search")
	}
	engine, err := einoengine.New(context.Background(), config.AIConfig{
		Engine:   config.EngineEino,
		Provider: os.Getenv("NBCO_SMOKE_PROVIDER"),
		APIKey:   os.Getenv("NBCO_SMOKE_KEY"),
		BaseURL:  os.Getenv("NBCO_SMOKE_BASE"),
		Model:    os.Getenv("NBCO_SMOKE_MODEL"),
		MaxTurns: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	catalog := []ai.Tool{
		{
			Name: "list_workers", Domain: "workers", Effect: ai.ToolEffectRead,
			Description: "列出当前用户可调用的 Worker、稳定 ID 与在线状态。",
			InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return `{"workers":[{"id":2,"name":"Worker Alpha","online":true}]}`, nil
			},
		},
		{
			Name: "delegate_worker_agent", Domain: "workers", Effect: ai.ToolEffectExecute,
			Description: "把需要持续判断的目标交给指定 Worker 上的 Codex 或 Claude Agent。",
			InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"worker_id": map[string]any{"type": "integer"},
					"task":      map[string]any{"type": "string"},
				}, "required": []string{"worker_id", "task"},
			},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				calls.Add(1)
				return "已创建并持久化 Worker Agent 任务（任务内部编号 42），分配给 Worker Alpha；主题 scope=research:event-transport。worker 会通过交互式 PTY 启动或恢复该主题的原生 Agent 会话；本轮无需重复创建，进度和完成结果会由系统通知用户。", nil
			},
		},
	}
	for i := 0; i < 172; i++ {
		catalog = append(catalog, ai.Tool{
			Name: fmt.Sprintf("unrelated_capability_%03d", i), Domain: "other", Effect: ai.ToolEffectRead,
			Description: "与当前请求无关的占位能力。", InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) { return `{}`, nil },
		})
	}
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode:     ai.TurnModeDeep,
		System:   "你是公司运营中枢，按当前用户目标使用授权工具。",
		UserText: "请让在线的 AI Worker 使用 Codex 比较 PostgreSQL LISTEN/NOTIFY 与 Redis Streams 作为内部事件传输的差异，并返回结论。",
		Tools:    catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 || strings.TrimSpace(result.Text) == "" {
		t.Fatalf("native tool search did not execute: result=%+v", result)
	}
	t.Logf("native tool search passed: calls=%d result=%q exposure=%+v steps=%+v",
		calls.Load(), result.Text, result.ToolExposure, result.Steps)
}

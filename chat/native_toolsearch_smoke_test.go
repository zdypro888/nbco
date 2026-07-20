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

func TestSmokeRealScheduleFollowUp(t *testing.T) {
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
	var scheduleCalls atomic.Int32
	var taskCalls atomic.Int32
	catalog := []ai.Tool{
		{
			Name: "list_schedules", Domain: "comms", Effect: ai.ToolEffectRead,
			Description: "查看定时提醒、计划推送和持久自动化；这些属于日程，不是普通工作任务。status 可查 active、done、cancelled 或 all。",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"status": map[string]any{"type": "string"},
			}},
			Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				scheduleCalls.Add(1)
				var args struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(raw, &args)
				if args.Status == "" || args.Status == "active" {
					return "（无活跃定时提醒或自动化）", nil
				}
				return "提醒内部编号 5 [done/once] 金色项目成员名单确认；日本时间 2026-07-20 10:30 已投递给杨桑，无未来执行。", nil
			},
		},
		{
			Name: "cancel_schedule", Domain: "comms", Effect: ai.ToolEffectWrite,
			Description: "取消尚未执行的定时提醒、计划推送或持久自动化；它们属于日程，不是普通工作任务。",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"schedule_id": map[string]any{"type": "integer"},
				"query":       map[string]any{"type": "string"},
			}},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				scheduleCalls.Add(1)
				return "日程内部编号 5 已经执行完成，没有未来投递；未重复取消，也没有删除审计记录。", nil
			},
		},
		{
			Name: "get_my_tasks", Domain: "work", Effect: ai.ToolEffectRead,
			Description: "查看当前用户作为执行人的普通工作任务，不包含定时提醒、计划推送或持久自动化；后者使用 list_schedules。",
			InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				taskCalls.Add(1)
				return "（无普通工作任务）", nil
			},
		},
	}
	for i := 0; i < 171; i++ {
		catalog = append(catalog, ai.Tool{
			Name: fmt.Sprintf("unrelated_capability_%03d", i), Domain: "other", Effect: ai.ToolEffectRead,
			Description: "与当前请求无关的占位能力。", InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) { return `{}`, nil },
		})
	}
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode: ai.TurnModeDeep,
		System: `你是公司的 AI 运营中枢。工具定义和结果是状态事实来源。
查询只读取；用户明确要求改变外部状态时，使用匹配的写入工具执行。
不同领域的对象不可混用；只有对应工具成功后才能确认状态变化。
当前时间：2026-07-20 11:03 +09:00（Asia/Tokyo）。`,
		History: []ai.Message{
			{Role: ai.RoleUser, Content: "周一把金色项目成员名单发给杨桑确认。"},
			{Role: ai.RoleAssistant, Content: "已设置单次日程，计划在日本时间 7 月 20 日 10:30 发送。"},
			{Role: ai.RoleUser, Content: "已经发了吗？"},
			{Role: ai.RoleAssistant, Content: "已发送给杨桑。"},
		},
		UserText: "这件事解决了。删除吧",
		Tools:    catalog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if scheduleCalls.Load() == 0 || taskCalls.Load() != 0 || strings.TrimSpace(result.Text) == "" {
		t.Fatalf("schedule follow-up used wrong domain: schedules=%d tasks=%d result=%q exposure=%+v steps=%+v",
			scheduleCalls.Load(), taskCalls.Load(), result.Text, result.ToolExposure, result.Steps)
	}
	t.Logf("schedule follow-up passed: schedules=%d result=%q exposure=%+v steps=%+v",
		scheduleCalls.Load(), result.Text, result.ToolExposure, result.Steps)
}

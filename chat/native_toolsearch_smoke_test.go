package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestSmokeRealWorkerChoosesAdaptiveAgentForResearch(t *testing.T) {
	if os.Getenv("NBCO_SMOKE_NATIVE_SEARCH") == "" {
		t.Skip("set NBCO_SMOKE_NATIVE_SEARCH=1 and NBCO_SMOKE_* to run real worker routing")
	}
	engine, err := einoengine.New(context.Background(), config.AIConfig{
		Engine: config.EngineEino, Provider: os.Getenv("NBCO_SMOKE_PROVIDER"), APIKey: os.Getenv("NBCO_SMOKE_KEY"),
		BaseURL: os.Getenv("NBCO_SMOKE_BASE"), Model: os.Getenv("NBCO_SMOKE_MODEL"), MaxTurns: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	var directCalls, agentCalls atomic.Int32
	tools := []ai.Tool{
		{
			Name: "list_workers", Effect: ai.ToolEffectRead, LoadMode: ai.ToolLoadImmediate,
			Description: "列出当前用户可调用的 Worker 与稳定 ID。", InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return `[{"id":2,"name":"NBAI","online":true}]`, nil
			},
		},
		{
			Name: "run_worker_command", Effect: ai.ToolEffectExecute, LoadMode: ai.ToolLoadImmediate,
			Description: "只执行退出码可完整表达技术结果的确定性原子命令。退出码不证明业务目标；需要解释输出、核验外部数据或继续判断时必须使用 delegate_worker_agent。",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"worker_id": map[string]any{"type": "integer"}, "command": map[string]any{"type": "string"},
			}, "required": []string{"worker_id", "command"}},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				directCalls.Add(1)
				return `{"status":"accepted"}`, nil
			},
		},
		{
			Name: "delegate_worker_agent", Effect: ai.ToolEffectExecute, LoadMode: ai.ToolLoadImmediate,
			Description: "把需要访问资料、解释结果、连续判断或形成可核验结论的目标交给 Worker 的交互式 PTY Agent。",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"worker_id": map[string]any{"type": "integer"}, "instruction": map[string]any{"type": "string"},
			}, "required": []string{"worker_id", "instruction"}},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				agentCalls.Add(1)
				return `{"status":"accepted","completion":"asynchronous","run_id":42}`, nil
			},
		},
	}
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode: ai.TurnModeDeep, Model: os.Getenv("NBCO_SMOKE_MODEL"),
		System:   "按目标和工具定义自主选择执行方式；需要执行时调用工具，异步受理后如实报告。",
		UserText: "再次尝试用NBAI查询世界杯结果。",
		Tools:    tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agentCalls.Load() != 1 || directCalls.Load() != 0 {
		t.Fatalf("worker research routing used wrong executor: agent=%d direct=%d result=%q steps=%+v",
			agentCalls.Load(), directCalls.Load(), result.Text, result.Steps)
	}
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

func TestSmokeRealDeliveredScheduleStatusDoesNotResend(t *testing.T) {
	if os.Getenv("NBCO_SMOKE_NATIVE_SEARCH") == "" {
		t.Skip("set NBCO_SMOKE_NATIVE_SEARCH=1 and NBCO_SMOKE_* to run real native tool search")
	}
	engine, err := einoengine.New(context.Background(), config.AIConfig{
		Engine: config.EngineEino, Provider: os.Getenv("NBCO_SMOKE_PROVIDER"), APIKey: os.Getenv("NBCO_SMOKE_KEY"),
		BaseURL: os.Getenv("NBCO_SMOKE_BASE"), Model: os.Getenv("NBCO_SMOKE_MODEL"), MaxTurns: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	var scheduleCalls atomic.Int32
	var sendCalls atomic.Int32
	catalog := []ai.Tool{
		{
			Name: "list_schedules", Domain: "comms", Effect: ai.ToolEffectRead,
			Description: "查看定时提醒、计划推送和持久自动化及其真实投递状态。status 可查 active、done、cancelled 或 all。",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"status": map[string]any{"type": "string"},
			}},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				scheduleCalls.Add(1)
				return "提醒内部编号 5 [done/once] 金色项目成员名单确认；日本时间 2026-07-20 10:30 已成功投递给杨桑，无未来执行。", nil
			},
		},
		{
			Name: "send_message", Domain: "comms", Effect: ai.ToolEffectExecute,
			Description: "立即向指定员工发送一条新消息；这是会产生外部副作用的新投递，不用于查询既有投递状态。",
			InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"user_id": map[string]any{"type": "integer"},
					"message": map[string]any{"type": "string"},
				}, "required": []string{"user_id", "message"},
			},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				sendCalls.Add(1)
				return "已发送给杨桑。", nil
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
	runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := engine.RunTurn(runCtx, &ai.TurnRequest{
		Mode: ai.TurnModeDeep,
		System: `你是公司的 AI 运营中枢。工具定义和结果是状态事实来源。
查询只读取；只有用户明确要求新的外部状态变化时才执行写入或发送。
先读取可变状态，并根据工具返回决定下一步；已成功发生的副作用不得因状态查询而重复执行。
当前时间：2026-07-20 10:59 +09:00（Asia/Tokyo）。`,
		History: []ai.Message{
			{Role: ai.RoleUser, Content: "周一把金色项目成员名单发给杨桑确认。"},
			{Role: ai.RoleAssistant, Content: "已设置单次日程，计划在日本时间 7 月 20 日 10:30 发送。"},
		},
		UserText: "现在发送了吗",
		Tools:    catalog,
	})
	if err != nil {
		t.Fatalf("delivered status run failed after schedules=%d sends=%d: %v", scheduleCalls.Load(), sendCalls.Load(), err)
	}
	if scheduleCalls.Load() == 0 || sendCalls.Load() != 0 || strings.TrimSpace(result.Text) == "" {
		t.Fatalf("delivered schedule status caused a duplicate send: schedules=%d sends=%d result=%q exposure=%+v steps=%+v",
			scheduleCalls.Load(), sendCalls.Load(), result.Text, result.ToolExposure, result.Steps)
	}
	t.Logf("delivered status passed: schedules=%d sends=%d result=%q exposure=%+v steps=%+v",
		scheduleCalls.Load(), sendCalls.Load(), result.Text, result.ToolExposure, result.Steps)
}

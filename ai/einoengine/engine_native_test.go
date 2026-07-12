package einoengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
)

type scriptedModelState struct {
	mu sync.Mutex
	fn func(input []*schema.Message, tools []*schema.ToolInfo) (*schema.Message, error)
}

type scriptedModel struct {
	state *scriptedModelState
	tools []*schema.ToolInfo
}

func (m *scriptedModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	clone := *m
	clone.tools = append([]*schema.ToolInfo(nil), tools...)
	return &clone, nil
}

func (m *scriptedModel) Generate(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return m.respond(input, opts...)
}

func (m *scriptedModel) Stream(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	response, err := m.respond(input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{response}), nil
}

func (m *scriptedModel) respond(input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	options := model.GetCommonOptions(&model.Options{Tools: append([]*schema.ToolInfo(nil), m.tools...)}, opts...)
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	return m.state.fn(input, options.Tools)
}

func newNativeTestEngine(chatModel model.ToolCallingChatModel, runtime RuntimeStore) *Engine {
	return &Engine{
		cfg:          config.AIConfig{Provider: config.ProviderOpenAI, MaxTokens: 4096},
		maxTurns:     8,
		defaultModel: "test",
		models:       map[string]model.ToolCallingChatModel{"test": chatModel},
		runtime:      runtime,
	}
}

func TestDeepAgentSearchesAndExecutesDeferredTool(t *testing.T) {
	var executed int
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, visible []*schema.ToolInfo) (*schema.Message, error) {
		visibleNames := make(map[string]bool, len(visible))
		for _, info := range visible {
			visibleNames[info.Name] = true
		}
		last := input[len(input)-1]
		switch {
		case last.Role == schema.Tool && last.ToolName == "record_value":
			return schema.AssistantMessage("操作完成", nil), nil
		case last.Role == schema.Tool && last.ToolName == "tool_search":
			if !visibleNames["record_value"] {
				return nil, fmt.Errorf("deferred tool was not activated: %v", visibleNames)
			}
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "write-1", Type: "function",
				Function: schema.FunctionCall{Name: "record_value", Arguments: `{"value":"alpha"}`},
			}}), nil
		default:
			if !visibleNames["tool_search"] || !visibleNames["write_todos"] || visibleNames["record_value"] {
				return nil, fmt.Errorf("unexpected initial tools: %v", visibleNames)
			}
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "search-1", Type: "function",
				Function: schema.FunctionCall{Name: "tool_search", Arguments: `{"query":"select:record_value"}`},
			}}), nil
		}
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, nil)
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		System:   "完成用户要求。",
		UserText: "记录 alpha",
		Tools: []ai.Tool{{
			Name:        "record_value",
			Description: "保存一个值",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"value": map[string]any{"type": "string"}},
				"required":   []string{"value"},
			},
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				executed++
				return `{"ok":true}`, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "操作完成" || executed != 1 {
		t.Fatalf("result=%q executed=%d steps=%+v", result.Text, executed, result.Steps)
	}
	if len(result.Steps) < 2 || result.Steps[0].ToolName != "tool_search" || result.Steps[1].ToolName != "record_value" {
		t.Fatalf("Eino tool loop steps = %+v", result.Steps)
	}
}

func TestOneShotUsesOneToolFreeGeneration(t *testing.T) {
	var calls int
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, visible []*schema.ToolInfo) (*schema.Message, error) {
		calls++
		if len(visible) != 0 {
			return nil, fmt.Errorf("one-shot unexpectedly exposed tools: %v", visible)
		}
		users := 0
		for _, message := range input {
			if message.Role == schema.User {
				users++
			}
		}
		return schema.AssistantMessage(fmt.Sprintf("users=%d", users), nil), nil
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, session.NewInMemoryStore[*schema.Message](nil))
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode:      ai.TurnModeOneShot,
		SessionID: "91",
		System:    "count",
		History: []ai.Message{
			{Role: ai.RoleUser, Content: "first"},
			{Role: ai.RoleAssistant, Content: "ack"},
		},
		UserText: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "users=2" || calls != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
	if result.EngineSession != "" {
		t.Fatalf("one-shot created a durable session: %q", result.EngineSession)
	}
}

func TestOneShotRejectsAgentCapabilitiesAndUnknownMode(t *testing.T) {
	engine := newNativeTestEngine(&scriptedModel{state: &scriptedModelState{
		fn: func([]*schema.Message, []*schema.ToolInfo) (*schema.Message, error) {
			return schema.AssistantMessage("unexpected", nil), nil
		},
	}}, nil)
	_, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode:  ai.TurnModeOneShot,
		Tools: []ai.Tool{{Name: "must_not_run"}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot expose tools") {
		t.Fatalf("one-shot tools error = %v", err)
	}
	_, err = engine.RunTurn(context.Background(), &ai.TurnRequest{Mode: ai.TurnMode("guess")})
	if err == nil || !strings.Contains(err.Error(), "unsupported Eino turn mode") {
		t.Fatalf("unknown mode error = %v", err)
	}
	_, err = engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode: ai.TurnModeOneShot, EngineSession: "existing",
	})
	if err == nil || !strings.Contains(err.Error(), "requires a capability scope") {
		t.Fatalf("missing continuation scope error = %v", err)
	}
	engine.runtime = session.NewInMemoryStore[*schema.Message](nil)
	_, err = engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode: ai.TurnModeOneShot, SessionID: "7", EngineSession: "wrong",
		SessionCapability: "stable",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot create or rotate") {
		t.Fatalf("one-shot session rotation error = %v", err)
	}
}

func TestOneShotCanCloseExistingDeepSession(t *testing.T) {
	runtime := session.NewInMemoryStore[*schema.Message](nil)
	var calls int
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, visible []*schema.ToolInfo) (*schema.Message, error) {
		calls++
		if calls == 1 {
			foundSearch := false
			for _, info := range visible {
				foundSearch = foundSearch || info.Name == "tool_search"
			}
			if !foundSearch {
				return nil, fmt.Errorf("deep turn did not expose tool_search: %v", visible)
			}
			return schema.AssistantMessage("first", nil), nil
		}
		if len(visible) != 0 {
			return nil, fmt.Errorf("one-shot unexpectedly exposed tools: %v", visible)
		}
		users := 0
		for _, message := range input {
			if message.Extra != nil {
				if marked, _ := message.Extra[einoToolSearchReminderKey].(bool); marked {
					return nil, fmt.Errorf("one-shot replayed deferred-tool reminder")
				}
			}
			if message.Role == schema.User {
				users++
			}
		}
		return schema.AssistantMessage(fmt.Sprintf("users=%d", users), nil), nil
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, runtime)
	first, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode: ai.TurnModeDeep, SessionID: "92", SessionCapability: "stable", System: "count", UserText: "work",
		Tools: []ai.Tool{{
			Name: "probe", Description: "test probe",
			InputSchema: map[string]any{"type": "object"},
			Handler:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode: ai.TurnModeOneShot, SessionID: "92", EngineSession: first.EngineSession,
		SessionCapability: "stable", System: "count", UserText: "render closure",
	})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Text != "users=2" || closed.EngineSession != first.EngineSession {
		t.Fatalf("closed=%+v first_session=%q", closed, first.EngineSession)
	}
}

func TestManagedSessionReplaysHistoryAcrossEngineInstances(t *testing.T) {
	runtime := session.NewInMemoryStore[*schema.Message](nil)
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, _ []*schema.ToolInfo) (*schema.Message, error) {
		users := 0
		for _, message := range input {
			if message.Role == schema.User {
				users++
			}
		}
		return schema.AssistantMessage(fmt.Sprintf("users=%d", users), nil), nil
	}
	model := &scriptedModel{state: state}
	firstEngine := newNativeTestEngine(model, runtime)
	first, err := firstEngine.RunTurn(context.Background(), &ai.TurnRequest{
		SessionID: "42", System: "count", UserText: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.EngineSession == "" || first.Text != "users=1" {
		t.Fatalf("first=%+v", first)
	}

	// A new Engine simulates a process restart. The caller-provided History is
	// intentionally wrong; the managed Eino session must remain authoritative.
	secondEngine := newNativeTestEngine(model, runtime)
	second, err := secondEngine.RunTurn(context.Background(), &ai.TurnRequest{
		SessionID:     "42",
		EngineSession: first.EngineSession,
		System:        "count",
		History:       []ai.Message{{Role: ai.RoleUser, Content: "must not be replayed"}},
		UserText:      "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.EngineSession != first.EngineSession || second.Text != "users=2" {
		t.Fatalf("second=%+v first_session=%q", second, first.EngineSession)
	}
}

func TestDeepAgentLoadsSkillContentOnDemand(t *testing.T) {
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, visible []*schema.ToolInfo) (*schema.Message, error) {
		last := input[len(input)-1]
		if last.Role == schema.Tool && last.ToolName == "skill" {
			if !strings.Contains(last.Content, "先核对目标，再执行") {
				return nil, fmt.Errorf("skill content was not loaded: %q", last.Content)
			}
			return schema.AssistantMessage("已按流程处理", nil), nil
		}
		for _, info := range visible {
			if info.Name == "skill" {
				if !strings.Contains(info.Desc, "nbco-skill-9") || strings.Contains(info.Desc, "先核对目标，再执行") {
					return nil, fmt.Errorf("skill metadata/content boundary is wrong: %q", info.Desc)
				}
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID: "skill-1", Type: "function",
					Function: schema.FunctionCall{Name: "skill", Arguments: `{"skill":"nbco-skill-9"}`},
				}}), nil
			}
		}
		return nil, fmt.Errorf("skill tool is not visible")
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, nil)
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		System:   "按需使用流程。",
		UserText: "执行标准流程",
		Skills: []ai.Skill{{
			Name:        "nbco-skill-9",
			Description: "标准执行流程",
			Content:     "先核对目标，再执行",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "已按流程处理" || len(result.Steps) == 0 || result.Steps[0].ToolName != "skill" {
		t.Fatalf("result=%+v", result)
	}
}

func TestCapabilityChangeRotatesManagedSession(t *testing.T) {
	engine := newNativeTestEngine(&scriptedModel{state: &scriptedModelState{
		fn: func([]*schema.Message, []*schema.ToolInfo) (*schema.Message, error) {
			return schema.AssistantMessage("ok", nil), nil
		},
	}}, session.NewInMemoryStore[*schema.Message](nil))
	base := &ai.TurnRequest{SessionID: "7", EngineSession: "eino:chat:7:old:initial"}
	first := engine.engineSessionID(base)
	base.Tools = []ai.Tool{{Name: "new_capability"}}
	base.EngineSession = first
	second := engine.engineSessionID(base)
	if first == second || !strings.Contains(second, "eino:chat:7:") {
		t.Fatalf("session did not rotate: first=%q second=%q", first, second)
	}
	base.EngineSession = second
	if got := engine.engineSessionID(base); got != second {
		t.Fatalf("stable capability set rotated again: got=%q want=%q", got, second)
	}
}

func TestFailedManagedTurnRollsBackToLastCommit(t *testing.T) {
	runtime := session.NewInMemoryStore[*schema.Message](nil)
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, _ []*schema.ToolInfo) (*schema.Message, error) {
		lastUser := ""
		for i := len(input) - 1; i >= 0; i-- {
			if input[i].Role == schema.User {
				lastUser = input[i].Content
				break
			}
		}
		if lastUser == "bad turn" {
			return nil, fmt.Errorf("synthetic model failure")
		}
		var users []string
		for _, message := range input {
			if message.Role == schema.User {
				users = append(users, message.Content)
			}
		}
		return schema.AssistantMessage(strings.Join(users, "|"), nil), nil
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, runtime)
	first, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		SessionID: "88", System: "test", UserText: "first turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		SessionID: "88", EngineSession: first.EngineSession, System: "test", UserText: "bad turn",
	}); err == nil {
		t.Fatal("failing turn unexpectedly succeeded")
	}
	third, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		SessionID: "88", EngineSession: first.EngineSession, System: "test", UserText: "third turn",
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Text != "first turn|third turn" || strings.Contains(third.Text, "bad turn") {
		t.Fatalf("failed turn remained active after rollback: %q", third.Text)
	}
}

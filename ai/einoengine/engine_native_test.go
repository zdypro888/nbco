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
	mu          sync.Mutex
	fn          func(input []*schema.Message, tools []*schema.ToolInfo) (*schema.Message, error)
	toolChoices []schema.ToolChoice
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
	if options.ToolChoice != nil {
		m.state.toolChoices = append(m.state.toolChoices, *options.ToolChoice)
	}
	return m.state.fn(input, options.Tools)
}

func finalResponse(_ string, text string) *schema.Message {
	return schema.AssistantMessage(text, nil)
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
			if info.Name == toolSearchToolName && info.Desc != conciseToolSearchDescription {
				return nil, fmt.Errorf("tool_search description was not compacted")
			}
		}
		last := input[len(input)-1]
		switch {
		case last.Role == schema.Tool && last.ToolName == "record_value":
			return finalResponse("final-write", "操作完成"), nil
		case last.Role == schema.Tool && last.ToolName == "tool_search":
			if !visibleNames["record_value"] {
				return nil, fmt.Errorf("deferred tool was not activated: %v", visibleNames)
			}
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "write-1", Type: "function",
				Function: schema.FunctionCall{Name: "record_value", Arguments: `{"value":"alpha"}`},
			}}), nil
		default:
			if !visibleNames["tool_search"] || visibleNames["write_todos"] || visibleNames["record_value"] {
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
	if len(state.toolChoices) != 0 {
		t.Fatalf("native Deep Agent tool choice was overridden: %v", state.toolChoices)
	}
}

func TestDeepAgentBlocksUnloadedDeferredToolExecution(t *testing.T) {
	var executed int
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, visible []*schema.ToolInfo) (*schema.Message, error) {
		visibleNames := make(map[string]bool, len(visible))
		for _, info := range visible {
			visibleNames[info.Name] = true
		}
		last := input[len(input)-1]
		switch {
		case last.Role == schema.Tool && last.ToolName == "record_value" &&
			strings.Contains(last.Content, "尚未在当前轮次通过 tool_search 加载"):
			if executed != 0 {
				return nil, fmt.Errorf("unloaded tool reached its handler")
			}
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "search-after-block", Type: "function",
				Function: schema.FunctionCall{Name: toolSearchToolName, Arguments: `{"query":"select:record_value"}`},
			}}), nil
		case last.Role == schema.Tool && last.ToolName == toolSearchToolName:
			if !visibleNames["record_value"] {
				return nil, fmt.Errorf("searched tool is not visible: %v", visibleNames)
			}
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "write-after-search", Type: "function",
				Function: schema.FunctionCall{Name: "record_value", Arguments: `{}`},
			}}), nil
		case last.Role == schema.Tool && last.ToolName == "record_value":
			return finalResponse("guard-final", "已执行一次"), nil
		default:
			if visibleNames["record_value"] {
				return nil, fmt.Errorf("deferred tool was initially visible")
			}
			// Simulate a model naming a deferred tool from the catalog without
			// first loading its schema through Eino tool_search.
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "direct-unloaded", Type: "function",
				Function: schema.FunctionCall{Name: "record_value", Arguments: `{}`},
			}}), nil
		}
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, nil)
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		System: "完成用户要求。", UserText: "保存值",
		Tools: []ai.Tool{{
			Name: "record_value", Description: "保存值", InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				executed++
				return `{"ok":true}`, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "已执行一次" || executed != 1 {
		t.Fatalf("result=%+v executed=%d", result, executed)
	}
	if len(result.Steps) != 4 || result.Steps[0].ToolName != "record_value" ||
		result.Steps[0].Err == "" || result.Steps[1].ToolName != toolSearchToolName ||
		result.Steps[2].ToolName != "record_value" || result.Steps[2].Err != "" {
		t.Fatalf("guarded tool steps = %+v", result.Steps)
	}
}

func TestDeepAgentUsesProductInstruction(t *testing.T) {
	state := &scriptedModelState{fn: func(input []*schema.Message, _ []*schema.ToolInfo) (*schema.Message, error) {
		if got := strings.TrimSpace(input[0].Content); got != "PRODUCT_CONTEXT_MARKER" {
			return nil, fmt.Errorf("deep agent did not use the product instruction directly: %q", got)
		}
		return finalResponse("product-final", "产品提示正常"), nil
	}}
	engine := newNativeTestEngine(&scriptedModel{state: state}, nil)
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		System: "PRODUCT_CONTEXT_MARKER", UserText: "测试",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "产品提示正常" {
		t.Fatalf("result=%+v", result)
	}
}

func TestDeepAgentMayRepeatSameToolAndArguments(t *testing.T) {
	var executed int
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, visible []*schema.ToolInfo) (*schema.Message, error) {
		last := input[len(input)-1]
		switch {
		case last.Role == schema.Tool && last.ToolName == "record_value":
			if executed < 2 {
				return schema.AssistantMessage("", []schema.ToolCall{{
					ID: fmt.Sprintf("repeat-%d", executed+1), Type: "function",
					Function: schema.FunctionCall{Name: "record_value", Arguments: `{"value":"same"}`},
				}}), nil
			}
			return finalResponse("final-repeat", "两次执行完成"), nil
		case last.Role == schema.Tool && last.ToolName == "tool_search":
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "repeat-1", Type: "function",
				Function: schema.FunctionCall{Name: "record_value", Arguments: `{"value":"same"}`},
			}}), nil
		default:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "search-repeat", Type: "function",
				Function: schema.FunctionCall{Name: "tool_search", Arguments: `{"query":"select:record_value"}`},
			}}), nil
		}
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, nil)
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		System:   "按需要重复执行工具。",
		UserText: "把相同值记录两次",
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
	if result.Text != "两次执行完成" || executed != 2 {
		t.Fatalf("result=%q executed=%d steps=%+v", result.Text, executed, result.Steps)
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
			return finalResponse("final-first", "first"), nil
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
		return finalResponse(fmt.Sprintf("final-users-%d", users), fmt.Sprintf("users=%d", users)), nil
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
			return finalResponse("final-skill", "已按流程处理"), nil
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

func TestToolContractChangeRotatesManagedSessionWithStableScope(t *testing.T) {
	base := []ai.Tool{{
		Name: "view_project", Description: "read project", Effect: ai.ToolEffectRead,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"project_id": map[string]any{"type": "integer"},
		}},
	}}
	first := capabilityFingerprint(base, "superadmin")
	unchanged := capabilityFingerprint(base, "superadmin")
	if first != unchanged {
		t.Fatalf("stable contract fingerprint changed: %q != %q", first, unchanged)
	}
	updated := append([]ai.Tool(nil), base...)
	updated[0].InputSchema = map[string]any{"type": "object", "properties": map[string]any{
		"project_id":    map[string]any{"type": "integer"},
		"include_tasks": map[string]any{"type": "boolean"},
	}}
	if got := capabilityFingerprint(updated, "superadmin"); got == first {
		t.Fatal("schema change did not rotate a scoped managed session")
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
		return finalResponse(fmt.Sprintf("final-history-%d", len(users)), strings.Join(users, "|")), nil
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

func TestDeepAgentDoesNotReactivateToolsFromEarlierTurns(t *testing.T) {
	runtime := session.NewInMemoryStore[*schema.Message](nil)
	var currentTurn int
	state := &scriptedModelState{}
	state.fn = func(input []*schema.Message, visible []*schema.ToolInfo) (*schema.Message, error) {
		last := input[len(input)-1]
		if last.Role == schema.User && last.Content == "second" {
			currentTurn = 2
		}
		if currentTurn == 2 {
			for _, info := range visible {
				if info.Name == "record_value" {
					return nil, fmt.Errorf("historical deferred tool leaked into next turn")
				}
			}
			return finalResponse("final-second", "second ok"), nil
		}
		switch {
		case last.Role == schema.Tool && last.ToolName == "record_value":
			return finalResponse("final-first-turn", "first ok"), nil
		case last.Role == schema.Tool && last.ToolName == toolSearchToolName:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "write-first", Type: "function",
				Function: schema.FunctionCall{Name: "record_value", Arguments: `{}`},
			}}), nil
		default:
			return schema.AssistantMessage("", []schema.ToolCall{{
				ID: "search-first", Type: "function",
				Function: schema.FunctionCall{Name: toolSearchToolName, Arguments: `{"query":"select:record_value"}`},
			}}), nil
		}
	}
	engine := newNativeTestEngine(&scriptedModel{state: state}, runtime)
	request := &ai.TurnRequest{
		SessionID: "101", UserText: "first", SessionCapability: "stable",
		Tools: []ai.Tool{{
			Name: "record_value", Description: "保存值", InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) { return `{"ok":true}`, nil },
		}},
	}
	first, err := engine.RunTurn(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.EngineSession = first.EngineSession
	request.UserText = "second"
	second, err := engine.RunTurn(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Text != "second ok" {
		t.Fatalf("second=%+v", second)
	}
}

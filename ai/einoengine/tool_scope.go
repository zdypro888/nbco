package einoengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/textfmt"
)

const (
	// Bump when the durable Deep Agent's tool or lifecycle contract changes so
	// managed sessions cannot replay traces produced by an incompatible runtime.
	deepAgentRuntimeVersion = "native-deep-toolsearch-v9"

	toolSearchToolName = "tool_search"
)

// turnToolMiddleware keeps deferred tools selected in older managed-session
// turns out of the current model request and records actual model exposure. It
// observes Eino's native agent loop without changing its completion semantics.
type turnToolMiddleware struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.Message]

	dynamicNames map[string]struct{}
	toolInfos    map[string]*schema.ToolInfo
	tools        map[string]ai.Tool
	exposure     *ai.ToolExposure
	sessionID    string

	mu            sync.Mutex
	selected      map[string]struct{}
	blockedCalls  map[string]struct{}
	replayedCalls map[string]struct{}
	effectCalls   map[string]*effectCall
	replayCounts  map[string]int
}

type effectCall struct {
	done   chan struct{}
	result string
	err    error
}

func newTurnToolMiddleware(ctx context.Context, sessionID string, tools []ai.Tool, exposure *ai.ToolExposure) (*turnToolMiddleware, error) {
	dynamicNames := make(map[string]struct{}, len(tools))
	toolInfos := make(map[string]*schema.ToolInfo, len(tools))
	toolsByName := make(map[string]ai.Tool, len(tools))
	for _, item := range tools {
		toolsByName[item.Name] = item
		if item.LoadMode == ai.ToolLoadImmediate {
			continue
		}
		dynamicNames[item.Name] = struct{}{}
		info, err := (&einoTool{t: item}).Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("读取延迟工具 %s schema: %w", item.Name, err)
		}
		toolInfos[item.Name] = info
	}
	return &turnToolMiddleware{
		dynamicNames:  dynamicNames,
		toolInfos:     toolInfos,
		tools:         toolsByName,
		exposure:      exposure,
		sessionID:     strings.TrimSpace(sessionID),
		selected:      make(map[string]struct{}),
		blockedCalls:  make(map[string]struct{}),
		replayedCalls: make(map[string]struct{}),
		effectCalls:   make(map[string]*effectCall),
		replayCounts:  make(map[string]int),
	}, nil
}

func (m *turnToolMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.Message],
	_ *adk.TypedModelContext[*schema.Message],
) (context.Context, *adk.TypedChatModelAgentState[*schema.Message], error) {
	if state == nil {
		return ctx, state, nil
	}
	m.rememberCurrentTurnSelections(state.Messages)
	selected := m.selectedSnapshot()

	infos := slices.Clone(state.ToolInfos)
	infos = slices.DeleteFunc(infos, func(info *schema.ToolInfo) bool {
		if info == nil {
			return false
		}
		_, dynamic := m.dynamicNames[info.Name]
		_, active := selected[info.Name]
		return dynamic && !active
	})
	existing := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		if info != nil {
			existing[info.Name] = struct{}{}
		}
	}
	selectedNames := slices.Collect(maps.Keys(selected))
	sort.Strings(selectedNames)
	for _, name := range selectedNames {
		if _, ok := existing[name]; ok {
			continue
		}
		if info := m.toolInfos[name]; info != nil {
			infos = append(infos, info)
			existing[name] = struct{}{}
		}
	}
	state.ToolInfos = infos
	state.DeferredToolInfos = nil
	m.observe(infos)
	return ctx, state, nil
}

// AfterModelRewriteState stops a model that remains stuck on an already
// replayed side effect. The first duplicate reaches the tool boundary and gets
// a structured replay result, giving the agent one normal chance to continue
// with other work. Repeating it again is a stagnation signal, so only those
// duplicate calls are removed; unrelated calls in the same response continue.
func (m *turnToolMiddleware) AfterModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.Message],
	_ *adk.TypedModelContext[*schema.Message],
) (context.Context, *adk.TypedChatModelAgentState[*schema.Message], error) {
	if state == nil || len(state.Messages) == 0 {
		return ctx, state, nil
	}
	message := state.Messages[len(state.Messages)-1]
	if message == nil || message.Role != schema.Assistant || len(message.ToolCalls) == 0 {
		return ctx, state, nil
	}

	kept := make([]schema.ToolCall, 0, len(message.ToolCalls))
	seenInMessage := make(map[string]struct{}, len(message.ToolCalls))
	removed := 0
	for _, call := range message.ToolCalls {
		key, sideEffect := m.effectCallKey(call.Function.Name, call.Function.Arguments)
		if !sideEffect {
			kept = append(kept, call)
			continue
		}
		if _, duplicate := seenInMessage[key]; duplicate {
			m.markReplayed(call.Function.Name, call.ID)
			removed++
			continue
		}
		seenInMessage[key] = struct{}{}
		if m.replayCount(key) > 0 {
			m.markReplayed(call.Function.Name, call.ID)
			removed++
			continue
		}
		kept = append(kept, call)
	}
	if removed == 0 {
		return ctx, state, nil
	}
	message.ToolCalls = kept
	if len(kept) == 0 && strings.TrimSpace(message.Content) == "" {
		message.Content = "本轮相同操作已有工具结果，系统已停止重复执行；请根据实际工具结果判断成功、失败或状态未知，并如实向用户说明。"
	}
	return ctx, state, nil
}

func (m *turnToolMiddleware) WrapModel(
	_ context.Context,
	inner einomodel.BaseModel[*schema.Message],
	_ *adk.TypedModelContext[*schema.Message],
) (einomodel.BaseModel[*schema.Message], error) {
	return &observedModel{inner: inner, onCall: m.recordModelCall}, nil
}

func (m *turnToolMiddleware) recordModelCall() {
	if m.exposure == nil {
		return
	}
	m.mu.Lock()
	m.exposure.ModelCalls++
	m.mu.Unlock()
}

func (m *turnToolMiddleware) rememberCurrentTurnSelections(messages []*schema.Message) {
	boundary := -1
	for i, message := range messages {
		if message != nil && message.Role == schema.User && !isToolSearchReminder(message) {
			boundary = i
		}
	}
	if boundary < 0 {
		return
	}
	var found []string
	for _, message := range messages[boundary+1:] {
		if message == nil || message.Role != schema.Tool || message.ToolName != toolSearchToolName {
			continue
		}
		var result struct {
			Matches []string `json:"matches"`
		}
		if json.Unmarshal([]byte(message.Content), &result) == nil {
			found = append(found, result.Matches...)
		}
	}
	if len(found) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, name := range found {
		if _, ok := m.dynamicNames[name]; ok {
			m.selected[name] = struct{}{}
		}
	}
}

func isToolSearchReminder(message *schema.Message) bool {
	if message == nil || message.Extra == nil {
		return false
	}
	marked, _ := message.Extra[einoToolSearchReminderKey].(bool)
	return marked
}

func (m *turnToolMiddleware) selectedSnapshot() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.selected)
}

func (m *turnToolMiddleware) callable(name string) bool {
	if m == nil {
		return true
	}
	if _, dynamic := m.dynamicNames[name]; !dynamic {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, selected := m.selected[name]
	return selected
}

func (m *turnToolMiddleware) guardInvokable(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
	return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
		if input == nil {
			return next(ctx, input)
		}
		if !m.callable(input.Name) {
			m.mu.Lock()
			m.blockedCalls[toolCallKey(input.Name, input.CallID)] = struct{}{}
			m.mu.Unlock()
			return &compose.ToolOutput{Result: deferredToolNotLoadedResult(input.Name)}, nil
		}
		key, sideEffect := m.effectCallKey(input.Name, input.Arguments)
		if !sideEffect {
			return next(ctx, input)
		}
		return m.invokeSideEffectOnce(ctx, input, key, next)
	}
}

func (m *turnToolMiddleware) invokeSideEffectOnce(
	ctx context.Context,
	input *compose.ToolInput,
	key string,
	next compose.InvokableToolEndpoint,
) (*compose.ToolOutput, error) {
	m.mu.Lock()
	if existing := m.effectCalls[key]; existing != nil {
		m.replayedCalls[toolCallKey(input.Name, input.CallID)] = struct{}{}
		m.replayCounts[key]++
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-existing.done:
			if existing.err != nil {
				return &compose.ToolOutput{Result: replayedFailedSideEffectResult(existing.err)}, nil
			}
			return &compose.ToolOutput{Result: replayedSideEffectResult(existing.result)}, nil
		}
	}
	current := &effectCall{done: make(chan struct{})}
	m.effectCalls[key] = current
	m.mu.Unlock()

	callCtx := ctx
	if m.sessionID != "" {
		sum := sha256.Sum256([]byte(m.sessionID + "\x00" + key))
		callCtx = ai.WithToolInvocationKey(ctx, fmt.Sprintf("%x", sum[:]))
	}
	output, err := next(callCtx, input)
	current.err = err
	if output != nil {
		current.result = output.Result
	}
	m.mu.Lock()
	close(current.done)
	m.mu.Unlock()
	return output, err
}

func (m *turnToolMiddleware) effectCallKey(name, arguments string) (string, bool) {
	if m == nil {
		return "", false
	}
	item, ok := m.tools[name]
	if !ok || (item.Effect != ai.ToolEffectWrite && item.Effect != ai.ToolEffectExecute) {
		return "", false
	}
	raw := json.RawMessage(strings.TrimSpace(arguments))
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if item.NormalizeInput != nil {
		raw = item.NormalizeInput(raw)
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			raw = canonical
		}
	}
	sum := sha256.Sum256(raw)
	return name + "\x00" + fmt.Sprintf("%x", sum[:]), true
}

func (m *turnToolMiddleware) replayCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.replayCounts[key]
}

func (m *turnToolMiddleware) blocked(toolName, callID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, blocked := m.blockedCalls[toolCallKey(toolName, callID)]
	return blocked
}

func (m *turnToolMiddleware) replayed(toolName, callID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, replayed := m.replayedCalls[toolCallKey(toolName, callID)]
	return replayed
}

func (m *turnToolMiddleware) markReplayed(toolName, callID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.replayedCalls[toolCallKey(toolName, callID)] = struct{}{}
	m.mu.Unlock()
}

func toolCallKey(toolName, callID string) string { return toolName + "\x00" + callID }

func deferredToolNotLoadedResult(name string) string {
	return "工具 " + name +
		" 未执行：它尚未在当前轮次通过 tool_search 加载。请先调用 tool_search，随后再调用该工具。"
}

func replayedSideEffectResult(firstResult string) string {
	result, _ := json.Marshal(map[string]any{
		"status":       "replayed",
		"message":      "相同的写入或执行操作已在本轮成功返回，本次没有再次执行。请继续处理尚未完成的目标或给出最终答复，不要重复调用。",
		"first_result": firstResult,
	})
	return string(result)
}

func replayedFailedSideEffectResult(firstErr error) string {
	message := "前一次相同的写入或执行调用返回了错误；它可能在报错前已经产生部分副作用，因此本轮没有盲目重放。请先用只读能力核实当前状态，再决定后续动作。"
	if firstErr != nil {
		message += " 原错误：" + textfmt.TruncateRunes(textfmt.RedactSecrets(firstErr.Error()), 1200)
	}
	result, _ := json.Marshal(map[string]any{
		"status":  "execution_state_unknown",
		"message": message,
	})
	return string(result)
}

func (m *turnToolMiddleware) observe(infos []*schema.ToolInfo) {
	if m.exposure == nil {
		return
	}
	names := make([]string, 0, len(infos))
	chars := 0
	for _, info := range infos {
		if info == nil {
			continue
		}
		names = append(names, info.Name)
		chars += len(info.Name) + len(info.Desc)
		if params, err := info.ParamsOneOf.ToJSONSchema(); err == nil && params != nil {
			if raw, err := json.Marshal(params); err == nil {
				chars += len(raw)
			}
		}
	}
	sort.Strings(names)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exposure.AgentIterations++
	if len(names) > m.exposure.PeakToolCount {
		m.exposure.PeakToolCount = len(names)
	}
	if chars >= m.exposure.PeakSchemaChars {
		m.exposure.PeakSchemaChars = chars
		m.exposure.PeakTools = slices.Clone(names)
	}
}

type observedModel struct {
	inner  einomodel.BaseModel[*schema.Message]
	onCall func()
}

func (m *observedModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	if m.onCall != nil {
		m.onCall()
	}
	return m.inner.Generate(ctx, input, opts...)
}

func (m *observedModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	if m.onCall != nil {
		m.onCall()
	}
	return m.inner.Stream(ctx, input, opts...)
}

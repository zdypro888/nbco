package einoengine

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/cloudwego/eino/adk"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/zdypro888/nbco/ai"
)

const (
	// Bump when the durable Deep Agent's tool or lifecycle contract changes so
	// managed sessions cannot replay traces produced by an incompatible runtime.
	deepAgentRuntimeVersion = "native-deep-toolsearch-v5"

	toolSearchToolName           = "tool_search"
	writeTodosToolName           = "write_todos"
	conciseToolSearchDescription = "按能力关键词或名称查找并加载当前轮次需要的延迟工具。知道名称时用 select:<tool_name>；否则用简短能力关键词。返回的工具已立即可调用，无需重复搜索。"
	conciseWriteTodosDescription = "为当前复杂请求维护临时步骤清单。仅在确有多个执行步骤时使用；及时把当前步骤标为 in_progress，完成后标为 completed。简单问答或单步操作无需调用。"
)

// turnToolMiddleware keeps deferred tools selected in older managed-session
// turns out of the current model request and records actual model exposure. It
// observes Eino's native agent loop without changing its completion semantics.
type turnToolMiddleware struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.Message]

	dynamicNames map[string]struct{}
	exposure     *ai.ToolExposure

	mu       sync.Mutex
	selected map[string]struct{}
}

func newTurnToolMiddleware(tools []ai.Tool, exposure *ai.ToolExposure) *turnToolMiddleware {
	dynamicNames := make(map[string]struct{}, len(tools))
	for _, item := range tools {
		dynamicNames[item.Name] = struct{}{}
	}
	return &turnToolMiddleware{
		dynamicNames: dynamicNames,
		exposure:     exposure,
		selected:     make(map[string]struct{}),
	}
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
	for i, info := range infos {
		if info == nil {
			continue
		}
		switch info.Name {
		case toolSearchToolName:
			clone := *info
			clone.Desc = conciseToolSearchDescription
			infos[i] = &clone
		case writeTodosToolName:
			clone := *info
			clone.Desc = conciseWriteTodosDescription
			infos[i] = &clone
		}
	}
	state.ToolInfos = infos
	state.DeferredToolInfos = nil
	m.observe(infos)
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
		if raw, err := json.Marshal(info.ParamsOneOf); err == nil {
			chars += len(raw)
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

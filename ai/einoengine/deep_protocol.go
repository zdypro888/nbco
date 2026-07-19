package einoengine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/zdypro888/nbco/ai"
)

const (
	finalResponseToolName = "final_response"
	toolSearchToolName    = "tool_search"
	writeTodosToolName    = "write_todos"

	deepCompletionInstruction = `

[轮次终止协议]
每次模型输出都必须调用一个工具。需要读取、修改或执行时，继续调用对应工具；只有当前请求已经回答完毕，或真实执行已经得到工具结果，或确实缺少权限/参数而无法继续时，才调用 final_response 结束本轮。不得用 final_response 承诺稍后执行尚未调用的动作，也不要把它和其他工具放在同一次输出中。`

	conciseToolSearchDescription = "按名称或语义查找并加载当前轮次需要的延迟工具。知道名称时用 select:<tool_name>；否则用简短能力关键词。返回的工具已立即可调用，无需重复搜索。"
	conciseWriteTodosDescription = "为当前复杂请求维护临时步骤清单。仅在确有多个执行步骤时使用；及时把当前步骤标为 in_progress，完成后标为 completed。简单问答或单步操作无需调用。"
	structuredRetryInstruction   = "[框架协议纠正] 上一个输出没有调用工具，因此未被接受。请重新处理同一请求：若仍需读取、修改或执行，调用或检索对应工具；若请求已完全回答、已有工具结果足以收尾、或确实无法继续，调用 final_response。不要只输出普通文本。"
)

// deepProtocolMiddleware makes completion an explicit agent action. This is a
// structural lifecycle contract, not a classifier for particular user phrases.
type deepProtocolMiddleware struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.Message]

	finalTool    *finalResponseTool
	finalInfo    *schema.ToolInfo
	dynamicNames map[string]struct{}
	exposure     *ai.ToolExposure

	mu       sync.Mutex
	selected map[string]struct{}
}

func newDeepProtocolMiddleware(ctx context.Context, tools []ai.Tool, exposure *ai.ToolExposure) (*deepProtocolMiddleware, error) {
	finalTool := &finalResponseTool{}
	finalInfo, err := finalTool.Info(ctx)
	if err != nil {
		return nil, err
	}
	dynamicNames := make(map[string]struct{}, len(tools))
	for _, item := range tools {
		dynamicNames[item.Name] = struct{}{}
	}
	return &deepProtocolMiddleware{
		finalTool:    finalTool,
		finalInfo:    finalInfo,
		dynamicNames: dynamicNames,
		exposure:     exposure,
		selected:     make(map[string]struct{}),
	}, nil
}

func (m *deepProtocolMiddleware) BeforeAgent(
	ctx context.Context,
	runCtx *adk.ChatModelAgentContext[*schema.Message],
) (context.Context, *adk.ChatModelAgentContext[*schema.Message], error) {
	if runCtx == nil {
		return ctx, runCtx, nil
	}
	next := *runCtx
	next.Instruction += deepCompletionInstruction
	next.Tools = append(slices.Clone(runCtx.Tools), m.finalTool)
	next.ReturnDirectly = maps.Clone(runCtx.ReturnDirectly)
	if next.ReturnDirectly == nil {
		next.ReturnDirectly = make(map[string]bool, 1)
	}
	next.ReturnDirectly[finalResponseToolName] = true
	return ctx, &next, nil
}

func (m *deepProtocolMiddleware) BeforeModelRewriteState(
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

	foundFinal := false
	for i, info := range infos {
		if info == nil {
			continue
		}
		switch info.Name {
		case finalResponseToolName:
			foundFinal = true
			infos[i] = m.finalInfo
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
	if !foundFinal {
		infos = append(infos, m.finalInfo)
	}
	state.ToolInfos = infos
	state.DeferredToolInfos = nil
	m.observe(infos)
	return ctx, state, nil
}

func deepModelRetryConfig() *adk.ModelRetryConfig {
	config := modelRetryConfig()
	baseRetry := config.ShouldRetry
	config.ShouldRetry = func(ctx context.Context, retryCtx *adk.RetryContext) *adk.RetryDecision {
		if retryCtx != nil && retryCtx.Err == nil &&
			(retryCtx.OutputMessage == nil || len(retryCtx.OutputMessage.ToolCalls) == 0) {
			messages := slices.Clone(retryCtx.InputMessages)
			if retryCtx.OutputMessage != nil {
				messages = append(messages, retryCtx.OutputMessage)
			}
			messages = append(messages, schema.UserMessage(structuredRetryInstruction))
			return &adk.RetryDecision{
				Retry:                 true,
				ModifiedInputMessages: messages,
				RejectReason:          "missing structured tool call",
			}
		}
		return baseRetry(ctx, retryCtx)
	}
	return config
}

// If a provider emits a terminal call alongside work calls, keep the work and
// let the next agent iteration decide how to conclude from its results.
func (*deepProtocolMiddleware) AfterModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.Message],
	_ *adk.TypedModelContext[*schema.Message],
) (context.Context, *adk.TypedChatModelAgentState[*schema.Message], error) {
	if state == nil || len(state.Messages) == 0 {
		return ctx, state, nil
	}
	last := state.Messages[len(state.Messages)-1]
	if last == nil || last.Role != schema.Assistant || len(last.ToolCalls) < 2 {
		return ctx, state, nil
	}
	hasFinal, hasWork := false, false
	for _, call := range last.ToolCalls {
		if call.Function.Name == finalResponseToolName {
			hasFinal = true
		} else {
			hasWork = true
		}
	}
	if !hasFinal {
		return ctx, state, nil
	}

	clone := *last
	if hasWork {
		clone.ToolCalls = slices.DeleteFunc(slices.Clone(last.ToolCalls), func(call schema.ToolCall) bool {
			return call.Function.Name == finalResponseToolName
		})
	} else {
		clone.ToolCalls = slices.Clone(last.ToolCalls[:1])
	}
	state.Messages = slices.Clone(state.Messages)
	state.Messages[len(state.Messages)-1] = &clone
	return ctx, state, nil
}

func (m *deepProtocolMiddleware) WrapModel(
	_ context.Context,
	inner einomodel.BaseModel[*schema.Message],
	_ *adk.TypedModelContext[*schema.Message],
) (einomodel.BaseModel[*schema.Message], error) {
	return &forcedToolChoiceModel{inner: inner, onCall: m.recordModelCall}, nil
}

func (m *deepProtocolMiddleware) recordModelCall() {
	if m.exposure == nil {
		return
	}
	m.mu.Lock()
	m.exposure.ModelCalls++
	m.mu.Unlock()
}

func (m *deepProtocolMiddleware) rememberCurrentTurnSelections(messages []*schema.Message) {
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

func (m *deepProtocolMiddleware) selectedSnapshot() map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.selected)
}

func (m *deepProtocolMiddleware) observe(infos []*schema.ToolInfo) {
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

type forcedToolChoiceModel struct {
	inner  einomodel.BaseModel[*schema.Message]
	onCall func()
}

func (m *forcedToolChoiceModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	if m.onCall != nil {
		m.onCall()
	}
	next := append(slices.Clone(opts), einomodel.WithToolChoice(schema.ToolChoiceForced))
	return m.inner.Generate(ctx, input, next...)
}

func (m *forcedToolChoiceModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	if m.onCall != nil {
		m.onCall()
	}
	next := append(slices.Clone(opts), einomodel.WithToolChoice(schema.ToolChoiceForced))
	return m.inner.Stream(ctx, input, next...)
}

type finalResponseTool struct{}

func (*finalResponseTool) Info(context.Context) (*schema.ToolInfo, error) {
	params, err := toJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"response": map[string]any{
				"type":        "string",
				"description": "发送给用户的完整最终答复。只能陈述已经回答、已经由工具证实、或当前明确受阻的事实。",
			},
		},
		"required":             []string{"response"},
		"additionalProperties": false,
	})
	if err != nil {
		return nil, fmt.Errorf("构建 %s schema: %w", finalResponseToolName, err)
	}
	return &schema.ToolInfo{
		Name:        finalResponseToolName,
		Desc:        "结束当前轮次并把 response 作为最终答复发送给用户。若仍需调用任何工具完成请求，不得使用本工具。",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(params),
	}, nil
}

func (*finalResponseTool) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("解析最终答复: %w", err)
	}
	response := strings.TrimSpace(args.Response)
	if response == "" {
		return "", fmt.Errorf("最终答复不能为空")
	}
	return response, nil
}

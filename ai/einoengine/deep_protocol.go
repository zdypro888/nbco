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
	// Bump when the durable Deep Agent's internal tool or lifecycle contract
	// changes. It is part of the managed-session fingerprint so old tool traces
	// cannot teach a new runtime an obsolete protocol.
	deepAgentProtocolVersion = "native-deep-semantic-tools-v3"

	finalResponseToolName = "final_response"
	toolSearchToolName    = "tool_search"
	writeTodosToolName    = "write_todos"

	deepCompletionInstruction = `

[轮次终止协议]
每次模型输出都必须调用一个工具。需要读取、修改或执行时，继续调用对应工具；只有当前请求已经回答完毕，或真实执行已经得到工具结果，或确实缺少权限/参数而无法继续时，才调用 final_response 结束本轮。
final_response.outcome 必须准确选择：answer=只回答事实/问题且没有未执行动作；action_result=本轮写入或执行工具已经返回，答复必须忠实反映其结果；blocked=权限、系统错误等使请求无法继续；clarify=缺少用户才能提供的必要信息。不得用 final_response 承诺稍后执行尚未调用的动作，也不要把它和其他工具放在同一次输出中。`

	conciseToolSearchDescription   = "按名称或语义查找并加载当前轮次需要的延迟工具。知道名称时用 select:<tool_name>；否则用简短能力关键词。返回的工具已立即可调用，无需重复搜索。"
	conciseWriteTodosDescription   = "为当前复杂请求维护临时步骤清单。仅在确有多个执行步骤时使用；及时把当前步骤标为 in_progress，完成后标为 completed。简单问答或单步操作无需调用。"
	structuredRetryInstruction     = "[框架协议纠正] 上一个输出没有调用工具，因此未被接受。请重新处理同一请求：若仍需读取、修改或执行，调用或检索对应工具；若请求已完全回答、已有工具结果足以收尾、或确实无法继续，调用 final_response。不要只输出普通文本。"
	actionEvidenceRetryInstruction = "[框架协议纠正] 上一个 final_response 声明了 action_result，但本轮尚无写入或执行工具返回，因而没有可核对的动作结果。请继续调用所需工具；若只是回答事实或能力，请用 answer 且不要描述尚未发生的动作；若无法继续，请如实用 blocked 或 clarify。"
	actionOutcomeRetryInstruction  = "[框架协议纠正] 本轮已有写入或执行工具返回，不能用 answer 隐去动作结果。请根据工具返回如实使用 action_result；若结果表明无法继续，则使用 blocked。"
	invalidFinalRetryInstruction   = "[框架协议纠正] 上一个 final_response 不符合终止协议。请只调用一次 final_response，提供非空 response，并准确选择 answer、action_result、blocked 或 clarify；若仍需工具，继续执行而不要收尾。"
	unknownToolRetryInstruction    = "[框架协议纠正] 上一个输出调用了本轮不存在的工具，因此未执行任何工具。请只使用当前模型请求中提供的工具 schema；需要其他授权能力时先调用 tool_search，不要猜测工具名。"
)

type toolExecution struct {
	effect          string
	handlerReturned bool
}

// deepProtocolMiddleware makes completion an explicit agent action. This is a
// structural lifecycle contract, not a classifier for particular user phrases.
type deepProtocolMiddleware struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.Message]

	finalTool    *finalResponseTool
	finalInfo    *schema.ToolInfo
	dynamicInfos map[string]*schema.ToolInfo
	toolEffects  map[string]string
	exposure     *ai.ToolExposure

	mu         sync.Mutex
	available  map[string]struct{}
	selected   map[string]struct{}
	executions []toolExecution
}

func newDeepProtocolMiddleware(
	ctx context.Context,
	tools []ai.Tool,
	preferred []string,
	exposure *ai.ToolExposure,
) (*deepProtocolMiddleware, error) {
	finalTool := &finalResponseTool{}
	finalInfo, err := finalTool.Info(ctx)
	if err != nil {
		return nil, err
	}
	dynamicInfos := make(map[string]*schema.ToolInfo, len(tools))
	toolEffects := make(map[string]string, len(tools))
	for _, item := range tools {
		info, infoErr := (&einoTool{t: item}).Info(ctx)
		if infoErr != nil {
			return nil, fmt.Errorf("读取工具 %s schema: %w", item.Name, infoErr)
		}
		dynamicInfos[item.Name] = info
		toolEffects[item.Name] = item.Effect
	}
	selected := make(map[string]struct{}, len(preferred))
	for _, name := range preferred {
		if _, ok := dynamicInfos[name]; ok {
			selected[name] = struct{}{}
		}
	}
	return &deepProtocolMiddleware{
		finalTool:    finalTool,
		finalInfo:    finalInfo,
		dynamicInfos: dynamicInfos,
		toolEffects:  toolEffects,
		exposure:     exposure,
		selected:     selected,
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
	available := make(map[string]struct{}, len(next.Tools))
	for _, item := range next.Tools {
		info, err := item.Info(ctx)
		if err != nil {
			return ctx, runCtx, fmt.Errorf("读取 Agent 工具 schema: %w", err)
		}
		available[info.Name] = struct{}{}
	}
	m.mu.Lock()
	m.available = available
	m.mu.Unlock()
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
		_, dynamic := m.dynamicInfos[info.Name]
		_, active := selected[info.Name]
		return dynamic && !active
	})
	present := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		if info != nil {
			present[info.Name] = struct{}{}
		}
	}
	selectedNames := make([]string, 0, len(selected))
	for name := range selected {
		selectedNames = append(selectedNames, name)
	}
	sort.Strings(selectedNames)
	for _, name := range selectedNames {
		if _, ok := present[name]; ok {
			continue
		}
		if info, ok := m.dynamicInfos[name]; ok {
			clone := *info
			infos = append(infos, &clone)
			present[name] = struct{}{}
		}
	}

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

func (m *deepProtocolMiddleware) modelRetryConfig() *adk.ModelRetryConfig {
	config := modelRetryConfig()
	baseRetry := config.ShouldRetry
	config.ShouldRetry = func(ctx context.Context, retryCtx *adk.RetryContext) *adk.RetryDecision {
		if retryCtx != nil && retryCtx.Err == nil &&
			(retryCtx.OutputMessage == nil || len(retryCtx.OutputMessage.ToolCalls) == 0) {
			return m.protocolRetry(retryCtx, "missing structured tool call", structuredRetryInstruction)
		}
		if name := m.unknownToolCall(retryCtx); name != "" {
			return m.protocolRetry(retryCtx, "unknown tool call: "+name, unknownToolRetryInstruction)
		}
		if reason, instruction := m.invalidCompletion(retryCtx); reason != "" {
			return m.protocolRetry(retryCtx, reason, instruction)
		}
		return baseRetry(ctx, retryCtx)
	}
	return config
}

func (m *deepProtocolMiddleware) unknownToolCall(retryCtx *adk.RetryContext) string {
	if retryCtx == nil || retryCtx.Err != nil || retryCtx.OutputMessage == nil {
		return ""
	}
	m.mu.Lock()
	available := maps.Clone(m.available)
	m.mu.Unlock()
	for _, call := range retryCtx.OutputMessage.ToolCalls {
		name := strings.TrimSpace(call.Function.Name)
		if name == "" {
			return "<empty>"
		}
		if _, ok := available[name]; !ok {
			return name
		}
	}
	return ""
}

func (m *deepProtocolMiddleware) invalidCompletion(retryCtx *adk.RetryContext) (string, string) {
	if retryCtx == nil || retryCtx.Err != nil || retryCtx.OutputMessage == nil {
		return "", ""
	}
	var finalCalls []schema.ToolCall
	hasWork := false
	for _, call := range retryCtx.OutputMessage.ToolCalls {
		if call.Function.Name == finalResponseToolName {
			finalCalls = append(finalCalls, call)
		} else {
			hasWork = true
		}
	}
	// Mixed output is normalized by AfterModelRewriteState: work executes first,
	// and a later model iteration must decide how to conclude from its result.
	if len(finalCalls) == 0 || hasWork {
		return "", ""
	}
	if len(finalCalls) != 1 {
		return "invalid final_response count", invalidFinalRetryInstruction
	}
	args, err := parseFinalResponseArguments(finalCalls[0].Function.Arguments)
	if err != nil {
		return "invalid final_response arguments", invalidFinalRetryInstruction
	}
	hasActionEvidence := m.hasActionEvidence()
	if args.Outcome == ai.CompletionOutcomeActionResult && !hasActionEvidence {
		return "action_result without action evidence", actionEvidenceRetryInstruction
	}
	if args.Outcome == ai.CompletionOutcomeAnswer && hasActionEvidence {
		return "answer after action evidence", actionOutcomeRetryInstruction
	}
	return "", ""
}

func (m *deepProtocolMiddleware) protocolRetry(retryCtx *adk.RetryContext, reason, instruction string) *adk.RetryDecision {
	m.mu.Lock()
	if m.exposure != nil {
		m.exposure.ProtocolRetries++
	}
	m.mu.Unlock()
	return &adk.RetryDecision{
		Retry:                 true,
		ModifiedInputMessages: append(slices.Clone(retryCtx.InputMessages), schema.UserMessage(instruction)),
		RejectReason:          reason,
	}
}

func (m *deepProtocolMiddleware) hasActionEvidence() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, execution := range m.executions {
		if execution.handlerReturned &&
			(execution.effect == ai.ToolEffectWrite || execution.effect == ai.ToolEffectExecute) {
			return true
		}
	}
	return false
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

func (m *deepProtocolMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	toolCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	if toolCtx == nil {
		return endpoint, nil
	}
	effect, ok := m.toolEffects[toolCtx.Name]
	if !ok {
		return endpoint, nil
	}
	return func(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
		result, err := endpoint(ctx, argumentsInJSON, opts...)
		m.mu.Lock()
		m.executions = append(m.executions, toolExecution{
			effect:          effect,
			handlerReturned: err == nil,
		})
		m.mu.Unlock()
		return result, err
	}, nil
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
		if _, ok := m.dynamicInfos[name]; ok {
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

type finalResponseArgs struct {
	Outcome  ai.CompletionOutcome `json:"outcome"`
	Response string               `json:"response"`
}

type finalResponseTool struct{}

func (*finalResponseTool) Info(context.Context) (*schema.ToolInfo, error) {
	params, err := toJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"outcome": map[string]any{
				"type": "string",
				"enum": []string{
					string(ai.CompletionOutcomeAnswer),
					string(ai.CompletionOutcomeActionResult),
					string(ai.CompletionOutcomeBlocked),
					string(ai.CompletionOutcomeClarify),
				},
				"description": "本轮收尾依据：answer=无需未执行动作的回答；action_result=本轮写入/执行工具已返回；blocked=无法继续；clarify=缺少必要输入。",
			},
			"response": map[string]any{
				"type":        "string",
				"description": "发送给用户的完整最终答复。只能陈述已经回答、已经由工具证实、或当前明确受阻的事实。",
			},
		},
		"required":             []string{"outcome", "response"},
		"additionalProperties": false,
	})
	if err != nil {
		return nil, fmt.Errorf("构建 %s schema: %w", finalResponseToolName, err)
	}
	return &schema.ToolInfo{
		Name:        finalResponseToolName,
		Desc:        "以机器可验证的 outcome 结束当前轮次，并把 response 发送给用户。action_result 仅在本轮写入或执行工具已返回后有效；若仍需工具，不得使用本工具。",
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(params),
	}, nil
}

func (*finalResponseTool) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	args, err := parseFinalResponseArguments(argumentsInJSON)
	if err != nil {
		return "", err
	}
	return args.Response, nil
}

func parseFinalResponseArguments(argumentsInJSON string) (finalResponseArgs, error) {
	var args finalResponseArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return args, fmt.Errorf("解析最终答复: %w", err)
	}
	args.Response = strings.TrimSpace(args.Response)
	if args.Response == "" {
		return args, fmt.Errorf("最终答复不能为空")
	}
	switch args.Outcome {
	case ai.CompletionOutcomeAnswer, ai.CompletionOutcomeActionResult,
		ai.CompletionOutcomeBlocked, ai.CompletionOutcomeClarify:
		return args, nil
	default:
		return args, fmt.Errorf("无效的最终答复 outcome: %q", args.Outcome)
	}
}

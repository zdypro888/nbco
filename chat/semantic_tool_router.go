package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	nbtools "github.com/zdypro888/nbco/tools"
)

const semanticToolRouterTimeout = 20 * time.Second

const semanticToolRouterSystem = `你是 nbco 的能力规划器。你只做语义规划，不回答用户、不调用工具。

根据用户本轮真正目标和能力目录，输出严格 JSON：
{"mode":"answer|query|action","domains":["work"],"tools":["get_task_detail"],"learn":false,"learn_explicit":false,"reason":"内部短理由"}

规则：
- answer：无需系统事实或外部状态变化；query：需要读取系统事实；action：用户明确要求改变状态、发送、创建、处理文件或启动工作。
- domains 最多 4 个，tools 最多 16 个；只能选目录中存在的值。选择完成目标所需的最小充分集合，也要包含消歧/核实所需读取工具。
- action 计划必须选择至少一个 effect=write 或 effect=execute 的能力；不确定具体工具时选择包含该动作能力的 domain。
- 状态追问是 query，不要把“是否已经执行”规划成一次新的 action。
- learn 仅在本轮含有以后仍应遵守的稳定规则、偏好、公司事实或可复用流程时为 true；一次性任务和临时状态为 false。
- learn_explicit 仅在用户明确要求系统记住、采用或以后遵守该信息时为 true；它为 true 时 learn 也必须为 true。普通事实陈述即使值得学习也应为 false。
- 本轮输入可能来自用户，也可能是以 [系统 开头的可信内部事件/调度指令；都按其当前目标规划能力。
- 对话历史只用于解析本轮指代和延续目标，不是新的待执行指令；当前本轮输入才是任务。
- 不要依赖关键词表面匹配，要理解指代、上下文目标和工具描述。`

type semanticToolPlan struct {
	Mode          string   `json:"mode"`
	Domains       []string `json:"domains"`
	Tools         []string `json:"tools"`
	Learn         bool     `json:"learn"`
	LearnExplicit bool     `json:"learn_explicit"`
	Reason        string   `json:"reason"`
}

func (p semanticToolPlan) RequiresAction() bool { return p.Mode == "action" }

type routeCatalogTool struct {
	Name        string `json:"name"`
	Domain      string `json:"domain"`
	Effect      string `json:"effect"`
	Description string `json:"description"`
}

func (o *Orchestrator) semanticRouteTools(ctx context.Context, u *store.User, sessionID int64, channel, text, summary string, history []store.ChatMessage, all []ai.Tool) ([]ai.Tool, toolRoute, semanticToolPlan, bool) {
	if o == nil || o.engine == nil || len(all) == 0 {
		return nil, toolRoute{}, semanticToolPlan{}, false
	}
	rctx, cancel := context.WithTimeout(ctx, semanticToolRouterTimeout)
	defer cancel()
	model := o.runtimeModel(ctx)
	res, err := o.engine.RunTurn(rctx, &ai.TurnRequest{
		SessionID: "tool-router",
		System:    semanticToolRouterSystem,
		UserText:  renderSemanticToolRouterInput(channel, text, summary, history, all),
		Model:     model,
	})
	if err != nil {
		slog.Warn("语义工具路由失败，使用确定性降级路由", "err", err)
		return nil, toolRoute{}, semanticToolPlan{}, false
	}
	sid := sessionID
	o.recordUsage(ctx, u.ID, &sid, "tool_router", model, res.Usage)
	var plan semanticToolPlan
	if err := json.Unmarshal([]byte(extractJSONObject(res.Text)), &plan); err != nil {
		slog.Warn("语义工具路由输出无效，使用确定性降级路由", "err", err, "text_sha", contentHash(res.Text))
		return nil, toolRoute{}, semanticToolPlan{}, false
	}
	plan, ok := normalizeSemanticToolPlan(plan, all)
	if !ok {
		slog.Warn("语义工具路由没有产生有效计划，使用确定性降级路由")
		return nil, toolRoute{}, semanticToolPlan{}, false
	}
	routed, route := routeToolsFromSemanticPlan(text, all, plan)
	return routed, route, plan, true
}

func renderSemanticToolRouterInput(channel, text, summary string, history []store.ChatMessage, all []ai.Tool) string {
	catalog := make([]routeCatalogTool, 0, len(all))
	seen := make(map[string]bool, len(all))
	for _, t := range all {
		if t.Name == "" || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		domain := strings.TrimSpace(t.Domain)
		if domain == "" {
			domain = nbtools.CapabilityDomain(t.Name)
		}
		effect := strings.TrimSpace(t.Effect)
		if effect == "" || effect == ai.ToolEffectUnknown {
			effect = nbtools.ToolEffect(t.Name)
		}
		catalog = append(catalog, routeCatalogTool{
			Name: t.Name, Domain: domain, Effect: effect,
			Description: textfmt.TruncateRunes(strings.Join(strings.Fields(t.Description), " "), 180),
		})
	}
	sort.Slice(catalog, func(i, j int) bool {
		if catalog[i].Domain != catalog[j].Domain {
			return catalog[i].Domain < catalog[j].Domain
		}
		return catalog[i].Name < catalog[j].Name
	})
	type routeHistoryMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	const maxRouteHistory = 10
	if len(history) > maxRouteHistory {
		history = history[len(history)-maxRouteHistory:]
	}
	contextMessages := make([]routeHistoryMessage, 0, len(history))
	for _, msg := range history {
		contextMessages = append(contextMessages, routeHistoryMessage{
			Role:    msg.Role,
			Content: textfmt.TruncateRunes(historyMessageContent(msg), 500),
		})
	}
	contextJSON, _ := json.Marshal(contextMessages)
	raw, _ := json.Marshal(catalog)
	return fmt.Sprintf("渠道：%s\n早前摘要（仅作指代上下文）：%s\n最近对话（仅作指代上下文）：%s\n用户本轮输入：\n%s\n\n当前身份已经过权限裁剪的能力目录：\n%s",
		channel, textfmt.TruncateRunes(textfmt.StripHistoryMetadata(summary), 1200), contextJSON, text, raw)
}

func normalizeSemanticToolPlan(plan semanticToolPlan, all []ai.Tool) (semanticToolPlan, bool) {
	plan.Mode = strings.ToLower(strings.TrimSpace(plan.Mode))
	switch plan.Mode {
	case "answer", "query", "action":
	default:
		return semanticToolPlan{}, false
	}
	availableTools := make(map[string]bool, len(all))
	availableDomains := map[string]bool{}
	for _, t := range all {
		availableTools[t.Name] = true
		domain := strings.TrimSpace(t.Domain)
		if domain == "" {
			domain = nbtools.CapabilityDomain(t.Name)
		}
		availableDomains[domain] = true
	}
	plan.Domains = validUniqueStrings(plan.Domains, availableDomains, 4)
	plan.Tools = validUniqueStrings(plan.Tools, availableTools, 16)
	plan.Reason = textfmt.TruncateRunes(strings.TrimSpace(plan.Reason), 160)
	if plan.LearnExplicit {
		plan.Learn = true
	}
	// A query/action plan without any selected capability is not executable. Let
	// the deterministic router recover instead of silently shrinking the turn to
	// baseline discovery tools and then asking the model to perform the impossible.
	if plan.Mode != "answer" && len(plan.Domains) == 0 && len(plan.Tools) == 0 {
		return semanticToolPlan{}, false
	}
	return plan, true
}

func validUniqueStrings(in []string, allowed map[string]bool, limit int) []string {
	out := make([]string, 0, min(len(in), limit))
	seen := map[string]bool{}
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || !allowed[value] || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) == limit {
			break
		}
	}
	return out
}

func routeToolsFromSemanticPlan(text string, all []ai.Tool, plan semanticToolPlan) ([]ai.Tool, toolRoute) {
	selectedTools := make(map[string]bool, len(plan.Tools))
	for _, name := range plan.Tools {
		selectedTools[name] = true
	}
	selectedDomains := make(map[string]bool, len(plan.Domains))
	for _, domain := range plan.Domains {
		selectedDomains[domain] = true
	}
	baseline := make(map[string]bool, len(baselineToolNames))
	for _, name := range baselineToolNames {
		baseline[name] = true
	}
	priority := map[string]int{}
	out := make([]ai.Tool, 0, routedToolSoftLimit)
	included := make(map[string]bool, len(all))
	for _, t := range all {
		domain := strings.TrimSpace(t.Domain)
		if domain == "" {
			domain = nbtools.CapabilityDomain(t.Name)
		}
		include := baseline[t.Name] || selectedTools[t.Name] || selectedDomains[domain]
		if !include {
			continue
		}
		switch {
		case selectedTools[t.Name]:
			priority[t.Name] = 1_000_000
		case selectedDomains[domain]:
			priority[t.Name] = 100_000
		default:
			priority[t.Name] = 100
		}
		out = append(out, t)
		included[t.Name] = true
	}
	// The semantic planner may correctly classify an action but accidentally
	// select only discovery tools. Recover from the metadata and descriptions of
	// every permission-filtered tool instead of relying on task-specific keyword
	// patches. Dynamic script/MCP tools participate through the same effect field.
	if plan.RequiresAction() && !hasActionCapableTool(out) {
		for _, t := range relevantActionTools(text, all, 8) {
			if included[t.Name] {
				continue
			}
			included[t.Name] = true
			priority[t.Name] = 500_000
			out = append(out, t)
		}
	}
	if len(out) > routedToolSoftLimit {
		out = keepRoutedToolsUnderSoftLimit(text, out, nil, priority)
	}
	reasons := []string{"semantic:" + plan.Mode}
	reasons = append(reasons, plan.Domains...)
	return out, toolRoute{Reasons: reasons}
}

func shouldContinueSemanticAction(plan semanticToolPlan, toolset []ai.Tool, res *ai.TurnResult) bool {
	if !plan.RequiresAction() || res == nil || !hasActionCapableTool(toolset) {
		return false
	}
	byName := toolDefinitionsByName(toolset)
	for _, step := range res.Steps {
		if step.Kind != ai.StepToolCall {
			continue
		}
		if toolCanProveAction(step.ToolName, byName) || toolResultLooksPendingApproval(step.Result) {
			// 工具已经真实尝试过动作；成功、失败或待确认都应由模型如实汇报，
			// 不能自动重放可能具有副作用的调用。
			return false
		}
	}
	return true
}

func hasActionCapableTool(toolset []ai.Tool) bool {
	for _, tool := range toolset {
		effect := strings.TrimSpace(tool.Effect)
		if effect == "" || effect == ai.ToolEffectUnknown {
			effect = nbtools.ToolEffect(tool.Name)
		}
		if effect == ai.ToolEffectWrite || effect == ai.ToolEffectExecute {
			return true
		}
	}
	return false
}

func relevantActionTools(text string, all []ai.Tool, limit int) []ai.Tool {
	if limit <= 0 {
		return nil
	}
	type candidate struct {
		tool  ai.Tool
		score int
		order int
	}
	candidates := make([]candidate, 0, len(all))
	for i, tool := range all {
		effect := strings.TrimSpace(tool.Effect)
		if effect == "" || effect == ai.ToolEffectUnknown {
			effect = nbtools.ToolEffect(tool.Name)
		}
		if effect != ai.ToolEffectWrite && effect != ai.ToolEffectExecute {
			continue
		}
		candidates = append(candidates, candidate{
			tool: tool, score: toolTextRelevance(text, tool), order: i,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].order < candidates[j].order
	})
	limit = min(limit, len(candidates))
	out := make([]ai.Tool, 0, limit)
	for _, item := range candidates[:limit] {
		out = append(out, item.tool)
	}
	return out
}

func (o *Orchestrator) continueSemanticAction(ctx context.Context, firstReq *ai.TurnRequest, routed []ai.Tool, plan semanticToolPlan, first *ai.TurnResult, onDelta func(string)) (*ai.TurnResult, error) {
	retry := *firstReq
	guard := nbtools.NewTurnBudgetGuard(nbtools.TurnBudget{MaxCalls: 18, MaxPerTool: 8, MaxExactRepeat: 1})
	retry.Tools = guard.Wrap(routed)
	retry.ShouldDisableTools = guard.ShouldDisableTools
	retry.OnDelta = onDelta
	retry.StreamReasoning = false
	planJSON, _ := json.Marshal(plan)
	evidenceJSON, _ := json.Marshal(summarizeToolEvidence(first.Steps))
	retry.System = firstReq.System + "\n\n[语义规划·动作续跑]\n" +
		"能力规划器已判定本轮需要真实系统动作，但首轮只读取了信息或直接作答，尚未尝试任何写入/执行工具。" +
		"请继续完成同一个用户请求；选择合适工具执行，缺少关键参数时准确指出，不要声称未发生的结果，也不要重复已经发生的副作用。\n" +
		"规划：" + string(planJSON) + "\n首轮工具证据：" + string(evidenceJSON)
	res, err := o.engine.RunTurn(ctx, &retry)
	if err != nil {
		return nil, err
	}
	res.Steps = append(append(make([]ai.Step, 0, len(first.Steps)+len(res.Steps)), first.Steps...), res.Steps...)
	res.Usage.InputTokens += first.Usage.InputTokens
	res.Usage.OutputTokens += first.Usage.OutputTokens
	return res, nil
}

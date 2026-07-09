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
)

const actionPlannerTimeout = 15 * time.Second

type actionPlan struct {
	RequiresAction  bool     `json:"requires_action"`
	Intent          string   `json:"intent"`
	ExpectedTools   []string `json:"expected_tools"`
	SuccessEvidence []string `json:"success_evidence"`
	MissingInfo     []string `json:"missing_info"`
	Confidence      float64  `json:"confidence"`

	Source string `json:"-"`
	Raw    string `json:"-"`
}

func (o *Orchestrator) maybePlanAction(ctx context.Context, u *store.User, channel, text string, toolset []ai.Tool) *actionPlan {
	if o == nil || o.engine == nil || !shouldRunActionPlanner(text) {
		return nil
	}
	plannerCtx, cancel := context.WithTimeout(ctx, actionPlannerTimeout)
	defer cancel()
	req := &ai.TurnRequest{
		SessionID:       "action-planner",
		System:          actionPlannerSystem(toolset),
		UserText:        actionPlannerUserText(u, channel, text),
		Model:           o.runtimeModel(ctx),
		StreamReasoning: false,
	}
	res, err := o.engine.RunTurn(plannerCtx, req)
	if err != nil {
		slog.Warn("动作规划器失败，使用保守守门", "user", u.ID, "err", err)
		return fallbackActionPlan(text, "planner_error")
	}
	plan, err := parseActionPlan(res.Text, toolNames(toolset))
	if err != nil {
		slog.Warn("动作规划器输出不可解析，使用保守守门", "user", u.ID, "reply_sha", contentHash(res.Text), "err", err)
		return fallbackActionPlan(text, "planner_parse_error")
	}
	if !plan.RequiresAction {
		return nil
	}
	plan.Source = "planner"
	plan.Raw = res.Text
	return plan
}

func shouldRunActionPlanner(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "[系统") {
		return false
	}
	return looksLikeSideEffectRequest(text) || looksLikeHeavyExecutionRequest(text)
}

func actionPlannerSystem(toolset []ai.Tool) string {
	var b strings.Builder
	b.WriteString("你是 nbco 的动作规划器，只判断本轮用户输入是否需要改变系统状态、外部投递、派工、授权、创建、修改、删除、部署、调用 worker/文件处理能力或执行命令。")
	b.WriteString("不要执行动作，不要回答用户，只输出严格 JSON。\n")
	b.WriteString("JSON schema: {\"requires_action\":bool,\"intent\":string,\"expected_tools\":[string],\"success_evidence\":[string],\"missing_info\":[string],\"confidence\":number}\n")
	b.WriteString("规则：\n")
	b.WriteString("- 只从可用工具中选择 expected_tools；不确定具体工具时 expected_tools 为空但 requires_action 仍可为 true。\n")
	b.WriteString("- 用户只是询问事实、解释概念、闲聊、让你分析普通文本且不要求落库/发送/执行时，requires_action=false；但要求处理上传文件、公司资料、代码仓库、命令行、worker 或产生产物时，requires_action=true。\n")
	b.WriteString("- 如果需要信息不足，requires_action=true，并在 missing_info 写缺什么；主对话应询问或说明未完成，不能声称已完成。\n")
	b.WriteString("- success_evidence 写最终能证明完成的工具返回事实，例如“schedule_push 返回已设置推送”。\n\n")
	b.WriteString("可用工具：\n")
	for _, t := range toolset {
		desc := strings.Join(strings.Fields(t.Description), " ")
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, textfmt.TruncateRunes(desc, 80))
	}
	return b.String()
}

func looksLikeHeavyExecutionRequest(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	keywords := []string{
		"worker", "agent", "codex", "claude", "pty", "shell", "cmd", "命令", "执行", "运行",
		"文件", "附件", "上传", "pdf", "xlsx", "excel", "图片", "照片", "资料", "报表", "产物",
		"代码", "仓库", "clone", "部署", "升级", "修复", "实现", "测试", "分析刚才", "整理这",
		"群监控", "监听", "telegram 群", "tg 群",
	}
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

func actionPlannerUserText(u *store.User, channel, text string) string {
	var role string
	if u != nil {
		role = u.Name
		if u.IsSuperadmin {
			role += "（超级管理员）"
		}
	}
	return fmt.Sprintf("channel=%s\ncurrent_user=%s\nuser_text=%s", channel, role, text)
}

func toolNames(toolset []ai.Tool) map[string]bool {
	m := make(map[string]bool, len(toolset))
	for _, t := range toolset {
		m[t.Name] = true
	}
	return m
}

func parseActionPlan(text string, available map[string]bool) (*actionPlan, error) {
	raw := extractJSONObject(text)
	if raw == "" {
		return nil, fmt.Errorf("no json object")
	}
	var p actionPlan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	p.Intent = strings.TrimSpace(p.Intent)
	p.ExpectedTools = filterKnownTools(p.ExpectedTools, available, 8)
	p.SuccessEvidence = cleanStringList(p.SuccessEvidence, 6, 160)
	p.MissingInfo = cleanStringList(p.MissingInfo, 6, 120)
	if p.Confidence < 0 {
		p.Confidence = 0
	}
	if p.Confidence > 1 {
		p.Confidence = 1
	}
	return &p, nil
}

func filterKnownTools(in []string, available map[string]bool, limit int) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" || !available[name] || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
		if len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out
}

func cleanStringList(in []string, limit, maxRunes int) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, textfmt.TruncateRunes(s, maxRunes))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func fallbackActionPlan(text, source string) *actionPlan {
	if !looksLikeSideEffectRequest(text) {
		return nil
	}
	return &actionPlan{
		RequiresAction:  true,
		Intent:          "用户请求可能需要系统操作",
		SuccessEvidence: []string{"至少一个相关工具成功返回"},
		Confidence:      0.4,
		Source:          source,
	}
}

func renderActionPlanContext(plan *actionPlan) string {
	if plan == nil || !plan.RequiresAction {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[本轮动作计划]\n")
	if plan.Intent != "" {
		b.WriteString("意图：" + plan.Intent + "\n")
	}
	if len(plan.ExpectedTools) > 0 {
		b.WriteString("预计工具：" + strings.Join(plan.ExpectedTools, ", ") + "\n")
	} else {
		b.WriteString("预计工具：规划器未能确定具体工具；你需要自行选择当前可见的合适工具，或说明缺少权限/参数。\n")
	}
	if len(plan.SuccessEvidence) > 0 {
		b.WriteString("完成证据：\n")
		for _, ev := range plan.SuccessEvidence {
			b.WriteString("- " + ev + "\n")
		}
	}
	if len(plan.MissingInfo) > 0 {
		b.WriteString("可能缺少的信息：\n")
		for _, item := range plan.MissingInfo {
			b.WriteString("- " + item + "\n")
		}
	}
	b.WriteString("完成口径：先看工具结果；工具成功就确认完成，工具没跑、失败、缺参数或无权限，就说明未完成以及下一步。\n")
	return b.String()
}

type toolEvidence struct {
	Tool    string `json:"tool"`
	OK      bool   `json:"ok"`
	Summary string `json:"summary,omitempty"`
}

type turnDiagnostics struct {
	Route           string   `json:"route,omitempty"`
	SystemChars     int      `json:"system_chars,omitempty"`
	HistoryChars    int      `json:"history_chars,omitempty"`
	ToolCount       int      `json:"tool_count,omitempty"`
	FullToolCount   int      `json:"full_tool_count,omitempty"`
	ToolSchemaChars int      `json:"tool_schema_chars,omitempty"`
	Tools           []string `json:"tools,omitempty"`
}

func summarizeToolEvidence(steps []ai.Step) []toolEvidence {
	var out []toolEvidence
	for _, st := range steps {
		if st.Kind != ai.StepToolCall {
			continue
		}
		ok := st.Err == "" && !toolResultLooksFailed(st.Result)
		summary := st.Err
		if summary == "" {
			summary = st.Result
		}
		out = append(out, toolEvidence{
			Tool:    st.ToolName,
			OK:      ok,
			Summary: textfmt.TruncateRunes(textfmt.RedactSecrets(strings.TrimSpace(summary)), 220),
		})
	}
	return out
}

func countToolEvidence(steps []ai.Step) (total, success int) {
	for _, st := range steps {
		if st.Kind != ai.StepToolCall {
			continue
		}
		total++
		if st.Err == "" && !toolResultLooksFailed(st.Result) {
			success++
		}
	}
	return total, success
}

func actionCompletionWithoutEvidence(plan *actionPlan, reply string, steps []ai.Step) bool {
	if plan == nil || !plan.RequiresAction {
		return false
	}
	trimmed := strings.TrimSpace(reply)
	if !isDegenerateVisibleReply(trimmed) && !claimsSideEffectDone(trimmed) {
		return false
	}
	return !hasSuccessfulActionEvidence(plan, steps)
}

func actionRequiresToolRecovery(plan *actionPlan, reply string, steps []ai.Step) bool {
	if plan == nil || !plan.RequiresAction {
		return false
	}
	if hasSuccessfulActionEvidence(plan, steps) {
		return false
	}
	if countToolCalls(steps) > 0 {
		return actionCompletionWithoutEvidence(plan, reply, steps)
	}
	trimmed := strings.TrimSpace(reply)
	if isDegenerateVisibleReply(trimmed) {
		return true
	}
	if actionReplyExplainsBlockedOrMissing(plan, trimmed) {
		return false
	}
	return true
}

func actionReplyExplainsBlockedOrMissing(plan *actionPlan, reply string) bool {
	s := strings.ToLower(strings.TrimSpace(reply))
	if s == "" {
		return false
	}
	if len(plan.MissingInfo) > 0 && strings.ContainsAny(s, "?？") {
		return true
	}
	needOrBlock := []string{
		"缺少", "需要", "请提供", "请告诉", "请确认", "确认后", "哪位", "哪个", "什么时候",
		"几点", "多少", "什么内容", "目标是谁", "发送给谁", "无法", "不能", "没法",
		"没有权限", "权限不足", "无权限", "当前渠道", "当前工具", "没有可用", "找不到",
		"未找到", "不存在", "需要授权", "需要到私聊", "pending approval", "permission",
		"not found", "missing", "need", "cannot", "can't", "forbidden",
	}
	for _, p := range needOrBlock {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func hasSuccessfulActionEvidence(plan *actionPlan, steps []ai.Step) bool {
	expected := map[string]bool{}
	if plan != nil {
		for _, name := range plan.ExpectedTools {
			expected[name] = true
		}
	}
	for _, st := range steps {
		if st.Kind != ai.StepToolCall {
			continue
		}
		if len(expected) > 0 && !expected[st.ToolName] {
			continue
		}
		if st.Err == "" && !toolResultLooksFailed(st.Result) {
			return true
		}
	}
	return false
}

func toolResultLooksFailed(result string) bool {
	s := strings.TrimSpace(strings.ToLower(result))
	if s == "" {
		return true
	}
	negative := []string{
		"待确认动作", "下一条明确确认", "征得明确同意", "pending approval",
		"重复调用", "不要继续重复", "没有成功", "未成功", "已跳过", "跳过",
		"不能为空", "必须", "需要", "不能", "无法", "失败", "错误", "无权限", "权限不足",
		"不存在", "未找到", "不属于你", "不允许", "不在", "请先", "没有可用", "没有对应",
		"当前入口未装配", "当前工具集", "not found", "forbidden", "permission", "invalid", "failed", "error",
	}
	for _, p := range negative {
		if strings.Contains(s, p) {
			return true
		}
	}
	positive := []string{"已", "成功", "完成", "ok", "✅", "created", "updated", "saved", "sent", "scheduled"}
	for _, p := range positive {
		if strings.Contains(s, p) {
			return false
		}
	}
	return false
}

func actionEvidenceFallback() string {
	return "这轮没有拿到能证明操作成功的工具结果，所以我不能说已经完成。请重新发一次明确指令；如果缺少参数或权限，我会直接说明，只有工具返回成功后才确认完成。"
}

func (o *Orchestrator) recordActionTurn(ctx context.Context, u *store.User, sess *store.ChatSession, channel, text string, plan *actionPlan, res *ai.TurnResult, diag turnDiagnostics) {
	if o == nil || o.store == nil || u == nil || sess == nil || plan == nil {
		return
	}
	outcome := actionTurnOutcome(plan, res)
	evidence := map[string]any{
		"planner_source":   plan.Source,
		"confidence":       plan.Confidence,
		"success_evidence": plan.SuccessEvidence,
		"missing_info":     plan.MissingInfo,
		"tool_evidence":    summarizeToolEvidence(nil),
	}
	if diag.Route != "" || diag.ToolCount > 0 || diag.SystemChars > 0 {
		evidence["turn_context"] = diag
	}
	if res != nil {
		evidence["tool_evidence"] = summarizeToolEvidence(res.Steps)
		evidence["finish_reason"] = res.FinishReason
	}
	toolCount, successToolCount := 0, 0
	replyExcerpt := ""
	if res != nil {
		toolCount, successToolCount = countToolEvidence(res.Steps)
		replyExcerpt = textfmt.RedactSecrets(textfmt.SanitizeVisibleReply(res.Text))
	}
	sid := sess.ID
	if err := o.store.RecordActionTurn(ctx, store.ActionTurnInput{
		UserID:           u.ID,
		SessionID:        &sid,
		Channel:          channel,
		UserTextHash:     contentHash(text),
		UserTextExcerpt:  textfmt.RedactSecrets(text),
		ReplyExcerpt:     replyExcerpt,
		RequiresAction:   plan.RequiresAction,
		Intent:           plan.Intent,
		ExpectedTools:    plan.ExpectedTools,
		Evidence:         evidence,
		Outcome:          outcome,
		ToolCount:        toolCount,
		SuccessToolCount: successToolCount,
	}); err != nil {
		slog.Warn("动作轮次记录失败", "session", sess.ID, "user", u.ID, "err", err)
	}
}

func actionTurnOutcome(plan *actionPlan, res *ai.TurnResult) string {
	if plan == nil || !plan.RequiresAction {
		return "no_action"
	}
	if res == nil {
		return "no_result"
	}
	if res.FinishReason == "blocked_action_evidence" || res.FinishReason == "blocked_no_tool_completion" {
		return res.FinishReason
	}
	if hasSuccessfulActionEvidence(plan, res.Steps) {
		return "evidence_ok"
	}
	if countToolCalls(res.Steps) > 0 {
		return "tool_attempted_without_success_evidence"
	}
	return "planned_without_tool"
}

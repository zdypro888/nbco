package chat

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	nbtools "github.com/zdypro888/nbco/tools"
)

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

func buildActionAuditPlan(text string, toolset []ai.Tool, res *ai.TurnResult) *actionPlan {
	if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "[系统") {
		return nil
	}
	requiresAction := looksLikeAuditableActionRequest(text)
	byName := toolDefinitionsByName(toolset)
	actualActionSet := map[string]bool{}
	var actualActions []string
	if res != nil {
		for _, st := range res.Steps {
			if st.Kind != ai.StepToolCall {
				continue
			}
			if toolCanProveAction(st.ToolName, byName) || toolResultLooksPendingApproval(st.Result) {
				requiresAction = true
				if toolCanProveAction(st.ToolName, byName) && !actualActionSet[st.ToolName] {
					actualActionSet[st.ToolName] = true
					actualActions = append(actualActions, st.ToolName)
				}
			}
		}
	}
	if !requiresAction {
		return nil
	}
	intent := "用户请求可能需要系统操作"
	if looksLikeMaterialActionIntent(text) || looksLikeFileReferenceRequest(text) {
		intent = "用户请求读取或分析最近上传的文件"
	} else if res != nil && countToolCalls(res.Steps) > 0 {
		intent = "本轮调用了系统工具"
	}
	expected := inferActionToolsForText(text, toolset, 8)
	expectedSet := make(map[string]bool, len(expected))
	for _, name := range expected {
		expectedSet[name] = true
	}
	for _, name := range actualActions {
		if !expectedSet[name] {
			expectedSet[name] = true
			expected = append(expected, name)
		}
	}
	return &actionPlan{
		RequiresAction:  true,
		Intent:          intent,
		ExpectedTools:   expected,
		SuccessEvidence: []string{"写入/执行工具成功返回，或工具明确返回待确认/失败状态"},
		Confidence:      0.5,
		Source:          "audit_heuristic",
	}
}

func buildActionAuditPlanWithSemantic(text string, toolset []ai.Tool, res *ai.TurnResult, semantic semanticToolPlan) *actionPlan {
	if semantic.Mode == "" {
		return buildActionAuditPlan(text, toolset, res)
	}
	byName := toolDefinitionsByName(toolset)
	actualAction := false
	for _, step := range res.Steps {
		if step.Kind == ai.StepToolCall && toolCanProveAction(step.ToolName, byName) {
			actualAction = true
			break
		}
	}
	if !semantic.RequiresAction() && !actualAction {
		return nil
	}
	expected := make([]string, 0, len(semantic.Tools))
	for _, name := range semantic.Tools {
		if t, ok := byName[name]; ok && nbtools.ToolCanProveActionTool(t) {
			expected = append(expected, name)
		}
	}
	if len(expected) == 0 {
		expected = inferActionToolsForText(text, toolset, 8)
	}
	return &actionPlan{
		RequiresAction:  true,
		Intent:          strings.TrimSpace(semantic.Reason),
		ExpectedTools:   expected,
		SuccessEvidence: []string{"语义规划选中的写入/执行工具成功返回，或工具明确返回待确认/失败状态"},
		Confidence:      0.85,
		Source:          "semantic_router",
	}
}

func looksLikeAuditableActionRequest(text string) bool {
	return !looksLikeActionStatusQuestion(text) && (looksLikeSideEffectRequest(text) || looksLikeHeavyExecutionRequest(text))
}

func looksLikeHeavyExecutionRequest(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	if looksLikeActionStatusQuestion(s) {
		return false
	}
	if looksLikeFileReferenceRequest(s) {
		return true
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

func looksLikeActionStatusQuestion(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	actionTerms := []string{
		"发送", "发出", "发了", "通知", "推送", "私信", "群发",
		"创建", "新建", "设置", "定时", "提醒", "更新", "修改", "删除", "取消",
		"执行", "运行", "部署", "升级", "clone", "克隆", "拉取", "同步",
		"worker", "任务", "工具", "操作",
		"send", "sent", "created", "updated", "deleted", "deployed", "scheduled", "executed", "ran", "cloned",
	}
	if !routeHasAny(s, actionTerms) {
		return false
	}
	questionTerms := []string{
		"了吗", "了没", "有没有", "是否", "是不是",
		"成功了吗", "成功没", "完成了吗", "完成没", "做了吗", "做了没",
		"发出去没", "执行了吗", "执行没", "clone了吗", "clone 了吗",
		"status", "done", "success",
	}
	return routeHasAny(s, questionTerms)
}

func toolNames(toolset []ai.Tool) map[string]bool {
	m := make(map[string]bool, len(toolset))
	for _, t := range toolset {
		m[t.Name] = true
	}
	return m
}

func inferActionToolsForText(text string, toolset []ai.Tool, limit int) []string {
	type candidate struct {
		name  string
		score int
		order int
	}
	var ranked []candidate
	for i, t := range toolset {
		if !nbtools.ToolCanProveActionTool(t) {
			continue
		}
		score := toolTextRelevance(text, t)
		if score <= 0 {
			continue
		}
		ranked = append(ranked, candidate{name: t.Name, score: score, order: i})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].order < ranked[j].order
	})
	// ExpectedTools is an audit hint, not an exhaustive allowlist. Keep only
	// top-scoring ties so weakly related write tools do not pollute failure
	// learning (actual action tools are appended separately after execution).
	if len(ranked) > 0 {
		top := ranked[0].score
		end := 0
		for end < len(ranked) && ranked[end].score == top && (limit <= 0 || end < limit) {
			end++
		}
		ranked = ranked[:end]
	}
	out := make([]string, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.name)
	}
	return out
}

func looksLikeMaterialActionIntent(intent string) bool {
	s := strings.ToLower(strings.TrimSpace(intent))
	if s == "" {
		return false
	}
	return routeHasAny(s, []string{
		"文件", "附件", "上传", "pdf", "xlsx", "excel", "图片", "照片", "资料", "材料",
		"读取", "解析", "分析", "file", "attachment", "material", "document",
	})
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

func hasSuccessfulActionEvidence(plan *actionPlan, steps []ai.Step) bool {
	return hasSuccessfulActionEvidenceWithTools(plan, nil, steps)
}

func hasSuccessfulActionEvidenceWithTools(plan *actionPlan, toolset []ai.Tool, steps []ai.Step) bool {
	expected := map[string]bool{}
	if plan != nil {
		for _, name := range plan.ExpectedTools {
			expected[name] = true
		}
	}
	byName := toolDefinitionsByName(toolset)
	for _, st := range steps {
		if st.Kind != ai.StepToolCall {
			continue
		}
		if st.Err != "" || toolResultLooksFailed(st.Result) {
			continue
		}
		if expected[st.ToolName] {
			return true
		}
		if toolCanProveAction(st.ToolName, byName) {
			return true
		}
	}
	return false
}

func toolDefinitionsByName(toolset []ai.Tool) map[string]ai.Tool {
	out := make(map[string]ai.Tool, len(toolset))
	for _, t := range toolset {
		out[t.Name] = t
	}
	return out
}

func toolCanProveAction(name string, byName map[string]ai.Tool) bool {
	if t, ok := byName[name]; ok {
		return nbtools.ToolCanProveActionTool(t)
	}
	return nbtools.ToolCanProveAction(name)
}

func toolResultLooksFailed(result string) bool {
	s := strings.TrimSpace(strings.ToLower(result))
	if s == "" {
		return true
	}
	if strings.Contains(s, strings.ToLower(nbtools.TurnBudgetExhaustedMarker)) {
		return true
	}
	if toolResultLooksPendingApproval(s) {
		return true
	}
	if ok, decided := toolResultCountEvidence(s); decided {
		return !ok
	}
	strongNegative := []string{
		"重复调用", "不要继续重复", "没有成功", "未成功", "已跳过", "跳过",
		"发送失败", "编辑失败", "删除失败", "置顶失败", "执行失败", "自检失败",
		"不能", "无法", "错误", "无效", "已过期", "已使用", "无权限", "权限不足", "不存在", "未找到", "不属于你", "不允许",
		"没有可用", "没有对应", "当前入口未装配", "当前工具集", "not found", "forbidden",
		"permission", "invalid", "failed", "error",
	}
	for _, p := range strongNegative {
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
	softNegative := []string{"不能为空", "必须", "需要", "请先", "不在", "失败"}
	for _, p := range softNegative {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func toolResultLooksPendingApproval(result string) bool {
	s := strings.TrimSpace(strings.ToLower(result))
	if s == "" {
		return false
	}
	for _, p := range []string{"待确认动作", "下一条明确确认", "征得明确同意", "pending approval"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func toolResultCountEvidence(s string) (ok, decided bool) {
	success, hasSuccess := extractChineseCountAfter(s, "成功")
	failed, hasFailed := extractChineseCountAfter(s, "失败")
	if !hasSuccess && !hasFailed {
		return false, false
	}
	if hasSuccess && success > 0 {
		return true, true
	}
	if hasSuccess && success == 0 {
		return false, true
	}
	if hasFailed && failed == 0 {
		return true, true
	}
	return false, false
}

func extractChineseCountAfter(s, marker string) (int, bool) {
	idx := strings.Index(s, marker)
	if idx < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(s[idx+len(marker):])
	if rest == "" {
		return 0, false
	}
	n := 0
	seen := false
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9':
			seen = true
			n = n*10 + int(r-'0')
		case seen:
			return n, true
		case r == ' ' || r == '\t' || r == ':' || r == '：':
			continue
		default:
			return 0, false
		}
	}
	return n, seen
}

func firstPendingApprovalStep(steps []ai.Step) (ai.Step, bool) {
	for _, st := range steps {
		if st.Kind == ai.StepToolCall && st.Err == "" && toolResultLooksPendingApproval(st.Result) {
			return st, true
		}
	}
	return ai.Step{}, false
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
	if _, ok := firstPendingApprovalStep(res.Steps); ok {
		return "pending_approval"
	}
	if hasSuccessfulActionEvidence(plan, res.Steps) {
		return "evidence_ok"
	}
	if countToolCalls(res.Steps) > 0 {
		return "tool_attempted_without_success_evidence"
	}
	return "planned_without_tool"
}

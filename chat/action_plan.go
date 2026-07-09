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
		return fallbackActionPlanWithTools(text, "planner_error", toolset)
	}
	plan, err := parseActionPlan(res.Text, toolNames(toolset))
	if err != nil {
		slog.Warn("动作规划器输出不可解析，使用保守守门", "user", u.ID, "reply_sha", contentHash(res.Text), "err", err)
		return fallbackActionPlanWithTools(text, "planner_parse_error", toolset)
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
	if looksLikeActionStatusQuestion(text) {
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
	b.WriteString("- expected_tools 可以填写多个同一动作类别下的可替代执行工具；不要只因为列表里有 start_workflow 就忽略更直接的发送、定时、群设置或资料分析工具。\n")
	b.WriteString("- 用户只是询问事实、解释概念、闲聊、让你分析普通文本且不要求落库/发送/执行时，requires_action=false；但要求处理上传文件、公司资料、代码仓库、命令行、worker 或产生产物时，requires_action=true。\n")
	b.WriteString("- 用户问“做了吗/发了吗/clone 了吗/成功了吗/刚才有没有执行”等核实状态的问题，requires_action=false；主对话应使用读取类工具核实并回答，不要要求新的执行成功证据。\n")
	b.WriteString("- 用户围绕最近上传/刚发的文件问“能看懂/能读取/这个吗/解析一下”时，属于需要工具确认或派工，requires_action=true。\n")
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
	p.ExpectedTools = expandExpectedToolsForIntent(p.Intent, p.ExpectedTools, available, 8)
	if len(p.ExpectedTools) == 0 {
		p.ExpectedTools = inferActionToolsForText(p.Intent, available, 8)
	}
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
	return fallbackActionPlanWithTools(text, source, nil)
}

func fallbackActionPlanWithTools(text, source string, toolset []ai.Tool) *actionPlan {
	if looksLikeActionStatusQuestion(text) {
		return nil
	}
	if !looksLikeSideEffectRequest(text) && !looksLikeHeavyExecutionRequest(text) {
		return nil
	}
	plan := &actionPlan{
		RequiresAction:  true,
		Intent:          "用户请求可能需要系统操作",
		SuccessEvidence: []string{"至少一个相关工具成功返回"},
		Confidence:      0.4,
		Source:          source,
	}
	available := toolNames(toolset)
	if looksLikeFileReferenceRequest(text) || routeHasAny(strings.ToLower(text), []string{"文件", "附件", "上传", "pdf", "xlsx", "excel", "图片", "照片", "资料"}) {
		if expected := materialActionTools(available); len(expected) > 0 {
			plan.Intent = "用户请求读取或分析最近上传的文件"
			plan.ExpectedTools = expected
			plan.SuccessEvidence = []string{"文件分析/worker 派工工具返回已创建任务或已完成分析"}
			plan.Confidence = 0.55
		}
		return plan
	}
	if expected := inferActionToolsForText(text, available, 8); len(expected) > 0 {
		plan.ExpectedTools = expected
		plan.SuccessEvidence = []string{"对应动作类别的执行型工具返回成功结果"}
		plan.Confidence = 0.5
	}
	return plan
}

func expandExpectedToolsForIntent(intent string, expected []string, available map[string]bool, limit int) []string {
	if inferred := inferActionToolsForText(intent, available, limit); len(inferred) > 0 {
		return mergeToolLists(expected, inferred, limit)
	}
	return expected
}

func inferActionToolsForText(text string, available map[string]bool, limit int) []string {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return nil
	}
	var groups [][]string
	if looksLikeMaterialActionIntent(s) || looksLikeFileReferenceRequest(s) {
		groups = append(groups, materialActionTools(available))
	}
	if routeHasAny(s, []string{"定时", "定期", "提醒", "闹钟", "每天", "每周", "每月", "明天", "后天", "早上", "晚上", "schedule"}) {
		groups = append(groups, availableToolsInOrder(available, "schedule_once", "schedule_repeating", "schedule_push"))
	}
	if routeHasAny(s, []string{"通知", "发送", "发给", "私信", "群发", "群里发", "转发", "告知", "消息", "send", "notify"}) {
		groups = append(groups, availableToolsInOrder(available,
			"send_message", "send_telegram_group_message", "send_file",
			"create_data_collection_campaign", "send_data_collection_reminder",
		))
	}
	infoObjects := []string{"员工信息", "个人信息", "个人档案", "档案", "手机号", "手机", "职位", "组别", "邮箱", "联系方式"}
	if routeHasAny(s, []string{"收集", "完善", "补充", "填报", "登记"}) && routeHasAny(s, infoObjects) {
		groups = append(groups, availableToolsInOrder(available,
			"create_data_collection_campaign", "send_data_collection_reminder", "send_message",
		))
	}
	if routeHasAny(s, []string{"群监控", "监听", "群消息", "日报", "自动总结", "自动汇总", "重要事项", "自动邀请", "group monitor"}) {
		groups = append(groups, availableToolsInOrder(available,
			"set_telegram_group_monitor", "set_telegram_group_listen", "set_telegram_group_auto_invite",
			"bind_telegram_group_project", "update_telegram_group_info",
		))
	}
	peopleDirect := routeHasAny(s, []string{"邀请", "入职", "权限", "授权", "改名", "重命名"})
	peopleWrite := routeHasAny(s, []string{"保存", "更新", "修改", "添加", "删除", "录入", "设置", "改成", "改为"}) && routeHasAny(s, infoObjects)
	if peopleDirect || peopleWrite {
		groups = append(groups, availableToolsInOrder(available,
			"invite_employee", "update_user_info", "save_infos_on_user", "save_my_infos",
			"grant_active_perm", "revoke_active_perm", "grant_passive_perm", "revoke_passive_perm",
			"add_info_field", "remove_info_field",
		))
	}
	if routeHasAny(s, []string{"项目", "任务", "派", "分配", "验收", "通过", "打回", "进度", "里程碑", "目标"}) {
		groups = append(groups, availableToolsInOrder(available,
			"create_project", "assign_task", "update_assigned_task", "reassign_task",
			"accept_task", "reject_task", "add_progress", "create_goal", "add_milestone",
			"decompose_milestone",
		))
	}
	if routeHasAny(s, []string{"删除", "删掉", "删了", "取消", "作废", "没用了", "不用了", "归档", "delete", "cancel", "archive"}) {
		groups = append(groups, availableToolsInOrder(available,
			"delete_assigned_task", "delete_project", "cancel_schedule", "delete_knowledge",
			"close_data_collection_campaign", "archive_project", "revoke_worker",
			"delete_telegram_group_message",
		))
	}
	if routeHasAny(s, []string{"记住", "记下来", "以后", "默认", "规则", "规矩", "沉淀", "学习", "涨记性", "skill", "知识库"}) {
		groups = append(groups, availableToolsInOrder(available,
			"save_rule", "save_knowledge", "save_skill", "propose_learning_candidate",
		))
	}
	if routeHasAny(s, []string{"worker", "agent", "codex", "claude", "pty", "shell", "cmd", "命令", "执行", "运行", "代码", "仓库", "clone", "部署", "升级", "修复", "实现", "测试"}) {
		groups = append(groups, availableToolsInOrder(available,
			"start_workflow", "start_worker_skill", "run_worker_command",
			"create_worker", "issue_worker_bind_code", "set_worker_admin", "revoke_worker",
		))
	}
	if routeHasAny(s, []string{"底层", "最底层", "兜底", "强制", "数据库", "查库", "写库", "sql", "final fallback"}) {
		groups = append(groups, availableToolsInOrder(available,
			"low_level_db_query", "low_level_db_exec",
		))
	}
	var out []string
	for _, group := range groups {
		out = mergeToolLists(out, group, limit)
	}
	return out
}

func mergeToolLists(a, b []string, limit int) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, name := range list {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
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

func materialActionTools(available map[string]bool) []string {
	return availableToolsInOrder(available, "start_workflow", "analyze_company_materials", "start_worker_skill")
}

func availableToolsInOrder(available map[string]bool, names ...string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if available[name] {
			out = append(out, name)
		}
	}
	return out
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
		b.WriteString("候选工具：" + strings.Join(plan.ExpectedTools, ", ") + "\n")
	} else {
		b.WriteString("候选工具：规划器未能确定具体工具；你需要自行选择当前可见的合适工具，或说明缺少权限/参数。\n")
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
	b.WriteString("执行口径：候选工具只是提示，不是限制；以真实工具返回为准。写入/执行工具成功才能确认完成；工具没跑、失败、待确认、缺参数或无权限，就说明当前状态和下一步。\n")
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
	if countToolCalls(steps) > 0 {
		return false
	}
	trimmed := strings.TrimSpace(reply)
	if isDegenerateVisibleReply(trimmed) {
		return true
	}
	if actionReplyExplainsBlockedOrMissing(plan, trimmed) {
		return false
	}
	return claimsSideEffectDone(trimmed)
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
		"高危", "待确认", "未执行", "没有执行", "还没有执行",
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
		if st.Err != "" || toolResultLooksFailed(st.Result) {
			continue
		}
		if expected[st.ToolName] {
			return true
		}
		if nbtools.ToolCanProveAction(st.ToolName) {
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

func actionEvidenceFallback() string {
	return "这轮没有拿到能证明操作成功的工具结果，所以我不能说已经完成。请重新发一次明确指令；如果缺少参数或权限，我会直接说明，只有工具返回成功后才确认完成。"
}

func actionEvidenceFallbackForTurn(userText string, steps []ai.Step) string {
	if st, ok := firstPendingApprovalStep(steps); ok {
		op := strings.TrimSpace(textfmt.RedactSecrets(userText))
		if op == "" {
			op = "刚才请求的高危操作"
		} else {
			op = "「" + textfmt.TruncateRunes(op, 180) + "」"
		}
		return fmt.Sprintf("还没有执行。系统已把%s登记为待确认的高危操作（工具：%s）。\n\n如果确认要继续，请在下一条消息明确回复“确认执行”。收到确认后，我会用同一参数再次调用工具；未确认前不会执行。", op, st.ToolName)
	}
	if ev, ok := firstBlockingToolEvidence(steps); ok {
		return "没有完成。工具返回：" + ev.Summary
	}
	return actionEvidenceFallback()
}

func firstPendingApprovalStep(steps []ai.Step) (ai.Step, bool) {
	for _, st := range steps {
		if st.Kind == ai.StepToolCall && st.Err == "" && toolResultLooksPendingApproval(st.Result) {
			return st, true
		}
	}
	return ai.Step{}, false
}

func firstBlockingToolEvidence(steps []ai.Step) (toolEvidence, bool) {
	for _, st := range steps {
		if st.Kind != ai.StepToolCall {
			continue
		}
		if st.Err == "" && !toolResultLooksFailed(st.Result) {
			continue
		}
		summary := st.Err
		if summary == "" {
			summary = st.Result
		}
		return toolEvidence{
			Tool:    st.ToolName,
			OK:      false,
			Summary: textfmt.TruncateRunes(textfmt.RedactSecrets(strings.TrimSpace(summary)), 260),
		}, true
	}
	return toolEvidence{}, false
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

func (o *Orchestrator) maybeRecordActionFailureLearning(ctx context.Context, u *store.User, channel, userText, replyText string, sessionID, userMsgID, assistantMsgID int64, plan *actionPlan, res *ai.TurnResult) {
	if o == nil || o.store == nil || u == nil || plan == nil || !plan.RequiresAction {
		return
	}
	outcome := actionTurnOutcome(plan, res)
	switch outcome {
	case "evidence_ok", "no_action", "pending_approval":
		return
	}
	intent := strings.TrimSpace(plan.Intent)
	if intent == "" {
		intent = "需要系统动作的请求"
	}
	title := "动作失败样本：" + textfmt.TruncateRunes(intent, 48)
	if ok, err := o.store.LearningCandidateExists(ctx, store.LearningKindSummary, title, store.LearningStatusPending, store.LearningStatusPublished); err != nil {
		slog.Warn("动作失败学习候选去重失败", "title", title, "err", err)
	} else if ok {
		return
	}
	evidence, _ := json.Marshal(map[string]any{
		"source":       "action_guard",
		"channel":      channel,
		"session_id":   sessionID,
		"user_msg_id":  userMsgID,
		"reply_msg_id": assistantMsgID,
		"outcome":      outcome,
		"expected":     plan.ExpectedTools,
		"user_text":    textfmt.TruncateRunes(userText, 600),
		"reply_text":   textfmt.TruncateRunes(replyText, 600),
	})
	content := fmt.Sprintf("本轮用户请求需要系统动作，但最终结果为 %s。复盘时请判断是否缺工具、缺权限、路由没暴露工具、提示词误导，或模型没有按工具优先原则执行。\n\n用户请求：%s\n\n助手回复：%s",
		outcome, textfmt.TruncateRunes(userText, 800), textfmt.TruncateRunes(replyText, 800))
	createdBy := u.ID
	if _, err := o.store.CreateLearningCandidate(ctx, store.LearningCandidateInput{
		Kind:       store.LearningKindSummary,
		Scope:      "global",
		Title:      title,
		Content:    content,
		Tags:       []string{"action_failure", "feedback_loop", "outcome:" + outcome},
		Evidence:   evidence,
		Confidence: 0.55,
		Status:     store.LearningStatusPending,
		SourceType: "action_guard",
		SourceRef:  fmt.Sprintf("session:%d/message:%d", sessionID, userMsgID),
		CreatedBy:  &createdBy,
	}); err != nil {
		slog.Warn("动作失败学习候选记录失败", "title", title, "err", err)
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
		if _, ok := firstPendingApprovalStep(res.Steps); ok {
			return "pending_approval"
		}
		return res.FinishReason
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

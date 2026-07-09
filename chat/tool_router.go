package chat

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/zdypro888/nbco/ai"
)

const routedToolSoftLimit = 48

type toolRoute struct {
	Reasons []string
}

func (r toolRoute) Summary() string {
	if len(r.Reasons) == 0 {
		return "baseline"
	}
	return strings.Join(r.Reasons, ",")
}

func (r toolRoute) Has(reason string) bool {
	for _, got := range r.Reasons {
		if got == reason {
			return true
		}
	}
	return false
}

func routeTurnTools(channel, text string, all []ai.Tool) ([]ai.Tool, toolRoute) {
	available := toolNames(all)
	include := map[string]bool{}
	var route toolRoute
	addReason := func(reason string) {
		for _, got := range route.Reasons {
			if got == reason {
				return
			}
		}
		route.Reasons = append(route.Reasons, reason)
	}
	add := func(names ...string) {
		for _, name := range names {
			if available[name] {
				include[name] = true
			}
		}
	}
	addGroup := func(reason string, names ...string) {
		addReason(reason)
		add(names...)
	}

	add(baselineToolNames...)
	lower := strings.ToLower(strings.TrimSpace(text))
	if isGroupChannel(channel) {
		addGroup("telegram_group", telegramGroupToolNames...)
	}
	if routeHasAny(lower, peopleRouteKeywords) {
		addGroup("people", peopleToolNames...)
	}
	if routeHasAny(lower, taskRouteKeywords) {
		addGroup("work", workToolNames...)
	}
	if routeHasAny(lower, scheduleRouteKeywords) {
		addGroup("schedule", scheduleToolNames...)
	}
	if routeHasAny(lower, workerRouteKeywords) {
		addGroup("worker", workerToolNames...)
	}
	if routeHasAny(lower, fileRouteKeywords) {
		addGroup("files", fileToolNames...)
	}
	if routeHasAny(lower, telegramRouteKeywords) {
		addGroup("telegram_group", telegramGroupToolNames...)
	}
	if routeHasAny(lower, memoryRouteKeywords) {
		addGroup("memory", memoryToolNames...)
	}
	if routeHasAny(lower, permissionRouteKeywords) {
		addGroup("permission", permissionToolNames...)
	}
	if routeHasAny(lower, opsRouteKeywords) {
		addGroup("ops", opsToolNames...)
	}
	if routeHasAny(lower, scriptRouteKeywords) {
		addGroup("script", scriptToolNames...)
	}
	if looksLikeSideEffectRequest(text) {
		addReason("action")
	}

	out := make([]ai.Tool, 0, len(include))
	for _, t := range all {
		if include[t.Name] {
			out = append(out, t)
		}
	}
	if len(out) > routedToolSoftLimit {
		out = keepRoutedToolsUnderSoftLimit(out, include)
	}
	if len(route.Reasons) == 0 {
		route.Reasons = []string{"baseline"}
	}
	return out, route
}

func keepRoutedToolsUnderSoftLimit(in []ai.Tool, include map[string]bool) []ai.Tool {
	if len(in) <= routedToolSoftLimit {
		return in
	}
	pinned := map[string]bool{}
	for _, name := range append([]string{}, baselineToolNames...) {
		if include[name] {
			pinned[name] = true
		}
	}
	for _, name := range []string{
		"send_message", "schedule_push", "assign_task", "update_user_info",
		"invite_employee", "start_worker_skill", "start_workflow", "run_worker_command",
		"list_telegram_groups", "send_telegram_group_message", "save_rule", "save_skill",
	} {
		if include[name] {
			pinned[name] = true
		}
	}
	out := make([]ai.Tool, 0, routedToolSoftLimit)
	for _, t := range in {
		if pinned[t.Name] {
			out = append(out, t)
		}
	}
	for _, t := range in {
		if len(out) >= routedToolSoftLimit {
			break
		}
		if pinned[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

func routeHasAny(text string, keywords []string) bool {
	if text == "" {
		return false
	}
	for _, kw := range keywords {
		if kw != "" && strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func routedToolNames(ts []ai.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

func toolSchemaChars(ts []ai.Tool) int {
	total := 0
	for _, t := range ts {
		total += len(t.Name) + len(t.Description)
		if raw, err := jsonMarshalToolSchema(t.InputSchema); err == nil {
			total += len(raw)
		}
	}
	return total
}

func jsonMarshalToolSchema(v map[string]any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

var baselineToolNames = []string{
	"list_capabilities",
	"search_knowledge", "get_knowledge", "list_recent_knowledge",
	"search_history",
	"search_skills", "load_skill", "propose_learning_candidate",
	"list_recent_files",
	"get_my_profile", "update_my_profile", "get_my_infos", "save_my_infos",
	"get_my_tasks", "get_my_all_tasks", "get_task_detail",
	"list_workers",
	"list_roles", "activate_role",
	"list_workflows",
	"get_api_token_status",
	"list_action_turns",
}

var peopleToolNames = []string{
	"list_users", "get_user_info", "update_user_info", "bulk_update_user_info",
	"get_user_stats", "list_info_fields", "add_info_field", "remove_info_field",
	"view_user_infos", "get_my_infos_on_user", "save_infos_on_user",
	"invite_employee", "cancel_invites", "send_message",
}

var permissionToolNames = []string{
	"list_my_active_perms", "list_my_passive_perms", "grant_my_passive_perm", "revoke_my_passive_perm",
	"grant_active_perm", "revoke_active_perm", "grant_passive_perm", "revoke_passive_perm", "view_user_perms",
	"create_org_group", "list_org_groups", "add_org_group_member",
}

var workToolNames = []string{
	"get_my_projects", "get_my_tasks", "get_my_all_tasks", "get_task_detail", "view_my_task_tree",
	"update_my_task_status", "get_review_queue", "accept_task", "reject_task",
	"save_checklist", "toggle_checklist", "add_progress", "attach_to_task",
	"split_my_task", "get_assigned_tasks", "update_assigned_task", "delete_assigned_task", "reassign_task",
	"create_project", "list_my_projects", "assign_task", "view_project", "archive_project", "delete_project",
	"delegate_review",
	"create_goal", "add_milestone", "decompose_milestone", "update_goal", "close_goal",
	"update_milestone", "close_milestone", "link_task_to_milestone", "view_goals", "get_goal_detail", "get_milestone_detail",
	"refresh_decision_queue", "list_decision_queue", "company_overview",
}

var scheduleToolNames = []string{
	"schedule_once", "schedule_repeating", "schedule_push", "cancel_schedule", "list_schedules", "send_message",
}

var workerToolNames = []string{
	"list_workers", "create_worker", "issue_worker_bind_code", "run_worker_command", "revoke_worker", "set_worker_admin",
	"analyze_company_materials", "start_worker_skill", "list_workflows", "start_workflow",
}

var fileToolNames = []string{
	"list_recent_files", "send_file", "attach_to_task",
	"analyze_company_materials", "start_worker_skill", "list_workflows", "start_workflow",
}

var telegramGroupToolNames = []string{
	"list_telegram_groups", "get_telegram_group", "list_telegram_group_members",
	"resolve_telegram_group_members", "get_telegram_group_member", "set_telegram_group_listen",
	"set_telegram_group_auto_invite", "send_telegram_group_message", "edit_telegram_group_message",
	"delete_telegram_group_message", "pin_telegram_group_message", "unpin_telegram_group_message",
	"update_telegram_group_info", "bind_telegram_group_project",
}

var memoryToolNames = []string{
	"save_knowledge", "search_knowledge", "get_knowledge", "update_knowledge", "delete_knowledge", "list_recent_knowledge",
	"search_history", "save_rule", "list_rules", "set_rule_pinned",
	"search_skills", "load_skill", "save_skill", "update_skill",
	"propose_learning_candidate", "list_learning_candidates", "approve_learning_candidate", "reject_learning_candidate",
	"score_learning_candidates", "list_knowledge_versions", "rollback_knowledge", "list_material_entities",
	"list_eval_cases", "create_eval_case",
}

var opsToolNames = []string{
	"list_capabilities", "list_action_turns", "get_ai_settings", "set_ai_settings", "ai_usage_stats",
	"list_eval_cases", "create_eval_case", "get_api_token_status", "generate_api_token", "revoke_api_token",
}

var scriptToolNames = []string{
	"list_script_tools", "create_script_tool", "update_script_tool", "test_script_tool", "enable_script_tool",
}

var peopleRouteKeywords = []string{
	"员工", "同事", "成员", "人事", "用户", "个人", "档案", "画像", "自我介绍", "评价", "手机号",
	"电话", "邮箱", "职位", "角色", "组别", "部门", "信息", "资料", "名单", "ceo", "老板",
	"黄桑", "真人", "邀请", "入职", "加入", "私信", "发给", "通知", "群发", "改名", "重命名",
	"完善", "补充", "有人", "消息", "发消息", "转发", "推送", "告知",
	"user", "employee", "member", "profile", "invite", "message", "notify", "rename",
}

var permissionRouteKeywords = []string{
	"权限", "授权", "可授予", "管理权限", "查看权限", "编辑权限", "转授权", "上级", "下级",
	"组长", "部门", "组织组", "项目组", "grant", "revoke", "permission", "role",
}

var taskRouteKeywords = []string{
	"任务", "项目", "派活", "分配", "指派", "验收", "打回", "通过", "进度", "里程碑", "目标",
	"工作", "待办", "复盘", "周报", "日报", "拆分", "改派", "审核", "排期", "截止", "负责人",
	"project", "task", "assign", "review", "goal", "milestone",
}

var scheduleRouteKeywords = []string{
	"定时", "提醒", "推送", "每天", "每周", "每月", "明天", "后天", "早上", "晚上", "几点",
	"周期", "循环", "取消提醒", "日程", "值日", "schedule", "remind",
}

var workerRouteKeywords = []string{
	"worker", "ai worker", "工作机", "客户端", "pty", "codex", "claude", "命令", "cmd", "shell",
	"执行", "运行", "部署", "升级", "自升级", "clone", "仓库", "代码", "修复", "实现", "开发",
	"测试", "agent", "资料分析", "admin worker", "机器人",
	"im.app", "服务器", "生产", "服务日志", "运行日志", "聊天记录", "数据库",
}

var fileRouteKeywords = []string{
	"文件", "附件", "上传", "下载", "发送文件", "传文件", "表格", "xlsx", "excel", "pdf", "txt",
	"照片", "图片", "资料", "刚才那", "这两个", "这些", "整理", "分析文件", "产物", "报表",
	"file", "attachment", "spreadsheet", "document",
}

var telegramRouteKeywords = []string{
	"telegram", "tg", "群", "群聊", "群组", "监听", "bot", "botfather", "privacy", "at", "@",
	"群成员", "置顶", "撤回", "删除消息", "编辑消息", "群名", "群公告", "group",
}

var memoryRouteKeywords = []string{
	"知识库", "记住", "以后", "默认", "规则", "skill", "技能", "流程", "sop", "沉淀", "学习",
	"归纳", "总结", "不要忘", "经验", "记下来", "涨记性", "候选", "回滚", "版本",
}

var opsRouteKeywords = []string{
	"模型", "model", "ai设置", "ai 设置", "eino", "token", "access token", "api token", "apikey",
	"日志", "执行日志", "动作记录", "刚才做了吗", "有没有调用", "为什么没执行", "发出去没",
	"用量", "tokens", "评测", "回归测试", "控制中心", "管理中心", "状态", "health",
	"im.app", "生产", "运行数据", "聊天记录",
}

var scriptRouteKeywords = []string{
	"脚本", "script", "starlark", "lua", "python", "工具", "自动化工具", "函数", "计算", "格式化",
}

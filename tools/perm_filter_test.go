package tools

import (
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

func namesOf(ts []ai.Tool) map[string]bool {
	m := map[string]bool{}
	for _, t := range ts {
		m[t.Name] = true
	}
	return m
}

// 无任何授权的普通用户：看不到权限门控工具，自助工具齐全。
func TestForUserPlainUserHidesGatedTools(t *testing.T) {
	u := &store.User{ID: 2, Name: "员工", Status: store.UserActive}
	names := namesOf(ForUser(Deps{}, u, nil))
	for _, gone := range []string{
		"create_project", "assign_task", "delegate_review",
		"invite_employee", "send_message", "create_data_collection_campaign", "send_data_collection_reminder",
		"update_user_info", "bulk_update_user_info", "save_infos_on_user",
		"grant_passive_perm", "revoke_passive_perm", "view_user_perms",
		"company_overview", "get_ai_settings", "set_ai_settings", "ai_usage_stats", "list_system_activity", "create_role", "disable_user",
		"low_level_db_query", "low_level_db_exec",
		"create_worker", "issue_worker_bind_code", "run_worker_command", "revoke_worker", "analyze_company_materials",
		"start_workflow",
		"save_skill", "update_skill",
		"send_telegram_group_message", "edit_telegram_group_message", "delete_telegram_group_message",
		"pin_telegram_group_message", "unpin_telegram_group_message", "update_telegram_group_info",
		"set_telegram_group_listen", "set_telegram_group_auto_invite", "set_telegram_group_monitor", "set_telegram_group_digest", "list_telegram_group_messages",
	} {
		if names[gone] {
			t.Errorf("无授权用户不应看到 %s", gone)
		}
	}
	for _, keep := range []string{
		"get_my_tasks", "update_my_task_status", "accept_task", "split_my_task",
		"save_knowledge", "search_knowledge", "list_workers", "list_roles",
		"activate_role", "schedule_once", "get_my_profile", "grant_active_perm",
		"list_data_collection_campaigns", "get_data_collection_campaign",
		"list_telegram_groups", "get_telegram_group", "list_telegram_group_members", "resolve_telegram_group_members", "get_telegram_group_member",
		"search_skills", "load_skill",
		"list_workflows", "list_capabilities", "list_action_turns",
	} {
		if !names[keep] {
			t.Errorf("无授权用户应保留 %s", keep)
		}
	}
}

// 拿到 create_project 授权后，派活三件套出现。
func TestFilterByPermGrantUnlocks(t *testing.T) {
	u := &store.User{ID: 2, Status: store.UserActive}
	ts := []ai.Tool{{Name: "assign_task"}, {Name: "delegate_review"}, {Name: "get_my_tasks"}, {Name: "company_overview"}}
	grants := []store.Grant{{Kind: store.KindActive, UserID: 2, Action: perm.ActCreateProject, Target: "5"}}
	names := namesOf(filterByPerm(ts, u, grants))
	if !names["assign_task"] || !names["delegate_review"] {
		t.Error("有 create_project 授权应解锁派活工具")
	}
	if !names["get_my_tasks"] {
		t.Error("自助工具不应被裁剪")
	}
	if names["company_overview"] {
		t.Error("超管专属工具不因普通授权解锁")
	}
}

func TestFilterByPermUsesToolMetadataForExternalTools(t *testing.T) {
	u := &store.User{ID: 2, Status: store.UserActive}
	ts := []ai.Tool{
		{Name: "external_admin", RequiredAction: reqSuper},
		{Name: "external_worker", RequiredAction: perm.ActManageWorker},
	}
	if got := namesOf(filterByPerm(append([]ai.Tool(nil), ts...), u, nil)); len(got) != 0 {
		t.Fatalf("无授权用户看到了外部工具: %v", got)
	}
	grants := []store.Grant{{Kind: store.KindActive, UserID: u.ID, Action: perm.ActManageWorker, Target: store.TargetAll}}
	got := namesOf(filterByPerm(append([]ai.Tool(nil), ts...), u, grants))
	if !got["external_worker"] || got["external_admin"] {
		t.Fatalf("外部工具元数据权限裁剪错误: %v", got)
	}
	super := &store.User{ID: 1, IsSuperadmin: true, Status: store.UserActive}
	got = namesOf(filterByPerm(append([]ai.Tool(nil), ts...), super, nil))
	if !got["external_worker"] || !got["external_admin"] {
		t.Fatalf("超管应看到全部外部工具: %v", got)
	}
}

// 拿到 manage_worker 授权后，AI 员工管理工具出现（目标级校验在 handler 内另做）。
func TestManageWorkerGrantUnlocks(t *testing.T) {
	u := &store.User{ID: 2, Status: store.UserActive}
	ts := []ai.Tool{{Name: "create_worker"}, {Name: "issue_worker_bind_code"}, {Name: "revoke_worker"}, {Name: "analyze_company_materials"}, {Name: "start_workflow"}, {Name: "company_overview"}, {Name: "set_worker_admin"}}
	grants := []store.Grant{{Kind: store.KindActive, UserID: 2, Action: perm.ActManageWorker, Target: store.TargetAll}}
	names := namesOf(filterByPerm(ts, u, grants))
	for _, want := range []string{"create_worker", "issue_worker_bind_code", "revoke_worker", "analyze_company_materials", "start_workflow"} {
		if !names[want] {
			t.Errorf("有 manage_worker 授权应解锁 %s", want)
		}
	}
	if names["company_overview"] {
		t.Error("超管专属工具不因 manage_worker 解锁")
	}
	if names["set_worker_admin"] {
		t.Error("admin worker 设置权不因 manage_worker 解锁")
	}
}

func TestInviteEmployeeAliasNormalizesToGenerateKey(t *testing.T) {
	if got := normalizeActiveAction("invite_employee"); got != perm.ActGenerateKey {
		t.Fatalf("invite_employee alias = %q, want %q", got, perm.ActGenerateKey)
	}
	if got := normalizeActiveAction(" generate_key "); got != perm.ActGenerateKey {
		t.Fatalf("generate_key trim = %q", got)
	}
	list := activeActionList()
	var found, foundGroup bool
	for _, item := range list {
		if item == perm.ActGenerateKey+"(invite_employee)" {
			found = true
		}
		if item == perm.ActManageTGGroup {
			foundGroup = true
		}
	}
	if !found {
		t.Fatalf("activeActionList 应展示邀请员工别名: %+v", list)
	}
	if !foundGroup {
		t.Fatalf("activeActionList 应展示 Telegram 群管理权限: %+v", list)
	}
}

func TestManageTelegramGroupGrantUnlocks(t *testing.T) {
	u := &store.User{ID: 2, Status: store.UserActive}
	grants := []store.Grant{{Kind: store.KindActive, UserID: 2, Action: perm.ActManageTGGroup, Target: store.TargetAll}}
	got := namesOf(filterByPerm(telegramGroupTools(Deps{}, u), u, grants))
	for _, name := range []string{"set_telegram_group_listen", "set_telegram_group_monitor", "set_telegram_group_digest", "list_telegram_group_messages", "send_telegram_group_message", "update_telegram_group_info"} {
		if !got[name] {
			t.Fatalf("manage_telegram_group 应解锁 %s", name)
		}
	}
}

func TestCanManagePermTarget(t *testing.T) {
	actor := &store.User{ID: 10, Status: store.UserActive}
	subordinate := &store.User{ID: 20, Status: store.UserActive}
	peer := &store.User{ID: 30, Status: store.UserActive}
	super := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	grants := []store.Grant{{Kind: store.KindActive, Action: perm.ActManagePerm, Target: "20"}}
	if msg := canManagePermTarget(actor, subordinate, grants); msg != "" {
		t.Fatalf("有 manage_perm:20 应可管理下属, got %q", msg)
	}
	if msg := canManagePermTarget(actor, peer, grants); msg == "" {
		t.Fatal("没有 manage_perm:30 不应能管理同级")
	}
	if msg := canManagePermTarget(actor, super, []store.Grant{{Kind: store.KindActive, Action: perm.ActManagePerm, Target: store.TargetAll}}); msg == "" {
		t.Fatal("非超管即使有 _all 也不应能管理超级管理员")
	}
	if msg := canManagePermTarget(super, actor, nil); msg != "" {
		t.Fatalf("超管应旁路目标级校验, got %q", msg)
	}
}

// worker 机器账号：只保留白名单最小集。
func TestForUserWorkerMinimalSet(t *testing.T) {
	w := &store.User{ID: 3, Name: "小码", Status: store.UserActive, IsWorker: true}
	ts := ForUser(Deps{}, w, nil)
	names := namesOf(ts)
	for n := range names {
		if !workerAllowed[n] {
			t.Errorf("worker 工具集出现白名单外的 %s", n)
		}
	}
	for _, keep := range []string{"get_my_tasks", "add_progress", "save_knowledge", "search_knowledge", "search_skills", "load_skill"} {
		if !names[keep] {
			t.Errorf("worker 应保留 %s", keep)
		}
	}
	for _, gone := range []string{"generate_api_token", "list_workers", "run_worker_command", "grant_active_perm", "schedule_once", "accept_task"} {
		if names[gone] {
			t.Errorf("worker 不应有 %s", gone)
		}
	}
}

func TestForUserAdminWorkerSeesSystemTools(t *testing.T) {
	w := &store.User{ID: 3, Name: "nbco-admin-worker", Status: store.UserActive, IsWorker: true, IsSuperadmin: true}
	names := namesOf(ForUser(Deps{}, w, nil))
	for _, want := range []string{"assign_task", "set_worker_admin", "analyze_company_materials", "approve_learning_candidate"} {
		if !names[want] {
			t.Errorf("admin worker 应看到 %s", want)
		}
	}
}

// 超管：全量可见（包括权限门控与超管专属）。
func TestForUserSuperadminSeesAll(t *testing.T) {
	su := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	names := namesOf(ForUser(Deps{}, su, nil))
	for _, want := range []string{
		"assign_task", "delegate_review", "invite_employee", "company_overview", "get_ai_settings", "set_ai_settings",
		"ai_usage_stats", "create_worker", "run_worker_command", "analyze_company_materials", "send_message", "update_user_info", "bulk_update_user_info", "grant_passive_perm",
		"create_data_collection_campaign", "list_data_collection_campaigns", "get_data_collection_campaign", "send_data_collection_reminder", "close_data_collection_campaign",
		"save_skill", "update_skill", "search_skills", "load_skill", "start_worker_skill",
		"list_workflows", "start_workflow", "list_capabilities", "list_system_activity",
		"low_level_db_query", "low_level_db_exec",
		"list_telegram_group_members", "resolve_telegram_group_members", "get_telegram_group_member",
		"send_telegram_group_message", "edit_telegram_group_message", "delete_telegram_group_message",
		"pin_telegram_group_message", "unpin_telegram_group_message", "update_telegram_group_info",
		"set_telegram_group_listen", "set_telegram_group_auto_invite", "set_telegram_group_monitor", "set_telegram_group_digest", "list_telegram_group_messages",
	} {
		if !names[want] {
			t.Errorf("超管应看到 %s", want)
		}
	}
}

// 注册表里的名字必须真实存在（防拼写漂移导致门控失效）。
func TestToolPermRegistryNamesExist(t *testing.T) {
	su := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	names := namesOf(ForUser(Deps{}, su, nil))
	for n := range toolPerm {
		if !names[n] {
			t.Errorf("toolPerm 注册了不存在的工具 %s", n)
		}
	}
	for n := range workerAllowed {
		if !names[n] {
			t.Errorf("workerAllowed 注册了不存在的工具 %s", n)
		}
	}
}

func TestStripGroupSensitive(t *testing.T) {
	su := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	full := namesOf(ForUser(Deps{}, su, nil))
	grouped := namesOf(StripGroupSensitive(ForUser(Deps{}, su, nil)))
	// 群里必须剔除的机密/高危工具。
	for _, gone := range []string{
		"generate_api_token", "revoke_api_token", "invite_employee", "cancel_invites", "send_message",
		"grant_active_perm", "grant_passive_perm", "disable_user", "create_worker", "run_worker_command",
		"get_ai_settings", "set_ai_settings", "ai_usage_stats", "schedule_push", "update_user_info", "bulk_update_user_info", "save_skill", "update_skill",
		"low_level_db_query", "low_level_db_exec",
		"send_file", "start_workflow", "start_worker_skill",
		"create_data_collection_campaign", "list_data_collection_campaigns", "get_data_collection_campaign", "send_data_collection_reminder", "close_data_collection_campaign",
		"list_action_turns", "list_system_activity",
		"send_telegram_group_message", "edit_telegram_group_message", "delete_telegram_group_message",
		"pin_telegram_group_message", "unpin_telegram_group_message", "update_telegram_group_info",
		"set_telegram_group_listen", "set_telegram_group_auto_invite", "set_telegram_group_monitor", "set_telegram_group_digest", "list_telegram_group_messages",
	} {
		if !full[gone] {
			t.Fatalf("前置条件：超管私聊应有 %s", gone)
		}
		if grouped[gone] {
			t.Errorf("群里不应有高危工具 %s", gone)
		}
	}
	// 日常工具保留。
	for _, keep := range []string{
		"get_my_tasks", "assign_task", "delegate_review", "search_knowledge", "company_overview",
		"list_telegram_groups", "get_telegram_group", "list_telegram_group_members", "resolve_telegram_group_members", "get_telegram_group_member",
		"search_skills", "load_skill",
	} {
		if !grouped[keep] {
			t.Errorf("群里应保留 %s", keep)
		}
	}
}

func TestStripApprovalRequired(t *testing.T) {
	su := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	full := namesOf(ForUser(Deps{}, su, nil))
	stripped := namesOf(StripApprovalRequired(ForUser(Deps{}, su, nil)))
	for gone := range approvalRequired {
		if !full[gone] {
			t.Fatalf("approvalRequired 注册了不存在的工具 %s", gone)
		}
		if stripped[gone] {
			t.Fatalf("MCP/无确认轮次入口不应暴露高危审批工具 %s", gone)
		}
	}
	if !stripped["company_overview"] {
		t.Fatal("非审批工具不应被剔除")
	}
}

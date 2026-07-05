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
		"invite_employee", "send_message", "update_user_info", "save_infos_on_user",
		"grant_passive_perm", "revoke_passive_perm", "view_user_perms",
		"company_overview", "get_ai_settings", "set_ai_settings", "create_worker", "run_worker_command", "create_role", "disable_user",
	} {
		if names[gone] {
			t.Errorf("无授权用户不应看到 %s", gone)
		}
	}
	for _, keep := range []string{
		"get_my_tasks", "update_my_task_status", "accept_task", "split_my_task",
		"save_knowledge", "search_knowledge", "list_workers", "list_roles",
		"activate_role", "schedule_once", "get_my_profile", "grant_active_perm",
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
	for _, keep := range []string{"get_my_tasks", "add_progress", "save_knowledge", "search_knowledge"} {
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

// 超管：全量可见（包括权限门控与超管专属）。
func TestForUserSuperadminSeesAll(t *testing.T) {
	su := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	names := namesOf(ForUser(Deps{}, su, nil))
	for _, want := range []string{"assign_task", "delegate_review", "invite_employee", "company_overview", "get_ai_settings", "set_ai_settings", "create_worker", "run_worker_command", "send_message", "grant_passive_perm"} {
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
		"get_ai_settings", "set_ai_settings", "schedule_push",
	} {
		if !full[gone] {
			t.Fatalf("前置条件：超管私聊应有 %s", gone)
		}
		if grouped[gone] {
			t.Errorf("群里不应有高危工具 %s", gone)
		}
	}
	// 日常工具保留。
	for _, keep := range []string{"get_my_tasks", "assign_task", "delegate_review", "search_knowledge", "company_overview"} {
		if !grouped[keep] {
			t.Errorf("群里应保留 %s", keep)
		}
	}
}

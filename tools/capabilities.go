package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

const (
	CapabilityPeople     = "people"
	CapabilityWork       = "work"
	CapabilityWorkers    = "workers"
	CapabilityMemory     = "memory"
	CapabilityComms      = "comms"
	CapabilityAutomation = "automation"
	CapabilityOps        = "ops"
	CapabilityResearch   = "research"
	CapabilityExtension  = "extension"
)

const (
	ToolEffectRead    = ai.ToolEffectRead
	ToolEffectWrite   = ai.ToolEffectWrite
	ToolEffectExecute = ai.ToolEffectExecute
	ToolEffectUnknown = ai.ToolEffectUnknown
)

type Capability struct {
	Name             string `json:"name"`
	Domain           string `json:"domain"`
	Effect           string `json:"effect"`
	LoadMode         string `json:"load_mode"`
	Completion       string `json:"completion"`
	Description      string `json:"description"`
	RequiredAction   string `json:"required_action,omitempty"`
	Risk             string `json:"risk"`
	Available        bool   `json:"available"`
	SuperadminOnly   bool   `json:"superadmin_only"`
	WorkerAllowed    bool   `json:"worker_allowed"`
	GroupAllowed     bool   `json:"group_allowed"`
	ApprovalRequired bool   `json:"approval_required"`
}

var domainLabels = map[string]string{
	CapabilityPeople:     "人员与权限",
	CapabilityWork:       "目标/项目/任务",
	CapabilityWorkers:    "AI Worker",
	CapabilityMemory:     "知识/规则/学习",
	CapabilityComms:      "沟通/文件/群",
	CapabilityAutomation: "自动化/脚本/工作流",
	CapabilityOps:        "运维",
	CapabilityResearch:   "公开信息检索",
	CapabilityExtension:  "外部扩展",
}

var domainOrder = []string{
	CapabilityPeople,
	CapabilityWork,
	CapabilityWorkers,
	CapabilityMemory,
	CapabilityComms,
	CapabilityAutomation,
	CapabilityOps,
	CapabilityResearch,
	CapabilityExtension,
}

var toolDomain = map[string]string{
	// people
	"get_my_profile":         CapabilityPeople,
	"update_my_profile":      CapabilityPeople,
	"get_my_infos":           CapabilityPeople,
	"save_my_infos":          CapabilityPeople,
	"view_user_infos":        CapabilityPeople,
	"get_my_infos_on_user":   CapabilityPeople,
	"save_infos_on_user":     CapabilityPeople,
	"list_my_active_perms":   CapabilityPeople,
	"list_my_passive_perms":  CapabilityPeople,
	"grant_my_passive_perm":  CapabilityPeople,
	"revoke_my_passive_perm": CapabilityPeople,
	"grant_active_perm":      CapabilityPeople,
	"revoke_active_perm":     CapabilityPeople,
	"grant_passive_perm":     CapabilityPeople,
	"revoke_passive_perm":    CapabilityPeople,
	"view_user_perms":        CapabilityPeople,
	"list_users":             CapabilityPeople,
	"get_user_info":          CapabilityPeople,
	"get_users_info":         CapabilityPeople,
	"update_user_info":       CapabilityPeople,
	"bulk_update_user_info":  CapabilityPeople,
	"invite_employee":        CapabilityPeople,
	"cancel_invites":         CapabilityPeople,
	"get_user_stats":         CapabilityPeople,
	"list_info_fields":       CapabilityPeople,
	"add_info_field":         CapabilityPeople,
	"remove_info_field":      CapabilityPeople,
	"disable_user":           CapabilityPeople,
	"enable_user":            CapabilityPeople,
	"list_roles":             CapabilityPeople,
	"activate_role":          CapabilityPeople,
	"create_role":            CapabilityPeople,
	"update_role":            CapabilityPeople,
	"delete_role":            CapabilityPeople,
	"create_org_group":       CapabilityPeople,
	"list_org_groups":        CapabilityPeople,
	"add_org_group_member":   CapabilityPeople,
	"get_api_token_status":   CapabilityPeople,
	"generate_api_token":     CapabilityPeople,
	"revoke_api_token":       CapabilityPeople,

	// work
	"get_my_projects":        CapabilityWork,
	"get_my_tasks":           CapabilityWork,
	"get_my_all_tasks":       CapabilityWork,
	"get_task_detail":        CapabilityWork,
	"view_my_task_tree":      CapabilityWork,
	"update_my_task_status":  CapabilityWork,
	"get_review_queue":       CapabilityWork,
	"accept_task":            CapabilityWork,
	"reject_task":            CapabilityWork,
	"save_checklist":         CapabilityWork,
	"toggle_checklist":       CapabilityWork,
	"add_progress":           CapabilityWork,
	"attach_to_task":         CapabilityWork,
	"split_my_task":          CapabilityWork,
	"get_assigned_tasks":     CapabilityWork,
	"update_assigned_task":   CapabilityWork,
	"set_task_participants":  CapabilityWork,
	"cancel_assigned_task":   CapabilityWork,
	"delete_assigned_task":   CapabilityWork,
	"reassign_task":          CapabilityWork,
	"create_project":         CapabilityWork,
	"list_my_projects":       CapabilityWork,
	"assign_task":            CapabilityWork,
	"view_project":           CapabilityWork,
	"archive_project":        CapabilityWork,
	"delete_project":         CapabilityWork,
	"delegate_review":        CapabilityWork,
	"create_goal":            CapabilityWork,
	"add_milestone":          CapabilityWork,
	"decompose_milestone":    CapabilityWork,
	"update_goal":            CapabilityWork,
	"close_goal":             CapabilityWork,
	"update_milestone":       CapabilityWork,
	"close_milestone":        CapabilityWork,
	"link_task_to_milestone": CapabilityWork,
	"view_goals":             CapabilityWork,
	"get_goal_detail":        CapabilityWork,
	"get_milestone_detail":   CapabilityWork,
	"refresh_decision_queue": CapabilityWork,
	"list_decision_queue":    CapabilityWork,
	"company_overview":       CapabilityWork,
	"search_workspace":       CapabilityWork,

	// workers
	"list_workers":              CapabilityWorkers,
	"create_worker":             CapabilityWorkers,
	"issue_worker_bind_code":    CapabilityWorkers,
	"run_worker_command":        CapabilityWorkers,
	"delegate_worker_agent":     CapabilityWorkers,
	"list_worker_runs":          CapabilityWorkers,
	"cancel_worker_run":         CapabilityWorkers,
	"revoke_worker":             CapabilityWorkers,
	"set_worker_admin":          CapabilityWorkers,
	"analyze_company_materials": CapabilityWorkers,
	"start_worker_skill":        CapabilityWorkers,

	// memory
	"save_knowledge":             CapabilityMemory,
	"search_knowledge":           CapabilityMemory,
	"get_knowledge":              CapabilityMemory,
	"update_knowledge":           CapabilityMemory,
	"delete_knowledge":           CapabilityMemory,
	"set_knowledge_active":       CapabilityMemory,
	"list_recent_knowledge":      CapabilityMemory,
	"search_history":             CapabilityMemory,
	"save_rule":                  CapabilityMemory,
	"list_rules":                 CapabilityMemory,
	"set_rule_pinned":            CapabilityMemory,
	"search_skills":              CapabilityMemory,
	"load_skill":                 CapabilityMemory,
	"save_skill":                 CapabilityMemory,
	"update_skill":               CapabilityMemory,
	"propose_learning_candidate": CapabilityMemory,
	"list_learning_candidates":   CapabilityMemory,
	"approve_learning_candidate": CapabilityMemory,
	"reject_learning_candidate":  CapabilityMemory,
	"score_learning_candidates":  CapabilityMemory,
	"list_knowledge_versions":    CapabilityMemory,
	"rollback_knowledge":         CapabilityMemory,
	"list_material_entities":     CapabilityMemory,
	"list_eval_cases":            CapabilityMemory,
	"create_eval_case":           CapabilityMemory,

	// comms
	"schedule_once":                   CapabilityComms,
	"schedule_repeating":              CapabilityComms,
	"schedule_once_push":              CapabilityComms,
	"schedule_recurring_push":         CapabilityComms,
	"update_schedule":                 CapabilityComms,
	"cancel_schedule":                 CapabilityComms,
	"list_schedules":                  CapabilityComms,
	"list_recent_files":               CapabilityComms,
	"delete_file":                     CapabilityComms,
	"send_file":                       CapabilityComms,
	"send_message":                    CapabilityComms,
	"create_data_collection_campaign": CapabilityComms,
	"list_data_collection_campaigns":  CapabilityComms,
	"get_data_collection_campaign":    CapabilityComms,
	"send_data_collection_reminder":   CapabilityComms,
	"close_data_collection_campaign":  CapabilityComms,
	"list_telegram_groups":            CapabilityComms,
	"get_telegram_group":              CapabilityComms,
	"list_telegram_group_members":     CapabilityComms,
	"resolve_telegram_group_members":  CapabilityComms,
	"get_telegram_group_member":       CapabilityComms,
	"set_telegram_group_listen":       CapabilityComms,
	"set_telegram_group_auto_invite":  CapabilityComms,
	"set_telegram_group_monitor":      CapabilityComms,
	"set_telegram_group_digest":       CapabilityComms,
	"list_telegram_group_messages":    CapabilityComms,
	"send_telegram_group_message":     CapabilityComms,
	"edit_telegram_group_message":     CapabilityComms,
	"delete_telegram_group_message":   CapabilityComms,
	"pin_telegram_group_message":      CapabilityComms,
	"unpin_telegram_group_message":    CapabilityComms,
	"update_telegram_group_info":      CapabilityComms,
	"bind_telegram_group_project":     CapabilityComms,

	// automation
	"list_workflows":     CapabilityAutomation,
	"start_workflow":     CapabilityAutomation,
	"list_script_tools":  CapabilityAutomation,
	"create_script_tool": CapabilityAutomation,
	"update_script_tool": CapabilityAutomation,
	"test_script_tool":   CapabilityAutomation,
	"enable_script_tool": CapabilityAutomation,

	// ops
	"list_capabilities":    CapabilityOps,
	"list_action_turns":    CapabilityOps,
	"list_system_activity": CapabilityOps,
	"query_data":           CapabilityOps,
	"fetch_url":            CapabilityResearch,
	"get_ai_settings":      CapabilityOps,
	"set_ai_settings":      CapabilityOps,
	"ai_usage_stats":       CapabilityOps,
	"low_level_db_query":   CapabilityOps,
	"low_level_db_exec":    CapabilityOps,
}

var toolEffect = map[string]string{
	"accept_task":                     ToolEffectWrite,
	"activate_role":                   ToolEffectWrite,
	"add_info_field":                  ToolEffectWrite,
	"add_milestone":                   ToolEffectWrite,
	"add_org_group_member":            ToolEffectWrite,
	"add_progress":                    ToolEffectWrite,
	"ai_usage_stats":                  ToolEffectRead,
	"analyze_company_materials":       ToolEffectExecute,
	"approve_learning_candidate":      ToolEffectWrite,
	"archive_project":                 ToolEffectWrite,
	"assign_task":                     ToolEffectWrite,
	"attach_to_task":                  ToolEffectWrite,
	"bind_telegram_group_project":     ToolEffectWrite,
	"bulk_update_user_info":           ToolEffectWrite,
	"cancel_invites":                  ToolEffectWrite,
	"cancel_assigned_task":            ToolEffectWrite,
	"cancel_schedule":                 ToolEffectWrite,
	"close_data_collection_campaign":  ToolEffectWrite,
	"close_goal":                      ToolEffectWrite,
	"close_milestone":                 ToolEffectWrite,
	"company_overview":                ToolEffectRead,
	"create_eval_case":                ToolEffectWrite,
	"create_data_collection_campaign": ToolEffectWrite,
	"create_goal":                     ToolEffectWrite,
	"create_org_group":                ToolEffectWrite,
	"create_project":                  ToolEffectWrite,
	"create_role":                     ToolEffectWrite,
	"create_script_tool":              ToolEffectWrite,
	"create_worker":                   ToolEffectWrite,
	"decompose_milestone":             ToolEffectWrite,
	"delegate_review":                 ToolEffectWrite,
	"delegate_worker_agent":           ToolEffectExecute,
	"delete_assigned_task":            ToolEffectWrite,
	"delete_file":                     ToolEffectWrite,
	"delete_knowledge":                ToolEffectWrite,
	"delete_project":                  ToolEffectWrite,
	"delete_role":                     ToolEffectWrite,
	"delete_telegram_group_message":   ToolEffectWrite,
	"disable_user":                    ToolEffectWrite,
	"edit_telegram_group_message":     ToolEffectWrite,
	"enable_script_tool":              ToolEffectWrite,
	"enable_user":                     ToolEffectWrite,
	"generate_api_token":              ToolEffectWrite,
	"get_ai_settings":                 ToolEffectRead,
	"get_api_token_status":            ToolEffectRead,
	"get_assigned_tasks":              ToolEffectRead,
	"get_data_collection_campaign":    ToolEffectRead,
	"get_goal_detail":                 ToolEffectRead,
	"get_knowledge":                   ToolEffectRead,
	"get_milestone_detail":            ToolEffectRead,
	"get_my_all_tasks":                ToolEffectRead,
	"get_my_infos":                    ToolEffectRead,
	"get_my_infos_on_user":            ToolEffectRead,
	"get_my_profile":                  ToolEffectRead,
	"get_my_projects":                 ToolEffectRead,
	"get_my_tasks":                    ToolEffectRead,
	"get_review_queue":                ToolEffectRead,
	"get_task_detail":                 ToolEffectRead,
	"get_telegram_group":              ToolEffectRead,
	"list_telegram_group_messages":    ToolEffectRead,
	"get_telegram_group_member":       ToolEffectRead,
	"get_user_info":                   ToolEffectRead,
	"get_users_info":                  ToolEffectRead,
	"get_user_stats":                  ToolEffectRead,
	"grant_active_perm":               ToolEffectWrite,
	"grant_my_passive_perm":           ToolEffectWrite,
	"grant_passive_perm":              ToolEffectWrite,
	"invite_employee":                 ToolEffectWrite,
	"issue_worker_bind_code":          ToolEffectWrite,
	"link_task_to_milestone":          ToolEffectWrite,
	"list_action_turns":               ToolEffectRead,
	"list_system_activity":            ToolEffectRead,
	"query_data":                      ToolEffectRead,
	"fetch_url":                       ToolEffectRead,
	"list_capabilities":               ToolEffectRead,
	"list_data_collection_campaigns":  ToolEffectRead,
	"list_decision_queue":             ToolEffectRead,
	"list_eval_cases":                 ToolEffectRead,
	"list_info_fields":                ToolEffectRead,
	"list_knowledge_versions":         ToolEffectRead,
	"list_learning_candidates":        ToolEffectRead,
	"list_material_entities":          ToolEffectRead,
	"list_my_active_perms":            ToolEffectRead,
	"list_my_passive_perms":           ToolEffectRead,
	"list_my_projects":                ToolEffectRead,
	"list_org_groups":                 ToolEffectRead,
	"list_recent_files":               ToolEffectRead,
	"search_workspace":                ToolEffectRead,
	"list_recent_knowledge":           ToolEffectRead,
	"list_roles":                      ToolEffectRead,
	"list_rules":                      ToolEffectRead,
	"list_schedules":                  ToolEffectRead,
	"list_script_tools":               ToolEffectRead,
	"list_telegram_group_members":     ToolEffectRead,
	"list_telegram_groups":            ToolEffectRead,
	"list_users":                      ToolEffectRead,
	"list_workers":                    ToolEffectRead,
	"list_worker_runs":                ToolEffectRead,
	"list_workflows":                  ToolEffectRead,
	"low_level_db_exec":               ToolEffectWrite,
	"low_level_db_query":              ToolEffectRead,
	"load_skill":                      ToolEffectRead,
	"pin_telegram_group_message":      ToolEffectWrite,
	"propose_learning_candidate":      ToolEffectWrite,
	"reassign_task":                   ToolEffectWrite,
	"refresh_decision_queue":          ToolEffectWrite,
	"reject_learning_candidate":       ToolEffectWrite,
	"reject_task":                     ToolEffectWrite,
	"remove_info_field":               ToolEffectWrite,
	"resolve_telegram_group_members":  ToolEffectRead,
	"revoke_active_perm":              ToolEffectWrite,
	"revoke_api_token":                ToolEffectWrite,
	"revoke_my_passive_perm":          ToolEffectWrite,
	"revoke_passive_perm":             ToolEffectWrite,
	"revoke_worker":                   ToolEffectWrite,
	"rollback_knowledge":              ToolEffectWrite,
	"run_worker_command":              ToolEffectExecute,
	"cancel_worker_run":               ToolEffectWrite,
	"save_checklist":                  ToolEffectWrite,
	"save_infos_on_user":              ToolEffectWrite,
	"save_knowledge":                  ToolEffectWrite,
	"save_my_infos":                   ToolEffectWrite,
	"save_rule":                       ToolEffectWrite,
	"save_skill":                      ToolEffectWrite,
	"schedule_once":                   ToolEffectWrite,
	"schedule_once_push":              ToolEffectWrite,
	"schedule_recurring_push":         ToolEffectWrite,
	"schedule_repeating":              ToolEffectWrite,
	"update_schedule":                 ToolEffectWrite,
	"score_learning_candidates":       ToolEffectWrite,
	"search_history":                  ToolEffectRead,
	"search_knowledge":                ToolEffectRead,
	"search_skills":                   ToolEffectRead,
	"send_file":                       ToolEffectWrite,
	"send_data_collection_reminder":   ToolEffectWrite,
	"send_message":                    ToolEffectWrite,
	"set_task_participants":           ToolEffectWrite,
	"send_telegram_group_message":     ToolEffectWrite,
	"set_ai_settings":                 ToolEffectWrite,
	"set_rule_pinned":                 ToolEffectWrite,
	"set_knowledge_active":            ToolEffectWrite,
	"set_telegram_group_auto_invite":  ToolEffectWrite,
	"set_telegram_group_listen":       ToolEffectWrite,
	"set_telegram_group_monitor":      ToolEffectWrite,
	"set_telegram_group_digest":       ToolEffectWrite,
	"set_worker_admin":                ToolEffectWrite,
	"split_my_task":                   ToolEffectWrite,
	"start_worker_skill":              ToolEffectExecute,
	"start_workflow":                  ToolEffectExecute,
	"test_script_tool":                ToolEffectExecute,
	"toggle_checklist":                ToolEffectWrite,
	"unpin_telegram_group_message":    ToolEffectWrite,
	"update_assigned_task":            ToolEffectWrite,
	"update_goal":                     ToolEffectWrite,
	"update_knowledge":                ToolEffectWrite,
	"update_milestone":                ToolEffectWrite,
	"update_my_profile":               ToolEffectWrite,
	"update_my_task_status":           ToolEffectWrite,
	"update_role":                     ToolEffectWrite,
	"update_script_tool":              ToolEffectWrite,
	"update_skill":                    ToolEffectWrite,
	"update_telegram_group_info":      ToolEffectWrite,
	"update_user_info":                ToolEffectWrite,
	"view_goals":                      ToolEffectRead,
	"view_my_task_tree":               ToolEffectRead,
	"view_project":                    ToolEffectRead,
	"view_user_infos":                 ToolEffectRead,
	"view_user_perms":                 ToolEffectRead,
}

func capabilityTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		immediateTool(tool("list_capabilities", "查看 nbco 当前可用能力目录，按领域列出工具、权限、风险和当前用户是否可用。",
			obj(map[string]any{
				"domain":              p("string", "可选：people/work/workers/memory/comms/automation/ops/extension"),
				"include_unavailable": p("boolean", "可选：超管为 true 时显示当前入口不可用/无权限能力；普通用户会被忽略"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Domain             string `json:"domain"`
					IncludeUnavailable bool   `json:"include_unavailable"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				includeUnavailable := args.IncludeUnavailable && u != nil && u.IsSuperadmin
				caps, err := CapabilityRegistry(ctx, d, u, includeUnavailable)
				if err != nil {
					return "", err
				}
				domain := strings.TrimSpace(args.Domain)
				if domain != "" {
					filtered := caps[:0]
					for _, c := range caps {
						if c.Domain == domain {
							filtered = append(filtered, c)
						}
					}
					caps = filtered
				}
				return renderCapabilities(caps), nil
			})),
	}
}

func CapabilityRegistry(ctx context.Context, d Deps, u *store.User, includeUnavailable bool) ([]Capability, error) {
	if u == nil {
		u = &store.User{}
	}
	all := baseStaticTools(d, u)
	var grants []store.Grant
	if !u.IsSuperadmin && d.Store != nil {
		var err error
		grants, err = d.Store.PermsOf(ctx, u.ID)
		if err != nil {
			return nil, err
		}
	}
	all = append(all, dynamicScriptTools(ctx, d, u, grants)...)
	all = dedupeTools(all)
	available := map[string]bool{}
	filterInput := make([]ai.Tool, len(all))
	copy(filterInput, all)
	for _, t := range filterByPerm(filterInput, u, grants) {
		available[t.Name] = true
	}
	out := make([]Capability, 0, len(all))
	seen := map[string]bool{}
	for _, t := range all {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		c := capabilityForTool(t)
		c.Available = available[t.Name]
		if includeUnavailable || c.Available {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := domainRank(out[i].Domain), domainRank(out[j].Domain)
		if di != dj {
			return di < dj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func capabilityForTool(t ai.Tool) Capability {
	required := strings.TrimSpace(t.RequiredAction)
	if required == "" {
		if req, ok := toolPerm[t.Name]; ok {
			required = req
		}
	}
	superOnly := required == reqSuper
	risk := "normal"
	switch {
	case requiresApproval(t):
		risk = "approval"
	case t.GroupSensitive || groupSensitive[t.Name]:
		risk = "sensitive"
	case superOnly:
		risk = "admin"
	}
	domain := strings.TrimSpace(t.Domain)
	if domain == "" {
		domain = capabilityDomain(t.Name)
	}
	return Capability{
		Name:             t.Name,
		Domain:           domain,
		Effect:           effectForTool(t),
		LoadMode:         string(t.LoadMode),
		Completion:       string(t.Completion),
		Description:      t.Description,
		RequiredAction:   required,
		Risk:             risk,
		SuperadminOnly:   superOnly,
		WorkerAllowed:    workerAllowed[t.Name],
		GroupAllowed:     !t.GroupSensitive && !groupSensitive[t.Name],
		ApprovalRequired: requiresApproval(t),
	}
}

func ToolEffect(name string) string {
	if effect, ok := toolEffect[name]; ok {
		return effect
	}
	return ToolEffectUnknown
}

// IsBuiltinTool 报告名称是否属于 nbco 内建能力注册表。
func IsBuiltinTool(name string) bool {
	_, ok := toolEffect[name]
	return ok
}

func effectForTool(t ai.Tool) string {
	switch t.Effect {
	case ToolEffectRead, ToolEffectWrite, ToolEffectExecute:
		return t.Effect
	default:
		return ToolEffect(t.Name)
	}
}

func ToolCanProveAction(name string) bool {
	switch ToolEffect(name) {
	case ToolEffectWrite, ToolEffectExecute:
		return true
	default:
		return false
	}
}

// ToolCanProveActionTool 使用工具实例上的元数据判断成功调用能否证明动作完成。
func ToolCanProveActionTool(t ai.Tool) bool {
	switch effectForTool(t) {
	case ToolEffectWrite, ToolEffectExecute:
		return true
	default:
		return false
	}
}

// ReadOnlyTools returns only capabilities whose declared or built-in effect is
// read-only. Unknown external tools are excluded because a system-generated
// report must not guess whether an extension has side effects.
func ReadOnlyTools(in []ai.Tool) []ai.Tool {
	out := make([]ai.Tool, 0, len(in))
	for _, t := range in {
		if effectForTool(t) == ToolEffectRead {
			out = append(out, t)
		}
	}
	return out
}

// CapabilityDomain 返回工具所属的稳定能力领域。未知/外部工具归入 extension，
// 让上层语义路由不依赖维护另一份工具名清单。
func CapabilityDomain(name string) string {
	if d, ok := toolDomain[name]; ok {
		return d
	}
	if strings.HasPrefix(name, "script_") {
		return CapabilityAutomation
	}
	return CapabilityExtension
}

func capabilityDomain(name string) string { return CapabilityDomain(name) }

func domainRank(domain string) int {
	for i, d := range domainOrder {
		if d == domain {
			return i
		}
	}
	return len(domainOrder)
}

func renderCapabilities(caps []Capability) string {
	if len(caps) == 0 {
		return "（没有匹配的能力）"
	}
	var b strings.Builder
	b.WriteString("nbco 能力目录\n")
	cur := ""
	for _, c := range caps {
		if c.Domain != cur {
			cur = c.Domain
			label := domainLabels[cur]
			if label == "" {
				label = cur
			}
			fmt.Fprintf(&b, "\n%s\n", label)
		}
		status := "可用"
		if !c.Available {
			status = "不可用"
		}
		req := ""
		if c.RequiredAction != "" {
			req = "；权限=" + c.RequiredAction
		}
		flags := []string{status, "效果=" + c.Effect, "风险=" + c.Risk}
		if c.Completion == string(ai.ToolCompletionAsynchronous) {
			flags = append(flags, "异步受理")
		}
		if !c.GroupAllowed {
			flags = append(flags, "群聊禁用")
		}
		if c.WorkerAllowed {
			flags = append(flags, "worker可用")
		}
		fmt.Fprintf(&b, "• %s：%s（%s%s）\n", c.Name, firstSentence(c.Description), strings.Join(flags, "，"), req)
	}
	return strings.TrimSpace(b.String())
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, sep := range []string{"。", "\n"} {
		if i := strings.Index(s, sep); i >= 0 {
			return strings.TrimSpace(s[:i+len(sep)])
		}
	}
	return s
}

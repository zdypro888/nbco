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
	CapabilityExtension  = "extension"
)

type Capability struct {
	Name             string `json:"name"`
	Domain           string `json:"domain"`
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

	// workers
	"list_workers":              CapabilityWorkers,
	"create_worker":             CapabilityWorkers,
	"issue_worker_bind_code":    CapabilityWorkers,
	"run_worker_command":        CapabilityWorkers,
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
	"schedule_once":                  CapabilityComms,
	"schedule_repeating":             CapabilityComms,
	"schedule_push":                  CapabilityComms,
	"cancel_schedule":                CapabilityComms,
	"list_schedules":                 CapabilityComms,
	"list_recent_files":              CapabilityComms,
	"send_file":                      CapabilityComms,
	"send_message":                   CapabilityComms,
	"list_telegram_groups":           CapabilityComms,
	"get_telegram_group":             CapabilityComms,
	"list_telegram_group_members":    CapabilityComms,
	"resolve_telegram_group_members": CapabilityComms,
	"get_telegram_group_member":      CapabilityComms,
	"set_telegram_group_listen":      CapabilityComms,
	"set_telegram_group_auto_invite": CapabilityComms,
	"send_telegram_group_message":    CapabilityComms,
	"edit_telegram_group_message":    CapabilityComms,
	"delete_telegram_group_message":  CapabilityComms,
	"pin_telegram_group_message":     CapabilityComms,
	"unpin_telegram_group_message":   CapabilityComms,
	"update_telegram_group_info":     CapabilityComms,
	"bind_telegram_group_project":    CapabilityComms,

	// automation
	"list_workflows":     CapabilityAutomation,
	"start_workflow":     CapabilityAutomation,
	"list_script_tools":  CapabilityAutomation,
	"create_script_tool": CapabilityAutomation,
	"update_script_tool": CapabilityAutomation,
	"test_script_tool":   CapabilityAutomation,
	"enable_script_tool": CapabilityAutomation,

	// ops
	"list_capabilities": CapabilityOps,
	"get_ai_settings":   CapabilityOps,
	"set_ai_settings":   CapabilityOps,
	"ai_usage_stats":    CapabilityOps,
}

func capabilityTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("list_capabilities", "查看 nbco 当前可用能力目录。按领域列出工具、权限、风险和当前用户是否可用；用户问“你会什么/系统有哪些功能/某类能力在哪里”时调用。",
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
			}),
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
	all = append(all, dynamicScriptTools(d, u, grants)...)
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
	required := ""
	superOnly := false
	if req, ok := toolPerm[t.Name]; ok {
		if req == reqSuper {
			required = reqSuper
			superOnly = true
		} else {
			required = req
		}
	}
	risk := "normal"
	switch {
	case approvalRequired[t.Name]:
		risk = "approval"
	case groupSensitive[t.Name]:
		risk = "sensitive"
	case superOnly:
		risk = "admin"
	}
	return Capability{
		Name:             t.Name,
		Domain:           capabilityDomain(t.Name),
		Description:      t.Description,
		RequiredAction:   required,
		Risk:             risk,
		SuperadminOnly:   superOnly,
		WorkerAllowed:    workerAllowed[t.Name],
		GroupAllowed:     !groupSensitive[t.Name],
		ApprovalRequired: approvalRequired[t.Name],
	}
}

func capabilityDomain(name string) string {
	if d, ok := toolDomain[name]; ok {
		return d
	}
	if strings.HasPrefix(name, "script_") {
		return CapabilityAutomation
	}
	return CapabilityExtension
}

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
		flags := []string{status, "风险=" + c.Risk}
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

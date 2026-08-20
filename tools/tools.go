// Package tools 把领域能力组装成 ai.Tool 集合：
// 权限校验在 handler 内完成（工具即权限边界），每次调用写审计日志。
// 同一套工具供所有入口复用：TG 对话（经引擎）、HTTP API、外部 MCP。
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/knowledge"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/semantic"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	"github.com/zdypro888/nbco/workerhub"
)

// Deps 工具依赖。
type Deps struct {
	Store    *store.Store
	Notifier notify.Notifier
	TZ       *time.Location
	// Knowledge 知识库服务（语义+词法混合检索）。可为 nil（测试场景），
	// 此时知识工具回退到直接走 store 的词法检索。
	Knowledge *knowledge.Service
	// Semantic 是 Qdrant 统一语义索引。它只返回稳定ID；工具读取内容时仍
	// 必须经 Store 的权限感知读模型复核。
	Semantic *semantic.Service
	// Workers worker 实时通道（可为 nil）：派活时唤醒在线 worker 秒领任务、
	// 删任务时取消执行、展示真实在线状态。任务队列仍以数据库为准。
	Workers *workerhub.Hub
	// AIStreamReasoningDefault 是配置文件里的流式推理展示默认值；运行时 KV 设置优先。
	AIStreamReasoningDefault bool
	// PublicBaseURL 对外基地址（config.public_base_url）：worker 安装指引等
	// 面向用户的文案用它拼真实地址；为空时文案退回占位符，不硬编码任何域名。
	PublicBaseURL string
	// TelegramGroups 可选：Telegram 群控制器。未配置 Telegram 网关时为 nil/未注入，
	// 群控制工具会返回清晰错误；读状态仍可走 Store。
	TelegramGroups TelegramGroupController
	// Events 系统事件总线（可为 nil）：任务编排等场景把事件交 AI 分析决策。
	// 用接口 + 后注入容器解耦（events 包依赖 chat，chat 依赖 tools，不能反向 import）。
	Events Eventer
	// Extra 追加进每个用户工具集的外部工具（如外接 MCP server 的工具），
	// 与内建工具一样经过审计层。
	Extra []ai.Tool
	// SubcallAI is a constrained, tool-free AI subcall used by semantic planners
	// and exposed to script tools. Purpose keeps usage attributable by subsystem;
	// the caller identity is always preserved.
	SubcallAI func(ctx context.Context, u *store.User, req SubcallRequest) (string, error)
}

// SubcallRequest lets internal selectors bound cost and disable expensive model
// reasoning without coupling them to a specific provider API.
type SubcallRequest struct {
	Purpose         string
	Prompt          string
	MaxOutputTokens int
	Reasoning       ai.ReasoningMode
	JSONOutput      bool
}

// Eventer 系统事件出口（由 events.Bus 实现）。
type Eventer interface {
	Emit(kind string, deciderID int64, detail string)
}

type requiredEventer interface {
	EmitRequired(kind string, deciderID int64, detail string)
}

// EventHub 后注入的 Eventer 容器（装配顺序：deps → orch → bus，bus 建好后 Set）。
type EventHub struct {
	mu sync.Mutex
	e  Eventer
}

// Set 注入实现（启动时一次）。
func (h *EventHub) Set(e Eventer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.e = e
}

// Emit 实现 Eventer；未注入时静默丢弃（测试/降级场景）。
func (h *EventHub) Emit(kind string, deciderID int64, detail string) {
	h.mu.Lock()
	e := h.e
	h.mu.Unlock()
	if e != nil {
		e.Emit(kind, deciderID, detail)
	}
}

// EmitRequired preserves the stronger delivery policy when the concrete event
// bus supports it, while keeping lightweight test Eventer implementations valid.
func (h *EventHub) EmitRequired(kind string, deciderID int64, detail string) {
	h.mu.Lock()
	e := h.e
	h.mu.Unlock()
	if e == nil {
		return
	}
	if required, ok := e.(requiredEventer); ok {
		required.EmitRequired(kind, deciderID, detail)
		return
	}
	e.Emit(kind, deciderID, detail)
}

// emitEvent 便捷入口：Deps.Events 可为 nil。
func emitEvent(d Deps, kind string, deciderID int64, detail string) {
	if d.Events != nil {
		d.Events.Emit(kind, deciderID, detail)
	}
}

func emitRequiredEvent(d Deps, kind string, deciderID int64, detail string) {
	if d.Events == nil {
		return
	}
	if required, ok := d.Events.(requiredEventer); ok {
		required.EmitRequired(kind, deciderID, detail)
		return
	}
	d.Events.Emit(kind, deciderID, detail)
}

// saveKnowledge / searchKnowledge：优先走 Knowledge 服务（含语义检索），
// nil 时回退直接走 store 的词法路径（测试或未装配场景）。
func (d Deps) saveKnowledge(ctx context.Context, title, content string, tags []string, authorID int64) (*store.Knowledge, error) {
	if d.Knowledge != nil {
		return d.Knowledge.Save(ctx, title, content, tags, authorID)
	}
	return d.Store.CreateKnowledge(ctx, title, content, tags, authorID)
}

func (d Deps) searchKnowledge(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	if d.Knowledge != nil {
		return d.Knowledge.Search(ctx, query, limit)
	}
	return d.Store.SearchKnowledge(ctx, query, limit)
}

func (d Deps) saveSkill(ctx context.Context, title, content string, tags []string, authorID int64) (*store.Knowledge, error) {
	if d.Knowledge != nil {
		return d.Knowledge.SaveSkill(ctx, title, content, tags, authorID)
	}
	return d.Store.CreateSkill(ctx, title, content, tags, authorID)
}

func (d Deps) searchSkills(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	if d.Knowledge != nil {
		return d.Knowledge.SearchSkills(ctx, query, limit)
	}
	return d.Store.SearchSkills(ctx, query, limit)
}

// wakeWorker 若目标是 worker 且实时在线，推送唤醒（尽力而为，轮询兜底）。
func wakeWorker(d Deps, u *store.User) {
	if d.Workers != nil && u != nil && u.IsWorker {
		d.Workers.Wake(u.ID)
	}
}

// ForUser 组装 user 的工具集并包上审计层。sessionID 可为 nil（HTTP API 场景）。
// 颗粒度分两层：
//  1. 装配期可见性（本函数）——按 toolPerm 注册表过滤：没有对应主动权限的用户
//     根本看不到该工具；worker 机器账号只拿白名单最小集。
//  2. handler 内目标级校验（各工具自带）——有能力 ≠ 对任意目标都行。
func ForUser(d Deps, u *store.User, sessionID *int64) []ai.Tool {
	return ForUserContext(context.Background(), d, u, sessionID)
}

// ForUserContext 与 ForUser 相同，但权限和动态工具装配会响应调用方取消。
func ForUserContext(ctx context.Context, d Deps, u *store.User, sessionID *int64) []ai.Tool {
	return ForUserContextWithTools(ctx, d, u, sessionID, nil)
}

// ForUserContextWithTools adds request-scoped host capabilities before the
// same permission, approval, audit and argument-normalization pipeline used by
// built-ins. It is the integration boundary for embedded products such as a UI
// workspace; host tools never bypass nbco governance and cannot shadow a core
// capability.
func ForUserContextWithTools(ctx context.Context, d Deps, u *store.User, sessionID *int64, hostTools []ai.Tool) []ai.Tool {
	ts := baseStaticTools(d, u)
	var grants []store.Grant
	if !u.IsSuperadmin && d.Store != nil {
		var err error
		if grants, err = d.Store.PermsOf(ctx, u.ID); err != nil {
			slog.Warn("加载权限失败，按无授权裁剪工具集", "user", u.ID, "err", err)
			grants = nil // 失败按最小权限处理（fail-closed）
		}
	}
	ts = append(ts, dynamicScriptTools(ctx, d, u, grants)...)
	ts = append(ts, hostTools...)
	ts = dedupeTools(ts)
	ts = filterByPerm(ts, u, grants)

	for i := range ts {
		// 审计包在审批外层：登记与执行两次调用都留审计记录。
		// 当前权限校验位于审计内、审批外：每次真实调用都重新读取用户和
		// 有效委托链，撤权/停用对已开始的 Agent 轮次立即生效且留下失败审计。
		// 参数归一化放最外层，让审计、审批哈希和实际 handler 看到同一份
		// schema 规范参数；MCP/HTTP 等非聊天入口也获得相同行为。
		ts[i] = withArgumentNormalization(withAudit(d.Store, u.ID, sessionID,
			withCurrentPermission(d.Store, u, withApproval(d.Store, u.ID, ts[i]))))
	}
	return ts
}

// dedupeTools keeps the first definition. Built-ins are assembled before MCP
// extras and dynamic scripts, so external configuration can never shadow a
// trusted core capability or make the model API reject duplicate tool names.
func dedupeTools(ts []ai.Tool) []ai.Tool {
	out := ts[:0]
	seen := make(map[string]bool, len(ts))
	for _, t := range ts {
		if seen[t.Name] {
			slog.Warn("忽略重名工具", "tool", t.Name)
			continue
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	return out
}

// baseStaticTools 是内建工具的唯一装配入口。ForUser 在此基础上追加动态脚本工具、
// 权限裁剪、审批与审计；能力注册中心也从这里读取真实工具集，避免维护第二份清单。
func baseStaticTools(d Deps, u *store.User) []ai.Tool {
	var ts []ai.Tool
	ts = append(ts, profileTools(d, u)...)
	ts = append(ts, permTools(d, u)...)
	ts = append(ts, taskTools(d, u)...)
	ts = append(ts, goalTools(d, u)...)
	ts = append(ts, reviewTools(d, u)...)
	ts = append(ts, scheduleTools(d, u)...)
	ts = append(ts, campaignTools(d, u)...)
	ts = append(ts, roleTools(d, u)...)
	ts = append(ts, knowledgeTools(d, u)...)
	ts = append(ts, memoryTools(d, u)...)
	ts = append(ts, fileTools(d, u)...)
	ts = append(ts, ruleTools(d, u)...)
	ts = append(ts, skillTools(d, u)...)
	ts = append(ts, learningTools(d, u)...)
	ts = append(ts, governanceTools(d, u)...)
	ts = append(ts, scriptToolManagementTools(d, u)...)
	ts = append(ts, workerTools(d, u)...)
	ts = append(ts, materialTools(d, u)...)
	ts = append(ts, workflowTools(d, u)...)
	ts = append(ts, capabilityTools(d, u)...)
	ts = append(ts, dataReadTools(d, u)...)
	ts = append(ts, webTools(d, u)...)
	ts = append(ts, telegramGroupTools(d, u)...)
	ts = append(ts, lowLevelTools(d, u)...)
	ts = append(ts, adminTools(d, u)...)
	ts = withPermissionTargetResolvers(ts, d)
	ts = append(ts, d.Extra...)
	return ts
}

func withPermissionTargetResolvers(ts []ai.Tool, d Deps) []ai.Tool {
	for i := range ts {
		if toolPerm[ts[i].Name] == perm.ActManageTGGroup {
			ts[i].ResolvePermissionTarget = telegramGroupPermissionTarget(d)
		}
	}
	return ts
}

// reqSuper 在 toolPerm 里表示「仅超管」。
const reqSuper = "superadmin"

// toolPerm 可见性注册表：工具名 → 所需主动权限（拥有任意目标即可见）或 reqSuper。
// 未列出的工具人人可见（自我视角/自助工具）。这是「什么权限能调用什么工具」的
// 单一事实来源，README 的权限矩阵与之对应。
var toolPerm = map[string]string{
	// 派活能力（含战略目标/里程碑拆解——能派活的人才能下目标）
	"create_project":      perm.ActCreateProject,
	"assign_task":         perm.ActCreateProject,
	"delegate_review":     perm.ActCreateProject,
	"create_goal":         perm.ActCreateProject,
	"add_milestone":       perm.ActCreateProject,
	"decompose_milestone": perm.ActCreateProject,
	// 人事/沟通能力
	"invite_employee":                 perm.ActGenerateKey,
	"send_message":                    perm.ActSendMsg,
	"create_data_collection_campaign": perm.ActSendMsg,
	"send_data_collection_reminder":   perm.ActSendMsg,
	"update_user_info":                perm.ActEditInfo,
	"bulk_update_user_info":           perm.ActEditInfo,
	"save_infos_on_user":              perm.ActWriteProfile,
	// 权限管理能力
	"grant_passive_perm":  perm.ActManagePerm,
	"revoke_passive_perm": perm.ActManagePerm,
	"view_user_perms":     perm.ActManagePerm,
	// 超管专属（组装函数已裁剪，这里再声明一层防御 + 让矩阵完整）
	"company_overview":     reqSuper,
	"get_ai_settings":      reqSuper,
	"set_ai_settings":      reqSuper,
	"ai_usage_stats":       reqSuper,
	"list_system_activity": reqSuper,
	"low_level_db_query":   reqSuper,
	"low_level_db_exec":    reqSuper,
	"add_info_field":       reqSuper,
	"remove_info_field":    reqSuper,
	"disable_user":         reqSuper,
	"enable_user":          reqSuper,
	"create_role":          reqSuper,
	"update_role":          reqSuper,
	"delete_role":          reqSuper,
	// AI 员工管理：有 manage_worker 权限即可，handler 内限定只能操作自己名下的
	// worker（超管不限）——和真人邀请（generate_key）一样是权限而非身份门槛
	"create_worker":             perm.ActManageWorker,
	"issue_worker_bind_code":    perm.ActManageWorker,
	"run_worker_command":        perm.ActManageWorker,
	"delegate_worker_agent":     perm.ActManageWorker,
	"cancel_worker_run":         perm.ActManageWorker,
	"revoke_worker":             perm.ActManageWorker,
	"set_worker_admin":          reqSuper,
	"analyze_company_materials": perm.ActManageWorker,
	"start_worker_skill":        perm.ActManageWorker,
	"start_workflow":            perm.ActManageWorker,
	// 群接入状态可读；控制类操作由可转授的 Telegram 群管理权限解锁。
	"set_telegram_group_listen":      perm.ActManageTGGroup,
	"set_telegram_group_auto_invite": perm.ActManageTGGroup,
	"set_telegram_group_monitor":     perm.ActManageTGGroup,
	"set_telegram_group_digest":      perm.ActManageTGGroup,
	"list_telegram_group_messages":   perm.ActManageTGGroup,
	"send_telegram_group_message":    perm.ActManageTGGroup,
	"edit_telegram_group_message":    perm.ActManageTGGroup,
	"delete_telegram_group_message":  perm.ActManageTGGroup,
	"pin_telegram_group_message":     perm.ActManageTGGroup,
	"unpin_telegram_group_message":   perm.ActManageTGGroup,
	"update_telegram_group_info":     perm.ActManageTGGroup,
	"bind_telegram_group_project":    perm.ActManageTGGroup,
	// 规则（Policy Memory）影响所有人的每一轮对话，只有超管能改
	"save_rule":                   reqSuper,
	"list_rules":                  reqSuper,
	"set_rule_pinned":             reqSuper,
	"save_skill":                  reqSuper,
	"update_skill":                reqSuper,
	"list_learning_candidates":    reqSuper,
	"approve_learning_candidate":  reqSuper,
	"classify_learning_candidate": reqSuper,
	"reject_learning_candidate":   reqSuper,
	"score_learning_candidates":   reqSuper,
	"list_knowledge_versions":     reqSuper,
	"rollback_knowledge":          reqSuper,
	"list_material_entities":      reqSuper,
	"list_org_groups":             reqSuper,
	"create_org_group":            reqSuper,
	"add_org_group_member":        reqSuper,
	"list_eval_cases":             reqSuper,
	"create_eval_case":            reqSuper,
	"list_script_tools":           reqSuper,
	"create_script_tool":          reqSuper,
	"update_script_tool":          reqSuper,
	"test_script_tool":            reqSuper,
	"enable_script_tool":          reqSuper,
}

// workerAllowed 机器账号（is_worker）的工具白名单：只保留干活与沉淀知识所需。
// Worker Access Token 也能访问 /api/chat 与 /mcp，最小化其能力面。
var workerAllowed = map[string]bool{
	"get_my_tasks":               true,
	"get_my_all_tasks":           true,
	"get_my_projects":            true,
	"get_task_detail":            true,
	"view_my_task_tree":          true,
	"update_my_task_status":      true,
	"add_progress":               true,
	"save_checklist":             true,
	"toggle_checklist":           true,
	"attach_to_task":             true,
	"save_knowledge":             true,
	"search_knowledge":           true,
	"get_knowledge":              true,
	"list_recent_knowledge":      true,
	"search_skills":              true,
	"load_skill":                 true,
	"propose_learning_candidate": true,
	"list_recent_files":          true,
	"search_workspace":           true,
	"query_data":                 true,
}

// groupSensitive 群共享会话里必须剔除的工具：结果含机密（Token）、或影响面
// 大/破坏性的操作。群历史全员可见且会被后续所有成员的轮次重放，这些必须回私聊做。
// 防的是「机密外泄进群」与「他人发言经共享历史注入驱动高危操作」两条路径。
var groupSensitive = map[string]bool{
	"generate_api_token":              true,
	"get_api_token_status":            true,
	"revoke_api_token":                true,
	"invite_employee":                 true,
	"cancel_invites":                  true,
	"grant_active_perm":               true,
	"revoke_active_perm":              true,
	"grant_passive_perm":              true,
	"revoke_passive_perm":             true,
	"disable_user":                    true,
	"enable_user":                     true,
	"update_user_info":                true, // 改「他人」记录：群历史注入可驱动篡改第三方信息
	"bulk_update_user_info":           true,
	"save_infos_on_user":              true,
	"create_worker":                   true,
	"issue_worker_bind_code":          true,
	"run_worker_command":              true,
	"delegate_worker_agent":           true,
	"cancel_worker_run":               true,
	"set_worker_admin":                true,
	"revoke_worker":                   true,
	"analyze_company_materials":       true,
	"start_worker_skill":              true,
	"start_workflow":                  true,
	"save_rule":                       true, // 群历史可被注入，规则变更回私聊做
	"list_rules":                      true,
	"set_rule_pinned":                 true,
	"save_skill":                      true,
	"update_skill":                    true,
	"list_learning_candidates":        true,
	"approve_learning_candidate":      true,
	"classify_learning_candidate":     true,
	"reject_learning_candidate":       true,
	"score_learning_candidates":       true,
	"list_knowledge_versions":         true,
	"rollback_knowledge":              true,
	"list_material_entities":          true,
	"list_org_groups":                 true,
	"create_org_group":                true,
	"add_org_group_member":            true,
	"list_eval_cases":                 true,
	"create_eval_case":                true,
	"list_script_tools":               true,
	"create_script_tool":              true,
	"update_script_tool":              true,
	"test_script_tool":                true,
	"enable_script_tool":              true,
	"search_history":                  true, // 会翻出发言人的私聊历史，群里禁用
	"list_action_turns":               true, // 动作日志含私聊输入和工具结果，群里禁用
	"list_system_activity":            true, // 全局工具流水含私聊参数和业务结果，群里禁用
	"ai_usage_stats":                  true,
	"get_ai_settings":                 true,
	"set_ai_settings":                 true,
	"low_level_db_query":              true,
	"low_level_db_exec":               true,
	"query_data":                      true,
	"remove_info_field":               true,
	"send_message":                    true, // 群里可直接说，无需借 bot 向他人/全体转发
	"send_file":                       true,
	"delete_file":                     true,
	"create_data_collection_campaign": true,
	"list_data_collection_campaigns":  true,
	"get_data_collection_campaign":    true,
	"send_data_collection_reminder":   true,
	"close_data_collection_campaign":  true,
	"refresh_decision_queue":          true,
	"list_decision_queue":             true,
	"schedule_once_push":              true, // 定向推送涉及他人，回私聊设更稳妥
	"schedule_recurring_push":         true,
	"update_schedule":                 true,
	"set_telegram_group_listen":       true,
	"set_telegram_group_auto_invite":  true,
	"set_telegram_group_monitor":      true,
	"set_telegram_group_digest":       true,
	"list_telegram_group_messages":    true,
	"send_telegram_group_message":     true,
	"edit_telegram_group_message":     true,
	"delete_telegram_group_message":   true,
	"pin_telegram_group_message":      true,
	"unpin_telegram_group_message":    true,
	"update_telegram_group_info":      true,
	"bind_telegram_group_project":     true,
}

// StripGroupSensitive 从工具集剔除群不宜的高危工具（群共享会话专用）。
func StripGroupSensitive(ts []ai.Tool) []ai.Tool {
	out := ts[:0]
	for _, t := range ts {
		if t.GroupSensitive || groupSensitive[t.Name] {
			continue
		}
		out = append(out, t)
	}
	return out
}

// StripApprovalRequired 剔除需要跨用户消息确认的高危工具。MCP 这类无 nbco
// 对话轮次的入口没有可验证的「下一条用户确认消息」，暴露这些工具只会让调用方
// 卡在无法核销的审批提示里。
func StripApprovalRequired(ts []ai.Tool) []ai.Tool {
	out := ts[:0]
	for _, t := range ts {
		if requiresApproval(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// filterByPerm 按注册表裁剪工具集（超管全通过；worker 走白名单）。
func filterByPerm(ts []ai.Tool, u *store.User, grants []store.Grant) []ai.Tool {
	out := ts[:0]
	for _, t := range ts {
		if u.IsWorker && !u.IsSuperadmin && !workerAllowed[t.Name] {
			continue
		}
		req, gated := requiredActionForTool(t)
		if gated && !u.IsSuperadmin {
			if req == reqSuper || !hasAnyActive(grants, req) {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func requiredActionForTool(t ai.Tool) (string, bool) {
	if required := strings.TrimSpace(t.RequiredAction); required != "" {
		return required, true
	}
	required, gated := toolPerm[t.Name]
	return required, gated
}

// withCurrentPermission revalidates the actor immediately before every tool
// invocation. Assembly-time filtering keeps the model's catalog small; this
// wrapper is the authoritative time-of-use gate for long-running Agent turns.
func withCurrentPermission(s *store.Store, actor *store.User, t ai.Tool) ai.Tool {
	if s == nil || actor == nil {
		return t
	}
	inner := t.Handler
	required, gated := requiredActionForTool(t)
	t.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
		current, err := s.UserByID(ctx, actor.ID)
		if err != nil {
			return "", fmt.Errorf("%w: 当前执行身份不存在", store.ErrForbidden)
		}
		if current.Status != store.UserActive {
			return "", fmt.Errorf("%w: 当前执行身份已停用", store.ErrForbidden)
		}
		// Handlers close over the actor snapshot used to assemble this Agent
		// turn. If its privilege class changed, do not let stale IsSuperadmin or
		// IsWorker flags influence target-level checks; the next turn will build
		// a fresh capability set from the new identity.
		if current.IsSuperadmin != actor.IsSuperadmin || current.IsWorker != actor.IsWorker {
			return "", fmt.Errorf("%w: 当前执行身份的权限级别已变化，请重新发起操作", store.ErrForbidden)
		}
		if current.IsWorker && !current.IsSuperadmin && !workerAllowed[t.Name] {
			return "", fmt.Errorf("%w: 当前 Worker 身份不能调用该能力", store.ErrForbidden)
		}
		if !gated || current.IsSuperadmin {
			return inner(ctx, args)
		}
		if required == reqSuper {
			return "", fmt.Errorf("%w: 该能力仅限超级管理员", store.ErrForbidden)
		}
		grants, err := s.PermsOf(ctx, current.ID)
		if err != nil {
			return "", err
		}
		if t.ResolvePermissionTarget != nil {
			target, err := t.ResolvePermissionTarget(ctx, args)
			if err != nil {
				return "", fmt.Errorf("%w: 无法确定权限目标: %v", store.ErrForbidden, err)
			}
			if !perm.CheckActiveTarget(grants, required, target) {
				return "", fmt.Errorf("%w: 当前没有 %s 对目标 %s 的有效委托", store.ErrForbidden, required, target)
			}
			return inner(ctx, args)
		}
		if !hasAnyActive(grants, required) {
			return "", fmt.Errorf("%w: 当前已没有 %s 的有效委托", store.ErrForbidden, required)
		}
		return inner(ctx, args)
	}
	return t
}

// withAudit 每次工具调用落一条审计记录（失败不阻断业务）。
// toolOutputLimit 工具返回送入对话历史的上限（rune）。对齐 script_tools 的 12000：
// 一个超大返回（500 任务的 view_project、worker 终端快照）能顶爆模型上下文窗口，
// 用户只得到「出错了请重试」且无截断兜底。这里在所有工具的统一包装层兜底截断。
const toolOutputLimit = 12000
const auditArgsLimit = 64 << 10

func withAudit(s *store.Store, userID int64, sessionID *int64, t ai.Tool) ai.Tool {
	inner := t.Handler
	name := t.Name
	t.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
		out, err := inner(ctx, args)
		result, ok := out, err == nil
		if err != nil {
			result = err.Error()
		}
		if s != nil {
			safeArgs := boundedAuditArgs(args)
			safeResult := textfmt.RedactSecrets(result)
			if aerr := s.Audit(ctx, userID, sessionID, name, safeArgs, truncate(safeResult, 2000), ok); aerr != nil {
				slog.Warn("审计写入失败", "tool", name, "err", aerr)
			}
		}
		// 成功输出超限统一截断（script_tools 已自带截断，重复截断无害）。
		if err == nil {
			out = truncateToolOutput(out)
		}
		return out, err
	}
	return t
}

func boundedAuditArgs(args json.RawMessage) json.RawMessage {
	redacted := textfmt.RedactSecrets(string(args))
	if len(redacted) <= auditArgsLimit && json.Valid([]byte(redacted)) {
		return json.RawMessage(redacted)
	}
	sum := sha256.Sum256([]byte(redacted))
	envelope, _ := json.Marshal(map[string]any{
		"truncated": true,
		"bytes":     len(redacted),
		"sha256":    hex.EncodeToString(sum[:]),
		"preview":   truncate(redacted, 4000),
	})
	return envelope
}

// truncateToolOutput 超过 toolOutputLimit rune 时截断并附分页提示，防顶爆上下文。
// 先用字节长度廉价预检（UTF-8 字节数 >= rune 数，故字节长度 ≤ limit 时 rune 数必然不超），
// 避免每次工具调用都做 O(n) 的 rune 转换——绝大多数工具输出很短，直接返回。
func truncateToolOutput(s string) string {
	if len(s) <= toolOutputLimit {
		return s
	}
	r := []rune(s)
	if len(r) <= toolOutputLimit {
		return s
	}
	return string(r[:toolOutputLimit]) + "\n\n…（输出已截断，结果过大；如需完整内容用带分页/筛选参数的工具分批查看）"
}

// --- schema 构建助手 ---

func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		// 必须是空对象而非 null：MCP 客户端（claude CLI）对 properties 做
		// record 校验，null 会导致整个 tools/list 被拒、所有工具失效。
		props = map[string]any{}
	}
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func p(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func enumP(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

func arr(itemType, desc string) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": map[string]any{"type": itemType}}
}

// decode 解析参数；出错返回可回给模型的提示。
func decode[T any](raw json.RawMessage, v *T) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("参数格式错误: %w", err)
	}
	return nil
}

// tool 便捷构造。
func tool(name, desc string, schema map[string]any, h func(ctx context.Context, args json.RawMessage) (string, error)) ai.Tool {
	return ai.Tool{Name: name, Description: desc, Domain: CapabilityDomain(name), Effect: ToolEffect(name), InputSchema: schema, Handler: h}
}

func immediateTool(t ai.Tool) ai.Tool {
	t.LoadMode = ai.ToolLoadImmediate
	return t
}

func asynchronousTool(t ai.Tool) ai.Tool {
	t.Completion = ai.ToolCompletionAsynchronous
	if t.Handler == nil {
		return t
	}
	type call struct {
		done   chan struct{}
		result string
		err    error
	}
	inner := t.Handler
	var mu sync.Mutex
	calls := make(map[string]*call)
	t.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
		key := canonicalArgsHash(args)
		mu.Lock()
		if existing := calls[key]; existing != nil {
			mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-existing.done:
				return existing.result, existing.err
			}
		}
		current := &call{done: make(chan struct{})}
		calls[key] = current
		mu.Unlock()

		current.result, current.err = inner(ctx, args)
		mu.Lock()
		if current.err != nil {
			// A failed attempt did not establish a durable accepted result. Let a
			// later model iteration repair its arguments or retry a transient error.
			delete(calls, key)
		}
		close(current.done)
		mu.Unlock()
		return current.result, current.err
	}
	return t
}

func experimentalTool(t ai.Tool) ai.Tool {
	t.Maturity = ai.ToolMaturityExperimental
	return t
}

// --- 通用小助手 ---

// parseTarget 解析 "用户ID或_all"。
func parseTarget(v string) (key string, id int64, isAll bool, err error) {
	v = strings.TrimSpace(v)
	if v == store.TargetAll {
		return store.TargetAll, 0, true, nil
	}
	id, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return "", 0, false, fmt.Errorf("目标必须是用户ID或 %q", store.TargetAll)
	}
	return v, id, false, nil
}

// mustUser 取用户并要求 active。
func mustUser(ctx context.Context, s *store.Store, id int64) (*store.User, error) {
	u, err := s.UserByID(ctx, id)
	if err != nil {
		return nil, errors.New("目标用户不存在")
	}
	if u.Status != store.UserActive {
		return nil, errors.New("目标用户已停用")
	}
	return u, nil
}

// resolveUserArg resolves an internal user_id, a visible unique name, or a
// Telegram identity selector such as "tg:123456" / "telegram:123456".
// User-facing directory tools expose stable employee IDs; names are mutable
// display labels. Telegram ID support is exact and internal: use it to resolve
// bindings, but prefer the nbco user_id once the employee is known.
func resolveUserArg(ctx context.Context, s *store.Store, id int64, names ...string) (*store.User, string, error) {
	if s == nil {
		return nil, "用户存储不可用。", nil
	}
	if id > 0 {
		u, err := mustUser(ctx, s, id)
		if err != nil {
			return nil, err.Error(), nil
		}
		return u, "", nil
	}
	selector := ""
	for _, name := range names {
		if selector = strings.TrimSpace(name); selector != "" {
			break
		}
	}
	if selector == "" {
		return nil, "请提供 user_id、user/user_name，或 tg:<Telegram ID>。", nil
	}
	if tgID, ok := parseTelegramUserSelector(selector); ok {
		return userByTelegramID(ctx, s, tgID)
	}
	if parsed, err := strconv.ParseInt(selector, 10, 64); err == nil && parsed > 0 {
		u, err := s.UserByID(ctx, parsed)
		if err == nil {
			if u.Status != store.UserActive {
				return nil, "目标用户已停用。", nil
			}
			return u, "", nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
		if u, msg, err := userByTelegramID(ctx, s, selector); err != nil || u != nil || msg != "" {
			return u, msg, err
		}
		return nil, fmt.Sprintf("没有找到内部编号或 Telegram ID 为「%s」的用户。", selector), nil
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return nil, "", err
	}
	if u, ambiguous := matchCompanyUser(users, selector, true); ambiguous {
		return nil, "匹配到多个同名用户，请用更完整的姓名。", nil
	} else if u != nil {
		return u, "", nil
	}
	if u, ambiguous := matchCompanyUser(users, selector, false); ambiguous {
		return nil, "匹配到多个相似用户，请用完整姓名。", nil
	} else if u != nil {
		return u, "", nil
	}
	return nil, fmt.Sprintf("没有找到名为「%s」的用户。", selector), nil
}

func parseTelegramUserSelector(selector string) (string, bool) {
	s := strings.TrimSpace(selector)
	lower := strings.ToLower(s)
	for _, prefix := range []string{"tg:", "tgid:", "tg_id:", "telegram:", "telegram_id:"} {
		if strings.HasPrefix(lower, prefix) {
			id := strings.TrimSpace(s[len(prefix):])
			return id, id != ""
		}
	}
	return "", false
}

func userByTelegramID(ctx context.Context, s *store.Store, tgID string) (*store.User, string, error) {
	tgID = strings.TrimSpace(tgID)
	if tgID == "" {
		return nil, "Telegram ID 不能为空。", nil
	}
	if _, err := strconv.ParseInt(tgID, 10, 64); err != nil {
		return nil, fmt.Sprintf("Telegram ID 必须是数字，收到「%s」。", tgID), nil
	}
	u, err := s.UserByIdentity(ctx, "telegram", tgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Sprintf("没有找到 Telegram ID 为「%s」的已绑定用户。", tgID), nil
		}
		return nil, "", err
	}
	if u.Status != store.UserActive {
		return nil, "目标用户已停用。", nil
	}
	return u, "", nil
}

func telegramUserSelector(tgID string) string {
	if strings.TrimSpace(tgID) == "" {
		return ""
	}
	return "tg:" + strings.TrimSpace(tgID)
}

func userName(ctx context.Context, s *store.Store, id int64) string {
	if s == nil {
		return "未知成员"
	}
	u, err := s.UserByID(ctx, id)
	if err != nil || strings.TrimSpace(u.Name) == "" {
		return "未知成员"
	}
	return u.Name
}

func internalRef(kind string, id int64) string {
	return fmt.Sprintf("%s内部编号 %d", kind, id)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return "…"
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n] + "…"
}

// fmtTime 用配置时区展示时间。
func fmtTime(t time.Time, tz *time.Location) string {
	return t.In(tz).Format("2006-01-02 15:04")
}

// Package tools 把领域能力组装成 ai.Tool 集合：
// 权限校验在 handler 内完成（工具即权限边界），每次调用写审计日志。
// 同一套工具供所有入口复用：TG 对话（经引擎）、HTTP API、外部 MCP。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/knowledge"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
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
}

// Eventer 系统事件出口（由 events.Bus 实现）。
type Eventer interface {
	Emit(kind string, deciderID int64, detail string)
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

// emitEvent 便捷入口：Deps.Events 可为 nil。
func emitEvent(d Deps, kind string, deciderID int64, detail string) {
	if d.Events != nil {
		d.Events.Emit(kind, deciderID, detail)
	}
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
	var ts []ai.Tool
	ts = append(ts, profileTools(d, u)...)
	ts = append(ts, permTools(d, u)...)
	ts = append(ts, taskTools(d, u)...)
	ts = append(ts, reviewTools(d, u)...)
	ts = append(ts, scheduleTools(d, u)...)
	ts = append(ts, roleTools(d, u)...)
	ts = append(ts, knowledgeTools(d, u)...)
	ts = append(ts, memoryTools(d, u)...)
	ts = append(ts, ruleTools(d, u)...)
	ts = append(ts, workerTools(d, u)...)
	ts = append(ts, telegramGroupTools(d, u)...)
	ts = append(ts, adminTools(d, u)...)
	ts = append(ts, d.Extra...)

	var grants []store.Grant
	if !u.IsSuperadmin && d.Store != nil {
		var err error
		if grants, err = d.Store.PermsOf(context.Background(), u.ID); err != nil {
			slog.Warn("加载权限失败，按无授权裁剪工具集", "user", u.ID, "err", err)
			grants = nil // 失败按最小权限处理（fail-closed）
		}
	}
	ts = filterByPerm(ts, u, grants)

	for i := range ts {
		// 审计包在审批外层：登记与执行两次调用都留审计记录。
		ts[i] = withAudit(d.Store, u.ID, sessionID, withApproval(d.Store, u.ID, ts[i]))
	}
	return ts
}

// reqSuper 在 toolPerm 里表示「仅超管」。
const reqSuper = "superadmin"

// toolPerm 可见性注册表：工具名 → 所需主动权限（拥有任意目标即可见）或 reqSuper。
// 未列出的工具人人可见（自我视角/自助工具）。这是「什么权限能调用什么工具」的
// 单一事实来源，README 的权限矩阵与之对应。
var toolPerm = map[string]string{
	// 派活能力
	"create_project":  perm.ActCreateProject,
	"assign_task":     perm.ActCreateProject,
	"delegate_review": perm.ActCreateProject,
	// 人事/沟通能力
	"invite_employee":    perm.ActGenerateKey,
	"send_message":       perm.ActSendMsg,
	"update_user_info":   perm.ActEditInfo,
	"save_infos_on_user": perm.ActWriteProfile,
	// 权限管理能力
	"grant_passive_perm":  perm.ActManagePerm,
	"revoke_passive_perm": perm.ActManagePerm,
	"view_user_perms":     perm.ActManagePerm,
	// 超管专属（组装函数已裁剪，这里再声明一层防御 + 让矩阵完整）
	"company_overview":  reqSuper,
	"get_ai_settings":   reqSuper,
	"set_ai_settings":   reqSuper,
	"ai_usage_stats":    reqSuper,
	"add_info_field":    reqSuper,
	"remove_info_field": reqSuper,
	"disable_user":      reqSuper,
	"enable_user":       reqSuper,
	"create_role":       reqSuper,
	"update_role":       reqSuper,
	"delete_role":       reqSuper,
	// AI 员工管理：有 manage_worker 权限即可，handler 内限定只能操作自己名下的
	// worker（超管不限）——和真人邀请（generate_key）一样是权限而非身份门槛
	"create_worker":          perm.ActManageWorker,
	"issue_worker_bind_code": perm.ActManageWorker,
	"run_worker_command":     perm.ActManageWorker,
	"revoke_worker":          perm.ActManageWorker,
	// 群接入状态可读；控制类操作限超管。群内仍可用 /listen。
	"set_telegram_group_listen":      reqSuper,
	"set_telegram_group_auto_invite": reqSuper,
	"send_telegram_group_message":    reqSuper,
	"edit_telegram_group_message":    reqSuper,
	"delete_telegram_group_message":  reqSuper,
	"pin_telegram_group_message":     reqSuper,
	"unpin_telegram_group_message":   reqSuper,
	"update_telegram_group_info":     reqSuper,
	// 规则（Policy Memory）影响所有人的每一轮对话，只有超管能改
	"save_rule":       reqSuper,
	"list_rules":      reqSuper,
	"set_rule_pinned": reqSuper,
}

// workerAllowed 机器账号（is_worker）的工具白名单：只保留干活与沉淀知识所需。
// Worker Access Token 也能访问 /api/chat 与 /mcp，最小化其能力面。
var workerAllowed = map[string]bool{
	"get_my_tasks":          true,
	"get_my_all_tasks":      true,
	"get_my_projects":       true,
	"get_task_detail":       true,
	"view_my_task_tree":     true,
	"update_my_task_status": true,
	"add_progress":          true,
	"save_checklist":        true,
	"toggle_checklist":      true,
	"attach_to_task":        true,
	"save_knowledge":        true,
	"search_knowledge":      true,
	"get_knowledge":         true,
	"list_recent_knowledge": true,
}

// groupSensitive 群共享会话里必须剔除的工具：结果含机密（Token）、或影响面
// 大/破坏性的操作。群历史全员可见且会被后续所有成员的轮次重放，这些必须回私聊做。
// 防的是「机密外泄进群」与「他人发言经共享历史注入驱动高危操作」两条路径。
var groupSensitive = map[string]bool{
	"generate_api_token":             true,
	"revoke_api_token":               true,
	"invite_employee":                true,
	"cancel_invites":                 true,
	"grant_active_perm":              true,
	"revoke_active_perm":             true,
	"grant_passive_perm":             true,
	"revoke_passive_perm":            true,
	"disable_user":                   true,
	"enable_user":                    true,
	"update_user_info":               true, // 改「他人」记录：群历史注入可驱动篡改第三方信息
	"save_infos_on_user":             true,
	"create_worker":                  true,
	"issue_worker_bind_code":         true,
	"run_worker_command":             true,
	"revoke_worker":                  true,
	"save_rule":                      true, // 群历史可被注入，规则变更回私聊做
	"list_rules":                     true,
	"set_rule_pinned":                true,
	"search_history":                 true, // 会翻出发言人的私聊历史，群里禁用
	"ai_usage_stats":                 true,
	"get_ai_settings":                true,
	"set_ai_settings":                true,
	"remove_info_field":              true,
	"send_message":                   true, // 群里可直接说，无需借 bot 向他人/全体转发
	"schedule_push":                  true, // 定向推送涉及他人，回私聊设更稳妥
	"set_telegram_group_listen":      true,
	"set_telegram_group_auto_invite": true,
	"send_telegram_group_message":    true,
	"edit_telegram_group_message":    true,
	"delete_telegram_group_message":  true,
	"pin_telegram_group_message":     true,
	"unpin_telegram_group_message":   true,
	"update_telegram_group_info":     true,
}

// StripGroupSensitive 从工具集剔除群不宜的高危工具（群共享会话专用）。
func StripGroupSensitive(ts []ai.Tool) []ai.Tool {
	out := ts[:0]
	for _, t := range ts {
		if groupSensitive[t.Name] {
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
		if u.IsWorker && !workerAllowed[t.Name] {
			continue
		}
		req, gated := toolPerm[t.Name]
		if gated && !u.IsSuperadmin {
			if req == reqSuper || !hasAnyActive(grants, req) {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// withAudit 每次工具调用落一条审计记录（失败不阻断业务）。
func withAudit(s *store.Store, userID int64, sessionID *int64, t ai.Tool) ai.Tool {
	inner := t.Handler
	name := t.Name
	t.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
		out, err := inner(ctx, args)
		result, ok := out, err == nil
		if err != nil {
			result = err.Error()
		}
		if aerr := s.Audit(ctx, userID, sessionID, name, args, truncate(result, 2000), ok); aerr != nil {
			slog.Warn("审计写入失败", "tool", name, "err", aerr)
		}
		return out, err
	}
	return t
}

// --- schema 构建助手 ---

func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		// 必须是空对象而非 null：MCP 客户端（claude CLI）对 properties 做
		// record 校验，null 会导致整个 tools/list 被拒、所有工具失效。
		props = map[string]any{}
	}
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func p(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
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
	return ai.Tool{Name: name, Description: desc, InputSchema: schema, Handler: h}
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
		return nil, fmt.Errorf("用户 %d 不存在", id)
	}
	if u.Status != store.UserActive {
		return nil, fmt.Errorf("用户 %d 已停用", id)
	}
	return u, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fmtTime 用配置时区展示时间。
func fmtTime(t time.Time, tz *time.Location) string {
	return t.In(tz).Format("2006-01-02 15:04")
}

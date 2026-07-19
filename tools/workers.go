package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

// workerTools AI 员工管理：列出对所有人开放（只看自己名下）；创建/绑定码/命令/
// 停用按 manage_worker 主动权限裁剪（toolPerm 注册表），且 handler 内做目标级
// 校验——非超管只能操作自己名下（owner）的 worker。
func workerTools(d Deps, u *store.User) []ai.Tool {
	list := tool("list_workers", "查看 AI 员工（client 用交互式 PTY 驱动 CLI 干活的虚拟成员）及其在线状态。", obj(nil),
		func(ctx context.Context, _ json.RawMessage) (string, error) {
			owner := u.ID
			if u.IsSuperadmin {
				owner = 0 // 超管看全部
			}
			ws, err := d.Store.ListWorkers(ctx, owner)
			if err != nil {
				return "", err
			}
			if len(ws) == 0 {
				return "（还没有 AI 员工。有 AI 员工管理权限（manage_worker）即可用 create_worker 创建。）", nil
			}
			ids := make([]int64, 0, len(ws))
			for _, w := range ws {
				ids = append(ids, w.ID)
			}
			caps, err := d.Store.WorkerCapabilities(ctx, ids)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			for _, w := range ws {
				status := "离线"
				if w.WorkerLastSeen != nil {
					status = "最近在线 " + fmtTime(*w.WorkerLastSeen, d.TZ)
				}
				if d.Workers != nil && d.Workers.Online(w.ID) {
					status = "🔗 在线（实时连接）"
				}
				if w.Status != store.UserActive {
					status = "已停用"
				}
				admin := ""
				if w.IsSuperadmin {
					admin = "，admin worker"
				}
				capText := ""
				if c := caps[w.ID]; c != nil {
					capText = fmt.Sprintf("；%s/%s %s，engine=%s，能力=%s，更新 %s",
						c.OS, c.Arch, c.CLIName, c.Engine, strings.Join(c.Capabilities, "/"), fmtTime(c.UpdatedAt, d.TZ))
				}
				fmt.Fprintf(&b, "- %s：%s（%s%s%s）\n", internalRef("worker", w.ID), w.Name, status, admin, capText)
			}
			return b.String(), nil
		})

	return []ai.Tool{
		list,
		tool("create_worker", "创建一个 AI worker 账号并签发一次性绑定码（24小时有效、一次一用；不是真人员工邀请）。在工作机上运行 nbco-worker bootstrap 时用绑定码兑换 access token——真正的长期凭据不会出现在对话里。需要 AI 员工管理权限（manage_worker）；创建者自动成为该 worker 的监护人。",
			obj(map[string]any{"name": p("string", "AI worker 名，如「小码」")}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Name) == "" {
					return "AI worker 名不能为空。", nil
				}
				w, code, err := d.Store.CreateWorker(ctx, args.Name, u.ID)
				if err != nil {
					return "", err
				}
				base := strings.TrimSpace(d.PublicBaseURL)
				if base == "" {
					base = "<服务器地址>"
				}
				return fmt.Sprintf("已创建 AI worker「%s」（%s）。一次性绑定码（24小时内有效，兑换即失效）：\n<code>%s</code>\n"+
					"工作机绑定时会自动用它兑换 Worker Access Token；token 不会在对话里出现。\n\n"+
					"macOS Apple Silicon 一键安装示例：\n"+
					"<pre>curl -fsSL -o nbco-worker %s/downloads/worker/nbco-worker-darwin-arm64\n"+
					"chmod +x nbco-worker\n"+
					"./nbco-worker bootstrap -install-service=true %s %s</pre>\n"+
					"Linux/Windows 也可从 /downloads/worker/ 下载对应平台二进制；bootstrap 会绑定并安装为系统服务。\n"+
					"绑定码过期或遗失时用 issue_worker_bind_code 补发。",
					w.Name, internalRef("worker", w.ID), code, base, base, code), nil
			}),

		tool("issue_worker_bind_code", "给已有 AI worker 补发一次性绑定码（旧码作废；已绑定机器的 token 在新码被兑换前仍有效）。用于绑定码过期、遗失或换机重绑。非超管只能给自己名下的 worker 补发。",
			obj(map[string]any{"worker_id": p("integer", "worker 用户ID")}, "worker_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					WorkerID int64 `json:"worker_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if _, msg := mustOwnWorker(ctx, d, u, args.WorkerID); msg != "" {
					return msg, nil
				}
				code, err := d.Store.NewWorkerBindCode(ctx, args.WorkerID, u.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该 AI worker 不存在或已停用。", nil
					}
					return "", err
				}
				return fmt.Sprintf("已补发绑定码（24小时内有效，兑换即失效）：\n<code>%s</code>\n"+
					"在工作机上运行 nbco-worker bind &lt;server&gt; %s 即可重绑；新码兑换后旧 token 自动作废。", code, code), nil
			}),

		tool("run_worker_command", "让指定 AI worker 在其工作机任务目录中执行一条确定性的 shell/cmd 命令，并把输出作为任务进度和完成汇报回传。适合探测状态、运行测试或已知的单步操作；需要读取输出后继续判断、初始化仓库、修改代码或部署时，应创建普通 worker Agent 任务或启动对应工作流，让 Agent 自行观察并适配。默认用 stdout/stderr pipe；只有确实需要终端行为时才设置 pty=true。非超管仅限自己名下的 worker；这是显式命令任务，不是常驻远控。",
			obj(map[string]any{
				"worker_id": p("integer", "目标 worker 用户ID"),
				"command":   p("string", "要执行的命令，如 go test ./..."),
				"pty":       p("boolean", "可选，是否用 PTY 执行；默认 false。普通命令不需要，交互/终端检测命令才需要"),
				"title":     p("string", "任务标题，可选"),
			}, "worker_id", "command"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					WorkerID int64  `json:"worker_id"`
					Command  string `json:"command"`
					PTY      bool   `json:"pty"`
					Title    string `json:"title"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				cmd := strings.TrimSpace(args.Command)
				if cmd == "" {
					return "command 不能为空。", nil
				}
				w, msg := mustOwnWorker(ctx, d, u, args.WorkerID)
				if msg != "" {
					return msg, nil
				}
				if w.Status != store.UserActive {
					return "目标 worker 已停用。", nil
				}
				title := strings.TrimSpace(args.Title)
				if title == "" {
					title = "执行命令"
				}
				t, err := createWorkerCommandTask(ctx, d, u, w, workerCommandTaskArgs{
					Title: title, Command: cmd, PTY: args.PTY,
					Description: "命令任务会在 worker 的主题工作目录中执行；如需回传文件，请按 worker 任务提示写入本轮产物目录。",
					Acceptance:  "完成汇报包含退出码和输出摘要；如生成产物文件，应自动上传。",
				})
				if err != nil {
					return "", err
				}
				mode := "pipe"
				if args.PTY {
					mode = "pty"
				}
				return fmt.Sprintf("已创建 worker 命令任务（%s），分配给 %s。命令会在该 worker 的任务工作目录中以 %s 模式执行。", internalRef("任务", t.ID), w.Name, mode), nil
			}),

		tool("delegate_worker_agent", "把需要观察结果、连续判断或多步处理的目标交给指定 AI worker 的 Codex/Claude 交互式 Agent。任务不包含预制 shell 命令，worker 必定通过 PTY 启动或恢复原生 Agent 会话；同一 worker 使用相同 scope_key 时会恢复该主题的工作目录、会话和摘要。适用于代码、研究、资料处理、排障等自适应工作。已知且无需判断的单条命令才使用 run_worker_command。非超管仅限自己名下的 worker。",
			obj(map[string]any{
				"worker_id":   p("integer", "目标 worker 用户ID"),
				"title":       p("string", "简短任务标题"),
				"instruction": p("string", "完整目标、背景、约束和期望动作；Agent 会自行观察并推进"),
				"acceptance":  p("string", "可验证的完成标准"),
				"scope_key":   p("string", "稳定主题标识，格式 type:identity；同一工作线始终复用同一个值，例如 repo:nbco"),
				"scope_title": p("string", "主题名称，可选；用于会话列表展示"),
				"priority":    enumP("任务优先级，可选", "low", "normal", "high"),
				"file_ids":    arr("integer", "可选，随任务下发给 worker 的系统文件ID"),
			}, "worker_id", "title", "instruction", "acceptance", "scope_key"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					WorkerID    int64   `json:"worker_id"`
					Title       string  `json:"title"`
					Instruction string  `json:"instruction"`
					Acceptance  string  `json:"acceptance"`
					ScopeKey    string  `json:"scope_key"`
					ScopeTitle  string  `json:"scope_title"`
					Priority    string  `json:"priority"`
					FileIDs     []int64 `json:"file_ids"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				args.Title = strings.TrimSpace(args.Title)
				args.Instruction = strings.TrimSpace(args.Instruction)
				args.Acceptance = strings.TrimSpace(args.Acceptance)
				if args.Title == "" || args.Instruction == "" || args.Acceptance == "" {
					return "title、instruction 和 acceptance 都不能为空。", nil
				}
				scopeType, scopeKey, msg := normalizeAgentScope(args.ScopeKey)
				if msg != "" {
					return msg, nil
				}
				w, msg := mustOwnWorker(ctx, d, u, args.WorkerID)
				if msg != "" {
					return msg, nil
				}
				if w.Status != store.UserActive {
					return "目标 worker 已停用。", nil
				}
				fileIDs, msg, err := accessibleWorkerFiles(ctx, d, u, args.FileIDs)
				if err != nil {
					return "", err
				}
				if msg != "" {
					return msg, nil
				}
				priority := strings.TrimSpace(args.Priority)
				if priority == "" {
					priority = "normal"
				}
				switch priority {
				case "low", "normal", "high":
				default:
					return "priority 必须是 low、normal 或 high。", nil
				}
				pj, err := d.Store.EnsureWorkerOperationsProject(ctx, u.ID)
				if err != nil {
					return "", err
				}
				t, err := d.Store.CreateTaskWithFileAttachments(ctx, &store.Task{
					ProjectID: pj.ID, AssignerID: u.ID, AssigneeID: w.ID,
					Title: args.Title, Goal: args.Title, Description: args.Instruction,
					Acceptance: args.Acceptance, Priority: priority,
					WorkerScopeType: scopeType, WorkerScopeKey: scopeKey,
					WorkerScopeTitle: firstNonEmpty(strings.TrimSpace(args.ScopeTitle), args.Title),
				}, fileIDs, "Agent 任务输入")
				if err != nil {
					return "", err
				}
				wakeWorker(d, w)
				return fmt.Sprintf("已创建 Worker Agent 任务（%s），分配给 %s；主题 scope=%s。worker 会通过交互式 PTY 启动或恢复该主题的原生 Agent 会话。", internalRef("任务", t.ID), w.Name, scopeKey), nil
			}),

		tool("revoke_worker", "停用一个 AI worker 并吊销其 Worker Access Token（历史任务保留）。非超管只能停用自己名下的 worker。",
			obj(map[string]any{"worker_id": p("integer", "worker 用户ID")}, "worker_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					WorkerID int64 `json:"worker_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if _, msg := mustOwnWorker(ctx, d, u, args.WorkerID); msg != "" {
					return msg, nil
				}
				reset, err := d.Store.RevokeWorker(ctx, args.WorkerID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该 AI 员工不存在。", nil
					}
					return "", err
				}
				if reset > 0 {
					return fmt.Sprintf("已停用。其名下 %d 个未完成任务已重置为待改派，建议尽快用 reassign_task 改派。", reset), nil
				}
				return "已停用。", nil
			}),

		tool("set_worker_admin", "把指定 AI worker 设置/取消为 admin worker。admin worker 视同系统级执行者，可获得完整工具能力；仅用于 nbco 维护、自升级、公司资料入库等可信工作机。",
			obj(map[string]any{
				"worker_id": p("integer", "worker 用户ID"),
				"admin":     p("boolean", "true=设为 admin worker；false=取消"),
			}, "worker_id", "admin"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if !u.IsSuperadmin {
					return "只有超级管理员可以设置 admin worker。", nil
				}
				var args struct {
					WorkerID int64 `json:"worker_id"`
					Admin    bool  `json:"admin"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				w, msg := mustOwnWorker(ctx, d, u, args.WorkerID)
				if msg != "" {
					return msg, nil
				}
				if w.Status != store.UserActive {
					return "目标 worker 已停用。", nil
				}
				if err := d.Store.SetWorkerAdmin(ctx, args.WorkerID, args.Admin); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该 AI worker 不存在。", nil
					}
					return "", err
				}
				if args.Admin {
					return fmt.Sprintf("已将 %s（%s）设置为 admin worker。它之后可执行系统级维护/资料入库任务。", w.Name, internalRef("worker", w.ID)), nil
				}
				return fmt.Sprintf("已取消 %s（%s）的 admin worker 权限。", w.Name, internalRef("worker", w.ID)), nil
			}),
	}
}

type workerCommandTaskArgs struct {
	Title       string
	Command     string
	PTY         bool
	Description string
	Acceptance  string
	Priority    string
	ScopeType   string
	ScopeKey    string
	ScopeTitle  string
}

func createWorkerCommandTask(ctx context.Context, d Deps, u, w *store.User, args workerCommandTaskArgs) (*store.Task, error) {
	pj, err := d.Store.EnsureWorkerOperationsProject(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = "执行命令"
	}
	description := strings.TrimSpace(args.Description)
	if description == "" {
		description = "命令任务会在 worker 的主题工作目录中执行；如需回传文件，请按 worker 任务提示写入本轮产物目录。"
	}
	acceptance := strings.TrimSpace(args.Acceptance)
	if acceptance == "" {
		acceptance = "完成汇报包含退出码和输出摘要；如生成产物文件，应自动上传。"
	}
	priority := strings.TrimSpace(args.Priority)
	if priority == "" {
		priority = "high"
	}
	scopeType := strings.TrimSpace(args.ScopeType)
	scopeKey := strings.TrimSpace(args.ScopeKey)
	scopeTitle := strings.TrimSpace(args.ScopeTitle)
	if scopeKey == "" {
		scopeType = "ops"
		scopeKey = fmt.Sprintf("ops:worker:%d", w.ID)
		scopeTitle = "Worker operations"
	}
	t, err := d.Store.CreateTask(ctx, &store.Task{
		ProjectID: pj.ID, AssignerID: u.ID, AssigneeID: w.ID,
		Title: title, Goal: "在 worker 工作机上执行显式命令并回传结果。",
		Description: description, Acceptance: acceptance, Priority: priority,
		WorkerCommand: strings.TrimSpace(args.Command), WorkerCommandPTY: args.PTY,
		WorkerScopeType: scopeType, WorkerScopeKey: scopeKey, WorkerScopeTitle: scopeTitle,
	})
	if err != nil {
		return nil, err
	}
	wakeWorker(d, w)
	return t, nil
}

func normalizeAgentScope(value string) (scopeType, scopeKey, message string) {
	scopeKey = strings.TrimSpace(value)
	if scopeKey == "" {
		return "", "", "scope_key 不能为空；它用于恢复同一主题的 Agent 会话。"
	}
	if len([]rune(scopeKey)) > 160 || strings.ContainsAny(scopeKey, "\r\n\t") {
		return "", "", "scope_key 不能超过 160 个字符，也不能包含控制字符。"
	}
	scopeType, identity, ok := strings.Cut(scopeKey, ":")
	scopeType = strings.TrimSpace(scopeType)
	identity = strings.TrimSpace(identity)
	if !ok || scopeType == "" || identity == "" || strings.ContainsAny(scopeType, " /\\") {
		return "", "", "scope_key 必须使用 type:identity 格式，例如 repo:nbco 或 project:42。"
	}
	scopeType = strings.ToLower(scopeType)
	return scopeType, scopeType + ":" + identity, ""
}

func accessibleWorkerFiles(ctx context.Context, d Deps, u *store.User, values []int64) ([]int64, string, error) {
	seen := make(map[int64]bool, len(values))
	out := make([]int64, 0, len(values))
	for _, id := range values {
		if id <= 0 {
			return nil, "file_ids 只能包含正整数。", nil
		}
		if seen[id] {
			continue
		}
		allowed, err := d.Store.UserCanAccessFile(ctx, u.ID, u.IsSuperadmin, id)
		if err != nil {
			return nil, "", err
		}
		if !allowed {
			return nil, fmt.Sprintf("无权访问文件 %d。", id), nil
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, "", nil
}

// mustOwnWorker 目标级校验：目标必须是 worker，且非超管只能操作自己名下
// （owner_id）的 worker。通过返回 (worker, "")，否则返回给模型的拒绝话术。
func mustOwnWorker(ctx context.Context, d Deps, u *store.User, workerID int64) (*store.User, string) {
	w, err := d.Store.UserByID(ctx, workerID)
	if err != nil || !w.IsWorker {
		return nil, "该 AI 员工不存在。"
	}
	if !u.IsSuperadmin && (w.OwnerID == nil || *w.OwnerID != u.ID) {
		return nil, "只能操作自己名下的 AI 员工。"
	}
	return w, ""
}

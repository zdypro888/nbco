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

// workerTools AI 员工管理：创建/列出/停用。创建与停用限超管（工具即权限边界，只组装给超管）；
// 列出对所有人开放（只看自己名下）。
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
				return "（还没有 AI 员工。超管可用 create_worker 创建。）", nil
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
				fmt.Fprintf(&b, "- #%d %s（%s）\n", w.ID, w.Name, status)
			}
			return b.String(), nil
		})

	if !u.IsSuperadmin {
		return []ai.Tool{list}
	}

	return []ai.Tool{
		list,
		tool("create_worker", "创建一个 AI worker 账号并签发 Worker Access Token（明文仅返回一次，长期用于 nbco-worker 客户端认证；不是真人员工邀请）。在工作机上运行 nbco-worker bind 绑定该 access token，它便能领取任务、用交互式 PTY 驱动 claude/codex 自动完成。超管专用。",
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
				w, token, err := d.Store.CreateWorker(ctx, args.Name, u.ID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已创建 AI worker「%s」。Worker Access Token（仅显示一次，请妥善保存）：\n<code>%s</code>\n"+
					"用途：只给 nbco-worker 客户端认证使用；不是给真人员工入职的邀请。\n"+
					"在工作机上运行：nbco-worker bind <nbco-server-url> <上面的 Worker Access Token>，再 nbco-worker run 即可上线接活。\n"+
					"例如：nbco-worker bind https://im.app:8443 <上面的 Worker Access Token>",
					w.Name, token), nil
			}),

		tool("run_worker_command", "让指定 AI worker 在其工作机任务目录中执行一条 shell/cmd 命令，并把输出作为任务进度和完成汇报回传。默认用 stdout/stderr pipe；只有确实需要终端行为时才设置 pty=true。超管专用；这是显式命令任务，不是常驻远控。",
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
				w, err := d.Store.UserByID(ctx, args.WorkerID)
				if err != nil || !w.IsWorker || w.Status != store.UserActive {
					return "目标 worker 不存在或不可用。", nil
				}
				pj, err := d.Store.EnsureWorkerCommandProject(ctx, u.ID)
				if err != nil {
					return "", err
				}
				title := strings.TrimSpace(args.Title)
				if title == "" {
					title = "执行命令"
				}
				t, err := d.Store.CreateTask(ctx, &store.Task{
					ProjectID: pj.ID, AssignerID: u.ID, AssigneeID: w.ID,
					Title: title, Goal: "在 worker 工作机上执行显式命令并回传结果。",
					Description:   "命令任务会在 worker 的本次任务工作目录中执行；如需回传文件，请让命令写入 artifacts/。",
					Acceptance:    "完成汇报包含退出码和输出摘要；如生成 artifacts/ 文件，应自动上传。",
					WorkerCommand: cmd, WorkerCommandPTY: args.PTY, Priority: "high",
				})
				if err != nil {
					return "", err
				}
				wakeWorker(d, w)
				mode := "pipe"
				if args.PTY {
					mode = "pty"
				}
				return fmt.Sprintf("已创建 worker 命令任务 #%d，分配给 %s。命令会在该 worker 的任务工作目录中以 %s 模式执行。", t.ID, w.Name, mode), nil
			}),

		tool("revoke_worker", "停用一个 AI worker 并吊销其 Worker Access Token（历史任务保留）。超管专用。",
			obj(map[string]any{"worker_id": p("integer", "worker 用户ID")}, "worker_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					WorkerID int64 `json:"worker_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.RevokeWorker(ctx, args.WorkerID); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该 AI 员工不存在。", nil
					}
					return "", err
				}
				return "已停用。", nil
			}),
	}
}

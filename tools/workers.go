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
		tool("create_worker", "创建一个 AI 员工并签发接入令牌（明文仅返回一次）。在工作机上运行 nbco-worker 并绑定该令牌，它便能领取任务、用交互式 PTY 驱动 claude/codex 自动完成。超管专用。",
			obj(map[string]any{"name": p("string", "员工名，如「小码」")}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Name) == "" {
					return "员工名不能为空。", nil
				}
				w, token, err := d.Store.CreateWorker(ctx, args.Name, u.ID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已创建 AI 员工「%s」。接入令牌（仅显示一次，请妥善保存）：\n<code>%s</code>\n"+
					"在工作机上运行：nbco-worker bind %s（令牌），再 nbco-worker run 即可上线接活。",
					w.Name, token, token), nil
			}),

		tool("revoke_worker", "停用一个 AI 员工并吊销其令牌（历史任务保留）。超管专用。",
			obj(map[string]any{"worker_id": p("integer", "员工用户ID")}, "worker_id"),
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

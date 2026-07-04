package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/perm"
	"github.com/zdypro888/nbco/internal/store"
)

const bindKeyTTL = 24 * time.Hour

// adminTools 用户管理与系统管理。超管专属工具只组装给超管，减小提示体积；
// 其余工具内部仍做权限校验（工具即权限边界）。
func adminTools(d Deps, u *store.User) []ai.Tool {
	ts := []ai.Tool{
		tool("list_users", "列出系统内用户（ID、名字、状态）。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				users, err := d.Store.ListUsers(ctx)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, other := range users {
					if other.ID == u.ID {
						continue
					}
					fmt.Fprintf(&b, "- #%d %s（%s）\n", other.ID, other.Name, other.Status)
				}
				if b.Len() == 0 {
					return "（没有其他用户）", nil
				}
				return b.String(), nil
			}),

		tool("get_user_info", "查看某用户的基本信息。需要对其 view_self_intro 主动权限。",
			obj(map[string]any{"user_id": p("integer", "用户ID")}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64 `json:"user_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActViewSelfIntro, args.UserID) {
						return "你没有权限查看该用户信息。", nil
					}
				}
				other, err := d.Store.UserByID(ctx, args.UserID)
				if err != nil {
					return fmt.Sprintf("用户 %d 不存在", args.UserID), nil
				}
				return renderUser(other), nil
			}),

		tool("update_user_info", "修改某用户的基本信息。需要对其 edit_info 主动权限。",
			obj(map[string]any{
				"user_id": p("integer", "用户ID"),
				"name":    p("string", "新名字（可选）"),
				"fields":  map[string]any{"type": "object", "description": "动态字段名→值（空串清除）", "additionalProperties": map[string]any{"type": "string"}},
			}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64             `json:"user_id"`
					Name   string            `json:"name"`
					Fields map[string]string `json:"fields"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActEditInfo, args.UserID) {
						return "你没有对该用户的 edit_info 权限。", nil
					}
				}
				if _, err := d.Store.UserByID(ctx, args.UserID); err != nil {
					return fmt.Sprintf("用户 %d 不存在", args.UserID), nil
				}
				if args.Name != "" {
					if err := d.Store.UpdateUserName(ctx, args.UserID, args.Name); err != nil {
						return "", err
					}
				}
				if len(args.Fields) > 0 {
					if err := d.Store.UpdateUserInfo(ctx, args.UserID, args.Fields); err != nil {
						return "", err
					}
				}
				return "已更新。", nil
			}),

		tool("generate_key", "生成入职绑定 Key（24小时有效、一次性）。需要 generate_key 权限。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !hasAnyActive(grants, perm.ActGenerateKey) {
						return "你没有 generate_key 权限。", nil
					}
				}
				bk, err := d.Store.CreateBindKey(ctx, u.ID, bindKeyTTL)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("绑定 Key：%s\n有效期至 %s，仅可使用一次。", bk.Key, fmtTime(bk.ExpiresAt, d.TZ)), nil
			}),

		tool("cancel_key", "作废我生成的全部未使用绑定 Key。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				if err := d.Store.CancelBindKeys(ctx, u.ID); err != nil {
					return "", err
				}
				return "已作废。", nil
			}),

		tool("send_message", "向指定用户发送消息。需要对其 send_msg 主动权限（超管不限）。",
			obj(map[string]any{
				"user_id": p("integer", "用户ID"),
				"text":    p("string", "消息内容"),
			}, "user_id", "text"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64  `json:"user_id"`
					Text   string `json:"text"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActSendMsg, args.UserID) {
						return "你没有对该用户的 send_msg 权限。", nil
					}
				}
				if _, err := mustUser(ctx, d.Store, args.UserID); err != nil {
					return err.Error(), nil
				}
				if d.Notifier == nil {
					return "当前入口不支持发送消息。", nil
				}
				if err := d.Notifier.Send(ctx, args.UserID, fmt.Sprintf("💬 来自 %s：\n%s", u.Name, args.Text)); err != nil {
					return "发送失败：" + err.Error(), nil
				}
				return "已发送。", nil
			}),

		tool("get_user_stats", "查看某用户的任务履历统计（当前负载、验收通过数、按时率）。看自己不限；看他人需对其 view_self_intro 权限。任务分配前先看这个。",
			obj(map[string]any{"user_id": p("integer", "用户ID")}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64 `json:"user_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.UserID != u.ID && !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActViewSelfIntro, args.UserID) {
						return "你没有权限查看该用户的统计。", nil
					}
				}
				other, err := d.Store.UserByID(ctx, args.UserID)
				if err != nil {
					return fmt.Sprintf("用户 %d 不存在", args.UserID), nil
				}
				st, err := d.Store.StatsOfAssignee(ctx, other.ID)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s（ID %d）的任务履历：\n", other.Name, other.ID)
				fmt.Fprintf(&b, "手上任务 %d（其中已过期 %d）· 待验收 %d\n", st.Open, st.OverdueNow, st.Awaiting)
				fmt.Fprintf(&b, "累计验收通过 %d", st.Accepted)
				if st.AcceptedWithDeadline > 0 {
					fmt.Fprintf(&b, "，有截止时间的 %d 个中按时 %d 个（%.0f%%）",
						st.AcceptedWithDeadline, st.AcceptedOnTime,
						float64(st.AcceptedOnTime)*100/float64(st.AcceptedWithDeadline))
				}
				b.WriteByte('\n')
				return b.String(), nil
			}),

		tool("list_info_fields", "查看当前定义的基本信息字段。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				fields, err := d.Store.ListInfoFields(ctx)
				if err != nil {
					return "", err
				}
				if len(fields) == 0 {
					return "（未定义字段）", nil
				}
				return strings.Join(fields, ", "), nil
			}),
	}

	if !u.IsSuperadmin {
		return ts
	}

	// --- 超管专属 ---
	ts = append(ts,
		tool("company_overview", "公司全景：全局任务统计、各项目进度、过期任务点名。写汇总/周报前先调它。超管专用。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				stats, err := d.Store.GlobalTaskStats(ctx, time.Now().Add(-7*24*time.Hour))
				if err != nil {
					return "", err
				}
				projects, err := d.Store.ListProjects(ctx)
				if err != nil {
					return "", err
				}
				counts, err := d.Store.ProjectTaskCounts(ctx)
				if err != nil {
					return "", err
				}
				overdue, err := d.Store.OverdueTasks(ctx, 20)
				if err != nil {
					return "", err
				}
				users, err := d.Store.ListUsers(ctx)
				if err != nil {
					return "", err
				}
				names := make(map[int64]string, len(users))
				for _, other := range users {
					names[other.ID] = other.Name
				}
				var b strings.Builder
				fmt.Fprintf(&b, "全局：进行中 %d（其中已过期 %d）· 待验收 %d · 近7天验收通过 %d\n",
					stats.Open, stats.Overdue, stats.Awaiting, stats.DoneSince)
				if len(projects) > 0 {
					b.WriteString("项目：\n")
					for _, pj := range projects {
						c := counts[pj.ID]
						fmt.Fprintf(&b, "- #%d %s（%s）：进行中 %d · 待验收 %d · 已完成 %d\n",
							pj.ID, pj.Name, pj.Status, c.Open, c.Awaiting, c.Accepted)
					}
				}
				if len(overdue) > 0 {
					b.WriteString("过期任务：\n")
					for _, t := range overdue {
						name := names[t.AssigneeID]
						if name == "" {
							name = fmt.Sprintf("用户%d", t.AssigneeID)
						}
						fmt.Fprintf(&b, "- #%d %s（执行人 %s，截止 %s）\n", t.ID, t.Title, name, fmtTime(*t.Deadline, d.TZ))
					}
				}
				return b.String(), nil
			}),

		tool("add_info_field", "添加一个基本信息字段定义。超管专用。",
			obj(map[string]any{"name": p("string", "字段名")}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				name := strings.TrimSpace(args.Name)
				if name == "" {
					return "字段名不能为空。", nil
				}
				if err := d.Store.AddInfoField(ctx, name); err != nil {
					if errors.Is(err, store.ErrConflict) {
						return "字段已存在。", nil
					}
					return "", err
				}
				return "已添加。", nil
			}),

		tool("remove_info_field", "移除一个基本信息字段定义（已有数据保留）。超管专用。",
			obj(map[string]any{"name": p("string", "字段名")}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.RemoveInfoField(ctx, args.Name); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "字段不存在。", nil
					}
					return "", err
				}
				return "已移除。", nil
			}),

		tool("disable_user", "停用一个用户（保留全部历史数据，不再能使用系统）。超管专用。",
			obj(map[string]any{"user_id": p("integer", "用户ID")}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64 `json:"user_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.UserID == u.ID {
					return "不能停用自己。", nil
				}
				if err := d.Store.SetUserStatus(ctx, args.UserID, store.UserDisabled); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "用户不存在。", nil
					}
					return "", err
				}
				return "已停用（历史数据保留）。", nil
			}),

		tool("enable_user", "重新启用一个已停用的用户。超管专用。",
			obj(map[string]any{"user_id": p("integer", "用户ID")}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64 `json:"user_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.SetUserStatus(ctx, args.UserID, store.UserActive); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "用户不存在。", nil
					}
					return "", err
				}
				return "已启用。", nil
			}),

		tool("create_role", "创建一个 AI 角色/Skill。超管专用。",
			obj(map[string]any{
				"name":         p("string", "角色名"),
				"trigger_desc": p("string", "什么场景下适合激活它"),
				"prompt":       p("string", "角色提示词"),
			}, "name", "prompt"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name        string `json:"name"`
					TriggerDesc string `json:"trigger_desc"`
					Prompt      string `json:"prompt"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if _, err := d.Store.CreateRole(ctx, args.Name, args.TriggerDesc, args.Prompt); err != nil {
					if errors.Is(err, store.ErrConflict) {
						return "同名角色已存在。", nil
					}
					return "", err
				}
				return "已创建。", nil
			}),

		tool("update_role", "更新角色的触发描述或提示词（空串字段不改）。超管专用。",
			obj(map[string]any{
				"name":         p("string", "角色名"),
				"trigger_desc": p("string", "新触发描述（可选）"),
				"prompt":       p("string", "新提示词（可选）"),
			}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name        string `json:"name"`
					TriggerDesc string `json:"trigger_desc"`
					Prompt      string `json:"prompt"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.UpdateRole(ctx, args.Name, args.TriggerDesc, args.Prompt); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "角色不存在。", nil
					}
					return "", err
				}
				return "已更新。", nil
			}),

		tool("delete_role", "删除一个角色。超管专用。",
			obj(map[string]any{"name": p("string", "角色名")}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.DeleteRole(ctx, args.Name); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "角色不存在。", nil
					}
					return "", err
				}
				return "已删除。", nil
			}),
	)
	return ts
}

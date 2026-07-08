package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
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

		tool("update_user_info", "修改某用户的基本信息。需要对其 edit_info 主动权限；值为空串/null/无表示清除字段，常见字段别名会自动归一。",
			obj(map[string]any{
				"user_id": p("integer", "用户ID"),
				"name":    p("string", "新名字（可选）"),
				"fields":  infoFieldsSchema("动态字段名→值（空串/null/无清除）"),
			}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64              `json:"user_id"`
					Name   string             `json:"name"`
					Fields map[string]*string `json:"fields"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				target, err := d.Store.UserByID(ctx, args.UserID)
				if err != nil {
					return fmt.Sprintf("用户 %d 不存在", args.UserID), nil
				}
				if !u.IsSuperadmin {
					if target.IsSuperadmin {
						return "不能修改超级管理员的信息。", nil
					}
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActEditInfo, args.UserID) {
						return "你没有对该用户的 edit_info 权限。", nil
					}
				}
				defined, err := d.Store.ListInfoFields(ctx)
				if err != nil {
					return "", err
				}
				fields, msg := normalizeInfoFieldsPtr(args.Fields, defined)
				if msg != "" {
					return msg, nil
				}
				if args.Name != "" {
					if err := d.Store.UpdateUserName(ctx, args.UserID, args.Name); err != nil {
						return "", err
					}
				}
				if len(fields) > 0 {
					if err := d.Store.UpdateUserInfo(ctx, args.UserID, fields); err != nil {
						return "", err
					}
				}
				return "已更新。", nil
			}),

		tool("bulk_update_user_info", "批量修改多名用户的基本信息。适合花名册/JSON 批量维护；比逐个调用 update_user_info 更稳。每行需要 user_id，可选 name、fields。需要对每个目标有 edit_info 主动权限。",
			obj(map[string]any{
				"updates": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"user_id": p("integer", "用户ID"),
							"name":    p("string", "新名字（可选）"),
							"fields":  infoFieldsSchema("动态字段名→值（空串/null/无清除）"),
						},
						"required": []string{"user_id"},
					},
				},
			}, "updates"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Updates []struct {
						UserID int64              `json:"user_id"`
						Name   string             `json:"name"`
						Fields map[string]*string `json:"fields"`
					} `json:"updates"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if len(args.Updates) == 0 {
					return "没有需要更新的记录。", nil
				}
				if len(args.Updates) > 100 {
					return "单次最多批量更新 100 条。", nil
				}
				defined, err := d.Store.ListInfoFields(ctx)
				if err != nil {
					return "", err
				}
				var grants []store.Grant
				if !u.IsSuperadmin {
					grants, err = d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
				}
				var ok, skipped int
				var b strings.Builder
				for i, row := range args.Updates {
					target, err := d.Store.UserByID(ctx, row.UserID)
					if err != nil {
						skipped++
						fmt.Fprintf(&b, "- 第 %d 条：目标不存在，已跳过。\n", i+1)
						continue
					}
					if !u.IsSuperadmin {
						switch {
						case target.IsSuperadmin:
							skipped++
							fmt.Fprintf(&b, "- %s：不能修改超级管理员，已跳过。\n", target.Name)
							continue
						case !perm.CheckActive(grants, perm.ActEditInfo, row.UserID):
							skipped++
							fmt.Fprintf(&b, "- %s：没有 edit_info 权限，已跳过。\n", target.Name)
							continue
						}
					}
					fields, msg := normalizeInfoFieldsPtr(row.Fields, defined)
					if msg != "" {
						skipped++
						fmt.Fprintf(&b, "- %s：%s，已跳过。\n", target.Name, msg)
						continue
					}
					if strings.TrimSpace(row.Name) != "" {
						if err := d.Store.UpdateUserName(ctx, row.UserID, row.Name); err != nil {
							return "", err
						}
					}
					if len(fields) > 0 {
						if err := d.Store.UpdateUserInfo(ctx, row.UserID, fields); err != nil {
							return "", err
						}
					}
					ok++
				}
				if b.Len() == 0 {
					return fmt.Sprintf("批量更新完成：成功 %d 条，跳过 0 条。", ok), nil
				}
				return fmt.Sprintf("批量更新完成：成功 %d 条，跳过 %d 条。\n%s", ok, skipped, b.String()), nil
			}),

		tool("invite_employee", "邀请真人员工加入系统：生成一次性 Telegram 邀请链接和兜底邀请码。可指定姓名/角色/备注/有效期；不是 worker access token。需要员工邀请权限。",
			obj(map[string]any{
				"name":      p("string", "被邀请员工姓名，可选；填写后绑定时直接作为系统姓名"),
				"role":      p("string", "被邀请员工角色/职位，可选，如 CEO、产品经理；绑定后写入用户动态信息 role"),
				"note":      p("string", "邀请备注，可选，仅用于审计/识别邀请用途"),
				"ttl_hours": p("integer", "有效小时数，可选，默认24，范围1-168"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name     string `json:"name"`
					Role     string `json:"role"`
					Note     string `json:"note"`
					TTLHours int    `json:"ttl_hours"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !hasAnyActive(grants, perm.ActGenerateKey) {
						return "你没有邀请员工权限。", nil
					}
				}
				ttl := bindKeyTTL
				if args.TTLHours > 0 {
					if args.TTLHours > 168 {
						args.TTLHours = 168
					}
					ttl = time.Duration(args.TTLHours) * time.Hour
				}
				bk, err := d.Store.CreateBindInvite(ctx, u.ID, ttl, args.Name, args.Role, args.Note)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				b.WriteString("已生成真人员工一次性邀请。\n")
				if bk.InvitedName != "" {
					fmt.Fprintf(&b, "邀请对象：%s\n", bk.InvitedName)
				}
				if bk.InvitedRole != "" {
					fmt.Fprintf(&b, "角色/职位：%s\n", bk.InvitedRole)
				}
				if link := employeeInviteLink(ctx, d, bk.Key); link != "" {
					fmt.Fprintf(&b, "Telegram 邀请链接：%s\n", link)
				}
				fmt.Fprintf(&b, "兜底邀请码：%s\n", bk.Key)
				fmt.Fprintf(&b, "有效期至 %s，仅可使用一次。\n", fmtTime(bk.ExpiresAt, d.TZ))
				b.WriteString("注意：这是一次性邀请，不是 worker access token，不能用于 nbco-worker bind。")
				return b.String(), nil
			}),

		tool("cancel_invites", "作废我生成的全部未使用真人员工邀请。", obj(nil),
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

		tool("get_ai_settings", "查看 AI 运行设置。超管专用。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				raw, err := d.Store.GetKV(ctx, store.KVAIStreamReasoning)
				if err != nil {
					return "", err
				}
				enabled := store.BoolSetting(raw, d.AIStreamReasoningDefault)
				source := "默认值"
				if strings.TrimSpace(raw) != "" {
					source = "运行时设置"
				}
				mode := "不展示模型推理过程，只展示最终正文"
				if enabled {
					mode = "展示模型推理过程与最终正文"
				}
				return fmt.Sprintf("AI 运行设置：\n- stream_reasoning: %t（%s，%s）", enabled, source, mode), nil
			}),

		tool("set_ai_settings", "修改 AI 运行设置。超管专用。stream_reasoning=false 时默认不向用户展示模型推理过程。",
			obj(map[string]any{
				"stream_reasoning": p("boolean", "是否在流式回复中展示模型推理过程"),
			}, "stream_reasoning"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					StreamReasoning bool `json:"stream_reasoning"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				value := "0"
				if args.StreamReasoning {
					value = "1"
				}
				if err := d.Store.SetKV(ctx, store.KVAIStreamReasoning, value); err != nil {
					return "", err
				}
				if args.StreamReasoning {
					return "已开启 stream_reasoning：后续流式回复会展示模型推理过程。", nil
				}
				return "已关闭 stream_reasoning：后续流式回复只展示最终正文。", nil
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

func employeeInviteLink(ctx context.Context, d Deps, key string) string {
	if d.Store == nil {
		return ""
	}
	username, err := d.Store.GetKV(ctx, store.KVTelegramBotUsername)
	if err != nil {
		return ""
	}
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return ""
	}
	return "https://t.me/" + username + "?start=" + key
}

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

const bindKeyTTL = 24 * time.Hour

type inviteEmployeeArgs struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Note     string `json:"note"`
	TTLHours int    `json:"ttl_hours"`
}

// CompanyOverview renders the same bounded, database-backed company snapshot
// used by the Agent tool. Scheduler-owned reports can preload it and use a
// tool-free Eino turn instead of making the model rediscover a closed dataset.
func CompanyOverview(ctx context.Context, s *store.Store, tz *time.Location) (string, error) {
	stats, err := s.GlobalTaskStats(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		return "", err
	}
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	counts, err := s.ProjectTaskCounts(ctx)
	if err != nil {
		return "", err
	}
	overdue, err := s.OverdueTasks(ctx, 20)
	if err != nil {
		return "", err
	}
	users, err := s.ListUsers(ctx)
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
			fmt.Fprintf(&b, "- %s：%s（%s）：进行中 %d · 待验收 %d · 已完成 %d\n",
				internalRef("项目", pj.ID), pj.Name, pj.Status, c.Open, c.Awaiting, c.Accepted)
		}
	}
	goals, err := s.ListGoals(ctx, true)
	if err != nil {
		return "", err
	}
	if len(goals) > 0 {
		gIDs := make([]int64, len(goals))
		for i, g := range goals {
			gIDs[i] = g.ID
		}
		gmc, err := s.GoalMilestoneCounts(ctx, gIDs)
		if err != nil {
			return "", err
		}
		b.WriteString("战略目标：\n")
		for _, g := range goals {
			fmt.Fprintf(&b, "- %s：%s（%s）：里程碑 %d/%d 达成\n",
				internalRef("目标", g.ID), g.Title, g.Status, gmc[g.ID].Achieved, gmc[g.ID].Total)
		}
	}
	if len(overdue) > 0 {
		b.WriteString("过期任务：\n")
		for _, task := range overdue {
			name := names[task.AssigneeID]
			if name == "" {
				name = "未知成员"
			}
			fmt.Fprintf(&b, "- %s：%s（执行人 %s，截止 %s）\n",
				internalRef("任务", task.ID), task.Title, name, fmtTime(*task.Deadline, tz))
		}
	}
	evidenceSince := time.Now().Add(-7 * 24 * time.Hour)
	evidenceStats, err := s.WorkEvidenceStatsSince(ctx, evidenceSince)
	if err != nil {
		return "", err
	}
	if evidenceStats.ObservedMessages > 0 || evidenceStats.StructuredItems > 0 {
		fmt.Fprintf(&b, "工作证据（近7天，区别于正式任务）：群聊消息 %d · 结构化摘要/进展 %d · 已识别成员 %d · 已绑定项目 %d\n",
			evidenceStats.ObservedMessages, evidenceStats.StructuredItems, evidenceStats.Actors, evidenceStats.Projects)
		recent, err := s.RecentWorkEvidence(ctx, evidenceSince, 10)
		if err != nil {
			return "", err
		}
		for _, item := range recent {
			label := item.Kind
			if item.ProjectName != "" {
				label += " / " + item.ProjectName
			}
			actor := item.ActorName
			if actor == "" {
				actor = item.Title
			}
			if actor != "" {
				label += " / " + actor
			}
			fmt.Fprintf(&b, "- [%s] %s：%s\n", item.EventAt.In(tz).Format("01-02 15:04"), label,
				textfmt.TruncateRunes(strings.TrimSpace(item.Content), 320))
		}
		b.WriteString("说明：以上是有来源的沟通/执行证据；未关联任务的内容不能直接视为已承诺、已完成或已验收。\n")
	}
	return b.String(), nil
}

// adminTools 用户管理与系统管理。超管专属工具只组装给超管，减小提示体积；
// 其余工具内部仍做权限校验（工具即权限边界）。
func adminTools(d Deps, u *store.User) []ai.Tool {
	ts := []ai.Tool{
		tool("list_users", "分页列出系统内用户目录，返回结构化 JSON。员工ID/user_id 是稳定业务编号，优先用于后续工具；姓名只是展示名，可能变化或重名。完整盘点时按 next_offset 继续，直到 has_more=false。",
			obj(map[string]any{
				"query":  p("string", "可选，按员工ID、姓名、human/worker 或状态筛选"),
				"limit":  p("integer", "每页数量，默认 50，最大 100"),
				"offset": p("integer", "分页偏移，默认 0"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Query  string `json:"query"`
					Limit  int    `json:"limit"`
					Offset int    `json:"offset"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.Limit <= 0 {
					args.Limit = 50
				}
				if args.Limit > 100 {
					args.Limit = 100
				}
				if args.Offset < 0 {
					return "offset 不能为负数。", nil
				}
				users, err := d.Store.ListUsers(ctx)
				if err != nil {
					return "", err
				}
				users = filterDirectoryUsers(users, args.Query)
				total := len(users)
				start := min(args.Offset, total)
				end := min(start+args.Limit, total)
				page := users[start:end]
				viewerActive, err := d.Store.PermsOf(ctx, u.ID)
				if err != nil {
					return "", err
				}
				stats := make(map[int64]userDirectoryStats, len(page))
				for _, other := range page {
					st, err := visibleProfileStats(ctx, d, u, other, viewerActive)
					if err != nil {
						return "", err
					}
					stats[other.ID] = st
				}
				return renderUserDirectory(page, u.ID, stats, total, start, args.Limit), nil
			}),

		tool("get_user_info", "查看某用户的基本信息。需要对其 view_self_intro 主动权限。优先传员工ID/user_id；tg_id 仅作 Telegram 精确绑定；姓名只是兜底且必须唯一。",
			obj(map[string]any{
				"user_id":   p("integer", "员工ID/系统用户ID（可选，优先）"),
				"tg_id":     p("string", "Telegram 用户 ID（可选，精确匹配已绑定身份）"),
				"user":      p("string", "姓名或 worker 名（可选；必须唯一匹配）"),
				"user_name": p("string", "同 user（可选）"),
				"target":    p("string", "用户ID | 唯一姓名 | tg:<Telegram ID>（可选）"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID   int64  `json:"user_id"`
					TGID     string `json:"tg_id"`
					User     string `json:"user"`
					UserName string `json:"user_name"`
					Target   string `json:"target"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				other, msg, err := resolveUserArg(ctx, d.Store, args.UserID, telegramUserSelector(args.TGID), args.Target, args.User, args.UserName)
				if err != nil {
					return "", err
				}
				if msg != "" {
					return msg, nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActViewSelfIntro, other.ID) {
						return "你没有权限查看该用户信息。", nil
					}
				}
				return renderUser(other), nil
			}),

		tool("get_users_info", "批量查看系统成员的基本信息与动态字段，避免逐个调用 get_user_info。user_ids 为空时读取当前有权查看的全部成员；非空时按稳定员工 ID 批量读取。每个目标仍独立执行 view_self_intro 权限校验。",
			obj(map[string]any{
				"user_ids": arr("integer", "可选员工 ID 列表；空表示全部可见成员，最多100人"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserIDs []int64 `json:"user_ids"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if len(args.UserIDs) > 100 {
					return "单次最多读取 100 名成员。", nil
				}
				users, err := d.Store.ListUsers(ctx)
				if err != nil {
					return "", err
				}
				selected := make(map[int64]bool, len(args.UserIDs))
				for _, id := range args.UserIDs {
					if id > 0 {
						selected[id] = true
					}
				}
				var grants []store.Grant
				if !u.IsSuperadmin {
					grants, err = d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
				}
				var b strings.Builder
				returned := 0
				for _, other := range users {
					if len(selected) > 0 && !selected[other.ID] {
						continue
					}
					if other.ID != u.ID && !u.IsSuperadmin && !perm.CheckActive(grants, perm.ActViewSelfIntro, other.ID) {
						continue
					}
					if returned >= 100 {
						break
					}
					if returned > 0 {
						b.WriteString("\n---\n")
					}
					b.WriteString(renderUser(other))
					returned++
				}
				if returned == 0 {
					return "没有找到有权查看的成员。", nil
				}
				if len(selected) > returned {
					fmt.Fprintf(&b, "\n请求 %d 个 ID，返回 %d 个；其余目标不存在或当前无权查看。", len(selected), returned)
				}
				return b.String(), nil
			}),

		tool("update_user_info", "修改系统成员的基本信息（真人员工和 AI worker 都是系统成员）。需要对其 edit_info 主动权限；优先传员工ID/user_id；tg_id 仅作 Telegram 精确绑定；姓名只是兜底且必须唯一；值为空串/null/无表示清除字段，常见字段别名会自动归一。",
			obj(map[string]any{
				"user_id":   p("integer", "员工ID/系统用户ID（可选，优先）"),
				"tg_id":     p("string", "Telegram 用户 ID（可选，精确匹配已绑定身份）"),
				"user":      p("string", "姓名或 worker 名（可选；必须唯一匹配）"),
				"user_name": p("string", "同 user（可选）"),
				"target":    p("string", "用户ID | 唯一姓名 | tg:<Telegram ID>（可选）"),
				"name":      p("string", "新名字（可选）"),
				"fields":    infoFieldsSchema("动态字段名→值（空串/null/无清除）"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID   int64              `json:"user_id"`
					TGID     string             `json:"tg_id"`
					User     string             `json:"user"`
					UserName string             `json:"user_name"`
					Target   string             `json:"target"`
					Name     string             `json:"name"`
					Fields   map[string]*string `json:"fields"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				target, msg, err := resolveUserArg(ctx, d.Store, args.UserID, telegramUserSelector(args.TGID), args.Target, args.User, args.UserName)
				if err != nil {
					return "", err
				}
				if msg != "" {
					return msg, nil
				}
				if !u.IsSuperadmin {
					if target.IsSuperadmin {
						return "不能修改超级管理员的信息。", nil
					}
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActEditInfo, target.ID) {
						return "你没有对该用户的 edit_info 权限。", nil
					}
				}
				fields, msg, err := normalizeInfoFieldsPtrForWrite(ctx, d.Store, args.Fields)
				if err != nil {
					return "", err
				}
				if msg != "" {
					return msg, nil
				}
				if args.Name != "" {
					if err := d.Store.UpdateUserName(ctx, target.ID, args.Name); err != nil {
						return "", err
					}
				}
				if len(fields) > 0 {
					if err := d.Store.UpdateUserInfo(ctx, target.ID, fields); err != nil {
						return "", err
					}
				}
				return "已更新。", nil
			}),

		tool("bulk_update_user_info", "批量修改多名系统成员的基本信息。适合花名册/JSON 批量维护；比逐个调用 update_user_info 更稳。每行需要 user_id，可选 name、fields。需要对每个目标有 edit_info 主动权限。",
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
				var grants []store.Grant
				if !u.IsSuperadmin {
					var err error
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
					fields, msg, ferr := normalizeInfoFieldsPtrForWrite(ctx, d.Store, row.Fields)
					if ferr != nil {
						return "", ferr
					}
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

		recoverableResultTool(tool("invite_employee", "邀请真人员工加入系统：生成一次性 Telegram 邀请链接和兜底邀请码。可指定姓名/角色/备注/有效期；不是 worker access token。需要员工邀请权限。",
			obj(map[string]any{
				"name":      p("string", "被邀请员工姓名，可选；填写后绑定时直接作为系统姓名"),
				"role":      p("string", "被邀请员工角色/职位，可选，如 CEO、产品经理；绑定后写入用户动态信息 role"),
				"note":      p("string", "邀请备注，可选，仅用于审计/识别邀请用途"),
				"ttl_hours": p("integer", "有效小时数，可选，默认24，范围1-168"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args inviteEmployeeArgs
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
				bk, err := d.Store.CreateBindInviteForRequest(ctx, u.ID, ttl, args.Name, args.Role, args.Note,
					toolInvocationRequestKey(ctx, u.ID, "invite_employee"))
				if err != nil {
					return "", err
				}
				return inviteEmployeeToolResult(ctx, d, bk), nil
			}), func(ctx context.Context, raw json.RawMessage) (string, bool, error) {
			var args inviteEmployeeArgs
			if err := decode(raw, &args); err != nil {
				return "", false, nil
			}
			bk, err := d.Store.BindInviteByRequest(ctx, u.ID, toolInvocationRequestKey(ctx, u.ID, "invite_employee"))
			if errors.Is(err, store.ErrNotFound) {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
			if bk.InvitedName != strings.TrimSpace(args.Name) || bk.InvitedRole != strings.TrimSpace(args.Role) || bk.Note != strings.TrimSpace(args.Note) {
				return "", false, store.ErrConflict
			}
			return inviteEmployeeToolResult(ctx, d, bk), true, nil
		}),

		tool("cancel_invites", "作废我生成的全部未使用真人员工邀请。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				if err := d.Store.CancelBindKeys(ctx, u.ID); err != nil {
					return "", err
				}
				return "已作废。", nil
			}),

		tool("send_message", "向指定用户或全体真人员工发送消息。需要 send_msg 主动权限（超管不限）。单人：优先传员工ID/user_id；tg_id 仅作 Telegram 精确绑定；姓名只是兜底且必须唯一。全体真人员工：target=\"_all\"；不要手动逐个发。",
			obj(map[string]any{
				"user_id":   p("integer", "员工ID/系统用户ID（可选，优先）"),
				"tg_id":     p("string", "Telegram 用户 ID（可选，精确匹配已绑定身份；也可在 target 里写 tg:<ID>）"),
				"user":      p("string", "姓名或 worker 名（可选；必须唯一匹配）"),
				"user_name": p("string", "同 user（可选）"),
				"target":    p("string", "self | _all | 用户ID | 唯一姓名 | tg:<Telegram ID>（可选；_all=全体真人员工，不含 AI worker，不含发起人自己）"),
				"text":      p("string", "消息内容"),
			}, "text"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID   int64  `json:"user_id"`
					TGID     string `json:"tg_id"`
					User     string `json:"user"`
					UserName string `json:"user_name"`
					Target   string `json:"target"`
					Text     string `json:"text"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if d.Notifier == nil {
					return "当前入口不支持发送消息。", nil
				}
				selector := messageTargetSelector(args.Target, args.User, args.UserName)
				if strings.TrimSpace(selector) == store.TargetAll {
					return sendMessageToAll(ctx, d, u, args.Text)
				}
				if strings.EqualFold(strings.TrimSpace(selector), "self") {
					args.UserID = u.ID
				}
				target, msg, err := resolveUserArg(ctx, d.Store, args.UserID, telegramUserSelector(args.TGID), args.Target, args.User, args.UserName)
				if err != nil {
					return "", err
				}
				if msg != "" {
					return msg, nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActSendMsg, target.ID) {
						return "你没有对该用户的 send_msg 权限。", nil
					}
				}
				body := fmt.Sprintf("💬 来自 %s：\n%s", u.Name, args.Text)
				delivery, err := notify.SendForToolInvocation(ctx, d.Store, d.Notifier, "send-message", target.ID, body)
				if err != nil || !delivery.Delivered {
					if err == nil {
						err = fmt.Errorf("投递结果不确定，系统未自动重发")
					}
					return "发送失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已发送给 %s。", target.Name), nil
			}),

		tool("get_user_stats", "查看某用户的任务履历统计（当前负载、验收通过数、按时率）。看自己不限；看他人需对其 view_self_intro 权限。任务分配前先看这个；优先传员工ID/user_id；tg_id 仅作 Telegram 精确绑定；姓名只是兜底。",
			obj(map[string]any{
				"user_id":   p("integer", "员工ID/系统用户ID（可选，优先）"),
				"tg_id":     p("string", "Telegram 用户 ID（可选，精确匹配已绑定身份）"),
				"user":      p("string", "姓名或 worker 名（可选；必须唯一匹配）"),
				"user_name": p("string", "同 user（可选）"),
				"target":    p("string", "用户ID | 唯一姓名 | tg:<Telegram ID>（可选）"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID   int64  `json:"user_id"`
					TGID     string `json:"tg_id"`
					User     string `json:"user"`
					UserName string `json:"user_name"`
					Target   string `json:"target"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				other, msg, err := resolveUserArg(ctx, d.Store, args.UserID, telegramUserSelector(args.TGID), args.Target, args.User, args.UserName)
				if err != nil {
					return "", err
				}
				if msg != "" {
					return msg, nil
				}
				if other.ID != u.ID && !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActViewSelfIntro, other.ID) {
						return "你没有权限查看该用户的统计。", nil
					}
				}
				st, err := d.Store.StatsOfAssignee(ctx, other.ID)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s 的任务履历：\n", other.Name)
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
		tool("company_overview", "读取公司级全局任务统计、各项目进度和过期任务；个人任务工具不能代表这些全局数据。超管专用。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				return CompanyOverview(ctx, d.Store, d.TZ)
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

type userDirectoryStats struct {
	SelfIntro  int
	PeerReview int
}

func visibleProfileStats(ctx context.Context, d Deps, viewer, subject *store.User, viewerActive []store.Grant) (userDirectoryStats, error) {
	all, err := d.Store.ProfilesOn(ctx, subject.ID)
	if err != nil {
		return userDirectoryStats{}, err
	}
	subjectPassive, err := d.Store.PassivePermsToward(ctx, subject.ID)
	if err != nil {
		return userDirectoryStats{}, err
	}
	var out userDirectoryStats
	for _, pr := range all {
		if !perm.CanViewProfile(viewer.ID, subject.ID, pr.AuthorID, viewer.IsSuperadmin, viewerActive, subjectPassive) {
			continue
		}
		if pr.AuthorID == subject.ID {
			out.SelfIntro++
		} else {
			out.PeerReview++
		}
	}
	return out, nil
}

func sendMessageToAll(ctx context.Context, d Deps, sender *store.User, text string) (string, error) {
	if sender == nil {
		return "当前用户不可用。", nil
	}
	if d.Notifier == nil {
		return "当前入口不支持发送消息。", nil
	}
	if !sender.IsSuperadmin && !hasActiveAll(ctx, d, sender.ID, perm.ActSendMsg) {
		return "给全体发送消息需要 send_msg:_all 权限。", nil
	}
	users, err := d.Store.ListUsers(ctx)
	if err != nil {
		return "", err
	}
	sent, failed := 0, 0
	var failedNames []string
	body := fmt.Sprintf("💬 来自 %s：\n%s", sender.Name, text)
	for _, target := range users {
		if target == nil || target.Status != store.UserActive || target.IsWorker || target.ID == sender.ID {
			continue
		}
		delivery, err := notify.SendForToolInvocation(ctx, d.Store, d.Notifier, "send-message-all", target.ID, body)
		if err != nil || !delivery.Delivered {
			failed++
			if len(failedNames) < 5 {
				failedNames = append(failedNames, target.Name)
			}
			continue
		}
		sent++
	}
	if sent == 0 && failed == 0 {
		return "没有可发送的真人员工。", nil
	}
	if failed == 0 {
		return fmt.Sprintf("已发送给全体真人员工：成功 %d 人。", sent), nil
	}
	return fmt.Sprintf("全体真人员工发送完成：成功 %d 人，失败 %d 人（失败示例：%s）。", sent, failed, strings.Join(failedNames, "、")), nil
}

func messageTargetSelector(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func filterDirectoryUsers(users []*store.User, query string) []*store.User {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return users
	}
	out := make([]*store.User, 0, len(users))
	for _, user := range users {
		if user == nil {
			continue
		}
		kind := "human"
		if user.IsWorker {
			kind = "worker"
		}
		haystack := strings.ToLower(fmt.Sprintf("%d\n%s\n%s\n%s", user.ID, user.Name, kind, user.Status))
		if strings.Contains(haystack, query) {
			out = append(out, user)
		}
	}
	return out
}

func renderUserDirectory(users []*store.User, currentID int64, stats map[int64]userDirectoryStats, total, offset, limit int) string {
	type directoryEntry struct {
		UserID              int64  `json:"user_id"`
		Name                string `json:"name"`
		Kind                string `json:"kind"`
		Status              string `json:"status"`
		IsCurrent           bool   `json:"is_current"`
		IsSuperadmin        bool   `json:"is_superadmin"`
		VisibleProfileCount int    `json:"visible_profile_count"`
		VisibleReviewCount  int    `json:"visible_review_count"`
	}
	type directoryResult struct {
		Users       []directoryEntry `json:"users"`
		Total       int              `json:"total"`
		Offset      int              `json:"offset"`
		Limit       int              `json:"limit"`
		HasMore     bool             `json:"has_more"`
		NextOffset  *int             `json:"next_offset,omitempty"`
		HumanCount  int              `json:"page_human_count"`
		WorkerCount int              `json:"page_worker_count"`
	}
	result := directoryResult{Users: make([]directoryEntry, 0, len(users)), Total: total, Offset: offset, Limit: limit}
	for _, other := range users {
		if other == nil {
			continue
		}
		kind := "human"
		if other.IsWorker {
			kind = "worker"
			result.WorkerCount++
		} else {
			result.HumanCount++
		}
		st := stats[other.ID]
		result.Users = append(result.Users, directoryEntry{
			UserID: other.ID, Name: other.Name, Kind: kind, Status: other.Status,
			IsCurrent: other.ID == currentID, IsSuperadmin: other.IsSuperadmin, VisibleProfileCount: st.SelfIntro,
			VisibleReviewCount: st.PeerReview,
		})
	}
	result.HasMore = offset+len(result.Users) < total
	if result.HasMore {
		next := offset + len(result.Users)
		result.NextOffset = &next
	}
	raw, _ := json.Marshal(result)
	return string(raw)
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

func inviteEmployeeToolResult(ctx context.Context, d Deps, bk *store.BindKey) string {
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
	return b.String()
}

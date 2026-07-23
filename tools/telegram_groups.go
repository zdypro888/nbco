package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

const telegramGroupToolLimit = 50
const telegramGroupMessageToolLimit = 120
const telegramGroupDigestSourceKind = store.ScheduleSourceTelegramGroupDigest

// telegramGroupTools 管理 Telegram 群这个外部实体，和用户/worker 一样走 list/get/update。
func telegramGroupTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("list_telegram_groups", "列出 bot 已记录的 Telegram 群及其接入和监听状态。结果里的 group_ref 是工作内存，可直接给后续群工具当参数；最终出口会清理内部引用。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				groups, err := d.Store.ListTelegramGroupStates(ctx, telegramGroupToolLimit)
				if err != nil {
					return "", err
				}
				if len(groups) == 0 {
					return "当前没有记录到 Telegram 群。", nil
				}
				var b strings.Builder
				b.WriteString("Telegram 群列表：\n")
				for _, g := range groups {
					fmt.Fprintf(&b, "- %s：%s，%s，%s，%s，每日摘要 %s，最近更新 %s；group_ref=%s（工作内存）\n",
						telegramGroupTitle(g), telegramGroupStatusText(g), telegramGroupListenText(g),
						telegramGroupAutoInviteText(ctx, d, g), telegramGroupMonitorText(ctx, d, g),
						telegramGroupDigestText(ctx, d, u, g), fmtTime(g.UpdatedAt, d.TZ), telegramGroupRef(g.ChatID))
				}
				return b.String(), nil
			}),

		tool("get_telegram_group", "查看一个 Telegram 群的接入、监听、监控和当前用户每日摘要配置。它不返回群消息；查询群里今天讨论了什么、日报、问题或摘要时使用 list_telegram_group_messages。group 可填群名、群名片段或 list_telegram_groups 返回的 group_ref。",
			obj(map[string]any{
				"group": p("string", "群名、群名片段或 group_ref"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group string `json:"group"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil {
					return "", err
				}
				if g == nil {
					return msg, nil
				}
				out := renderTelegramGroup(*g, d.TZ)
				if pj, err := d.Store.TelegramGroupProject(ctx, g.ChatID); err == nil && pj != nil {
					out += fmt.Sprintf("\n- 绑定项目：%s %s", internalRef("项目", pj.ID), pj.Name)
				}
				out += "\n- 自动邀请：" + telegramGroupAutoInviteText(ctx, d, *g)
				out += "\n- 智能监控：" + telegramGroupMonitorText(ctx, d, *g)
				out += "\n- 每日摘要：" + telegramGroupDigestText(ctx, d, u, *g)
				out += "\n- 群消息内容：本工具未查询；需要内容、日报或摘要时调用 list_telegram_group_messages"
				return out, nil
			}),

		tool("list_telegram_group_messages", "按群与时间范围读取系统实际收到并保存的 Telegram 群消息，跨越群会话重置。用于内容查询、汇总以及问题或决策分析；群配置工具不返回消息正文。date 按公司时区；发生截断时把返回的 next_cursor 传回 cursor 继续读取；无结果只表示 bot 在该时间范围没有记录，不能证明群里绝对没有消息。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref，可选；只有一个群时可省略"),
				"date":        p("string", "按公司时区查询的日期 YYYY-MM-DD；默认今天"),
				"since_hours": p("integer", "可选：改为查询最近 N 小时，优先于 date；最大 8760"),
				"limit":       p("integer", "最多返回最新多少条，默认 80，最大 120"),
				"cursor":      p("integer", "可选：上页返回的 next_cursor，用于继续读取更早消息"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group      string `json:"group"`
					Date       string `json:"date"`
					SinceHours int    `json:"since_hours"`
					Limit      int    `json:"limit"`
					Cursor     int64  `json:"cursor"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				from, to, msg := telegramGroupMessageRange(args.Date, args.SinceHours, d.TZ)
				if msg != "" {
					return msg, nil
				}
				limit := args.Limit
				if limit <= 0 {
					limit = 80
				}
				if limit > telegramGroupMessageToolLimit {
					limit = telegramGroupMessageToolLimit
				}
				page, err := d.Store.ListChannelMessagesPage(ctx, telegramGroupChannel(g.ChatID), from, to, args.Cursor, limit)
				if err != nil {
					return "", err
				}
				return renderTelegramGroupMessages(*g, page, from, to, d.TZ), nil
			}),

		tool("list_telegram_group_members", "查看 Telegram 群成员可见信息。注意：Bot API 不能枚举所有普通成员；本工具返回成员总数、管理员列表、以及系统已见过的发言人/加入者。",
			obj(map[string]any{
				"group": p("string", "群名、群名片段或 group_ref，可选；只有一个群时可省略"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group string `json:"group"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "Telegram 群成员可见信息：%s\n", telegramGroupTitle(*g))
				if c, err := telegramController(d); err == nil {
					if botMember, err := c.GetTelegramGroupBotMember(ctx, g.ChatID); err == nil && botMember != nil {
						fmt.Fprintf(&b, "- Bot 当前身份：%s。%s\n",
							telegramMemberStatusText(botMember.Status), telegramBotMemberCapabilityText(botMember.Status))
						if rights := telegramMemberRightsText(botMember.Rights); rights != "" {
							fmt.Fprintf(&b, "- Bot 管理员权限：%s\n", rights)
						}
					}
					if n, err := c.GetTelegramGroupMemberCount(ctx, g.ChatID); err == nil {
						fmt.Fprintf(&b, "- 成员总数：%d\n", n)
					}
					if admins, err := c.GetTelegramGroupAdministrators(ctx, g.ChatID); err == nil && len(admins) > 0 {
						b.WriteString("- 管理员：\n")
						for _, a := range admins {
							rights := telegramMemberRightsText(a.Rights)
							if rights != "" {
								rights = "；" + rights
							}
							fmt.Fprintf(&b, "  • %s（%s%s）\n", telegramMemberDisplay(a), telegramMemberStatusText(a.Status), rights)
						}
					}
				} else {
					b.WriteString("- 成员总数/管理员：Telegram 控制器不可用，暂不能实时查询。\n")
				}
				seen, err := d.Store.ListTelegramGroupSeenMembers(ctx, g.ChatID, 20)
				if err != nil {
					return "", err
				}
				if len(seen) > 0 {
					b.WriteString("- 最近见过的发言人/加入者（不是全量成员列表）：\n")
					for _, m := range seen {
						fmt.Fprintf(&b, "  • %s，最近 %s\n", telegramSeenMemberDisplay(m), fmtTime(m.LastSeen, d.TZ))
					}
				} else {
					b.WriteString("- 最近见过的发言人/加入者：暂无记录。\n")
				}
				b.WriteString("说明：即使 bot 是管理员，Telegram Bot API 也不提供一次性枚举全体普通成员的接口；如果要确认某个人是否在群里，请用 get_telegram_group_member。")
				return b.String(), nil
			}),

		tool("resolve_telegram_group_members", "批量对照 Telegram 群里已见过的人与 nbco 系统员工，区分已绑定成员、真人和 AI worker。内部用 Telegram ID 精确绑定，最终按姓名和绑定状态自然汇报。",
			obj(map[string]any{
				"group": p("string", "群名、群名片段或 group_ref，可选；只有一个群时可省略"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group string `json:"group"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				return renderTelegramGroupMemberBindings(ctx, d, *g)
			}),

		tool("get_telegram_group_member", "查询某个人是否在 Telegram 群里。user 可填公司用户姓名、系统用户ID、或最近见过的 Telegram 显示名；需要该用户已绑定 Telegram 或曾在群里发言/加入。",
			obj(map[string]any{
				"group": p("string", "群名、群名片段或 group_ref，可选；只有一个群时可省略"),
				"user":  p("string", "公司用户姓名、系统用户ID、或最近见过的 Telegram 显示名"),
			}, "user"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group string `json:"group"`
					User  string `json:"user"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				tgUserID, label, msg, err := resolveTelegramUserForGroup(ctx, d, *g, args.User)
				if err != nil || tgUserID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				member, err := c.GetTelegramGroupMember(ctx, g.ChatID, tgUserID)
				if err != nil {
					return fmt.Sprintf("没有确认 %s 在 %s 中，Telegram 返回：%s", label, telegramGroupTitle(*g), err.Error()), nil
				}
				return fmt.Sprintf("%s 在 %s 的状态：%s。", telegramMemberDisplay(*member), telegramGroupTitle(*g), telegramMemberStatusText(member.Status)), nil
			}),

		tool("set_telegram_group_listen", "开启或关闭 Telegram 群监听。需要 manage_telegram_group 权限；group 可填群名、群名片段或 group_ref。开启后普通群消息会进入群共享上下文但不主动插话。",
			obj(map[string]any{
				"group":  p("string", "群名、群名片段或 group_ref"),
				"listen": p("boolean", "true 开启监听，false 关闭监听"),
			}, "group", "listen"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group  string `json:"group"`
					Listen bool   `json:"listen"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil {
					return "", err
				}
				if g == nil {
					return msg, nil
				}
				if args.Listen {
					if err := ensureTelegramGroupTranscript(ctx, d, u, g); err != nil {
						return "", err
					}
					if err := d.Store.SetKV(ctx, store.TelegramGroupListenKey(g.ChatID), "1"); err != nil {
						return "", err
					}
					g.Listen = true
					g.UpdatedAt = time.Now()
					if err := d.Store.SaveTelegramGroupState(ctx, *g); err != nil {
						return "", err
					}
					return fmt.Sprintf("已更新 %s：%s。", telegramGroupTitle(*g), telegramGroupListenText(*g)), nil
				}
				if err := d.Store.SetKV(ctx, store.TelegramGroupListenKey(g.ChatID), ""); err != nil {
					return "", err
				}
				g.Listen = false
				g.UpdatedAt = time.Now()
				if err := d.Store.SaveTelegramGroupState(ctx, *g); err != nil {
					return "", err
				}
				return fmt.Sprintf("已更新 %s：%s。", telegramGroupTitle(*g), telegramGroupListenText(*g)), nil
			}),

		tool("set_telegram_group_auto_invite", "开启或关闭 Telegram 群自动邀请。需要 manage_telegram_group 权限；开启后，未加入系统的人在该群 @bot 时，系统生成真人员工一次性邀请，优先私发；无法私发时提示对方私聊 /start 自动领取。不会把邀请码发到群里。",
			obj(map[string]any{
				"group":   p("string", "群名、群名片段或 group_ref"),
				"enabled": p("boolean", "true 开启，false 关闭"),
			}, "group", "enabled"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group   string `json:"group"`
					Enabled bool   `json:"enabled"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				value := ""
				if args.Enabled {
					value = strconv.FormatInt(u.ID, 10)
				}
				if err := d.Store.SetKV(ctx, store.TelegramGroupAutoInviteKey(g.ChatID), value); err != nil {
					return "", err
				}
				if args.Enabled {
					return fmt.Sprintf("已开启 %s 的自动邀请。未加入系统的人在群里 @bot 时，我会生成真人员工一次性邀请并尽量私发；不会在群里公开邀请码。", telegramGroupTitle(*g)), nil
				}
				return fmt.Sprintf("已关闭 %s 的自动邀请。", telegramGroupTitle(*g)), nil
			}),

		tool("set_telegram_group_monitor", "开启或关闭 Telegram 群事件监控。需要 manage_telegram_group 权限；它只对开启后的新消息分批做 AI 判断，并在值得关注时私聊发起人，不逐条转发、不回看历史、也不按时钟生成每日摘要。按日汇总请另用 set_telegram_group_digest。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"enabled":     p("boolean", "true 开启，false 关闭"),
				"instruction": p("string", "可选：监控目标、关注条件与通知策略"),
			}, "group", "enabled"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group       string `json:"group"`
					Enabled     bool   `json:"enabled"`
					Instruction string `json:"instruction"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				now := time.Now()
				if args.Enabled {
					if err := ensureTelegramGroupTranscript(ctx, d, u, g); err != nil {
						return "", err
					}
				}
				_, err = d.Store.UpdateTelegramGroupMonitor(ctx, g.ChatID, func(mon *store.TelegramGroupMonitor) error {
					wasEnabled := mon.Enabled
					mon.Enabled = args.Enabled
					mon.GroupTitle = telegramGroupTitle(*g)
					mon.NotifyUserID = u.ID
					mon.CreatedBy = u.ID
					mon.UpdatedAt = now
					if strings.TrimSpace(args.Instruction) != "" {
						mon.Instruction = strings.TrimSpace(args.Instruction)
					}
					if !args.Enabled {
						mon.PendingCount = 0
						mon.BatchStartedAt = time.Time{}
						mon.AnalysisOwner = ""
						mon.AnalysisStartedAt = time.Time{}
						mon.AnalysisThrough = time.Time{}
						mon.AnalysisFailures = 0
						mon.Buffer = nil
					} else if !wasEnabled {
						mon.LastCheckedAt = now
						mon.BatchStartedAt = time.Time{}
						mon.PendingCount = 0
						mon.AnalysisOwner = ""
						mon.AnalysisStartedAt = time.Time{}
						mon.AnalysisThrough = time.Time{}
						mon.AnalysisFailures = 0
						mon.Buffer = nil
					}
					return nil
				})
				if err != nil {
					return "", err
				}
				if args.Enabled {
					digestNote := "每日摘要仍未开启"
					if status := telegramGroupDigestText(ctx, d, u, *g); status != "未设置" {
						digestNote = "每日摘要配置未改变：" + status
					}
					return fmt.Sprintf("已开启 %s 的事件监控，提醒对象为 %s。后续新消息会分批交给 AI 判断，值得关注时私聊汇总；没有回看历史，%s。",
						telegramGroupTitle(*g), telegramMonitorNotifyName(u), digestNote), nil
				}
				return fmt.Sprintf("已关闭 %s 的智能监控。每日摘要配置未改变。", telegramGroupTitle(*g)), nil
			}),

		tool("set_telegram_group_digest", "设置或关闭 Telegram 群每日摘要。需要 manage_telegram_group 权限；这是独立的只读持久自动化，到点后读取该群当天真实消息并由 AI 生成摘要私聊给当前用户。instruction 只改变摘要关注点，不能借摘要执行发送、建任务或更新状态等动作。它不改变事件监控。开启必须提供 daily_at，避免系统猜测发送时间。重复设置会幂等更新同一配置。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"enabled":     p("boolean", "true 开启或更新，false 关闭"),
				"daily_at":    p("string", "每天发送时刻 HH:MM（公司时区）；开启时必填"),
				"weekdays":    p("string", "可选星期过滤，如 1,2,3,4,5；空表示每天"),
				"instruction": p("string", "可选摘要重点，如只看风险、决策和待跟进事项"),
			}, "group", "enabled"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group       string `json:"group"`
					Enabled     bool   `json:"enabled"`
					DailyAt     string `json:"daily_at"`
					Weekdays    string `json:"weekdays"`
					Instruction string `json:"instruction"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				sourceKey := strconv.FormatInt(g.ChatID, 10)
				if !args.Enabled {
					err := d.Store.CancelAutomationSchedule(ctx, u.ID, telegramGroupDigestSourceKind, sourceKey)
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Sprintf("%s 没有启用当前用户的每日摘要。事件监控状态未改变。", telegramGroupTitle(*g)), nil
					}
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("已关闭 %s 给当前用户的每日摘要。事件监控状态未改变。", telegramGroupTitle(*g)), nil
				}
				dailyAt, err := normalizeDailyAt(args.DailyAt)
				if err != nil {
					return "开启每日摘要需要明确的 " + err.Error() + "。", nil
				}
				weekdays, err := normalizeWeekdays(args.Weekdays)
				if err != nil {
					return err.Error(), nil
				}
				if err := ensureTelegramGroupTranscript(ctx, d, u, g); err != nil {
					return "", err
				}
				sc, err := d.Store.UpsertAutomationSchedule(ctx, &store.Schedule{
					UserID: u.ID, Kind: store.ScheduleDaily, Message: telegramGroupDigestDirective(*g, args.Instruction),
					FireAt: store.NextDailyFire(time.Now(), dailyAt, weekdays, d.TZ),
					Target: store.ScheduleTargetSelf, Mode: store.ScheduleModeAI, DailyAt: dailyAt,
					Weekdays: weekdays, CreatedBy: u.ID,
					SourceKind: telegramGroupDigestSourceKind, SourceKey: sourceKey,
					Title: telegramGroupTitle(*g) + " 每日摘要", SourceMessageID: sourceMessageID(ctx),
				})
				if err != nil {
					return "", err
				}
				days := "每天"
				if sc.Weekdays != "" {
					days = "周" + sc.Weekdays
				}
				return fmt.Sprintf("已设置 %s 给 %s 的每日摘要：%s %s，首次触发 %s。到点读取当天群消息后生成；事件监控状态未改变。",
					telegramGroupTitle(*g), telegramMonitorNotifyName(u), days, sc.DailyAt, fmtTime(sc.FireAt, d.TZ)), nil
			}),

		tool("send_telegram_group_message", "向 Telegram 群发送消息。需要 manage_telegram_group 权限；群里通常直接发言即可，私聊里可用本工具代发到群。发送后返回 message_ref 供编辑、撤回、置顶等后续工具使用。",
			obj(map[string]any{
				"group":                p("string", "群名、群名片段或 group_ref"),
				"text":                 p("string", "要发送到群里的内容，可用 Telegram HTML/普通文本"),
				"disable_notification": p("boolean", "是否静默发送，可选"),
			}, "group", "text"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group               string `json:"group"`
					Text                string `json:"text"`
					DisableNotification bool   `json:"disable_notification"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				messageID, err := c.SendTelegramGroupMessage(ctx, g.ChatID, args.Text, args.DisableNotification)
				if err != nil {
					return "发送失败：" + err.Error(), nil
				}
				if err := d.Store.SaveTelegramGroupLastMessage(ctx, g.ChatID, messageID); err != nil {
					return "", err
				}
				return fmt.Sprintf("已发送到 %s。message_ref=%s（工作内存）",
					telegramGroupTitle(*g), telegramMessageRef(g.ChatID, messageID)), nil
			}),

		tool("edit_telegram_group_message", "编辑 Telegram 群消息。默认编辑该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。只能编辑 bot 自己可编辑的消息。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"text":        p("string", "新的消息内容"),
				"message_ref": p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":  p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
			}, "group", "text"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group      string `json:"group"`
					Text       string `json:"text"`
					MessageRef string `json:"message_ref"`
					MessageID  int    `json:"message_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.EditTelegramGroupMessage(ctx, g.ChatID, messageID, args.Text); err != nil {
					return "编辑失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已编辑 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("delete_telegram_group_message", "撤回/删除 Telegram 群消息。默认删除该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。删除他人消息要求 bot 在群里有删除消息权限。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"message_ref": p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":  p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group      string `json:"group"`
					MessageRef string `json:"message_ref"`
					MessageID  int    `json:"message_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.DeleteTelegramGroupMessage(ctx, g.ChatID, messageID); err != nil {
					return "删除失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已删除 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("pin_telegram_group_message", "置顶 Telegram 群消息。默认置顶该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。要求 bot 有置顶权限。",
			obj(map[string]any{
				"group":                p("string", "群名、群名片段或 group_ref"),
				"message_ref":          p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":           p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
				"disable_notification": p("boolean", "是否静默置顶，可选"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group               string `json:"group"`
					MessageRef          string `json:"message_ref"`
					MessageID           int    `json:"message_id"`
					DisableNotification bool   `json:"disable_notification"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.PinTelegramGroupMessage(ctx, g.ChatID, messageID, args.DisableNotification); err != nil {
					return "置顶失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已置顶 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("unpin_telegram_group_message", "取消置顶 Telegram 群消息。默认取消该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。要求 bot 有置顶管理权限。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"message_ref": p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":  p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group      string `json:"group"`
					MessageRef string `json:"message_ref"`
					MessageID  int    `json:"message_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.UnpinTelegramGroupMessage(ctx, g.ChatID, messageID); err != nil {
					return "取消置顶失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已取消置顶 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("update_telegram_group_info", "修改 Telegram 群标题或描述。需要 manage_telegram_group 权限；要求 bot 是群管理员且有修改群信息权限。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"title":       p("string", "新群标题，可选"),
				"description": p("string", "新群描述，可选；空串表示不修改"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group       string `json:"group"`
					Title       string `json:"title"`
					Description string `json:"description"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				changed := []string{}
				if strings.TrimSpace(args.Title) != "" {
					if err := c.SetTelegramGroupTitle(ctx, g.ChatID, strings.TrimSpace(args.Title)); err != nil {
						return "修改群标题失败：" + err.Error(), nil
					}
					changed = append(changed, "标题")
				}
				if strings.TrimSpace(args.Description) != "" {
					if err := c.SetTelegramGroupDescription(ctx, g.ChatID, strings.TrimSpace(args.Description)); err != nil {
						return "修改群描述失败：" + err.Error(), nil
					}
					changed = append(changed, "描述")
				}
				if len(changed) == 0 {
					return "没有需要修改的群信息。", nil
				}
				return fmt.Sprintf("已更新 %s 的%s。", telegramGroupTitle(*g), strings.Join(changed, "、")), nil
			}),
	}
}

func resolveTelegramGroup(ctx context.Context, d Deps, selector string) (*store.TelegramGroupState, string, error) {
	selector = strings.TrimSpace(selector)
	groups, err := d.Store.ListTelegramGroupStates(ctx, telegramGroupToolLimit)
	if err != nil {
		return nil, "", err
	}
	if len(groups) == 0 {
		return nil, "当前没有记录到 Telegram 群。", nil
	}
	if selector == "" && len(groups) == 1 {
		return &groups[0], "", nil
	}
	if chatID, ok := parseTelegramGroupRef(selector); ok {
		for i := range groups {
			if groups[i].ChatID == chatID {
				return &groups[i], "", nil
			}
		}
		return nil, "没有找到这个 Telegram 群。", nil
	}
	lower := strings.ToLower(selector)
	exact := []store.TelegramGroupState{}
	fuzzy := []store.TelegramGroupState{}
	for _, g := range groups {
		title := strings.ToLower(strings.TrimSpace(g.Title))
		switch {
		case title == lower:
			exact = append(exact, g)
		case selector != "" && strings.Contains(title, lower):
			fuzzy = append(fuzzy, g)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = fuzzy
	}
	switch len(matches) {
	case 0:
		return nil, "没有找到匹配的 Telegram 群。先调用 list_telegram_groups 查看可用群。", nil
	case 1:
		return &matches[0], "", nil
	default:
		var b strings.Builder
		b.WriteString("匹配到多个 Telegram 群，请用更完整的群名或 group_ref：\n")
		for _, g := range matches {
			fmt.Fprintf(&b, "- %s；group_ref=%s（工作内存）\n", telegramGroupTitle(g), telegramGroupRef(g.ChatID))
		}
		return nil, b.String(), nil
	}
}

func parseTelegramGroupRef(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "telegram:group:")
	s = strings.TrimPrefix(s, "group:")
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

func telegramController(d Deps) (TelegramGroupController, error) {
	if d.TelegramGroups == nil {
		return nil, fmt.Errorf("当前没有可用的 Telegram 群控制器")
	}
	return d.TelegramGroups, nil
}

func resolveTelegramMessageID(ctx context.Context, d Deps, g store.TelegramGroupState, messageRef string, messageID int) (int, string, error) {
	if chatID, msgID, ok := parseTelegramMessageRef(messageRef); ok {
		if chatID != 0 && chatID != g.ChatID {
			return 0, "message_ref 不属于这个 Telegram 群。", nil
		}
		return msgID, "", nil
	}
	if messageID > 0 {
		return messageID, "", nil
	}
	last, err := d.Store.TelegramGroupLastMessage(ctx, g.ChatID)
	if err != nil {
		if err == store.ErrNotFound {
			return 0, "没有可操作的最近群消息。请先用 send_telegram_group_message 发送，或提供 message_ref/message_id。", nil
		}
		return 0, "", err
	}
	return last, "", nil
}

func parseTelegramMessageRef(s string) (chatID int64, messageID int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	if strings.HasPrefix(s, "message:") {
		id, err := strconv.Atoi(strings.TrimPrefix(s, "message:"))
		return 0, id, err == nil && id > 0
	}
	parts := strings.Split(s, ":")
	if len(parts) == 5 && parts[0] == "telegram" && parts[1] == "group" && parts[3] == "message" {
		cid, cerr := strconv.ParseInt(parts[2], 10, 64)
		mid, merr := strconv.Atoi(parts[4])
		return cid, mid, cerr == nil && merr == nil && mid > 0
	}
	return 0, 0, false
}

func telegramMessageRef(chatID int64, messageID int) string {
	return fmt.Sprintf("telegram:group:%d:message:%d", chatID, messageID)
}

func telegramGroupRef(chatID int64) string {
	return fmt.Sprintf("telegram:group:%d", chatID)
}

func telegramGroupChannel(chatID int64) string {
	return telegramGroupRef(chatID)
}

// ensureTelegramGroupTranscript 建立群消息的持久落点。是否采集由监听、事件监控
// 和摘要各自的状态决定，不能让一个功能篡改另一个功能的开关。
func ensureTelegramGroupTranscript(ctx context.Context, d Deps, u *store.User, g *store.TelegramGroupState) error {
	if d.Store == nil || u == nil || g == nil {
		return errors.New("telegram 群采集依赖不完整")
	}
	if d.TelegramGroups != nil {
		if err := d.TelegramGroups.EnsureTelegramGroupSession(ctx, g.ChatID, u.ID); err != nil {
			return fmt.Errorf("建立群消息会话: %w", err)
		}
	}
	return nil
}

func telegramGroupMessageRange(date string, sinceHours int, tz *time.Location) (time.Time, time.Time, string) {
	if tz == nil {
		tz = time.Local
	}
	now := time.Now()
	if sinceHours > 0 {
		if sinceHours > 24*365 {
			return time.Time{}, time.Time{}, "since_hours 不能超过 8760。"
		}
		return now.Add(-time.Duration(sinceHours) * time.Hour), now, ""
	}
	date = strings.TrimSpace(date)
	if date == "" {
		date = now.In(tz).Format("2006-01-02")
	}
	from, err := time.ParseInLocation("2006-01-02", date, tz)
	if err != nil {
		return time.Time{}, time.Time{}, "date 格式应为 YYYY-MM-DD。"
	}
	if from.After(now.In(tz)) {
		return time.Time{}, time.Time{}, "date 不能晚于当前业务日期。"
	}
	to := from.AddDate(0, 0, 1)
	if to.After(now) {
		to = now
	}
	return from, to, ""
}

func renderTelegramGroupMessages(g store.TelegramGroupState, page store.ChannelMessagePage, from, to time.Time, tz *time.Location) string {
	if tz == nil {
		tz = time.Local
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Telegram 群消息事实：%s\n", telegramGroupTitle(g))
	fmt.Fprintf(&b, "时间范围：%s 至 %s（%s，左闭右开）\n",
		from.In(tz).Format("2006-01-02 15:04"), to.In(tz).Format("2006-01-02 15:04"), tz.String())
	if page.Total == 0 {
		b.WriteString("观察结果：recorded_messages=0。\n")
		b.WriteString("唯一可支持的结论：该范围没有系统已记录的群消息。\n")
		b.WriteString("禁止据此推断：Telegram 群内绝对无人发言、成员休假、节假日安排、群外活动或任何因果解释。")
		return b.String()
	}
	fmt.Fprintf(&b, "共 %d 条，返回 %d 条", page.Total, len(page.Messages))
	if page.Total > int64(len(page.Messages)) {
		b.WriteString("（仅最新部分）")
	}
	b.WriteString("。\n")
	for _, message := range page.Messages {
		content := strings.TrimSpace(clipRunes(message.Content, 600))
		content = strings.ReplaceAll(content, "\n", "\n  ")
		if message.Role == "assistant" {
			fmt.Fprintf(&b, "- %s nbco：%s\n", message.CreatedAt.In(tz).Format("01-02 15:04"), content)
			continue
		}
		fmt.Fprintf(&b, "- %s %s\n", message.CreatedAt.In(tz).Format("01-02 15:04"), content)
	}
	if page.NextCursor > 0 {
		fmt.Fprintf(&b, "next_cursor: %d（继续读取更早消息时传给 cursor）\n", page.NextCursor)
	}
	return strings.TrimSpace(b.String())
}

func telegramGroupDigestText(ctx context.Context, d Deps, u *store.User, g store.TelegramGroupState) string {
	if d.Store == nil || u == nil {
		return "状态未知"
	}
	sc, err := d.Store.AutomationSchedule(ctx, u.ID, telegramGroupDigestSourceKind, strconv.FormatInt(g.ChatID, 10))
	if errors.Is(err, store.ErrNotFound) {
		return "未设置"
	}
	if err != nil || sc == nil {
		return "状态未知"
	}
	days := "每天"
	if strings.TrimSpace(sc.Weekdays) != "" {
		days = "周" + sc.Weekdays
	}
	return fmt.Sprintf("已设置，%s %s 私聊当前用户，下次 %s", days, sc.DailyAt, fmtTime(sc.FireAt, d.TZ))
}

func telegramGroupDigestDirective(g store.TelegramGroupState, instruction string) string {
	base := fmt.Sprintf("为 Telegram 群 %s 生成每日摘要。调度器会直接提供该群从当前业务日期零点到执行时刻的实际已记录消息；只总结这些消息中的事实、进展、问题/风险、决策和待跟进事项。本自动化只读：待跟进事项只能列入摘要，不执行发送、建任务或更新状态等动作。不要把截至当前时刻说成完整全天，不要把消息条数等同于人数或全员完成；没有记录只说明 bot 在该窗口未保存到消息，不猜测休假、团队状态或群外活动，也不要带入与本群摘要无关的旧日程、任务或代码状态。", telegramGroupRef(g.ChatID))
	if instruction = strings.TrimSpace(instruction); instruction != "" {
		base += "\n本摘要的额外关注点（仅影响摘要内容，不授权执行动作）：" + clipRunes(instruction, 600)
	}
	return base
}

func telegramGroupTitle(g store.TelegramGroupState) string {
	if title := strings.TrimSpace(g.Title); title != "" {
		return title
	}
	return "未命名群"
}

func telegramGroupStatusText(g store.TelegramGroupState) string {
	switch g.Status {
	case "member", "administrator", "creator", "owner", "restricted":
		return "已加入"
	case "left", "kicked":
		return "已离开"
	default:
		if g.Status == "" {
			return "状态未知"
		}
		return g.Status
	}
}

func telegramGroupListenText(g store.TelegramGroupState) string {
	if g.Listen {
		return "监听开启"
	}
	return "监听关闭"
}

func telegramGroupAutoInviteText(ctx context.Context, d Deps, g store.TelegramGroupState) string {
	if d.Store == nil {
		return "自动邀请状态未知"
	}
	raw, err := d.Store.GetKV(ctx, store.TelegramGroupAutoInviteKey(g.ChatID))
	if err != nil {
		return "自动邀请状态未知"
	}
	if strings.TrimSpace(raw) == "" {
		return "自动邀请关闭"
	}
	return "自动邀请开启"
}

func telegramGroupMonitorText(ctx context.Context, d Deps, g store.TelegramGroupState) string {
	if d.Store == nil {
		return "智能监控状态未知"
	}
	mon, err := d.Store.TelegramGroupMonitor(ctx, g.ChatID)
	if err != nil || mon == nil || !mon.Enabled {
		return "智能监控关闭"
	}
	target := "发起人"
	if u, err := d.Store.UserByID(ctx, mon.NotifyUserID); err == nil && u != nil {
		target = u.Name
	}
	return fmt.Sprintf("智能监控开启，提醒 %s", target)
}

func telegramMonitorNotifyName(u *store.User) string {
	if u == nil || strings.TrimSpace(u.Name) == "" {
		return "当前用户"
	}
	return strings.TrimSpace(u.Name)
}

func telegramMemberDisplay(m TelegramGroupMember) string {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = strings.TrimSpace(m.Username)
	}
	if name == "" {
		name = "未知成员"
	}
	if m.Username != "" && !strings.Contains(name, "@") {
		name = fmt.Sprintf("%s（@%s）", name, strings.TrimPrefix(m.Username, "@"))
	}
	if m.IsBot {
		name += "（bot）"
	}
	return name
}

func telegramSeenMemberDisplay(m store.TelegramGroupSeenMember) string {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = strings.TrimSpace(m.Username)
	}
	if name == "" {
		name = "未知成员"
	}
	if m.Username != "" && !strings.Contains(name, "@") {
		name = fmt.Sprintf("%s（@%s）", name, strings.TrimPrefix(m.Username, "@"))
	}
	if m.IsBot {
		name += "（bot）"
	}
	return name
}

func renderTelegramGroupMemberBindings(ctx context.Context, d Deps, g store.TelegramGroupState) (string, error) {
	seen, err := d.Store.ListTelegramGroupSeenMembers(ctx, g.ChatID, 100)
	if err != nil {
		return "", err
	}
	var admins []TelegramGroupMember
	if c, err := telegramController(d); err == nil {
		admins, _ = c.GetTelegramGroupAdministrators(ctx, g.ChatID)
	}
	seenByTG := map[int64]store.TelegramGroupSeenMember{}
	for _, m := range seen {
		if m.UserID == 0 {
			continue
		}
		seenByTG[m.UserID] = m
	}
	for _, a := range admins {
		if a.UserID == 0 {
			continue
		}
		if _, ok := seenByTG[a.UserID]; !ok {
			seenByTG[a.UserID] = store.TelegramGroupSeenMember{
				ChatID:   g.ChatID,
				UserID:   a.UserID,
				Name:     a.Name,
				Username: a.Username,
				IsBot:    a.IsBot,
			}
		}
	}
	if len(seenByTG) == 0 {
		return fmt.Sprintf("%s 目前没有可对照的成员记录。Telegram Bot API 不能一次性枚举全体普通成员；需要成员发言、加入事件或管理员列表出现后才能识别。", telegramGroupTitle(g)), nil
	}

	users, err := d.Store.ListUsers(ctx)
	if err != nil {
		return "", err
	}
	boundByTG := map[int64]*store.User{}
	for _, u := range users {
		if u == nil || u.Status != store.UserActive {
			continue
		}
		ident, err := d.Store.IdentityOfUser(ctx, u.ID, "telegram")
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return "", err
		}
		tgUserID, err := strconv.ParseInt(strings.TrimSpace(ident.ExternalID), 10, 64)
		if err == nil && tgUserID != 0 {
			boundByTG[tgUserID] = u
		}
	}

	members := make([]store.TelegramGroupSeenMember, 0, len(seenByTG))
	for _, m := range seenByTG {
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		return strings.ToLower(telegramSeenMemberDisplay(members[i])) < strings.ToLower(telegramSeenMemberDisplay(members[j]))
	})

	var exact, suspected, unmatched int
	var b strings.Builder
	fmt.Fprintf(&b, "Telegram 群成员身份对照：%s\n", telegramGroupTitle(g))
	for _, m := range members {
		label := telegramSeenMemberDisplay(m)
		if u := boundByTG[m.UserID]; u != nil {
			exact++
			fmt.Fprintf(&b, "- %s → %s（已绑定，%s）\n", label, telegramCompanyUserLabel(u), "精确匹配")
			continue
		}
		if u, ambiguous := matchCompanyUserForSeen(users, m); ambiguous {
			suspected++
			fmt.Fprintf(&b, "- %s → 多个同名系统员工，需人工确认绑定\n", label)
		} else if u != nil {
			suspected++
			fmt.Fprintf(&b, "- %s → 疑似 %s（名称相同但未绑定）\n", label, telegramCompanyUserLabel(u))
		} else {
			unmatched++
			fmt.Fprintf(&b, "- %s → 未绑定系统员工\n", label)
		}
	}
	fmt.Fprintf(&b, "汇总：已绑定 %d，疑似 %d，未绑定 %d。说明：Telegram ID 仅用于内部精确匹配；对用户汇报时按姓名和绑定状态表达。", exact, suspected, unmatched)
	return b.String(), nil
}

func matchCompanyUserForSeen(users []*store.User, m store.TelegramGroupSeenMember) (*store.User, bool) {
	candidates := []string{
		strings.TrimSpace(m.Name),
		strings.TrimSpace(strings.TrimPrefix(m.Username, "@")),
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if u, ambiguous := matchCompanyUser(users, candidate, true); u != nil || ambiguous {
			return u, ambiguous
		}
	}
	return nil, false
}

func telegramCompanyUserLabel(u *store.User) string {
	if u == nil {
		return "未知员工"
	}
	switch {
	case u.IsWorker:
		return u.Name + "（AI worker）"
	case u.IsSuperadmin:
		return u.Name + "（超级管理员）"
	default:
		return u.Name + "（真人员工）"
	}
}

func telegramMemberStatusText(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "creator", "owner":
		return "群主"
	case "administrator":
		return "管理员"
	case "member":
		return "成员"
	case "restricted":
		return "受限成员"
	case "left":
		return "已离开"
	case "kicked", "banned":
		return "已移出"
	default:
		if strings.TrimSpace(status) == "" {
			return "状态未知"
		}
		return status
	}
}

func telegramBotMemberCapabilityText(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "creator", "owner", "administrator":
		return "管理员状态下可以接收成员变更事件、查询指定成员、管理消息/群信息（取决于具体管理员权限），但不能一次性拉取全体普通成员列表。"
	case "member", "restricted":
		return "普通成员状态下只能看到群消息与服务消息，成员变化记录会不完整。"
	case "left", "kicked", "banned":
		return "bot 当前不在群内，无法查询或管理该群。"
	default:
		return "权限状态未知，实时查询能力可能受限。"
	}
}

func telegramMemberRightsText(rights []string) string {
	if len(rights) == 0 {
		return ""
	}
	labels := make([]string, 0, len(rights))
	for _, r := range rights {
		switch r {
		case "owner":
			labels = append(labels, "群主")
		case "manage_chat":
			labels = append(labels, "管理群")
		case "delete_messages":
			labels = append(labels, "删除消息")
		case "manage_video_chats":
			labels = append(labels, "管理视频聊天")
		case "restrict_members":
			labels = append(labels, "限制成员")
		case "promote_members":
			labels = append(labels, "提升管理员")
		case "change_info":
			labels = append(labels, "修改群信息")
		case "invite_users":
			labels = append(labels, "邀请成员")
		case "post_messages":
			labels = append(labels, "发布消息")
		case "edit_messages":
			labels = append(labels, "编辑消息")
		case "pin_messages":
			labels = append(labels, "置顶消息")
		case "manage_topics":
			labels = append(labels, "管理话题")
		default:
			labels = append(labels, r)
		}
	}
	return strings.Join(labels, "、")
}

func resolveTelegramUserForGroup(ctx context.Context, d Deps, g store.TelegramGroupState, selector string) (int64, string, string, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return 0, "", "请提供要查询的人。", nil
	}
	if id, err := strconv.ParseInt(selector, 10, 64); err == nil && id > 0 {
		u, err := d.Store.UserByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return 0, "", "没有找到这个系统用户。", nil
			}
			return 0, "", "", err
		}
		return telegramUserIDFromCompanyUser(ctx, d, u)
	}
	users, err := d.Store.ListUsers(ctx)
	if err != nil {
		return 0, "", "", err
	}
	if u, ambiguous := matchCompanyUser(users, selector, true); ambiguous {
		return 0, "", "匹配到多个公司用户，请用更完整的姓名或系统用户ID。", nil
	} else if u != nil {
		return telegramUserIDFromCompanyUser(ctx, d, u)
	}
	if u, ambiguous := matchCompanyUser(users, selector, false); ambiguous {
		return 0, "", "匹配到多个公司用户，请用更完整的姓名或系统用户ID。", nil
	} else if u != nil {
		return telegramUserIDFromCompanyUser(ctx, d, u)
	}

	seen, err := d.Store.ListTelegramGroupSeenMembers(ctx, g.ChatID, 200)
	if err != nil {
		return 0, "", "", err
	}
	if m, ambiguous := matchSeenTelegramMember(seen, selector, true); ambiguous {
		return 0, "", "匹配到多个见过的 Telegram 成员，请用更完整的显示名或 @username。", nil
	} else if m != nil {
		return m.UserID, telegramSeenMemberDisplay(*m), "", nil
	}
	if m, ambiguous := matchSeenTelegramMember(seen, selector, false); ambiguous {
		return 0, "", "匹配到多个见过的 Telegram 成员，请用更完整的显示名或 @username。", nil
	} else if m != nil {
		return m.UserID, telegramSeenMemberDisplay(*m), "", nil
	}
	return 0, "", "没有找到这个人的 Telegram 身份。请先确认他已绑定公司账号，或让他在该群发言/加入后再查。", nil
}

func telegramUserIDFromCompanyUser(ctx context.Context, d Deps, u *store.User) (int64, string, string, error) {
	if u == nil {
		return 0, "", "没有找到这个系统用户。", nil
	}
	ident, err := d.Store.IdentityOfUser(ctx, u.ID, "telegram")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, "", fmt.Sprintf("%s 还没有绑定 Telegram。", u.Name), nil
		}
		return 0, "", "", err
	}
	tgUserID, err := strconv.ParseInt(strings.TrimSpace(ident.ExternalID), 10, 64)
	if err != nil || tgUserID == 0 {
		return 0, "", fmt.Sprintf("%s 的 Telegram 绑定数据不可用。", u.Name), nil
	}
	return tgUserID, u.Name, "", nil
}

func matchCompanyUser(users []*store.User, selector string, exact bool) (*store.User, bool) {
	needle := normalizeTelegramLookup(selector)
	if needle == "" {
		return nil, false
	}
	var matches []*store.User
	for _, u := range users {
		if u == nil || u.Status != store.UserActive {
			continue
		}
		name := normalizeTelegramLookup(u.Name)
		if name == "" {
			continue
		}
		if exact && name == needle || !exact && strings.Contains(name, needle) {
			matches = append(matches, u)
		}
	}
	if len(matches) == 1 {
		return matches[0], false
	}
	return nil, len(matches) > 1
}

func matchSeenTelegramMember(members []store.TelegramGroupSeenMember, selector string, exact bool) (*store.TelegramGroupSeenMember, bool) {
	needle := normalizeTelegramLookup(selector)
	if needle == "" {
		return nil, false
	}
	var matches []store.TelegramGroupSeenMember
	for _, m := range members {
		name := normalizeTelegramLookup(m.Name)
		username := normalizeTelegramLookup(strings.TrimPrefix(m.Username, "@"))
		if exact && (name == needle || username == needle) ||
			!exact && ((name != "" && strings.Contains(name, needle)) || (username != "" && strings.Contains(username, needle))) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return &matches[0], false
	}
	return nil, len(matches) > 1
}

func normalizeTelegramLookup(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(s, "@"))
	return strings.ToLower(s)
}

func renderTelegramGroup(g store.TelegramGroupState, tz *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Telegram 群：%s\n", telegramGroupTitle(g))
	fmt.Fprintf(&b, "- 接入状态：%s\n", telegramGroupStatusText(g))
	fmt.Fprintf(&b, "- 群类型：%s\n", g.Type)
	fmt.Fprintf(&b, "- 监听状态：%s\n", telegramGroupListenText(g))
	fmt.Fprintf(&b, "- 最近更新：%s\n", fmtTime(g.UpdatedAt, tz))
	fmt.Fprintf(&b, "- group_ref：%s（工作内存）", telegramGroupRef(g.ChatID))
	return b.String()
}

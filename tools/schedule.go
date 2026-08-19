package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

// hasActiveAll 是否拥有某主动权限的 _all 范围授权。
func hasActiveAll(ctx context.Context, d Deps, userID int64, action string) bool {
	grants, err := d.Store.PermsOf(ctx, userID)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Kind == store.KindActive && g.Action == action && g.Target == store.TargetAll {
			return true
		}
	}
	return false
}

const minRepeatInterval = 60 // 秒

func sourceMessageID(ctx context.Context) *int64 {
	turn, ok := approvalTurnFromContext(ctx)
	if !ok {
		return nil
	}
	id := turn.MessageID
	return &id
}

// scheduleTools 定时提醒。
func scheduleTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("schedule_once", "设置只发给当前用户的单次原文提醒。时间用 ISO8601（如 2026-07-05T09:00:00+08:00；不带时区按公司时区算）。message 会原样投递；需要定向给他人或到时由 AI 结合实时状态生成内容时使用 schedule_once_push。",
			obj(map[string]any{
				"at":      p("string", "触发时间 ISO8601"),
				"message": p("string", "需要原样投递的提醒内容"),
			}, "at", "message"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					At      string `json:"at"`
					Message string `json:"message"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				at, err := parseDeadline(args.At, d.TZ)
				if err != nil {
					return err.Error(), nil
				}
				if at == nil {
					return "必须提供触发时间。", nil
				}
				if !at.After(time.Now()) {
					return "触发时间必须在未来。", nil
				}
				sc, err := d.Store.CreateSchedule(ctx, &store.Schedule{
					UserID: u.ID, Kind: store.ScheduleOnce, Message: args.Message, FireAt: at.UTC(), CreatedBy: u.ID,
					SourceMessageID: sourceMessageID(ctx),
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已设置提醒（%s）：%s 提醒你「%s」。", internalRef("提醒", sc.ID), fmtTime(sc.FireAt, d.TZ), args.Message), nil
			}),

		tool("schedule_repeating", "设置只发给当前用户、按秒数间隔循环的原文提醒。最小间隔 60 秒。按每天/每周日历时间定向推送，或需要到时由 AI 结合实时状态生成内容时使用 schedule_recurring_push。",
			obj(map[string]any{
				"first_at":         p("string", "首次触发时间 ISO8601"),
				"interval_seconds": p("integer", "间隔秒数（≥60）"),
				"message":          p("string", "每次需要原样投递的提醒内容"),
			}, "first_at", "interval_seconds", "message"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					FirstAt         string `json:"first_at"`
					IntervalSeconds int64  `json:"interval_seconds"`
					Message         string `json:"message"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.IntervalSeconds < minRepeatInterval {
					return fmt.Sprintf("间隔不能小于 %d 秒。", minRepeatInterval), nil
				}
				at, err := parseDeadline(args.FirstAt, d.TZ)
				if err != nil {
					return err.Error(), nil
				}
				if at == nil {
					return "必须提供首次触发时间。", nil
				}
				sc, err := d.Store.CreateSchedule(ctx, &store.Schedule{
					UserID: u.ID, Kind: store.ScheduleRepeat, Message: args.Message,
					FireAt: at.UTC(), IntervalS: args.IntervalSeconds, CreatedBy: u.ID,
					SourceMessageID: sourceMessageID(ctx),
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已设置循环提醒（%s）：从 %s 起每 %d 秒。", internalRef("提醒", sc.ID), fmtTime(sc.FireAt, d.TZ), sc.IntervalS), nil
			}),

		scheduleOncePushTool(d, u),
		scheduleRecurringPushTool(d, u),

		tool("update_schedule", "原子修改自己创建的定时提醒、计划推送或持久自动化。它保留原日程ID、接收范围、模式、内容、来源绑定和执行历史；可修改时间规则或 recipient_policy。recipient_policy=mandatory 表示收件人不可退订，仅超管或拥有对应 manage_mandatory_schedule 权限者可设置；默认 optional。优先传 schedule_id；不知道编号时传 query，只有唯一活跃匹配才更新。不同种类支持的时间字段：once=at，repeat=first_at/interval_seconds，daily=daily_at/weekdays。",
			obj(map[string]any{
				"schedule_id":      p("integer", "日程ID（可选，优先）"),
				"query":            p("string", "不知道 ID 时按标题、内容或来源检索（可选）"),
				"at":               p("string", "once 的新触发时间 ISO8601"),
				"first_at":         p("string", "repeat 的新下次触发时间 ISO8601（可选）"),
				"interval_seconds": p("integer", "repeat 的新间隔秒数（可选，≥60）"),
				"daily_at":         p("string", "daily 的新触发时刻 HH:MM（可选）"),
				"weekdays":         p("string", "daily 的星期过滤；空字符串表示每天（可选）"),
				"recipient_policy": enumP("接收策略（可选）", store.ScheduleRecipientOptional, store.ScheduleRecipientMandatory),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ScheduleID      int64   `json:"schedule_id"`
					Query           string  `json:"query"`
					At              *string `json:"at"`
					FirstAt         *string `json:"first_at"`
					IntervalSeconds *int64  `json:"interval_seconds"`
					DailyAt         *string `json:"daily_at"`
					Weekdays        *string `json:"weekdays"`
					RecipientPolicy *string `json:"recipient_policy"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				sc, message, err := resolveActiveSchedule(ctx, d, u, args.ScheduleID, args.Query)
				if err != nil || message != "" {
					return message, err
				}
				if !u.IsSuperadmin && sc.CreatedBy != u.ID {
					return "只能修改自己创建的日程；收件人如需关闭可选日程，请修改自己的接收设置。", nil
				}
				var recipientPolicy *string
				resultingPolicy := sc.RecipientPolicy
				if resultingPolicy == "" {
					resultingPolicy = store.ScheduleRecipientOptional
				}
				if args.RecipientPolicy != nil {
					policy, err := normalizeScheduleRecipientPolicy(*args.RecipientPolicy)
					if err != nil {
						return err.Error(), nil
					}
					resultingPolicy = policy
					recipientPolicy = &policy
				}
				// A durable schedule survives later personnel changes, but every
				// mutation that leaves it mandatory must be authorized now. The
				// owner may always downgrade it to optional or cancel it.
				if resultingPolicy == store.ScheduleRecipientMandatory {
					if sc.Target == "" || sc.Target == store.ScheduleTargetSelf {
						return "只发给自己的日程无需设置 mandatory；可直接取消或调整日程。", nil
					}
					allowed, err := canSetMandatorySchedule(ctx, d, u, sc.Target, sc.UserID)
					if err != nil {
						return "", err
					}
					if !allowed {
						return "该日程保持 mandatory 需要当前仍对全部目标拥有 manage_mandatory_schedule 权限；你仍可将它改为 optional 或取消。", nil
					}
				}
				fireAt, intervalS := sc.FireAt, sc.IntervalS
				dailyAt, weekdays := sc.DailyAt, sc.Weekdays
				now := time.Now()
				switch sc.Kind {
				case store.ScheduleOnce:
					if args.FirstAt != nil || args.IntervalSeconds != nil || args.DailyAt != nil || args.Weekdays != nil || (args.At == nil && recipientPolicy == nil) {
						return "once 日程只能修改 at 或 recipient_policy，且至少提供一项。", nil
					}
					if args.At != nil {
						next, err := parseDeadline(*args.At, d.TZ)
						if err != nil {
							return err.Error(), nil
						}
						if next == nil || !next.After(now) {
							return "at 必须是未来时间。", nil
						}
						fireAt = next.UTC()
					}
				case store.ScheduleRepeat:
					if args.At != nil || args.DailyAt != nil || args.Weekdays != nil ||
						(args.FirstAt == nil && args.IntervalSeconds == nil && recipientPolicy == nil) {
						return "repeat 日程只能修改 first_at、interval_seconds 或 recipient_policy，且至少提供一项。", nil
					}
					if args.FirstAt != nil {
						next, err := parseDeadline(*args.FirstAt, d.TZ)
						if err != nil {
							return err.Error(), nil
						}
						if next == nil || !next.After(now) {
							return "first_at 必须是未来时间。", nil
						}
						fireAt = next.UTC()
					}
					if args.IntervalSeconds != nil {
						if *args.IntervalSeconds < minRepeatInterval {
							return fmt.Sprintf("间隔不能小于 %d 秒。", minRepeatInterval), nil
						}
						intervalS = *args.IntervalSeconds
					}
				case store.ScheduleDaily:
					if args.At != nil || args.FirstAt != nil || args.IntervalSeconds != nil ||
						(args.DailyAt == nil && args.Weekdays == nil && recipientPolicy == nil) {
						return "daily 日程只能修改 daily_at、weekdays 或 recipient_policy，且至少提供一项。", nil
					}
					if args.DailyAt != nil {
						dailyAt, err = normalizeDailyAt(*args.DailyAt)
						if err != nil {
							return err.Error(), nil
						}
					}
					if args.Weekdays != nil {
						weekdays, err = normalizeWeekdays(*args.Weekdays)
						if err != nil {
							return err.Error(), nil
						}
					}
					fireAt = store.NextDailyFire(now, dailyAt, weekdays, d.TZ)
				default:
					return "该日程种类暂不支持修改时间规则。", nil
				}
				updated, err := d.Store.UpdateScheduleVisible(
					ctx, sc.ID, u.ID, u.IsSuperadmin, fireAt, intervalS, dailyAt, weekdays, sourceMessageID(ctx), recipientPolicy,
				)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "日程不存在、已结束或不在你的可见范围。", nil
					}
					return "", err
				}
				return fmt.Sprintf("已更新%s：%s；接收策略 %s；接收范围、内容、模式和来源绑定均保持不变。",
					internalRef("日程", updated.ID), scheduleTimingDescription(updated, d.TZ), updated.RecipientPolicy), nil
			}),

		tool("cancel_schedule", "取消尚未执行的定时提醒、计划推送或持久自动化；它们属于日程，不是普通工作任务。只有用户明确要停止日程时才使用；修改时间、星期或间隔必须使用 update_schedule，不能取消后重建。优先传 schedule_id；不知道编号时传 query，工具会在当前可见的活跃日程中按标题、内容和来源查找，只有唯一匹配才取消。超级管理员可取消全局日程。",
			obj(map[string]any{
				"schedule_id": p("integer", "日程ID（可选，优先）"),
				"query":       p("string", "不知道 ID 时按标题、内容或来源检索（可选）"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ScheduleID int64  `json:"schedule_id"`
					Query      string `json:"query"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.ScheduleID <= 0 {
					if strings.TrimSpace(args.Query) == "" {
						return "请提供 schedule_id；不知道编号时请提供可用于消歧的 query。", nil
					}
					matches, err := findVisibleSchedules(ctx, d, u, args.Query, store.ScheduleActive)
					if err != nil {
						return "", err
					}
					switch len(matches) {
					case 0:
						historical, err := findVisibleSchedules(ctx, d, u, args.Query, "all")
						if err != nil {
							return "", err
						}
						switch len(historical) {
						case 0:
							return "没有找到匹配的定时提醒、计划推送或自动化。", nil
						case 1:
							if historical[0].Status == store.ScheduleActive {
								args.ScheduleID = historical[0].ID
								break
							}
							return inactiveScheduleResult(historical[0].Schedule, d.TZ), nil
						default:
							return "匹配到多条已结束日程，请根据稳定 ID 指定一条：\n" + renderScheduleCandidates(historical, d.TZ), nil
						}
					case 1:
						args.ScheduleID = matches[0].ID
					default:
						return "匹配到多条活跃日程，请根据稳定 ID 指定一条：\n" + renderScheduleCandidates(matches, d.TZ), nil
					}
				}
				if err := d.Store.CancelScheduleVisible(ctx, args.ScheduleID, u.ID, u.IsSuperadmin); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						sc, findErr := d.Store.ScheduleByID(ctx, args.ScheduleID)
						if findErr == nil && sc.Status == store.ScheduleActive && sc.CreatedBy != u.ID && !u.IsSuperadmin && scheduleTargetsUser(sc, u.ID) {
							if sc.RecipientPolicy == store.ScheduleRecipientMandatory {
								return "这是由其他人创建的强制接收日程，收件人不能取消或退订。", nil
							}
							return "不能取消其他人创建的共享日程；请改用 set_schedule_subscription 调整自己的接收状态。", nil
						}
						if findErr == nil && scheduleVisibleTo(sc, u) && sc.Status != store.ScheduleActive {
							return inactiveScheduleResult(*sc, d.TZ), nil
						}
						return "日程不存在或不在你的可见范围。", nil
					}
					return "", err
				}
				return fmt.Sprintf("已取消%s。", internalRef("日程", args.ScheduleID)), nil
			}),

		tool("list_schedules", "查看可见的定时提醒、计划推送和持久自动化；这些属于日程，不是普通工作任务。超级管理员查看全局队列。status 默认 active，也可查 done、cancelled 或 all。结果同时标明 optional/mandatory 接收策略；强制接收日程不能由收件人退订。返回稳定日程 ID、状态和触发时间；修改已有日程使用 update_schedule，停止才使用 cancel_schedule；逐次投递结果可用 query_data(source=deliveries) 深挖。",
			obj(map[string]any{
				"status": enumP("状态筛选，可省略", store.ScheduleActive, store.ScheduleDone, store.ScheduleCancelled, "all"),
				"limit":  p("integer", "最多返回条数，默认100，最大500"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Status string `json:"status"`
					Limit  int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				status := strings.ToLower(strings.TrimSpace(args.Status))
				switch status {
				case "", store.ScheduleActive, store.ScheduleDone, store.ScheduleCancelled, "all":
				default:
					return "status 只能是 active、done、cancelled 或 all。", nil
				}
				limit := args.Limit
				if limit <= 0 {
					limit = 100
				} else if limit > 500 {
					limit = 500
				}
				scs, err := d.Store.SchedulesVisible(ctx, u.ID, u.IsSuperadmin, status, limit)
				if err != nil {
					return "", err
				}
				if len(scs) == 0 {
					return "（无定时提醒或自动化）", nil
				}
				var b strings.Builder
				for i := range scs {
					sc := &scs[i]
					refLabel := "提醒"
					if strings.TrimSpace(sc.SourceKind) != "" {
						refLabel = "自动化"
					}
					whenLabel := "计划"
					if sc.Status == store.ScheduleActive {
						whenLabel = "下次"
					}
					fmt.Fprintf(&b, "- %s [%s/%s] %s %s %s", internalRef(refLabel, sc.ID), sc.Status, sc.Kind, scheduleDisplayMessage(ctx, d, &sc.Schedule), whenLabel, fmtTime(sc.FireAt, d.TZ))
					if sc.Kind == store.ScheduleRepeat {
						fmt.Fprintf(&b, "（每 %d 秒）", sc.IntervalS)
					}
					if sc.Kind == store.ScheduleDaily {
						fmt.Fprintf(&b, "（每天 %s", sc.DailyAt)
						if sc.Weekdays != "" {
							fmt.Fprintf(&b, "，周%s", sc.Weekdays)
						}
						b.WriteString("）")
					}
					if sc.Target != store.ScheduleTargetSelf && sc.Target != "" {
						fmt.Fprintf(&b, " 目标 %s", sc.Target)
					}
					if sc.Mode == store.ScheduleModeAI {
						b.WriteString(" [AI生成]")
					}
					if sc.RecipientPolicy == store.ScheduleRecipientMandatory {
						b.WriteString(" [强制接收]")
					} else if sc.TargetsViewer {
						if sc.DeliveryEnabled {
							b.WriteString(" [接收开启]")
						} else {
							b.WriteString(" [接收关闭]")
						}
					}
					if sc.LastFired != nil {
						fmt.Fprintf(&b, " 最近触发 %s", fmtTime(*sc.LastFired, d.TZ))
					}
					b.WriteString("\n")
				}
				return b.String(), nil
			}),

		tool("set_schedule_subscription", "设置当前用户自己是否接收可选定时通知，不修改共享日程本身。scope=schedule 用于关闭或恢复一条 optional 日程；scope=all 用于关闭或恢复所有当前及未来的 optional 日程。mandatory 日程不受个人退订影响。查询收到哪些日程先用 list_schedules。别人创建的推送不能用 cancel_schedule 全局取消。",
			obj(map[string]any{
				"scope":       enumP("作用范围", "schedule", "all"),
				"schedule_id": p("integer", "scope=schedule 时的稳定日程ID；不知道时可改传 query"),
				"query":       p("string", "scope=schedule 且不知道 ID 时，按标题或内容查找唯一日程"),
				"enabled":     p("boolean", "true=恢复接收，false=停止接收"),
			}, "scope", "enabled"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Scope      string `json:"scope"`
					ScheduleID int64  `json:"schedule_id"`
					Query      string `json:"query"`
					Enabled    bool   `json:"enabled"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				scheduleID := store.SchedulePreferenceAll
				switch args.Scope {
				case "all":
				case "schedule":
					var message string
					var err error
					scheduleID, message, err = resolveIncomingSchedule(ctx, d, u, args.ScheduleID, args.Query)
					if err != nil || message != "" {
						return message, err
					}
				default:
					return "scope 必须是 schedule 或 all。", nil
				}
				if err := d.Store.SetScheduleDeliveryPreference(ctx, u.ID, scheduleID, args.Enabled); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "日程不存在、已结束或并不发送给当前用户。", nil
					}
					if errors.Is(err, store.ErrConflict) {
						return "该日程为强制接收，不能由收件人退订。", nil
					}
					return "", err
				}
				state := "已停止接收"
				if args.Enabled {
					state = "已恢复接收"
				}
				if scheduleID == store.SchedulePreferenceAll {
					return state + "所有可选定时通知；强制接收日程不受影响，且不会修改创建者的共享日程。", nil
				}
				return fmt.Sprintf("%s%s；这不会修改创建者的共享日程。", state, internalRef("日程", scheduleID)), nil
			}),
	}
}

func resolveIncomingSchedule(ctx context.Context, d Deps, u *store.User, scheduleID int64, query string) (int64, string, error) {
	items, err := d.Store.SchedulesVisible(ctx, u.ID, u.IsSuperadmin, store.ScheduleActive, 500)
	if err != nil {
		return 0, "", err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	matches := make([]store.ScheduleView, 0, len(items))
	for _, item := range items {
		if !item.TargetsViewer {
			continue
		}
		if scheduleID > 0 {
			if item.ID == scheduleID {
				return item.ID, "", nil
			}
			continue
		}
		if query == "" {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{item.Title, item.Message, item.SourceKind, item.CreatorName}, "\n"))
		if strings.Contains(haystack, query) {
			matches = append(matches, item)
		}
	}
	if scheduleID > 0 {
		return 0, "日程不存在、已结束或并不发送给当前用户。", nil
	}
	if query == "" {
		return 0, "scope=schedule 时请提供 schedule_id；不知道编号时提供 query。", nil
	}
	switch len(matches) {
	case 0:
		return 0, "没有找到匹配且会发送给当前用户的活跃日程。", nil
	case 1:
		return matches[0].ID, "", nil
	default:
		return 0, "匹配到多条会发送给当前用户的活跃日程，请根据稳定 ID 指定一条：\n" + renderScheduleCandidates(matches, d.TZ), nil
	}
}

type pushScheduleArgs struct {
	Target          string
	Mode            string
	Content         string
	Title           string
	RecipientPolicy string
}

func scheduleOncePushTool(d Deps, u *store.User) ai.Tool {
	return tool("schedule_once_push",
		"设置只发生一次的定向推送；周期性推送使用 schedule_recurring_push。target 省略/self 只发给当前用户；_all 会向每名活跃真人分别投递，只有用户明确要求全体接收时才能使用。recipient_policy 默认 optional；只有用户明确要求不可退订时才设 mandatory，且需要对应 manage_mandatory_schedule 权限。mode=ai 时到点结合原始消息、触发时间和实时数据生成内容；mode=message 原样投递。给他人/全体设置需要对应 send_msg 权限（超管不限）。Telegram 群每日摘要必须使用 set_telegram_group_digest。",
		obj(map[string]any{
			"target":           p("string", "self（默认）| _all（全体成员）| 稳定用户ID"),
			"mode":             enumP("推送模式，可省略采用权限安全的默认值", store.ScheduleModeAI, store.ScheduleModeMessage),
			"content":          p("string", "ai 模式写目标与事实；message 模式写原文"),
			"title":            p("string", "可选，便于日程管理的短标题"),
			"recipient_policy": enumP("接收策略，可省略", store.ScheduleRecipientOptional, store.ScheduleRecipientMandatory),
			"at":               p("string", "唯一一次触发时间，ISO8601；不带时区按公司时区解析"),
		}, "content", "at"),
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Target          string `json:"target"`
				Mode            string `json:"mode"`
				Content         string `json:"content"`
				Title           string `json:"title"`
				RecipientPolicy string `json:"recipient_policy"`
				At              string `json:"at"`
			}
			if err := decode(raw, &args); err != nil {
				return err.Error(), nil
			}
			sc, message, err := preparePushSchedule(ctx, d, u, pushScheduleArgs{
				Target: args.Target, Mode: args.Mode, Content: args.Content, Title: args.Title, RecipientPolicy: args.RecipientPolicy,
			})
			if err != nil || message != "" {
				return message, err
			}
			at, err := parseDeadline(args.At, d.TZ)
			if err != nil {
				return err.Error(), nil
			}
			if at == nil || !at.After(time.Now()) {
				return "at 必须是未来时间。", nil
			}
			sc.Kind = store.ScheduleOnce
			sc.FireAt = at.UTC()
			created, err := d.Store.CreateSchedule(ctx, sc)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("已设置一次性推送（%s）：%s，目标 %s，模式 %s。",
				internalRef("提醒", created.ID), fmtTime(created.FireAt, d.TZ), created.Target, created.Mode), nil
		})
}

func scheduleRecurringPushTool(d Deps, u *store.User) ai.Tool {
	return tool("schedule_recurring_push",
		"设置按日历规则重复发生的定向推送；只发生一次的推送使用 schedule_once_push。target 省略/self 只发给当前用户；_all 会向每名活跃真人分别投递，只有用户明确要求全体接收时才能使用。recipient_policy 默认 optional；只有用户明确要求不可退订时才设 mandatory，且需要对应 manage_mandatory_schedule 权限。mode=ai 时每次结合实时数据生成内容；mode=message 原样投递。给他人/全体设置需要对应 send_msg 权限（超管不限）。Telegram 群每日摘要必须使用 set_telegram_group_digest。",
		obj(map[string]any{
			"target":           p("string", "self（默认）| _all（全体成员）| 稳定用户ID"),
			"mode":             enumP("推送模式，可省略采用权限安全的默认值", store.ScheduleModeAI, store.ScheduleModeMessage),
			"content":          p("string", "ai 模式写目标与事实；message 模式写原文"),
			"title":            p("string", "可选，便于日程管理的短标题"),
			"recipient_policy": enumP("接收策略，可省略", store.ScheduleRecipientOptional, store.ScheduleRecipientMandatory),
			"daily_at":         p("string", "每天触发时刻 HH:MM（公司时区）"),
			"weekdays":         p("string", "星期过滤，如 1,2,3,4,5；1=周一，7=周日；空=每天"),
		}, "content", "daily_at"),
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Target          string `json:"target"`
				Mode            string `json:"mode"`
				Content         string `json:"content"`
				Title           string `json:"title"`
				RecipientPolicy string `json:"recipient_policy"`
				DailyAt         string `json:"daily_at"`
				Weekdays        string `json:"weekdays"`
			}
			if err := decode(raw, &args); err != nil {
				return err.Error(), nil
			}
			sc, message, err := preparePushSchedule(ctx, d, u, pushScheduleArgs{
				Target: args.Target, Mode: args.Mode, Content: args.Content, Title: args.Title, RecipientPolicy: args.RecipientPolicy,
			})
			if err != nil || message != "" {
				return message, err
			}
			dailyAt, err := normalizeDailyAt(args.DailyAt)
			if err != nil {
				return err.Error(), nil
			}
			weekdays, err := normalizeWeekdays(args.Weekdays)
			if err != nil {
				return err.Error(), nil
			}
			sc.Kind = store.ScheduleDaily
			sc.DailyAt = dailyAt
			sc.Weekdays = weekdays
			sc.FireAt = store.NextDailyFire(time.Now(), dailyAt, weekdays, d.TZ)
			created, err := d.Store.CreateSchedule(ctx, sc)
			if err != nil {
				return "", err
			}
			desc := "每天 " + created.DailyAt
			if created.Weekdays != "" {
				desc += "（周" + created.Weekdays + "）"
			}
			return fmt.Sprintf("已设置周期推送（%s）：%s，目标 %s，模式 %s。首次触发 %s。",
				internalRef("提醒", created.ID), desc, created.Target, created.Mode, fmtTime(created.FireAt, d.TZ)), nil
		})
}

func preparePushSchedule(ctx context.Context, d Deps, u *store.User, args pushScheduleArgs) (*store.Schedule, string, error) {
	content := strings.TrimSpace(args.Content)
	if content == "" {
		return nil, "content 不能为空。", nil
	}
	mode := strings.TrimSpace(args.Mode)
	if mode != "" && mode != store.ScheduleModeMessage && mode != store.ScheduleModeAI {
		return nil, "mode 必须是 ai 或 message。", nil
	}
	target, receiver := store.ScheduleTargetSelf, u.ID
	switch selector := strings.TrimSpace(args.Target); selector {
	case "", store.ScheduleTargetSelf:
	case store.TargetAll:
		if !u.IsSuperadmin && !hasActiveAll(ctx, d, u.ID, perm.ActSendMsg) {
			return nil, "给全体设置推送需要 send_msg:_all 权限。", nil
		}
		target = store.ScheduleTargetAll
	default:
		_, id, _, err := parseTarget(selector)
		if err != nil {
			return nil, err.Error(), nil
		}
		targetUser, err := mustUser(ctx, d.Store, id)
		if err != nil {
			return nil, err.Error(), nil
		}
		if targetUser.IsWorker {
			return nil, "AI 员工不接收面向真人的日程推送。", nil
		}
		if id != u.ID {
			if !u.IsSuperadmin {
				grants, err := d.Store.PermsOf(ctx, u.ID)
				if err != nil {
					return nil, "", err
				}
				if !perm.CheckActive(grants, perm.ActSendMsg, id) {
					return nil, "给该成员设置推送需要对其 send_msg 权限。", nil
				}
			}
			target, receiver = strconv.FormatInt(id, 10), id
		}
	}
	if mode == "" {
		mode = store.ScheduleModeAI
		if !u.IsSuperadmin && target != store.ScheduleTargetSelf {
			mode = store.ScheduleModeMessage
		}
	}
	// AI-mode directives execute as the recipient at delivery time. Only a
	// superadmin may create such delegated automation for another identity.
	if mode == store.ScheduleModeAI && !u.IsSuperadmin && target != store.ScheduleTargetSelf {
		return nil, "非超管给他人或全体设置推送时只能使用 message 模式。", nil
	}
	policy, err := normalizeScheduleRecipientPolicy(args.RecipientPolicy)
	if err != nil {
		return nil, err.Error(), nil
	}
	if policy == store.ScheduleRecipientMandatory {
		if target == store.ScheduleTargetSelf {
			return nil, "只发给自己的日程无需设置 mandatory；可直接取消或调整日程。", nil
		}
		allowed, err := canSetMandatorySchedule(ctx, d, u, target, receiver)
		if err != nil {
			return nil, "", err
		}
		if !allowed {
			return nil, "设置强制接收需要对全部目标拥有 manage_mandatory_schedule 权限。", nil
		}
	}
	return &store.Schedule{
		UserID: receiver, Target: target, Mode: mode, Message: content,
		Title: strings.TrimSpace(args.Title), CreatedBy: u.ID, SourceMessageID: sourceMessageID(ctx), RecipientPolicy: policy,
	}, "", nil
}

func normalizeScheduleRecipientPolicy(raw string) (string, error) {
	switch value := strings.ToLower(strings.TrimSpace(raw)); value {
	case "", store.ScheduleRecipientOptional:
		return store.ScheduleRecipientOptional, nil
	case store.ScheduleRecipientMandatory:
		return store.ScheduleRecipientMandatory, nil
	default:
		return "", fmt.Errorf("recipient_policy 必须是 optional 或 mandatory")
	}
}

func canSetMandatorySchedule(ctx context.Context, d Deps, u *store.User, target string, receiverID int64) (bool, error) {
	if u.IsSuperadmin {
		return true, nil
	}
	if target == "" || target == store.ScheduleTargetSelf {
		return false, nil
	}
	grants, err := d.Store.PermsOf(ctx, u.ID)
	if err != nil {
		return false, err
	}
	if target == store.ScheduleTargetAll {
		for _, grant := range grants {
			if grant.Kind == store.KindActive && grant.Action == perm.ActManageMandatorySchedule && grant.Target == store.TargetAll {
				return true, nil
			}
		}
		return false, nil
	}
	return perm.CheckActive(grants, perm.ActManageMandatorySchedule, receiverID), nil
}

func resolveActiveSchedule(ctx context.Context, d Deps, u *store.User, scheduleID int64, query string) (*store.Schedule, string, error) {
	if scheduleID <= 0 {
		if strings.TrimSpace(query) == "" {
			return nil, "请提供 schedule_id；不知道编号时请提供可用于消歧的 query。", nil
		}
		matches, err := findVisibleSchedules(ctx, d, u, query, store.ScheduleActive)
		if err != nil {
			return nil, "", err
		}
		switch len(matches) {
		case 0:
			return nil, "没有找到匹配的活跃定时提醒、计划推送或自动化。", nil
		case 1:
			scheduleID = matches[0].ID
		default:
			return nil, "匹配到多条活跃日程，请根据稳定 ID 指定一条：\n" + renderScheduleCandidates(matches, d.TZ), nil
		}
	}
	sc, err := d.Store.ScheduleByID(ctx, scheduleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "日程不存在或不在你的可见范围。", nil
		}
		return nil, "", err
	}
	if !scheduleVisibleTo(sc, u) {
		return nil, "日程不存在或不在你的可见范围。", nil
	}
	if sc.Status != store.ScheduleActive {
		return nil, inactiveScheduleResult(*sc, d.TZ), nil
	}
	return sc, "", nil
}

func scheduleTimingDescription(sc *store.Schedule, tz *time.Location) string {
	switch sc.Kind {
	case store.ScheduleOnce:
		return "将在 " + fmtTime(sc.FireAt, tz) + " 触发一次"
	case store.ScheduleRepeat:
		return fmt.Sprintf("下次 %s，之后每 %d 秒", fmtTime(sc.FireAt, tz), sc.IntervalS)
	case store.ScheduleDaily:
		desc := "每天 " + sc.DailyAt
		if sc.Weekdays != "" {
			desc += "（周" + sc.Weekdays + "）"
		}
		return desc + "，下次 " + fmtTime(sc.FireAt, tz)
	default:
		return "下次 " + fmtTime(sc.FireAt, tz)
	}
}

func findVisibleSchedules(ctx context.Context, d Deps, u *store.User, query, status string) ([]store.ScheduleView, error) {
	items, err := d.Store.SchedulesVisible(ctx, u.ID, u.IsSuperadmin, status, 500)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return items, nil
	}
	matches := make([]store.ScheduleView, 0, len(items))
	for _, item := range items {
		haystack := strings.ToLower(strings.Join([]string{
			item.Title, item.Message, item.SourceKind, item.SourceKey,
			item.ReceiverName, item.CreatorName,
		}, "\n"))
		if strings.Contains(haystack, query) {
			matches = append(matches, item)
		}
	}
	return matches, nil
}

func renderScheduleCandidates(items []store.ScheduleView, tz *time.Location) string {
	var b strings.Builder
	for _, item := range items {
		label := strings.TrimSpace(item.Title)
		if label == "" {
			label = textfmt.TruncateRunes(strings.TrimSpace(item.Message), 80)
		}
		fmt.Fprintf(&b, "- %s [%s] %s，计划 %s\n", internalRef("日程", item.ID), item.Status, label, fmtTime(item.FireAt, tz))
	}
	return strings.TrimSpace(b.String())
}

func scheduleVisibleTo(sc *store.Schedule, u *store.User) bool {
	return sc != nil && u != nil && (u.IsSuperadmin || sc.UserID == u.ID || sc.CreatedBy == u.ID)
}

func scheduleTargetsUser(sc *store.Schedule, userID int64) bool {
	if sc == nil || userID <= 0 {
		return false
	}
	return sc.Target == store.ScheduleTargetAll || sc.UserID == userID || sc.Target == strconv.FormatInt(userID, 10)
}

func inactiveScheduleResult(sc store.Schedule, tz *time.Location) string {
	ref := internalRef("日程", sc.ID)
	switch sc.Status {
	case store.ScheduleDone:
		when := sc.FireAt
		if sc.LastFired != nil {
			when = *sc.LastFired
		}
		return fmt.Sprintf("%s 已于 %s 执行完成，没有未来执行；未作更改，历史记录继续保留用于审计。", ref, fmtTime(when, tz))
	case store.ScheduleCancelled:
		return fmt.Sprintf("%s 已取消，没有未来执行；未作更改。", ref)
	default:
		return fmt.Sprintf("%s 当前状态为 %s；未作更改。", ref, sc.Status)
	}
}

func normalizeDailyAt(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	hour, minute, ok := strings.Cut(raw, ":")
	if !ok || len(hour) < 1 || len(hour) > 2 || len(minute) != 2 || !asciiDigits(hour) || !asciiDigits(minute) {
		return "", fmt.Errorf("daily_at 格式应为 HH:MM，如 10:00")
	}
	hh, _ := strconv.Atoi(hour)
	mm, _ := strconv.Atoi(minute)
	if hh > 23 || mm > 59 {
		return "", fmt.Errorf("daily_at 格式应为 HH:MM，如 10:00")
	}
	return fmt.Sprintf("%02d:%02d", hh, mm), nil
}

func normalizeWeekdays(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	var seen [8]bool
	for _, part := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > 7 {
			return "", fmt.Errorf("weekdays 格式应为 1 到 7 的逗号列表，如 1,2,3,4,5")
		}
		seen[n] = true
	}
	parts := make([]string, 0, 7)
	for n := 1; n <= 7; n++ {
		if seen[n] {
			parts = append(parts, strconv.Itoa(n))
		}
	}
	return strings.Join(parts, ","), nil
}

func asciiDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func scheduleDisplayMessage(ctx context.Context, d Deps, sc *store.Schedule) string {
	if sc == nil {
		return ""
	}
	if title := strings.TrimSpace(sc.Title); title != "" {
		return title
	}
	if sc.SourceKind != telegramGroupDigestSourceKind {
		return sc.Message
	}
	chatID, err := strconv.ParseInt(sc.SourceKey, 10, 64)
	if err != nil || d.Store == nil {
		return "Telegram 群每日摘要"
	}
	group, err := d.Store.TelegramGroupState(ctx, chatID)
	if err != nil || group == nil {
		return "Telegram 群每日摘要"
	}
	return telegramGroupTitle(*group) + " 每日摘要"
}

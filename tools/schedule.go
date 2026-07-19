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

		tool("cancel_schedule", "取消一个定时提醒或持久自动化。优先传 schedule_id；不知道编号时传 query，工具会在当前可见的活跃日程中按标题、内容和来源查找，只有唯一匹配才取消。超级管理员可取消全局日程。",
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
					matches, err := findVisibleSchedules(ctx, d, u, args.Query)
					if err != nil {
						return "", err
					}
					switch len(matches) {
					case 0:
						return "没有找到匹配的活跃定时提醒或自动化。请先用 list_schedules 查看。", nil
					case 1:
						args.ScheduleID = matches[0].ID
					default:
						return "匹配到多条活跃日程，请根据稳定 ID 指定一条：\n" + renderScheduleCandidates(matches, d.TZ), nil
					}
				}
				if err := d.Store.CancelScheduleVisible(ctx, args.ScheduleID, u.ID, u.IsSuperadmin); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "日程不存在、已结束或不在你的可见范围。", nil
					}
					return "", err
				}
				return fmt.Sprintf("已取消%s。", internalRef("日程", args.ScheduleID)), nil
			}),

		tool("list_schedules", "查看可见的定时提醒和持久自动化；超级管理员查看全局队列。status 默认 active，也可查 done、cancelled 或 all，避免执行后的单次任务从视图消失。返回稳定日程 ID、状态和触发时间，可用于取消或核实；逐次投递结果可用 query_data(source=deliveries) 深挖。",
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
					if sc.LastFired != nil {
						fmt.Fprintf(&b, " 最近触发 %s", fmtTime(*sc.LastFired, d.TZ))
					}
					b.WriteString("\n")
				}
				return b.String(), nil
			}),
	}
}

type pushScheduleArgs struct {
	Target  string
	Mode    string
	Content string
	Title   string
}

func scheduleOncePushTool(d Deps, u *store.User) ai.Tool {
	return tool("schedule_once_push",
		"设置一次性定向推送。只要用户表达的是一个具体日期、明天、某个周几或其他单次发生时间，且没有明确说“每/每天/每周/定期”，就使用本工具。目标可以是自己、某个成员或全体。mode=ai 时到点结合原始消息、触发时间和实时数据生成内容；mode=message 仅在用户要求原文不改写时使用。给他人/全体设置需要对应 send_msg 权限（超管不限）。",
		obj(map[string]any{
			"target":  p("string", "self（默认）| _all（全体成员）| 稳定用户ID"),
			"mode":    enumP("推送模式，可省略采用权限安全的默认值", store.ScheduleModeAI, store.ScheduleModeMessage),
			"content": p("string", "ai 模式写目标与事实；message 模式写原文"),
			"title":   p("string", "可选，便于日程管理的短标题"),
			"at":      p("string", "唯一一次触发时间，ISO8601；不带时区按公司时区解析"),
		}, "content", "at"),
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Target  string `json:"target"`
				Mode    string `json:"mode"`
				Content string `json:"content"`
				Title   string `json:"title"`
				At      string `json:"at"`
			}
			if err := decode(raw, &args); err != nil {
				return err.Error(), nil
			}
			sc, message, err := preparePushSchedule(ctx, d, u, pushScheduleArgs{
				Target: args.Target, Mode: args.Mode, Content: args.Content, Title: args.Title,
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
		"设置明确重复的定向日历推送。只有用户明确表达“每/每天/每周/定期”等重复意图时才使用；单个日期、明天、某个周几必须使用 schedule_once_push。目标可以是自己、某个成员或全体。mode=ai 时每次结合实时数据生成内容；mode=message 原文投递。给他人/全体设置需要对应 send_msg 权限（超管不限）。",
		obj(map[string]any{
			"target":   p("string", "self（默认）| _all（全体成员）| 稳定用户ID"),
			"mode":     enumP("推送模式，可省略采用权限安全的默认值", store.ScheduleModeAI, store.ScheduleModeMessage),
			"content":  p("string", "ai 模式写目标与事实；message 模式写原文"),
			"title":    p("string", "可选，便于日程管理的短标题"),
			"daily_at": p("string", "每天触发时刻 HH:MM（公司时区）"),
			"weekdays": p("string", "星期过滤，如 1,2,3,4,5；1=周一，7=周日；空=每天"),
		}, "content", "daily_at"),
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Target   string `json:"target"`
				Mode     string `json:"mode"`
				Content  string `json:"content"`
				Title    string `json:"title"`
				DailyAt  string `json:"daily_at"`
				Weekdays string `json:"weekdays"`
			}
			if err := decode(raw, &args); err != nil {
				return err.Error(), nil
			}
			sc, message, err := preparePushSchedule(ctx, d, u, pushScheduleArgs{
				Target: args.Target, Mode: args.Mode, Content: args.Content, Title: args.Title,
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
	return &store.Schedule{
		UserID: receiver, Target: target, Mode: mode, Message: content,
		Title: strings.TrimSpace(args.Title), CreatedBy: u.ID, SourceMessageID: sourceMessageID(ctx),
	}, "", nil
}

func findVisibleSchedules(ctx context.Context, d Deps, u *store.User, query string) ([]store.ScheduleView, error) {
	items, err := d.Store.SchedulesVisible(ctx, u.ID, u.IsSuperadmin, store.ScheduleActive, 500)
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
		fmt.Fprintf(&b, "- %s %s，下次 %s\n", internalRef("日程", item.ID), label, fmtTime(item.FireAt, tz))
	}
	return strings.TrimSpace(b.String())
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

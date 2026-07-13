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
		tool("schedule_once", "设置单次原文提醒。时间用 ISO8601（如 2026-07-05T09:00:00+08:00；不带时区按公司时区算）。message 会原样投递；需要结合触发时间、来源上下文或实时状态智能措辞时使用 schedule_push 的 ai 模式。",
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

		tool("schedule_repeating", "设置循环原文提醒。最小间隔 60 秒。message 每次原样投递；需要结合每次触发时间、来源上下文或实时状态智能措辞时使用 schedule_push 的 ai 模式。",
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

		tool("schedule_push",
			"设置定向/周期性智能推送——把运营节奏落成规则：目标可以是自己、某个成员或全体（如上下班问候、例会提醒、周期检查）。"+
				"mode=ai 时 content 写目标和事实，每次触发会把原始用户消息及相关时间作为结构化上下文交给 AI，并由 AI 结合实时数据生成内容；"+
				"mode=message 只用于用户明确要求完全不改写的原文。mode 省略时，自己或超管创建的推送默认 ai；普通用户给他人推送默认 message。"+
				"时间二选一：daily_at（每天 HH:MM，可配 weekdays 限工作日）或 once_at（单次）。"+
				"给他人/全体设置需要对应 send_msg 权限（超管不限）。",
			obj(map[string]any{
				"target":   p("string", "self（默认）| _all（全体成员）| 用户ID"),
				"mode":     p("string", "ai（智能生成）| message（仅用户明确要求原文不改写时使用）；可省略采用安全默认"),
				"content":  p("string", "mode=ai 时写目标与已知事实，不必预先限制 AI 的自然措辞；mode=message 时是原文"),
				"daily_at": p("string", "每天触发时刻 HH:MM（公司时区），与 once_at 二选一"),
				"weekdays": p("string", "限定星期几：如 1,2,3,4,5 表示工作日（1=周一…7=周日），空=每天；仅配合 daily_at"),
				"once_at":  p("string", "单次触发时间 ISO8601，与 daily_at 二选一"),
			}, "content"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Target   string `json:"target"`
					Mode     string `json:"mode"`
					Content  string `json:"content"`
					DailyAt  string `json:"daily_at"`
					Weekdays string `json:"weekdays"`
					OnceAt   string `json:"once_at"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Content) == "" {
					return "content 不能为空。", nil
				}
				mode := strings.TrimSpace(args.Mode)
				if mode != "" && mode != store.ScheduleModeMessage && mode != store.ScheduleModeAI {
					return "mode 必须是 ai 或 message。", nil
				}

				// 目标解析 + send_msg 权限（handler 内目标级校验）。
				target, receiver := store.ScheduleTargetSelf, u.ID
				switch t := strings.TrimSpace(args.Target); t {
				case "", "self":
				case store.TargetAll:
					if !u.IsSuperadmin && !hasActiveAll(ctx, d, u.ID, perm.ActSendMsg) {
						return "给全体设置推送需要 send_msg:_all 权限。", nil
					}
					target = store.ScheduleTargetAll
				default:
					_, id, _, perr := parseTarget(t)
					if perr != nil {
						return perr.Error(), nil
					}
					tu, uerr := mustUser(ctx, d.Store, id)
					if uerr != nil {
						return uerr.Error(), nil
					}
					if tu.IsWorker {
						return "AI 员工不需要问候/提醒推送。", nil
					}
					if id != u.ID {
						if !u.IsSuperadmin {
							grants, gerr := d.Store.PermsOf(ctx, u.ID)
							if gerr != nil {
								return "", gerr
							}
							if !perm.CheckActive(grants, perm.ActSendMsg, id) {
								return "给该成员设置推送需要对其 send_msg 权限。", nil
							}
						}
						target, receiver = t, id
					}
				}
				if mode == "" {
					mode = store.ScheduleModeAI
					if !u.IsSuperadmin && target != store.ScheduleTargetSelf {
						mode = store.ScheduleModeMessage
					}
				}

				// mode=ai 的定向推送触发时会【以目标用户身份】跑一轮带其完整工具集的
				// AI，content 是创建者手写的指令——非超管借此可让更高权限者执行任意
				// 工具（权限放大+注入面）。因此非超管的 ai 模式只允许对自己；
				// 给他人/全体仍可用 message 模式原文投递。
				if mode == store.ScheduleModeAI && !u.IsSuperadmin && target != store.ScheduleTargetSelf {
					return "非超管给他人/全体设置的推送只能用 mode=message（原文投递）：ai 模式会以目标身份执行指令，存在越权风险。", nil
				}

				// 时间：daily_at 与 once_at 二选一。
				hasDaily := strings.TrimSpace(args.DailyAt) != ""
				hasOnce := strings.TrimSpace(args.OnceAt) != ""
				if hasDaily == hasOnce {
					return "daily_at 与 once_at 必须且只能提供一个。", nil
				}
				sc := &store.Schedule{
					UserID: receiver, Target: target, Mode: mode,
					Message: args.Content, CreatedBy: u.ID, SourceMessageID: sourceMessageID(ctx),
				}
				if hasDaily {
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
					sc.FireAt = store.NextDailyFire(time.Now(), sc.DailyAt, sc.Weekdays, d.TZ)
				} else {
					at, err := parseDeadline(args.OnceAt, d.TZ)
					if err != nil {
						return err.Error(), nil
					}
					if at == nil || !at.After(time.Now()) {
						return "once_at 必须是未来时间。", nil
					}
					sc.Kind = store.ScheduleOnce
					sc.FireAt = at.UTC()
				}
				created, err := d.Store.CreateSchedule(ctx, sc)
				if err != nil {
					return "", err
				}
				desc := "单次 " + fmtTime(created.FireAt, d.TZ)
				if created.Kind == store.ScheduleDaily {
					desc = "每天 " + created.DailyAt
					if created.Weekdays != "" {
						desc += "（周" + created.Weekdays + "）"
					}
				}
				return fmt.Sprintf("已设置推送（%s）：%s，目标 %s，模式 %s。首次触发 %s。",
					internalRef("提醒", created.ID), desc, target, mode, fmtTime(created.FireAt, d.TZ)), nil
			}),

		tool("cancel_schedule", "取消一个定时提醒。",
			obj(map[string]any{"schedule_id": p("integer", "提醒ID")}, "schedule_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ScheduleID int64 `json:"schedule_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.CancelSchedule(ctx, args.ScheduleID, u.ID); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "提醒不存在或不属于你。", nil
					}
					return "", err
				}
				return "已取消。", nil
			}),

		tool("list_schedules", "查看我的定时提醒和持久自动化。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				scs, err := d.Store.SchedulesOf(ctx, u.ID)
				if err != nil {
					return "", err
				}
				if len(scs) == 0 {
					return "（无定时提醒或自动化）", nil
				}
				var b strings.Builder
				for _, sc := range scs {
					refLabel := "提醒"
					if strings.TrimSpace(sc.SourceKind) != "" {
						refLabel = "自动化"
					}
					fmt.Fprintf(&b, "- %s [%s] %s 下次 %s", internalRef(refLabel, sc.ID), sc.Kind, scheduleDisplayMessage(ctx, d, sc), fmtTime(sc.FireAt, d.TZ))
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
					b.WriteString("\n")
				}
				return b.String(), nil
			}),
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

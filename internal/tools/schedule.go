package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/store"
)

const minRepeatInterval = 60 // 秒

// scheduleTools 定时提醒。
func scheduleTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("schedule_once", "设置单次定时提醒。时间用 ISO8601（如 2026-07-05T09:00:00+08:00；不带时区按公司时区算）。",
			obj(map[string]any{
				"at":      p("string", "触发时间 ISO8601"),
				"message": p("string", "提醒内容"),
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
					UserID: u.ID, Kind: store.ScheduleOnce, Message: args.Message, FireAt: at.UTC(),
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已设置提醒 #%d：%s 提醒你「%s」。", sc.ID, fmtTime(sc.FireAt, d.TZ), args.Message), nil
			}),

		tool("schedule_repeating", "设置循环定时提醒。最小间隔 60 秒。",
			obj(map[string]any{
				"first_at":         p("string", "首次触发时间 ISO8601"),
				"interval_seconds": p("integer", "间隔秒数（≥60）"),
				"message":          p("string", "提醒内容"),
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
					FireAt: at.UTC(), IntervalS: args.IntervalSeconds,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已设置循环提醒 #%d：从 %s 起每 %d 秒。", sc.ID, fmtTime(sc.FireAt, d.TZ), sc.IntervalS), nil
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

		tool("list_schedules", "查看我的定时提醒。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				scs, err := d.Store.SchedulesOf(ctx, u.ID)
				if err != nil {
					return "", err
				}
				if len(scs) == 0 {
					return "（无定时提醒）", nil
				}
				var b strings.Builder
				for _, sc := range scs {
					fmt.Fprintf(&b, "- #%d [%s] %s 下次 %s", sc.ID, sc.Kind, sc.Message, fmtTime(sc.FireAt, d.TZ))
					if sc.Kind == store.ScheduleRepeat {
						fmt.Fprintf(&b, "（每 %d 秒）", sc.IntervalS)
					}
					b.WriteString("\n")
				}
				return b.String(), nil
			}),
	}
}

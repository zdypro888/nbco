// Package sched 是 DB 驱动的调度器：定时提醒投递 + 每日待办汇总。
// 状态全在库里（schedules 表 + kv_state），进程随时可重启。
package sched

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zdypro888/nbco/internal/notify"
	"github.com/zdypro888/nbco/internal/store"
)

const (
	pollInterval    = 30 * time.Second
	kvDailySummary  = "daily_summary_last_day"
	summaryMaxTasks = 20
)

// Scheduler 调度器。
type Scheduler struct {
	store     *store.Store
	notifier  notify.Notifier
	tz        *time.Location
	dailyHour int // -1 关闭
}

// New 创建调度器。
func New(s *store.Store, n notify.Notifier, tz *time.Location, dailyHour int) *Scheduler {
	return &Scheduler{store: s, notifier: n, tz: tz, dailyHour: dailyHour}
}

// Run 阻塞运行直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	due, err := s.store.DueSchedules(ctx, now)
	if err != nil {
		slog.Error("取到期提醒失败", "err", err)
	}
	for _, sc := range due {
		if err := s.notifier.Send(ctx, sc.UserID, "⏰ 提醒："+sc.Message); err != nil {
			slog.Warn("提醒投递失败", "schedule", sc.ID, "user", sc.UserID, "err", err)
		}
	}
	s.maybeDailySummary(ctx)
}

// maybeDailySummary 每天在配置小时给每个有待办的用户推送任务清单。
// 用 kv_state 记录最后发送日，重启不重发。
func (s *Scheduler) maybeDailySummary(ctx context.Context) {
	if s.dailyHour < 0 {
		return
	}
	local := time.Now().In(s.tz)
	if local.Hour() != s.dailyHour {
		return
	}
	today := local.Format("2006-01-02")
	last, err := s.store.GetKV(ctx, kvDailySummary)
	if err != nil {
		slog.Error("读每日汇总状态失败", "err", err)
		return
	}
	if last == today {
		return
	}
	// 先写状态再发送：宁可漏发一天，不可无限重发。
	if err := s.store.SetKV(ctx, kvDailySummary, today); err != nil {
		slog.Error("写每日汇总状态失败", "err", err)
		return
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		slog.Error("每日汇总取用户失败", "err", err)
		return
	}
	for _, u := range users {
		if u.Status != store.UserActive {
			continue
		}
		tasks, err := s.store.TasksOfAssignee(ctx, u.ID, true)
		if err != nil {
			slog.Warn("每日汇总取任务失败", "user", u.ID, "err", err)
			continue
		}
		if len(tasks) == 0 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "☀️ 早上好，你今天有 %d 个待办：\n", len(tasks))
		for i, t := range tasks {
			if i >= summaryMaxTasks {
				fmt.Fprintf(&b, "…等共 %d 个\n", len(tasks))
				break
			}
			fmt.Fprintf(&b, "- #%d [%s] %s", t.ID, t.Status, t.Title)
			if t.Deadline != nil {
				fmt.Fprintf(&b, "（截止 %s）", t.Deadline.In(s.tz).Format("01-02 15:04"))
			}
			b.WriteByte('\n')
		}
		if err := s.notifier.Send(ctx, u.ID, b.String()); err != nil {
			slog.Warn("每日汇总投递失败", "user", u.ID, "err", err)
		}
	}
}

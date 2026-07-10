package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 定时任务种类与状态。
const (
	ScheduleOnce   = "once"
	ScheduleRepeat = "repeat"
	ScheduleDaily  = "daily" // 每天 HH:MM（可选工作日过滤），时刻存 DailyAt

	ScheduleActive    = "active"
	ScheduleDone      = "done"
	ScheduleCancelled = "cancelled"

	// 投递模式。
	ScheduleModeMessage = "message" // 原文投递
	ScheduleModeAI      = "ai"      // Message 是指令：到点给目标用户跑一轮 AI，推送其产出

	// 目标。
	ScheduleTargetSelf = "self"
	ScheduleTargetAll  = "_all"

	scheduleDeliveryLease = 10 * time.Minute
	scheduleClaimBatch    = 128
)

// Schedule 一条定时任务。FireAt 是下次触发时间（UTC）。
// Target/Mode/DailyAt/Weekdays 让「作息问候」这类运营节奏完全由数据表达：
// 代码只有通用机制，具体政策（几点、对谁、说什么）由 AI 按对话动态落库。
type Schedule struct {
	ID        int64
	UserID    int64 // 目标为单人时=接收者；target=_all 时仅作创建者记录
	Kind      string
	Message   string // mode=message: 消息原文；mode=ai: 给引擎的指令
	FireAt    time.Time
	IntervalS int64
	Status    string
	LastFired *time.Time
	CreatedAt time.Time
	Target    string // self | _all | <用户ID>
	Mode      string // message | ai
	DailyAt   string // "HH:MM"（公司时区），kind=daily 用
	Weekdays  string // "1,2,3,4,5"（1=周一…7=周日），空=每天
	CreatedBy int64
	// DeliveryClaimedAt 标识 DueSchedules 当前认领租约；ack 必须携带同一值。
	DeliveryClaimedAt *time.Time
}

type ScheduleView struct {
	Schedule
	ReceiverName string
	CreatorName  string
}

const scheduleCols = `id, user_id, kind, message, fire_at, interval_s, status, last_fired, created_at, target, mode, daily_at, weekdays, created_by, delivery_claimed_at`

func scanSchedule(row interface{ Scan(...any) error }) (*Schedule, error) {
	var sc Schedule
	if err := row.Scan(&sc.ID, &sc.UserID, &sc.Kind, &sc.Message, &sc.FireAt,
		&sc.IntervalS, &sc.Status, &sc.LastFired, &sc.CreatedAt,
		&sc.Target, &sc.Mode, &sc.DailyAt, &sc.Weekdays, &sc.CreatedBy, &sc.DeliveryClaimedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &sc, nil
}

// CreateSchedule 建定时任务（Target/Mode 空值归一为 self/message）。
func (s *Store) CreateSchedule(ctx context.Context, sc *Schedule) (*Schedule, error) {
	return scanSchedule(s.pool.QueryRow(ctx,
		`INSERT INTO schedules (user_id, kind, message, fire_at, interval_s, target, mode, daily_at, weekdays, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+scheduleCols,
		sc.UserID, sc.Kind, sc.Message, sc.FireAt, sc.IntervalS,
		nonEmpty(sc.Target, ScheduleTargetSelf), nonEmpty(sc.Mode, ScheduleModeMessage),
		sc.DailyAt, sc.Weekdays, sc.CreatedBy))
}

// SchedulesOf 某用户可见的活跃定时任务：给我的 + 我创建的（含定向给他人的）。
func (s *Store) SchedulesOf(ctx context.Context, userID int64) ([]*Schedule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+scheduleCols+` FROM schedules
		 WHERE (user_id = $1 OR created_by = $1) AND status = 'active' ORDER BY fire_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scs []*Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		scs = append(scs, sc)
	}
	return scs, rows.Err()
}

// SchedulesVisible 返回定时/自动化队列。superadmin 可看全局；普通用户只能看
// “发给我/我创建”的条目。status=active 默认只看活跃，all 包含 cancelled/done。
func (s *Store) SchedulesVisible(ctx context.Context, userID int64, superadmin bool, status string, limit int) ([]ScheduleView, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	status = strings.TrimSpace(status)
	where := []string{"true"}
	args := []any{limit}
	if !superadmin {
		args = append(args, userID)
		where = append(where, fmt.Sprintf("(s.user_id = $%d OR s.created_by = $%d)", len(args), len(args)))
	}
	switch status {
	case "", ScheduleActive:
		args = append(args, ScheduleActive)
		where = append(where, fmt.Sprintf("s.status = $%d", len(args)))
	case "all":
	default:
		args = append(args, status)
		where = append(where, fmt.Sprintf("s.status = $%d", len(args)))
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+scheduleColsWithAlias("s")+`, coalesce(u.name, ''), coalesce(cu.name, '')
		   FROM schedules s
		   LEFT JOIN users u ON u.id = s.user_id
		   LEFT JOIN users cu ON cu.id = s.created_by
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY
		    CASE s.status WHEN 'active' THEN 0 WHEN 'done' THEN 1 ELSE 2 END,
		    s.fire_at ASC,
		    s.id DESC
		  LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduleView{}
	for rows.Next() {
		var v ScheduleView
		if err := rows.Scan(&v.ID, &v.UserID, &v.Kind, &v.Message, &v.FireAt,
			&v.IntervalS, &v.Status, &v.LastFired, &v.CreatedAt,
			&v.Target, &v.Mode, &v.DailyAt, &v.Weekdays, &v.CreatedBy, &v.DeliveryClaimedAt,
			&v.ReceiverName, &v.CreatorName); err != nil {
			return nil, wrapErr(err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scheduleColsWithAlias(alias string) string {
	cols := strings.Split(scheduleCols, ", ")
	for i := range cols {
		cols[i] = alias + "." + cols[i]
	}
	return strings.Join(cols, ", ")
}

// CancelSchedule 取消（接收者或创建者都可取消）。
func (s *Store) CancelSchedule(ctx context.Context, id, userID int64) error {
	return s.execOne(ctx,
		`UPDATE schedules SET status = 'cancelled'
		 WHERE id = $1 AND (user_id = $2 OR created_by = $2) AND status = 'active'`, id, userID)
}

// DueSchedules 原子认领到期任务。这里只写短租约，不推进 fire_at/状态；
// 调度器投递成功后调用 MarkScheduleDelivered ack。进程崩溃或发送失败时，
// 租约过期后可重试，避免提醒被提前标记为已发送而丢失。
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]*Schedule, error) {
	stale := now.Add(-scheduleDeliveryLease)
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		   SELECT id FROM schedules
		    WHERE status = 'active' AND fire_at <= $1
		      AND (delivery_claimed_at IS NULL OR delivery_claimed_at <= $2)
		    ORDER BY fire_at, id
		    LIMIT $3
		    FOR UPDATE SKIP LOCKED
		 )
		 UPDATE schedules s SET delivery_claimed_at = $1
		 FROM due WHERE s.id = due.id
		 RETURNING `+scheduleColsWithAlias("s"), now, stale, scheduleClaimBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scs []*Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		scs = append(scs, sc)
	}
	return scs, rows.Err()
}

// ReleaseScheduleClaim 释放尚未开始执行的认领，让下一轮调度可立即重试。
func (s *Store) ReleaseScheduleClaim(ctx context.Context, id int64, claimAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE schedules SET delivery_claimed_at = NULL
		 WHERE id = $1 AND delivery_claimed_at = $2`, id, claimAt)
}

// MarkScheduleDelivered ack 一次成功投递并推进下一次触发时间。
func (s *Store) MarkScheduleDelivered(ctx context.Context, id int64, claimAt, firedAt time.Time, nextFireAt *time.Time, done bool) error {
	status := ScheduleActive
	if done {
		status = ScheduleDone
	}
	return s.execOne(ctx,
		`UPDATE schedules SET
		   last_fired = $2,
		   status = $3,
		   fire_at = COALESCE($4, fire_at),
		   delivery_claimed_at = NULL
		 WHERE id = $1 AND delivery_claimed_at = $5`, id, firedAt, status, nextFireAt, claimAt)
}

// UpdateScheduleFireAt 修正下次触发时间（daily 的工作日跳过/时区校正）。
func (s *Store) UpdateScheduleFireAt(ctx context.Context, id int64, fireAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE schedules SET fire_at = $2 WHERE id = $1 AND status = 'active'`, id, fireAt)
}

// NextDailyFire 计算 after 之后、时刻为 dailyAt（HH:MM，tz 时区）、且落在
// weekdays 过滤内的最近触发时间（UTC）。dailyAt 非法或 weekdays 全非法时
// 兜底 after+24h。纯函数，调度器与工具层共用。
func NextDailyFire(after time.Time, dailyAt, weekdays string, tz *time.Location) time.Time {
	var hh, mm int
	if _, err := fmt.Sscanf(dailyAt, "%d:%d", &hh, &mm); err != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return after.Add(24 * time.Hour).UTC()
	}
	local := after.In(tz)
	cand := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, tz)
	for i := 0; !cand.After(local) || !WeekdayAllowed(cand.Weekday(), weekdays); i++ {
		if i > 8 { // weekdays 全非法（如 "8,9"）：兜底，不死循环
			return after.Add(24 * time.Hour).UTC()
		}
		cand = cand.AddDate(0, 0, 1)
	}
	return cand.UTC()
}

// WeekdayAllowed weekdays 形如 "1,2,3,4,5"（1=周一…7=周日），空=每天。
func WeekdayAllowed(wd time.Weekday, weekdays string) bool {
	if strings.TrimSpace(weekdays) == "" {
		return true
	}
	n := int(wd)
	if n == 0 {
		n = 7 // 周日
	}
	for _, part := range strings.Split(weekdays, ",") {
		if strings.TrimSpace(part) == fmt.Sprint(n) {
			return true
		}
	}
	return false
}

// NextFireAt 全表最近一次触发时间（无活跃任务返回 ErrNotFound）。
func (s *Store) NextFireAt(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT fire_at FROM schedules WHERE status = 'active' ORDER BY fire_at LIMIT 1`).Scan(&t)
	return t, wrapErr(err)
}

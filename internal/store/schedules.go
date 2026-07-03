package store

import (
	"context"
	"time"
)

// 定时任务种类与状态。
const (
	ScheduleOnce   = "once"
	ScheduleRepeat = "repeat"

	ScheduleActive    = "active"
	ScheduleDone      = "done"
	ScheduleCancelled = "cancelled"
)

// Schedule 一条定时提醒。FireAt 是下次触发时间（UTC）。
type Schedule struct {
	ID        int64
	UserID    int64
	Kind      string
	Message   string
	FireAt    time.Time
	IntervalS int64
	Status    string
	LastFired *time.Time
	CreatedAt time.Time
}

const scheduleCols = `id, user_id, kind, message, fire_at, interval_s, status, last_fired, created_at`

func scanSchedule(row interface{ Scan(...any) error }) (*Schedule, error) {
	var sc Schedule
	if err := row.Scan(&sc.ID, &sc.UserID, &sc.Kind, &sc.Message, &sc.FireAt,
		&sc.IntervalS, &sc.Status, &sc.LastFired, &sc.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &sc, nil
}

// CreateSchedule 建定时任务。
func (s *Store) CreateSchedule(ctx context.Context, sc *Schedule) (*Schedule, error) {
	return scanSchedule(s.pool.QueryRow(ctx,
		`INSERT INTO schedules (user_id, kind, message, fire_at, interval_s)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+scheduleCols,
		sc.UserID, sc.Kind, sc.Message, sc.FireAt, sc.IntervalS))
}

// SchedulesOf 某用户的定时任务（活跃的）。
func (s *Store) SchedulesOf(ctx context.Context, userID int64) ([]*Schedule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+scheduleCols+` FROM schedules WHERE user_id = $1 AND status = 'active' ORDER BY fire_at`, userID)
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

// CancelSchedule 取消（只有本人的才可取消，owner 校验在调用方）。
func (s *Store) CancelSchedule(ctx context.Context, id, userID int64) error {
	return s.execOne(ctx,
		`UPDATE schedules SET status = 'cancelled' WHERE id = $1 AND user_id = $2 AND status = 'active'`, id, userID)
}

// DueSchedules 取出到期任务并原子推进状态：
// once → done；repeat → fire_at 前滚一个间隔。返回本次应触发的任务。
// 用单条 UPDATE...RETURNING 保证多次调用/多实例不重复触发。
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]*Schedule, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE schedules SET
		   last_fired = $1,
		   status = CASE WHEN kind = 'once' THEN 'done' ELSE status END,
		   fire_at = CASE WHEN kind = 'repeat' THEN fire_at + make_interval(secs => interval_s) ELSE fire_at END
		 WHERE status = 'active' AND fire_at <= $1
		 RETURNING `+scheduleCols, now)
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

// NextFireAt 全表最近一次触发时间（无活跃任务返回 ErrNotFound）。
func (s *Store) NextFireAt(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT fire_at FROM schedules WHERE status = 'active' ORDER BY fire_at LIMIT 1`).Scan(&t)
	return t, wrapErr(err)
}

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 事件处理结果（events 表 outcome 字段取值）。
const (
	EventOutcomeHandled    = "handled"     // AI 产出通知并投递成功
	EventOutcomeSkipped    = "skipped"     // AI 判定不值得打扰，静默
	EventOutcomeFallback   = "fallback"    // AI 不可用/失败/空答复，降级原文推送
	EventOutcomeDropped    = "dropped"     // 决策人不可达（不存在/非活跃），事件丢弃
	EventOutcomeSendFailed = "send_failed" // AI 产出但通知投递失败

	// EventDeliveryModeGenerating is an internal durable boundary. Once set,
	// a reclaimed event must not rerun the AI turn because that turn may already
	// have executed side-effecting tools before the process was interrupted.
	EventDeliveryModeGenerating = "generating"
)

// eventReplyMax 审计记录里 reply 摘要的最大字符数（rune）。
const (
	eventReplyMax    = 12000
	eventClaimLease  = 5 * time.Minute
	eventMaxAttempts = 6
)

type Event struct {
	ID           int64
	Kind         string
	DeciderID    int64
	Detail       string
	Outcome      string
	Reply        string
	Status       string
	Attempts     int
	AvailableAt  time.Time
	ClaimedAt    *time.Time
	DeliveryMode string
	LastError    string
	CreatedAt    time.Time
	HandledAt    *time.Time
}

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var e Event
	if err := row.Scan(&e.ID, &e.Kind, &e.DeciderID, &e.Detail, &e.Outcome, &e.Reply,
		&e.Status, &e.Attempts, &e.AvailableAt, &e.ClaimedAt, &e.DeliveryMode,
		&e.LastError, &e.CreatedAt, &e.HandledAt); err != nil {
		return nil, wrapErr(err)
	}
	return &e, nil
}

// RecordEvent 落一条系统事件处理记录（审计/可观测）。在事件处理完成后调用一次，
// outcome 见 EventOutcome* 常量。失败不阻断业务（调用方记日志）。
func (s *Store) RecordEvent(ctx context.Context, kind string, deciderID int64, detail, outcome, reply string) error {
	return s.execOne(ctx,
		`INSERT INTO events (kind, decider_id, detail, outcome, reply, status, handled_at)
		 VALUES ($1,$2,$3,$4,$5,'done',now())`,
		kind, deciderID, detail, outcome, truncateRunes(reply, eventReplyMax))
}

// EnqueueEvent 先持久化再返回。事务咨询锁把同一事件的五分钟去重变成跨实例原子操作。
func (s *Store) EnqueueEvent(ctx context.Context, kind string, deciderID int64, detail string, dedupeWindow time.Duration) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	key := fmt.Sprintf("%s|%d|%s", kind, deciderID, detail)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return 0, false, err
	}
	var existing int64
	err = tx.QueryRow(ctx,
		`SELECT id FROM events WHERE kind=$1 AND decider_id=$2 AND detail=$3 AND created_at > now()-$4::interval
		 ORDER BY id DESC LIMIT 1`, kind, deciderID, detail, fmt.Sprintf("%f seconds", dedupeWindow.Seconds())).Scan(&existing)
	if err == nil {
		return existing, false, tx.Commit(ctx)
	}
	if wrapErr(err) != ErrNotFound {
		return 0, false, err
	}
	var id int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO events (kind, decider_id, detail, status, handled_at)
		 VALUES ($1,$2,$3,'pending',NULL) RETURNING id`, kind, deciderID, detail).Scan(&id); err != nil {
		return 0, false, wrapErr(err)
	}
	return id, true, tx.Commit(ctx)
}

func (s *Store) DueEvents(ctx context.Context, now time.Time, limit int) ([]*Event, error) {
	if limit <= 0 || limit > 100 {
		limit = 32
	}
	stale := now.Add(-eventClaimLease)
	if _, err := s.pool.Exec(ctx,
		`UPDATE events SET status='failed', outcome=$4, claimed_at=NULL, handled_at=now(),
		        last_error=CASE WHEN last_error='' THEN 'retry budget exhausted after interrupted claim' ELSE last_error END
		  WHERE attempts >= $1 AND ((status='pending' AND available_at <= $2)
		    OR (status='processing' AND claimed_at <= $3))`,
		eventMaxAttempts, now, stale, EventOutcomeSendFailed); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		   SELECT id FROM events
		    WHERE attempts < $3 AND available_at <= $1
		      AND (status='pending' OR (status='processing' AND claimed_at <= $2))
		    ORDER BY available_at, id LIMIT $4 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE events e SET status='processing', claimed_at=$1, attempts=attempts+1
		 FROM due WHERE e.id=due.id RETURNING `+eventColsWithAlias("e"), now, stale, eventMaxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func eventColsWithAlias(alias string) string {
	cols := []string{"id", "kind", "decider_id", "detail", "outcome", "reply", "status", "attempts", "available_at", "claimed_at", "delivery_mode", "last_error", "created_at", "handled_at"}
	for i := range cols {
		cols[i] = alias + "." + cols[i]
	}
	return strings.Join(cols, ", ")
}

func (s *Store) BeginEventDecision(ctx context.Context, id int64, claimAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE events SET delivery_mode=$3, last_error=''
		 WHERE id=$1 AND status='processing' AND claimed_at=$2 AND reply=''`,
		id, claimAt, EventDeliveryModeGenerating)
}

func (s *Store) PrepareEventDelivery(ctx context.Context, id int64, claimAt time.Time, mode, reply string) error {
	return s.execOne(ctx,
		`UPDATE events SET delivery_mode=$3, reply=$4, last_error=''
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, mode, truncateRunes(reply, eventReplyMax))
}

func (s *Store) CompleteEvent(ctx context.Context, id int64, claimAt time.Time, outcome string) error {
	return s.execOne(ctx,
		`UPDATE events SET status='done', outcome=$3, claimed_at=NULL, handled_at=now(), last_error=''
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, outcome)
}

func (s *Store) RetryEvent(ctx context.Context, id int64, claimAt time.Time, attempts int, cause string) error {
	cause = truncateRunes(cause, 500)
	if attempts >= eventMaxAttempts {
		return s.execOne(ctx,
			`UPDATE events SET status='failed', outcome=$3, claimed_at=NULL, handled_at=now(), last_error=$4
			 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, EventOutcomeSendFailed, cause)
	}
	delay := time.Duration(1<<min(attempts, 6)) * time.Minute
	return s.execOne(ctx,
		`UPDATE events SET status='pending', claimed_at=NULL, available_at=now()+$3::interval, last_error=$4
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, fmt.Sprintf("%d seconds", int(delay.Seconds())), cause)
}

// truncateRunes 按 rune 数截断（不破坏 UTF-8），超出加省略号。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

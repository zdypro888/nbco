package store

import (
	"context"
	"database/sql"
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

	// 领域自动化来源。调度器仍保持通用，领域入口用稳定来源标识实现幂等配置。
	ScheduleSourceTelegramGroupDigest = "telegram_group_digest"

	scheduleDeliveryLease        = 10 * time.Minute
	scheduleClaimBatch           = 128
	scheduleRecipientLease       = 6 * time.Minute
	scheduleRecipientBatch       = 128
	scheduleRecipientMaxAttempts = 5
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
	// SourceKind/SourceKey 把领域自动化稳定绑定到一条日程。普通提醒为空；
	// 领域工具可幂等更新自己的日程，不靠标题或消息文本查找。
	SourceKind string
	SourceKey  string
	// Title 是面向用户的名称；Message 可继续保存机器执行指令，不需要泄露到界面。
	Title string
	// SourceMessageID points to the user message that created or last updated
	// this schedule. Scheduled AI can recover the original wording and timestamp
	// instead of reinterpreting relative dates from the delivery day.
	SourceMessageID *int64
	// DeliveryClaimedAt 标识 DueSchedules 当前认领租约；ack 必须携带同一值。
	DeliveryClaimedAt *time.Time
}

type ScheduleView struct {
	Schedule
	ReceiverName string
	CreatorName  string
}

// ScheduleDelivery 是一次日程触发对一个接收人的独立投递。日程本身只负责
// 生成 occurrence；每个接收人单独 claim/retry，避免部分失败导致整批重发。
type ScheduleDelivery struct {
	ID           int64
	ScheduleID   int64
	OccurrenceAt time.Time
	UserID       int64
	Mode         string
	Message      string
	Title        string
	ResultText   string
	Status       string
	Attempts     int
	AvailableAt  time.Time
	ClaimedAt    *time.Time
	DeliveredAt  *time.Time
	LastError    string
	CreatedAt    time.Time
}

const scheduleDeliveryCols = `id, schedule_id, occurrence_at, user_id, mode, message, title, result_text, status, attempts, available_at, claimed_at, delivered_at, last_error, created_at`

func scanScheduleDelivery(row interface{ Scan(...any) error }) (*ScheduleDelivery, error) {
	var d ScheduleDelivery
	if err := row.Scan(&d.ID, &d.ScheduleID, &d.OccurrenceAt, &d.UserID, &d.Mode, &d.Message,
		&d.Title, &d.ResultText, &d.Status, &d.Attempts, &d.AvailableAt, &d.ClaimedAt, &d.DeliveredAt,
		&d.LastError, &d.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &d, nil
}

const scheduleCols = `id, user_id, kind, message, fire_at, interval_s, status, last_fired, created_at, target, mode, daily_at, weekdays, created_by, delivery_claimed_at, source_kind, source_key, title, source_message_id`

func scanSchedule(row interface{ Scan(...any) error }) (*Schedule, error) {
	var sc Schedule
	var sourceMessageID sql.NullInt64
	if err := row.Scan(&sc.ID, &sc.UserID, &sc.Kind, &sc.Message, &sc.FireAt,
		&sc.IntervalS, &sc.Status, &sc.LastFired, &sc.CreatedAt,
		&sc.Target, &sc.Mode, &sc.DailyAt, &sc.Weekdays, &sc.CreatedBy, &sc.DeliveryClaimedAt,
		&sc.SourceKind, &sc.SourceKey, &sc.Title, &sourceMessageID); err != nil {
		return nil, wrapErr(err)
	}
	if sourceMessageID.Valid {
		id := sourceMessageID.Int64
		sc.SourceMessageID = &id
	}
	return &sc, nil
}

// CreateSchedule 建定时任务（Target/Mode 空值归一为 self/message）。
func (s *Store) CreateSchedule(ctx context.Context, sc *Schedule) (*Schedule, error) {
	return scanSchedule(s.pool.QueryRow(ctx,
		`INSERT INTO schedules (user_id, kind, message, fire_at, interval_s, target, mode, daily_at, weekdays, created_by, source_kind, source_key, title, source_message_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING `+scheduleCols,
		sc.UserID, sc.Kind, sc.Message, sc.FireAt, sc.IntervalS,
		nonEmpty(sc.Target, ScheduleTargetSelf), nonEmpty(sc.Mode, ScheduleModeMessage),
		sc.DailyAt, sc.Weekdays, sc.CreatedBy, strings.TrimSpace(sc.SourceKind), strings.TrimSpace(sc.SourceKey), strings.TrimSpace(sc.Title), sc.SourceMessageID))
}

// UpsertAutomationSchedule 幂等创建或更新一个领域自动化日程。唯一身份是
// (created_by, source_kind, source_key)，普通提醒不设置 source，不受影响。
func (s *Store) UpsertAutomationSchedule(ctx context.Context, sc *Schedule) (*Schedule, error) {
	if sc == nil || strings.TrimSpace(sc.SourceKind) == "" || strings.TrimSpace(sc.SourceKey) == "" {
		return nil, fmt.Errorf("automation schedule requires source_kind and source_key")
	}
	return scanSchedule(s.pool.QueryRow(ctx,
		`INSERT INTO schedules
		   (user_id, kind, message, fire_at, interval_s, target, mode, daily_at, weekdays, created_by, source_kind, source_key, title, source_message_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 ON CONFLICT (created_by, source_kind, source_key)
		 WHERE status = 'active' AND source_kind <> '' AND source_key <> ''
		 DO UPDATE SET
		   user_id = EXCLUDED.user_id,
		   kind = EXCLUDED.kind,
		   message = EXCLUDED.message,
		   fire_at = EXCLUDED.fire_at,
		   interval_s = EXCLUDED.interval_s,
		   target = EXCLUDED.target,
		   mode = EXCLUDED.mode,
		   daily_at = EXCLUDED.daily_at,
		   weekdays = EXCLUDED.weekdays,
		   title = EXCLUDED.title,
		   source_message_id = COALESCE(EXCLUDED.source_message_id, schedules.source_message_id),
		   last_fired = NULL,
		   delivery_claimed_at = NULL,
		   updated_at = now()
		 RETURNING `+scheduleCols,
		sc.UserID, sc.Kind, sc.Message, sc.FireAt, sc.IntervalS,
		nonEmpty(sc.Target, ScheduleTargetSelf), nonEmpty(sc.Mode, ScheduleModeMessage),
		sc.DailyAt, sc.Weekdays, sc.CreatedBy, strings.TrimSpace(sc.SourceKind), strings.TrimSpace(sc.SourceKey), strings.TrimSpace(sc.Title), sc.SourceMessageID))
}

func (s *Store) ScheduleByID(ctx context.Context, id int64) (*Schedule, error) {
	return scanSchedule(s.pool.QueryRow(ctx, `SELECT `+scheduleCols+` FROM schedules WHERE id = $1`, id))
}

func (s *Store) AutomationSchedule(ctx context.Context, createdBy int64, sourceKind, sourceKey string) (*Schedule, error) {
	return scanSchedule(s.pool.QueryRow(ctx,
		`SELECT `+scheduleCols+` FROM schedules
		 WHERE created_by = $1 AND source_kind = $2 AND source_key = $3 AND status = 'active'`,
		createdBy, strings.TrimSpace(sourceKind), strings.TrimSpace(sourceKey)))
}

// HasActiveAutomationSchedule 判断任一所有者是否启用了指定领域自动化。采集链路
// 用它保留自动化需要的事实，而不依赖某个用户的界面设置。
func (s *Store) HasActiveAutomationSchedule(ctx context.Context, sourceKind, sourceKey string) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM schedules
		    WHERE source_kind = $1 AND source_key = $2 AND status = 'active'
		 )`, strings.TrimSpace(sourceKind), strings.TrimSpace(sourceKey)).Scan(&active)
	return active, err
}

func (s *Store) CancelAutomationSchedule(ctx context.Context, createdBy int64, sourceKind, sourceKey string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx,
		`UPDATE schedules SET status = 'cancelled', delivery_claimed_at = NULL, updated_at = now()
		 WHERE created_by = $1 AND source_kind = $2 AND source_key = $3 AND status = 'active'
		 RETURNING id`, createdBy, strings.TrimSpace(sourceKind), strings.TrimSpace(sourceKey)).Scan(&id); err != nil {
		return wrapErr(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE schedule_deliveries SET status = 'cancelled', claimed_at = NULL WHERE schedule_id = $1 AND status IN ('pending','processing')`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
		    CASE WHEN s.status = 'active' THEN s.fire_at END ASC,
		    CASE WHEN s.status <> 'active' THEN COALESCE(s.last_fired, s.updated_at, s.fire_at) END DESC,
		    s.id DESC
		  LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduleView{}
	for rows.Next() {
		var v ScheduleView
		var sourceMessageID sql.NullInt64
		if err := rows.Scan(&v.ID, &v.UserID, &v.Kind, &v.Message, &v.FireAt,
			&v.IntervalS, &v.Status, &v.LastFired, &v.CreatedAt,
			&v.Target, &v.Mode, &v.DailyAt, &v.Weekdays, &v.CreatedBy, &v.DeliveryClaimedAt,
			&v.SourceKind, &v.SourceKey, &v.Title, &sourceMessageID,
			&v.ReceiverName, &v.CreatorName); err != nil {
			return nil, wrapErr(err)
		}
		if sourceMessageID.Valid {
			id := sourceMessageID.Int64
			v.SourceMessageID = &id
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
	return s.CancelScheduleVisible(ctx, id, userID, false)
}

// CancelScheduleVisible cancels an active schedule within the caller's
// visibility boundary. Superadmins may operate the global schedule queue;
// ordinary users remain limited to schedules they receive or created.
func (s *Store) CancelScheduleVisible(ctx context.Context, id, userID int64, superadmin bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx,
		`UPDATE schedules SET status = 'cancelled', delivery_claimed_at = NULL, updated_at = now()
		 WHERE id = $1 AND ($3 OR user_id = $2 OR created_by = $2) AND status = 'active'`, id, userID, superadmin)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE schedule_deliveries SET status = 'cancelled', claimed_at = NULL WHERE schedule_id = $1 AND status IN ('pending','processing')`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
		   delivery_claimed_at = NULL,
		   updated_at = now()
		 WHERE id = $1 AND delivery_claimed_at = $5`, id, firedAt, status, nextFireAt, claimAt)
}

// FanOutScheduleOccurrence 原子完成两件事：为本次 occurrence 创建逐人投递记录，
// 并推进日程到下一次。进程在事务后崩溃也只会留下可重试的 recipient rows。
func (s *Store) FanOutScheduleOccurrence(ctx context.Context, sc *Schedule, userIDs []int64, firedAt time.Time, nextFireAt *time.Time, done bool) error {
	if sc == nil || sc.DeliveryClaimedAt == nil {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, userID := range userIDs {
		if userID <= 0 {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schedule_deliveries (schedule_id, occurrence_at, user_id, mode, message, title)
			 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (schedule_id, occurrence_at, user_id) DO NOTHING`,
			sc.ID, sc.FireAt, userID, sc.Mode, sc.Message, sc.Title); err != nil {
			return err
		}
	}
	status := ScheduleActive
	if done {
		status = ScheduleDone
	}
	tag, err := tx.Exec(ctx,
		`UPDATE schedules SET last_fired=$2, status=$3, fire_at=COALESCE($4, fire_at),
		 updated_at=now(), delivery_claimed_at=NULL
		 WHERE id=$1 AND delivery_claimed_at=$5`, sc.ID, firedAt, status, nextFireAt, *sc.DeliveryClaimedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// DueScheduleDeliveries 原子认领逐接收人投递。processing 租约过期会重领；
// 达到最大尝试次数后置 failed，不再无限轰炸。
func (s *Store) DueScheduleDeliveries(ctx context.Context, now time.Time) ([]*ScheduleDelivery, error) {
	stale := now.Add(-scheduleRecipientLease)
	if _, err := s.pool.Exec(ctx,
		`UPDATE schedule_deliveries SET status='failed', claimed_at=NULL,
		        last_error=CASE WHEN last_error='' THEN 'retry budget exhausted after interrupted claim' ELSE last_error END
		  WHERE attempts >= $1 AND ((status='pending' AND available_at <= $2)
		    OR (status='processing' AND claimed_at <= $3))`, scheduleRecipientMaxAttempts, now, stale); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		   SELECT id FROM schedule_deliveries
		    WHERE attempts < $3 AND available_at <= $1
		      AND (status = 'pending' OR (status = 'processing' AND claimed_at <= $2))
		    ORDER BY available_at, id LIMIT $4 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE schedule_deliveries d
		    SET status='processing', claimed_at=$1, attempts=attempts+1
		   FROM due WHERE d.id=due.id
		 RETURNING `+scheduleDeliveryColsWithAlias("d"), now, stale, scheduleRecipientMaxAttempts, scheduleRecipientBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ScheduleDelivery
	for rows.Next() {
		d, err := scanScheduleDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scheduleDeliveryColsWithAlias(alias string) string {
	parts := strings.Split(scheduleDeliveryCols, ", ")
	for i := range parts {
		parts[i] = alias + "." + parts[i]
	}
	return strings.Join(parts, ", ")
}

func (s *Store) MarkScheduleDeliveryDelivered(ctx context.Context, id int64, claimAt, deliveredAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE schedule_deliveries SET status='delivered', delivered_at=$3, claimed_at=NULL, last_error=''
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, deliveredAt)
}

// PrepareScheduleDeliveryResult stores the exact AI-generated message before
// transport. A retry can then resend it without another model turn.
func (s *Store) PrepareScheduleDeliveryResult(ctx context.Context, id int64, claimAt time.Time, result string) error {
	return s.execOne(ctx,
		`UPDATE schedule_deliveries SET result_text=$3, last_error=''
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`,
		id, claimAt, truncateRunes(result, 12000))
}

// MarkScheduleDeliveryFailed records a permanent recipient-specific failure.
// Callers should reserve RetryScheduleDelivery for transient generation, storage,
// or transport failures so inactive/deleted recipients do not consume retry slots.
func (s *Store) MarkScheduleDeliveryFailed(ctx context.Context, id int64, claimAt time.Time, cause string) error {
	return s.execOne(ctx,
		`UPDATE schedule_deliveries SET status='failed', claimed_at=NULL, last_error=$3
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, truncateRunes(cause, 500))
}

func (s *Store) ReleaseScheduleDeliveryClaim(ctx context.Context, id int64, claimAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE schedule_deliveries SET status='pending', claimed_at=NULL, attempts=greatest(attempts-1, 0)
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt)
}

func (s *Store) RetryScheduleDelivery(ctx context.Context, id int64, claimAt time.Time, attempts int, cause string) error {
	if attempts >= scheduleRecipientMaxAttempts {
		return s.execOne(ctx,
			`UPDATE schedule_deliveries SET status='failed', claimed_at=NULL, last_error=$3
			 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, truncateRunes(cause, 500))
	}
	delay := time.Duration(1<<min(attempts, 6)) * time.Minute
	return s.execOne(ctx,
		`UPDATE schedule_deliveries SET status='pending', claimed_at=NULL, available_at=now()+$3::interval, last_error=$4
		 WHERE id=$1 AND status='processing' AND claimed_at=$2`, id, claimAt, fmt.Sprintf("%d seconds", int(delay.Seconds())), truncateRunes(cause, 500))
}

// UpdateScheduleFireAt 修正下次触发时间（daily 的工作日跳过/时区校正）。
func (s *Store) UpdateScheduleFireAt(ctx context.Context, id int64, fireAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE schedules SET fire_at = $2, updated_at = now() WHERE id = $1 AND status = 'active'`, id, fireAt)
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

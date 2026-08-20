package store

import (
	"context"
	"strings"
	"time"
)

const (
	NotificationDeliveryStarted   = "started"
	NotificationDeliveryDelivered = "delivered"
	NotificationDeliveryFailed    = "failed"
)

type NotificationDelivery struct {
	Key         string
	UserID      int64
	ContentHash string
	Status      string
	Attempts    int
	StartedAt   time.Time
	DeliveredAt *time.Time
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const notificationDeliveryCols = `delivery_key, user_id, content_hash, status, attempts,
	started_at, delivered_at, last_error, created_at, updated_at`

func scanNotificationDelivery(row interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var d NotificationDelivery
	if err := row.Scan(&d.Key, &d.UserID, &d.ContentHash, &d.Status, &d.Attempts,
		&d.StartedAt, &d.DeliveredAt, &d.LastError, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &d, nil
}

// BeginNotificationDelivery atomically crosses the no-replay boundary for one
// logical external notification. created=false means another attempt already
// started or settled it; callers must inspect Status and must not send again.
func (s *Store) BeginNotificationDelivery(ctx context.Context, key string, userID int64, contentHash string) (delivery *NotificationDelivery, created bool, err error) {
	key = strings.TrimSpace(key)
	contentHash = strings.TrimSpace(contentHash)
	if key == "" || userID <= 0 || contentHash == "" {
		return nil, false, ErrNotFound
	}

	delivery, err = scanNotificationDelivery(s.pool.QueryRow(ctx,
		`INSERT INTO notification_deliveries (delivery_key, user_id, content_hash)
		 VALUES ($1,$2,$3) ON CONFLICT (delivery_key) DO NOTHING
		 RETURNING `+notificationDeliveryCols, key, userID, contentHash))
	if err == nil {
		return delivery, true, nil
	}
	if err != ErrNotFound {
		return nil, false, err
	}
	delivery, err = scanNotificationDelivery(s.pool.QueryRow(ctx,
		`SELECT `+notificationDeliveryCols+` FROM notification_deliveries WHERE delivery_key=$1`, key))
	if err != nil {
		return nil, false, err
	}
	if delivery.UserID != userID || delivery.ContentHash != contentHash {
		// The occurrence key remains authoritative even if a reclaimed generator
		// produced different text. Return the existing boundary so callers can
		// settle without sending either variant again, while surfacing the bug.
		return delivery, false, ErrConflict
	}
	return delivery, false, nil
}

func (s *Store) MarkNotificationDeliveryDelivered(ctx context.Context, key string, deliveredAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE notification_deliveries
		    SET status='delivered', delivered_at=$2, last_error='', updated_at=now()
		  WHERE delivery_key=$1 AND status='started'`, strings.TrimSpace(key), deliveredAt)
}

func (s *Store) MarkNotificationDeliveryFailed(ctx context.Context, key, cause string) error {
	return s.execOne(ctx,
		`UPDATE notification_deliveries
		    SET status='failed', last_error=$2, updated_at=now()
		  WHERE delivery_key=$1 AND status='started'`, strings.TrimSpace(key), truncateRunes(strings.TrimSpace(cause), 500))
}

// DeleteExpiredDeliveryReceipts bounds the high-volume terminal delivery
// ledger. Uncertain started rows are intentionally retained: deleting them
// would permit a later replay of an external side effect whose outcome is not
// provable from PostgreSQL.
func (s *Store) DeleteExpiredDeliveryReceipts(ctx context.Context, before time.Time) (notifications, actions, workerLLMCalls int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	notificationTag, err := tx.Exec(ctx,
		`DELETE FROM notification_deliveries
		  WHERE status IN ('delivered','failed') AND updated_at < $1`, before)
	if err != nil {
		return 0, 0, 0, err
	}
	actionTag, err := tx.Exec(ctx,
		`DELETE FROM external_action_receipts
		  WHERE status IN ('completed','failed') AND updated_at < $1`, before)
	if err != nil {
		return 0, 0, 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE external_action_receipts
		    SET result_text='', result_expires_at=NULL
		  WHERE result_expires_at IS NOT NULL AND result_expires_at < now()`); err != nil {
		return 0, 0, 0, err
	}
	workerLLMTag, err := tx.Exec(ctx,
		`DELETE FROM worker_llm_calls
		  WHERE status IN ('completed','failed') AND updated_at < $1`, before)
	if err != nil {
		return 0, 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, 0, err
	}
	return notificationTag.RowsAffected(), actionTag.RowsAffected(), workerLLMTag.RowsAffected(), nil
}

// ClearExpiredExternalActionResults removes recoverable one-time plaintext as
// soon as its short replay window ends while retaining the durable no-replay
// receipt. Receipt retention and secret retention deliberately use different
// schedules.
func (s *Store) ClearExpiredExternalActionResults(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE external_action_receipts
		 SET result_text='', result_expires_at=NULL
		 WHERE result_expires_at IS NOT NULL AND result_expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

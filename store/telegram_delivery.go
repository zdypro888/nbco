package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const (
	TelegramInboundPending    = "pending"
	TelegramInboundProcessing = "processing"
	TelegramInboundDone       = "done"
	TelegramInboundFailed     = "failed"

	TelegramDeliveryStarted   = "started"
	TelegramDeliveryDelivered = "delivered"
	TelegramDeliveryFailed    = "failed"

	telegramInboundLease       = 2 * time.Minute
	telegramInboundMaxAttempts = 10
)

type TelegramInboundUpdate struct {
	UpdateID    int64
	Payload     json.RawMessage
	PayloadHash string
	Status      string
	Attempts    int
	AvailableAt time.Time
	ClaimedAt   *time.Time
	ClaimOwner  string
	ProcessedAt *time.Time
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const telegramInboundCols = `update_id, payload, payload_hash, status, attempts, available_at, claimed_at, claim_owner, processed_at, last_error, created_at, updated_at`

func scanTelegramInboundUpdate(row interface{ Scan(...any) error }) (*TelegramInboundUpdate, error) {
	var update TelegramInboundUpdate
	if err := row.Scan(&update.UpdateID, &update.Payload, &update.PayloadHash, &update.Status,
		&update.Attempts, &update.AvailableAt, &update.ClaimedAt, &update.ClaimOwner, &update.ProcessedAt,
		&update.LastError, &update.CreatedAt, &update.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &update, nil
}

// EnqueueTelegramInboundUpdate is the webhook acknowledgement boundary.
// Repeated immutable updates are accepted; an update_id with different bytes
// is rejected as a source identity conflict.
func (s *Store) EnqueueTelegramInboundUpdate(ctx context.Context, updateID int64, payload json.RawMessage, payloadHash string) (bool, error) {
	payloadHash = strings.TrimSpace(payloadHash)
	if updateID <= 0 || len(payload) == 0 || !json.Valid(payload) || payloadHash == "" {
		return false, ErrNotFound
	}
	var inserted int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO telegram_inbound_updates (update_id, payload, payload_hash)
		 VALUES ($1,$2,$3) ON CONFLICT (update_id) DO NOTHING
		 RETURNING update_id`, updateID, payload, payloadHash).Scan(&inserted)
	if err == nil {
		return true, nil
	}
	if wrapErr(err) != ErrNotFound {
		return false, err
	}
	var existingHash string
	if err := s.pool.QueryRow(ctx,
		`SELECT payload_hash FROM telegram_inbound_updates WHERE update_id=$1`, updateID).Scan(&existingHash); err != nil {
		return false, wrapErr(err)
	}
	if existingHash != payloadHash {
		return false, ErrConflict
	}
	return false, nil
}

func (s *Store) ClaimTelegramInboundUpdates(ctx context.Context, owner string, limit int) ([]*TelegramInboundUpdate, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	// available_at and retry delays are written by PostgreSQL. Read the lease
	// clock from the same source so host clock skew cannot delay or steal work.
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	stale := now.Add(-telegramInboundLease)
	if _, err := s.pool.Exec(ctx,
		`UPDATE telegram_inbound_updates
		    SET status='failed', claimed_at=NULL, claim_owner='',
		        last_error=CASE WHEN last_error='' THEN 'retry budget exhausted after interrupted processing' ELSE last_error END
		  WHERE attempts >= $1 AND ((status='pending' AND available_at <= $2)
		     OR (status='processing' AND claimed_at <= $3))`,
		telegramInboundMaxAttempts, now, stale); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		   SELECT update_id FROM telegram_inbound_updates
		    WHERE attempts < $3 AND available_at <= $1
		      AND (status='pending' OR (status='processing' AND claimed_at <= $2))
		    ORDER BY update_id LIMIT $4 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE telegram_inbound_updates u
		    SET status='processing', claimed_at=$1, claim_owner=$5, attempts=attempts+1
		   FROM due WHERE u.update_id=due.update_id
		 RETURNING `+telegramInboundColsWithAlias("u"), now, stale, telegramInboundMaxAttempts, limit, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*TelegramInboundUpdate, 0, limit)
	for rows.Next() {
		update, err := scanTelegramInboundUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, update)
	}
	return out, rows.Err()
}

func telegramInboundColsWithAlias(alias string) string {
	parts := strings.Split(telegramInboundCols, ", ")
	for i := range parts {
		parts[i] = alias + "." + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
}

func (s *Store) CompleteTelegramInboundUpdates(ctx context.Context, owner string, updateIDs []int64, processedAt time.Time) error {
	if len(updateIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE telegram_inbound_updates
		    SET status='done', processed_at=$3, claimed_at=NULL, claim_owner='', last_error=''
		  WHERE update_id=ANY($1) AND status='processing' AND claim_owner=$2`, updateIDs, strings.TrimSpace(owner), processedAt)
	return err
}

func (s *Store) RetryTelegramInboundUpdates(ctx context.Context, owner string, updateIDs []int64, cause string) error {
	if len(updateIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE telegram_inbound_updates
		    SET status='pending', claimed_at=NULL, claim_owner='', available_at=now()+interval '2 seconds', last_error=$3
		  WHERE update_id=ANY($1) AND status='processing' AND claim_owner=$2`,
		updateIDs, strings.TrimSpace(owner), truncateRunes(strings.TrimSpace(cause), 500))
	return err
}

func (s *Store) FailTelegramInboundUpdate(ctx context.Context, owner string, updateID int64, cause string) error {
	return s.execOne(ctx,
		`UPDATE telegram_inbound_updates
		    SET status='failed', claimed_at=NULL, claim_owner='', last_error=$3
		  WHERE update_id=$1 AND status='processing' AND claim_owner=$2`,
		updateID, strings.TrimSpace(owner), truncateRunes(strings.TrimSpace(cause), 500))
}

func (s *Store) HeartbeatTelegramInboundClaims(ctx context.Context, owner string, updateIDs []int64) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return ErrNotFound
	}
	if len(updateIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE telegram_inbound_updates SET claimed_at=clock_timestamp()
		  WHERE status='processing' AND claim_owner=$1 AND update_id=ANY($2)`, owner, updateIDs)
	return err
}

func (s *Store) DeleteExpiredTelegramTransportRecords(ctx context.Context, before time.Time) (updates, parts int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	updateTag, err := tx.Exec(ctx,
		`DELETE FROM telegram_inbound_updates WHERE status IN ('done','failed') AND updated_at < $1`, before)
	if err != nil {
		return 0, 0, err
	}
	partTag, err := tx.Exec(ctx,
		`DELETE FROM telegram_delivery_parts WHERE status IN ('delivered','failed') AND updated_at < $1`, before)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return updateTag.RowsAffected(), partTag.RowsAffected(), nil
}

type TelegramDeliveryPart struct {
	DeliveryKey       string
	PartIndex         int
	PartCount         int
	ChatID            int64
	ContentHash       string
	Status            string
	TelegramMessageID *int64
	DeliveredAt       *time.Time
	LastError         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const telegramDeliveryPartCols = `delivery_key, part_index, part_count, chat_id, content_hash, status, telegram_message_id, delivered_at, last_error, created_at, updated_at`

func scanTelegramDeliveryPart(row interface{ Scan(...any) error }) (*TelegramDeliveryPart, error) {
	var part TelegramDeliveryPart
	if err := row.Scan(&part.DeliveryKey, &part.PartIndex, &part.PartCount, &part.ChatID,
		&part.ContentHash, &part.Status, &part.TelegramMessageID, &part.DeliveredAt,
		&part.LastError, &part.CreatedAt, &part.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &part, nil
}

func (s *Store) BeginTelegramDeliveryPart(ctx context.Context, key string, partIndex, partCount int, chatID int64, contentHash string) (*TelegramDeliveryPart, bool, error) {
	key = strings.TrimSpace(key)
	contentHash = strings.TrimSpace(contentHash)
	if key == "" || partIndex < 0 || partCount <= 0 || partIndex >= partCount || chatID == 0 || contentHash == "" {
		return nil, false, ErrNotFound
	}
	part, err := scanTelegramDeliveryPart(s.pool.QueryRow(ctx,
		`INSERT INTO telegram_delivery_parts (delivery_key, part_index, part_count, chat_id, content_hash)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (delivery_key, part_index) DO NOTHING
		 RETURNING `+telegramDeliveryPartCols, key, partIndex, partCount, chatID, contentHash))
	if err == nil {
		return part, true, nil
	}
	if err != ErrNotFound {
		return nil, false, err
	}
	part, err = scanTelegramDeliveryPart(s.pool.QueryRow(ctx,
		`SELECT `+telegramDeliveryPartCols+` FROM telegram_delivery_parts
		  WHERE delivery_key=$1 AND part_index=$2`, key, partIndex))
	if err != nil {
		return nil, false, err
	}
	if part.PartCount != partCount || part.ChatID != chatID || part.ContentHash != contentHash {
		return part, false, ErrConflict
	}
	return part, false, nil
}

func (s *Store) MarkTelegramDeliveryPartDelivered(ctx context.Context, key string, partIndex int, messageID int64, at time.Time) error {
	return s.execOne(ctx,
		`UPDATE telegram_delivery_parts
		    SET status='delivered', telegram_message_id=$3, delivered_at=$4, last_error=''
		  WHERE delivery_key=$1 AND part_index=$2 AND status='started'`, key, partIndex, messageID, at)
}

func (s *Store) MarkTelegramDeliveryPartFailed(ctx context.Context, key string, partIndex int, cause string) error {
	return s.execOne(ctx,
		`UPDATE telegram_delivery_parts
		    SET status='failed', last_error=$3
		  WHERE delivery_key=$1 AND part_index=$2 AND status='started'`, key, partIndex, truncateRunes(strings.TrimSpace(cause), 500))
}

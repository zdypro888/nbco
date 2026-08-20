package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	DomainOutboxPending    = "pending"
	DomainOutboxProcessing = "processing"
	DomainOutboxDone       = "done"
	DomainOutboxFailed     = "failed"

	domainOutboxClaimLease  = 2 * time.Minute
	domainOutboxMaxAttempts = 10
)

type DomainOutboxEvent struct {
	ID            int64
	OccurrenceKey string
	Topic         string
	Payload       json.RawMessage
	Status        string
	Attempts      int
	AvailableAt   time.Time
	ClaimedAt     *time.Time
	ClaimOwner    string
	CompletedAt   *time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const domainOutboxEventCols = `id, occurrence_key, topic, payload, status, attempts, available_at, claimed_at, claim_owner, completed_at, last_error, created_at, updated_at`

func scanDomainOutboxEvent(row interface{ Scan(...any) error }) (*DomainOutboxEvent, error) {
	var event DomainOutboxEvent
	if err := row.Scan(&event.ID, &event.OccurrenceKey, &event.Topic, &event.Payload,
		&event.Status, &event.Attempts, &event.AvailableAt, &event.ClaimedAt,
		&event.ClaimOwner, &event.CompletedAt, &event.LastError, &event.CreatedAt,
		&event.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &event, nil
}

// EnqueueDomainOutboxEvent records one immutable domain occurrence. Reusing an
// occurrence key with a different topic or payload is a conflict, never a new
// side effect.
func (s *Store) EnqueueDomainOutboxEvent(ctx context.Context, occurrenceKey, topic string, payload json.RawMessage) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, created, err := enqueueDomainOutboxEventTx(ctx, tx, occurrenceKey, topic, payload)
	if err != nil {
		return id, created, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return id, created, nil
}

func enqueueDomainOutboxEventTx(ctx context.Context, tx pgx.Tx, occurrenceKey, topic string, payload json.RawMessage) (int64, bool, error) {
	occurrenceKey = strings.TrimSpace(occurrenceKey)
	topic = strings.TrimSpace(topic)
	if tx == nil || occurrenceKey == "" || topic == "" || len(payload) == 0 || !json.Valid(payload) {
		return 0, false, ErrNotFound
	}
	var id int64
	err := tx.QueryRow(ctx,
		`INSERT INTO domain_outbox_events (occurrence_key, topic, payload)
		 VALUES ($1,$2,$3) ON CONFLICT (occurrence_key) DO NOTHING
		 RETURNING id`, occurrenceKey, topic, payload).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if wrapErr(err) != ErrNotFound {
		return 0, false, err
	}
	var existingTopic string
	var samePayload bool
	if err := tx.QueryRow(ctx,
		`SELECT id, topic, payload = $2::jsonb
		   FROM domain_outbox_events WHERE occurrence_key=$1`, occurrenceKey, payload).
		Scan(&id, &existingTopic, &samePayload); err != nil {
		return 0, false, wrapErr(err)
	}
	if existingTopic != topic || !samePayload {
		return id, false, ErrConflict
	}
	return id, false, nil
}

// ClaimDomainOutboxEvents leases due events for the supplied consumer topics.
// PostgreSQL provides both ordering and lease time so host clock skew cannot
// steal or delay work.
func (s *Store) ClaimDomainOutboxEvents(ctx context.Context, owner string, topics []string, limit int) ([]*DomainOutboxEvent, error) {
	owner = strings.TrimSpace(owner)
	topics = normalizeDomainOutboxTopics(topics)
	if owner == "" || len(topics) == 0 {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	stale := now.Add(-domainOutboxClaimLease)
	if _, err := s.pool.Exec(ctx,
		`UPDATE domain_outbox_events
		    SET status='failed', claimed_at=NULL, claim_owner='', completed_at=$2,
		        last_error=CASE WHEN last_error='' THEN 'retry budget exhausted after interrupted delivery' ELSE last_error END
		  WHERE topic=ANY($1) AND attempts >= $3
		    AND ((status='pending' AND available_at <= $2)
		      OR (status='processing' AND claimed_at <= $4))`,
		topics, now, domainOutboxMaxAttempts, stale); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		   SELECT id FROM domain_outbox_events
		    WHERE topic=ANY($1) AND attempts < $4 AND available_at <= $2
		      AND (status='pending' OR (status='processing' AND claimed_at <= $3))
		    ORDER BY available_at, id LIMIT $5 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE domain_outbox_events e
		    SET status='processing', claimed_at=$2, claim_owner=$6, attempts=attempts+1
		   FROM due WHERE e.id=due.id
		 RETURNING `+domainOutboxEventColsWithAlias("e"), topics, now, stale, domainOutboxMaxAttempts, limit, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*DomainOutboxEvent, 0, limit)
	for rows.Next() {
		event, err := scanDomainOutboxEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, wrapErr(rows.Err())
}

func normalizeDomainOutboxTopics(topics []string) []string {
	out := make([]string, 0, len(topics))
	seen := make(map[string]struct{}, len(topics))
	for _, topic := range topics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			continue
		}
		if _, exists := seen[topic]; exists {
			continue
		}
		seen[topic] = struct{}{}
		out = append(out, topic)
	}
	return out
}

func domainOutboxEventColsWithAlias(alias string) string {
	parts := strings.Split(domainOutboxEventCols, ", ")
	for i := range parts {
		parts[i] = alias + "." + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
}

func (s *Store) CompleteDomainOutboxEvent(ctx context.Context, id int64, owner string, claimedAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE domain_outbox_events
		    SET status='done', claimed_at=NULL, claim_owner='', completed_at=now(), last_error=''
		  WHERE id=$1 AND status='processing' AND claim_owner=$2 AND claimed_at=$3`,
		id, strings.TrimSpace(owner), claimedAt)
}

func (s *Store) RetryDomainOutboxEvent(ctx context.Context, id int64, owner string, claimedAt time.Time, cause string) error {
	return s.execOne(ctx,
		`UPDATE domain_outbox_events
		    SET status='pending', claimed_at=NULL, claim_owner='',
		        available_at=now()+interval '2 seconds', last_error=$4
		  WHERE id=$1 AND status='processing' AND claim_owner=$2 AND claimed_at=$3`,
		id, strings.TrimSpace(owner), claimedAt, truncateRunes(strings.TrimSpace(cause), 500))
}

func (s *Store) FailDomainOutboxEvent(ctx context.Context, id int64, owner string, claimedAt time.Time, cause string) error {
	return s.execOne(ctx,
		`UPDATE domain_outbox_events
		    SET status='failed', claimed_at=NULL, claim_owner='', completed_at=now(), last_error=$4
		  WHERE id=$1 AND status='processing' AND claim_owner=$2 AND claimed_at=$3`,
		id, strings.TrimSpace(owner), claimedAt, truncateRunes(strings.TrimSpace(cause), 500))
}

func (s *Store) DeleteExpiredDomainOutboxEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM domain_outbox_events
		  WHERE status IN ('done','failed') AND updated_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

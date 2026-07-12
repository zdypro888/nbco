package einoengine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zdypro888/nbco/textfmt"
)

// RuntimeStore is the durable state Eino needs for managed sessions and
// interrupt/cancel checkpoints. It intentionally contains no nbco business
// state; chat messages remain in the product-facing store.
type RuntimeStore interface {
	adk.SessionEventStore[*schema.Message]
	adk.CheckPointStore
	adk.CheckPointDeleter
}

// PostgresRuntimeStore persists Eino's append-only event log and opaque
// checkpoints in the same PostgreSQL database as nbco.
type PostgresRuntimeStore struct {
	pool       *pgxpool.Pool
	serializer schema.Serializer
}

func NewPostgresRuntimeStore(pool *pgxpool.Pool) *PostgresRuntimeStore {
	return &PostgresRuntimeStore{
		pool:       pool,
		serializer: &schema.HumanReadableSerializer{},
	}
}

func (s *PostgresRuntimeStore) AppendEvents(ctx context.Context, sessionID string, events []*adk.SessionEvent[*schema.Message]) error {
	if s == nil || s.pool == nil {
		return errors.New("eino runtime store is unavailable")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("eino session id is empty")
	}
	type encodedEvent struct {
		id      string
		kind    adk.SessionEventKind
		payload []byte
	}
	encoded := make([]encodedEvent, 0, len(events))
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		if event == nil || strings.TrimSpace(event.EventID) == "" {
			return adk.ErrInvalidEventID
		}
		if _, exists := seen[event.EventID]; exists {
			return adk.ErrDuplicateEventID
		}
		seen[event.EventID] = struct{}{}
		if err := adk.NormalizeSessionEventKind(event); err != nil {
			return err
		}
		payload, err := s.serializer.Marshal(event)
		if err != nil {
			return fmt.Errorf("serialize Eino session event %q: %w", event.EventID, err)
		}
		// Tool results and user inputs can contain one-time credentials. The live
		// agent keeps the original event for this turn, while only the durable copy
		// is redacted so a later replay cannot expose the credential again.
		payload = []byte(textfmt.RedactSecrets(string(payload)))
		var check adk.SessionEvent[*schema.Message]
		if err := s.serializer.Unmarshal(payload, &check); err != nil {
			return fmt.Errorf("validate redacted Eino session event %q: %w", event.EventID, err)
		}
		encoded = append(encoded, encodedEvent{id: event.EventID, kind: event.Kind, payload: payload})
	}
	if len(encoded) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, event := range encoded {
		_, err = tx.Exec(ctx,
			`INSERT INTO eino_session_events (session_id, event_id, kind, payload)
			 VALUES ($1, $2, $3, $4)`,
			sessionID, event.id, string(event.kind), event.payload)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return adk.ErrDuplicateEventID
			}
			return fmt.Errorf("append Eino session event %q: %w", event.id, err)
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresRuntimeStore) LoadEvents(ctx context.Context, sessionID string, req *adk.LoadSessionEventsRequest) (*adk.LoadSessionEventsResult[*schema.Message], error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("eino runtime store is unavailable")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("eino session id is empty")
	}
	if req == nil {
		req = &adk.LoadSessionEventsRequest{}
	}

	args := []any{sessionID}
	clauses := []string{"session_id = $1"}
	if req.After != "" {
		var seq int64
		err := s.pool.QueryRow(ctx,
			`SELECT seq FROM eino_session_events WHERE session_id = $1 AND event_id = $2`,
			sessionID, req.After).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, adk.ErrEventIDOutOfRange
		}
		if err != nil {
			return nil, fmt.Errorf("resolve Eino session cursor: %w", err)
		}
		args = append(args, seq)
		operator := ">"
		if req.Reverse {
			operator = "<"
		}
		clauses = append(clauses, fmt.Sprintf("seq %s $%d", operator, len(args)))
	}
	if len(req.Kinds) > 0 {
		kinds := make([]string, 0, len(req.Kinds))
		for _, kind := range req.Kinds {
			kinds = append(kinds, string(kind))
		}
		args = append(args, kinds)
		clauses = append(clauses, fmt.Sprintf("kind = ANY($%d::text[])", len(args)))
	}
	order := "ASC"
	if req.Reverse {
		order = "DESC"
	}
	query := `SELECT event_id, kind, payload FROM eino_session_events WHERE ` +
		strings.Join(clauses, " AND ") + ` ORDER BY seq ` + order
	if req.Limit > 0 {
		args = append(args, req.Limit+1)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load Eino session events: %w", err)
	}
	defer rows.Close()
	result := &adk.LoadSessionEventsResult[*schema.Message]{}
	for rows.Next() {
		var eventID, kind string
		var payload []byte
		if err := rows.Scan(&eventID, &kind, &payload); err != nil {
			return nil, err
		}
		if req.Limit > 0 && len(result.Events) == req.Limit {
			result.Next = result.Events[len(result.Events)-1].EventID
			continue
		}
		var event adk.SessionEvent[*schema.Message]
		if err := s.serializer.Unmarshal(payload, &event); err != nil {
			return nil, fmt.Errorf("decode Eino session event %q: %w", eventID, err)
		}
		if event.EventID != eventID || string(event.Kind) != kind {
			return nil, fmt.Errorf("corrupt Eino session event %q: envelope does not match indexed identity", eventID)
		}
		result.Events = append(result.Events, &event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresRuntimeStore) Get(ctx context.Context, checkpointID string) ([]byte, bool, error) {
	if s == nil || s.pool == nil {
		return nil, false, errors.New("eino runtime store is unavailable")
	}
	var payload []byte
	err := s.pool.QueryRow(ctx,
		`SELECT payload FROM eino_checkpoints WHERE checkpoint_id = $1`, checkpointID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

func (s *PostgresRuntimeStore) Set(ctx context.Context, checkpointID string, payload []byte) error {
	if s == nil || s.pool == nil {
		return errors.New("eino runtime store is unavailable")
	}
	if strings.TrimSpace(checkpointID) == "" {
		return errors.New("eino checkpoint id is empty")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO eino_checkpoints (checkpoint_id, payload) VALUES ($1, $2)
		 ON CONFLICT (checkpoint_id) DO UPDATE
		 SET payload = EXCLUDED.payload, updated_at = now()`,
		checkpointID, payload)
	return err
}

func (s *PostgresRuntimeStore) Delete(ctx context.Context, checkpointID string) error {
	if s == nil || s.pool == nil {
		return errors.New("eino runtime store is unavailable")
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM eino_checkpoints WHERE checkpoint_id = $1`, checkpointID)
	return err
}

// DeleteSession abandons an uncommitted first run. Once a session has a
// committed idle boundary Engine uses Eino's append-only RollbackSession
// instead, preserving the audit timeline.
func (s *PostgresRuntimeStore) DeleteSession(ctx context.Context, sessionID string) error {
	if s == nil || s.pool == nil {
		return errors.New("eino runtime store is unavailable")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM eino_session_events WHERE session_id = $1`, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eino_checkpoints WHERE checkpoint_id = $1`,
		"session/"+sessionID+"/runner_checkpoint"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

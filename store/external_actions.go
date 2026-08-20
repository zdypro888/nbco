package store

import (
	"context"
	"strings"
	"time"
)

const (
	ExternalActionStarted   = "started"
	ExternalActionCompleted = "completed"
	ExternalActionFailed    = "failed"
)

type ExternalActionReceipt struct {
	Key         string
	Kind        string
	PayloadHash string
	Status      string
	LastError   string
	ResultText  string
	ResultUntil *time.Time
	StartedAt   time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const externalActionReceiptCols = `action_key, kind, payload_hash, status,
	last_error, result_text, result_expires_at, started_at, completed_at, created_at, updated_at`

func scanExternalActionReceipt(row interface{ Scan(...any) error }) (*ExternalActionReceipt, error) {
	var receipt ExternalActionReceipt
	if err := row.Scan(&receipt.Key, &receipt.Kind, &receipt.PayloadHash, &receipt.Status,
		&receipt.LastError, &receipt.ResultText, &receipt.ResultUntil,
		&receipt.StartedAt, &receipt.CompletedAt,
		&receipt.CreatedAt, &receipt.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &receipt, nil
}

// BeginExternalAction is an at-most-once boundary for a transport-owned
// programmatic action. created=false is terminal for the caller: a prior
// process may have mutated state immediately before it stopped.
func (s *Store) BeginExternalAction(ctx context.Context, key, kind, payloadHash string) (receipt *ExternalActionReceipt, created bool, err error) {
	key = strings.TrimSpace(key)
	kind = strings.TrimSpace(kind)
	payloadHash = strings.TrimSpace(payloadHash)
	if key == "" || kind == "" || payloadHash == "" {
		return nil, false, ErrNotFound
	}
	receipt, err = scanExternalActionReceipt(s.pool.QueryRow(ctx,
		`INSERT INTO external_action_receipts (action_key, kind, payload_hash)
		 VALUES ($1,$2,$3) ON CONFLICT (action_key) DO NOTHING
		 RETURNING `+externalActionReceiptCols, key, kind, payloadHash))
	if err == nil {
		return receipt, true, nil
	}
	if err != ErrNotFound {
		return nil, false, err
	}
	receipt, err = scanExternalActionReceipt(s.pool.QueryRow(ctx,
		`SELECT `+externalActionReceiptCols+` FROM external_action_receipts WHERE action_key=$1`, key))
	if err != nil {
		return nil, false, err
	}
	if receipt.Kind != kind || receipt.PayloadHash != payloadHash {
		return receipt, false, ErrConflict
	}
	return receipt, false, nil
}

func (s *Store) CompleteExternalAction(ctx context.Context, key string) error {
	return s.execOne(ctx,
		`UPDATE external_action_receipts
		    SET status='completed', completed_at=now(), last_error='',
		        result_text='', result_expires_at=NULL, updated_at=now()
		  WHERE action_key=$1 AND status='started'`, strings.TrimSpace(key))
}

// CompleteExternalActionWithResult keeps the exact bounded tool result long
// enough to recover a lost response. The receipt itself remains authoritative
// after the result expires and still prevents duplicate side effects.
func (s *Store) CompleteExternalActionWithResult(ctx context.Context, key, result string, resultUntil time.Time) error {
	if resultUntil.IsZero() {
		return s.CompleteExternalAction(ctx, key)
	}
	return s.execOne(ctx,
		`UPDATE external_action_receipts
		    SET status='completed', completed_at=now(), last_error='',
		        result_text=$2, result_expires_at=$3, updated_at=now()
		  WHERE action_key=$1 AND status='started'`,
		strings.TrimSpace(key), result, resultUntil)
}

// CompleteRecoverableExternalAction settles an action only after its domain
// has independently proved an idempotent result. It is intended for resumable
// publication flows such as file intake, where a retry can safely recover a
// previously failed or interrupted transport receipt without repeating the
// canonical business object.
func (s *Store) CompleteRecoverableExternalAction(ctx context.Context, key string) error {
	return s.execOne(ctx,
		`UPDATE external_action_receipts
		    SET status='completed', completed_at=COALESCE(completed_at, now()), last_error='', updated_at=now()
		  WHERE action_key=$1 AND status IN ('started','failed','completed')`, strings.TrimSpace(key))
}

// CompleteRecoverableExternalActionWithResult is the result-bearing variant
// used after a domain-specific read proves what an interrupted invocation
// committed. Unlike the normal completion path it may recover a failed receipt.
func (s *Store) CompleteRecoverableExternalActionWithResult(ctx context.Context, key, result string, resultUntil time.Time) error {
	return s.execOne(ctx,
		`UPDATE external_action_receipts
		    SET status='completed', completed_at=COALESCE(completed_at, now()), last_error='',
		        result_text=$2, result_expires_at=$3, updated_at=now()
		  WHERE action_key=$1 AND status IN ('started','failed','completed')`,
		strings.TrimSpace(key), result, resultUntil)
}

func (s *Store) FailExternalAction(ctx context.Context, key, cause string) error {
	return s.execOne(ctx,
		`UPDATE external_action_receipts
		    SET status='failed', completed_at=now(), last_error=$2,
		        result_text='', result_expires_at=NULL, updated_at=now()
		  WHERE action_key=$1 AND status='started'`, strings.TrimSpace(key), truncateRunes(strings.TrimSpace(cause), 500))
}

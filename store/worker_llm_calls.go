package store

import (
	"context"
	"strings"
	"time"
)

const (
	WorkerLLMCallStarted   = "started"
	WorkerLLMCallCompleted = "completed"
	WorkerLLMCallFailed    = "failed"
)

type WorkerLLMCall struct {
	WorkerID    int64
	RequestID   string
	RequestHash string
	Status      string
	HTTPStatus  *int
	Response    []byte
	LastError   string
	StartedAt   time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const workerLLMCallCols = `worker_id, request_id, request_hash, status,
	http_status, response_body, last_error, started_at, completed_at, created_at, updated_at`

func scanWorkerLLMCall(row interface{ Scan(...any) error }) (*WorkerLLMCall, error) {
	var call WorkerLLMCall
	if err := row.Scan(&call.WorkerID, &call.RequestID, &call.RequestHash, &call.Status,
		&call.HTTPStatus, &call.Response, &call.LastError, &call.StartedAt,
		&call.CompletedAt, &call.CreatedAt, &call.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &call, nil
}

// BeginWorkerLLMCall claims one logical model request. An existing row is
// authoritative: completed responses can be replayed, while started/failed
// calls must not cross the upstream boundary again.
func (s *Store) BeginWorkerLLMCall(ctx context.Context, workerID int64, requestID, requestHash string) (*WorkerLLMCall, bool, error) {
	requestID = strings.TrimSpace(requestID)
	requestHash = strings.TrimSpace(requestHash)
	if workerID <= 0 || requestID == "" || requestHash == "" {
		return nil, false, ErrNotFound
	}
	call, err := scanWorkerLLMCall(s.pool.QueryRow(ctx,
		`INSERT INTO worker_llm_calls (worker_id, request_id, request_hash)
		 VALUES ($1,$2,$3) ON CONFLICT (worker_id, request_id) DO NOTHING
		 RETURNING `+workerLLMCallCols, workerID, requestID, requestHash))
	if err == nil {
		return call, true, nil
	}
	if err != ErrNotFound {
		return nil, false, err
	}
	call, err = scanWorkerLLMCall(s.pool.QueryRow(ctx,
		`SELECT `+workerLLMCallCols+` FROM worker_llm_calls WHERE worker_id=$1 AND request_id=$2`,
		workerID, requestID))
	if err != nil {
		return nil, false, err
	}
	if call.RequestHash != requestHash {
		return call, false, ErrConflict
	}
	return call, false, nil
}

func (s *Store) CompleteWorkerLLMCall(ctx context.Context, workerID int64, requestID string, status int, response []byte) error {
	return s.execOne(ctx,
		`UPDATE worker_llm_calls
		    SET status='completed', http_status=$3, response_body=$4,
		        last_error='', completed_at=now(), updated_at=now()
		  WHERE worker_id=$1 AND request_id=$2 AND status='started'`,
		workerID, strings.TrimSpace(requestID), status, response)
}

func (s *Store) FailWorkerLLMCall(ctx context.Context, workerID int64, requestID, cause string) error {
	return s.execOne(ctx,
		`UPDATE worker_llm_calls
		    SET status='failed', last_error=$3, completed_at=now(), updated_at=now()
		  WHERE worker_id=$1 AND request_id=$2 AND status='started'`,
		workerID, strings.TrimSpace(requestID), truncateRunes(strings.TrimSpace(cause), 500))
}

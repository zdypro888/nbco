package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	memoryMiningLease       = 4 * time.Minute
	memoryMiningMaxAttempts = 5
)

type MemoryMiningJob struct {
	ID                 int64
	UserID             int64
	Channel            string
	SessionID          int64
	UserMessageID      int64
	AssistantMessageID int64
	ToolEvidence       string
	ExplicitCommit     bool
	Status             string
	Attempts           int
	AvailableAt        time.Time
	ClaimedAt          *time.Time
	LastError          string
	CreatedAt          time.Time
	CompletedAt        *time.Time
}

const memoryMiningJobCols = `id, user_id, channel, session_id, user_message_id, assistant_message_id, tool_evidence, explicit_commit, status, attempts, available_at, claimed_at, last_error, created_at, completed_at`

func scanMemoryMiningJob(row interface{ Scan(...any) error }) (*MemoryMiningJob, error) {
	var j MemoryMiningJob
	if err := row.Scan(&j.ID, &j.UserID, &j.Channel, &j.SessionID, &j.UserMessageID,
		&j.AssistantMessageID, &j.ToolEvidence, &j.ExplicitCommit, &j.Status, &j.Attempts, &j.AvailableAt,
		&j.ClaimedAt, &j.LastError, &j.CreatedAt, &j.CompletedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &j, nil
}

func (s *Store) EnqueueMemoryMiningJob(ctx context.Context, userID int64, channel string, sessionID, userMessageID, assistantMessageID int64, toolEvidence string, explicitCommit bool) error {
	if userID <= 0 || sessionID <= 0 || userMessageID <= 0 || assistantMessageID <= 0 {
		return ErrNotFound
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO memory_mining_jobs
		   (user_id, channel, session_id, user_message_id, assistant_message_id, tool_evidence, explicit_commit)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (session_id, user_message_id) DO NOTHING`,
		userID, strings.TrimSpace(channel), sessionID, userMessageID, assistantMessageID, toolEvidence, explicitCommit)
	return wrapErr(err)
}

func (s *Store) DueMemoryMiningJobs(ctx context.Context, limit int) ([]*MemoryMiningJob, error) {
	if limit <= 0 || limit > 32 {
		limit = 4
	}
	stale := time.Now().Add(-memoryMiningLease)
	if _, err := s.pool.Exec(ctx,
		`UPDATE memory_mining_jobs SET status='failed', claimed_at=NULL, completed_at=now(),
		        last_error=CASE WHEN last_error='' THEN 'retry budget exhausted after interrupted claim' ELSE last_error END
		  WHERE attempts >= $1 AND ((status='pending' AND available_at <= now())
		    OR (status='processing' AND claimed_at <= $2))`, memoryMiningMaxAttempts, stale); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`WITH due AS (
		   SELECT id FROM memory_mining_jobs
		    WHERE attempts < $2
		      AND ((status = 'pending' AND available_at <= now())
		        OR (status = 'processing' AND claimed_at <= now() - $3::interval))
		    ORDER BY available_at, id
		    LIMIT $1 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE memory_mining_jobs j
		    SET status = 'processing', attempts = attempts + 1, claimed_at = now()
		   FROM due WHERE j.id = due.id
		 RETURNING `+memoryMiningJobColsWithAlias("j"), limit, memoryMiningMaxAttempts, memoryMiningLease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MemoryMiningJob
	for rows.Next() {
		j, err := scanMemoryMiningJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func memoryMiningJobColsWithAlias(alias string) string {
	parts := strings.Split(memoryMiningJobCols, ", ")
	for i := range parts {
		parts[i] = alias + "." + parts[i]
	}
	return strings.Join(parts, ", ")
}

func (s *Store) CompleteMemoryMiningJob(ctx context.Context, id int64, claimAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE memory_mining_jobs
		    SET status = 'done', claimed_at = NULL, completed_at = now(), last_error = ''
		  WHERE id = $1 AND status = 'processing' AND claimed_at = $2`, id, claimAt)
}

func (s *Store) RetryMemoryMiningJob(ctx context.Context, id int64, claimAt time.Time, attempts int, cause string) error {
	cause = truncateRunes(cause, 2000)
	if attempts >= memoryMiningMaxAttempts {
		return s.execOne(ctx,
			`UPDATE memory_mining_jobs
			    SET status = 'failed', claimed_at = NULL, last_error = $3, completed_at = now()
			  WHERE id = $1 AND status = 'processing' AND claimed_at = $2`, id, claimAt, cause)
	}
	delay := time.Duration(1<<min(attempts, 6)) * 5 * time.Second
	return s.execOne(ctx,
		`UPDATE memory_mining_jobs
		    SET status = 'pending', available_at = now() + $3::interval,
		        claimed_at = NULL, last_error = $4, completed_at = NULL
		  WHERE id = $1 AND status = 'processing' AND claimed_at = $2`,
		id, claimAt, fmt.Sprintf("%d seconds", int(delay.Seconds())), cause)
}

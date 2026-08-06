package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	automationRunLease       = 10 * time.Minute
	automationRunMaxAttempts = 6

	AutomationOutcomeSucceeded = "succeeded"
	AutomationOutcomeNoChange  = "no_change"
	AutomationOutcomeFailed    = "failed"
	AutomationOutcomeUncertain = "uncertain"
)

type AutomationRun struct {
	AutomationKey string
	OccurrenceKey string
	SubjectID     int64
	Status        string
	Attempts      int
	AvailableAt   time.Time
	ClaimedAt     *time.Time
	CompletedAt   *time.Time
	LastError     string
	ActionStarted bool
	ResultText    string
	Outcome       string
	ExpiresAt     *time.Time
	UpdatedAt     time.Time
}

const automationRunCols = `automation_key, occurrence_key, subject_id, status, attempts, available_at, claimed_at, completed_at, last_error, action_started, result_text, outcome, expires_at, updated_at`

func scanAutomationRun(row interface{ Scan(...any) error }) (*AutomationRun, error) {
	var run AutomationRun
	if err := row.Scan(&run.AutomationKey, &run.OccurrenceKey, &run.SubjectID, &run.Status,
		&run.Attempts, &run.AvailableAt, &run.ClaimedAt, &run.CompletedAt, &run.LastError,
		&run.ActionStarted, &run.ResultText, &run.Outcome, &run.ExpiresAt, &run.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &run, nil
}

// BeginAutomationAction persists the no-replay boundary before a scheduled AI
// maintenance turn receives write/execute tools. A reclaimed run with this bit
// set must inspect current state instead of executing the original action again.
func (s *Store) BeginAutomationAction(ctx context.Context, run *AutomationRun) error {
	if run == nil || run.ClaimedAt == nil {
		return ErrNotFound
	}
	return s.execOne(ctx,
		`UPDATE automation_runs SET action_started=true, updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3
		   AND status='processing' AND claimed_at=$4 AND NOT action_started`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt)
}

// PrepareAutomationResult stores the generated report before notification. A
// transport retry then resends this exact text without rerunning the AI agent.
func (s *Store) PrepareAutomationResult(ctx context.Context, run *AutomationRun, result, outcome, cause string) error {
	if run == nil || run.ClaimedAt == nil {
		return ErrNotFound
	}
	switch outcome {
	case AutomationOutcomeSucceeded, AutomationOutcomeNoChange, AutomationOutcomeFailed, AutomationOutcomeUncertain:
	default:
		return fmt.Errorf("invalid automation outcome %q", outcome)
	}
	return s.execOne(ctx,
		`UPDATE automation_runs SET result_text=$5, outcome=$6, last_error=$7, updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3
		   AND status='processing' AND claimed_at=$4`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt,
		truncateRunes(result, 12000), outcome, truncateRunes(cause, 500))
}

// ClaimAutomationRun 创建并认领一个“自动化+周期+对象”执行单元。完成状态永久去重，
// processing 崩溃后按租约恢复，失败按 available_at 退避。
func (s *Store) ClaimAutomationRun(ctx context.Context, key, occurrence string, subjectID int64, now time.Time) (*AutomationRun, error) {
	return s.ClaimAutomationRunUntil(ctx, key, occurrence, subjectID, now, time.Time{})
}

// HasActiveAutomationRun reports whether a key currently owns a live lease.
// Stale processing rows are deliberately ignored so ClaimAutomationRunUntil
// can recover them after a crashed process.
func (s *Store) HasActiveAutomationRun(ctx context.Context, key string, now time.Time) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM automation_runs
			 WHERE automation_key=$1 AND status='processing' AND claimed_at > $2
		)`, key, now.Add(-automationRunLease)).Scan(&active)
	return active, wrapErr(err)
}

func (s *Store) AutomationRunByKey(ctx context.Context, key, occurrence string, subjectID int64) (*AutomationRun, error) {
	return scanAutomationRun(s.pool.QueryRow(ctx,
		`SELECT `+automationRunCols+` FROM automation_runs
		  WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3`,
		key, occurrence, subjectID))
}

// ClaimAutomationRunUntil also records the end of the occurrence's useful
// execution window. Expired rows become terminal instead of remaining visible
// as pending work forever.
func (s *Store) ClaimAutomationRunUntil(
	ctx context.Context,
	key, occurrence string,
	subjectID int64,
	now, expiresAt time.Time,
) (*AutomationRun, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var expiry any
	if !expiresAt.IsZero() {
		expiry = expiresAt
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO automation_runs (automation_key, occurrence_key, subject_id, available_at, expires_at)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (automation_key, occurrence_key, subject_id) DO UPDATE
		 SET expires_at=COALESCE(EXCLUDED.expires_at, automation_runs.expires_at), updated_at=now()
		 WHERE automation_runs.status IN ('pending','processing')
		    OR (automation_runs.status='failed' AND automation_runs.outcome='')`,
		key, occurrence, subjectID, now, expiry); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE automation_runs SET status='failed', claimed_at=NULL, completed_at=now(),
		        outcome=CASE WHEN outcome='' THEN 'failed' ELSE outcome END,
		        last_error=CASE WHEN last_error='' THEN 'automation occurrence expired' ELSE last_error END,
		        updated_at=now()
		  WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3
		    AND (status='pending' OR (status='processing' AND claimed_at <= $5))
		    AND expires_at IS NOT NULL AND expires_at <= $4`,
		key, occurrence, subjectID, now, now.Add(-automationRunLease)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE automation_runs SET status='failed', claimed_at=NULL, completed_at=now(),
		        outcome=CASE WHEN outcome='' THEN 'failed' ELSE outcome END,
		        last_error=CASE WHEN last_error='' THEN 'retry budget exhausted after interrupted claim' ELSE last_error END,
		        updated_at=now()
		  WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND attempts >= $6
		    AND ((status='pending' AND available_at <= $4)
		      OR (status='processing' AND claimed_at <= $5))`,
		key, occurrence, subjectID, now, now.Add(-automationRunLease), automationRunMaxAttempts); err != nil {
		return nil, err
	}
	run, err := scanAutomationRun(tx.QueryRow(ctx,
		`UPDATE automation_runs SET status='processing', claimed_at=$4, completed_at=NULL,
		        attempts=attempts+1, updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3
		   AND attempts < $6 AND available_at <= $4
		   AND (expires_at IS NULL OR expires_at > $4)
		   AND (status='pending' OR (status='failed' AND outcome='') OR (status='processing' AND claimed_at <= $5))
		 RETURNING `+automationRunCols,
		key, occurrence, subjectID, now, now.Add(-automationRunLease), automationRunMaxAttempts))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The transaction may have just moved an exhausted interrupted run
			// to failed. Commit that terminal cleanup even though there is no row
			// for this caller to execute.
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return nil, commitErr
			}
			return nil, ErrNotFound
		}
		return nil, err
	}
	return run, tx.Commit(ctx)
}

func (s *Store) CompleteAutomationRun(ctx context.Context, run *AutomationRun) error {
	if run == nil || run.ClaimedAt == nil {
		return ErrNotFound
	}
	return s.execOne(ctx,
		`UPDATE automation_runs
		    SET status=CASE WHEN outcome IN ('failed','uncertain') THEN 'failed' ELSE 'done' END,
		        claimed_at=NULL, completed_at=now(),
		        last_error=CASE WHEN outcome IN ('failed','uncertain') THEN last_error ELSE '' END,
		        updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND status='processing' AND claimed_at=$4`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt)
}

func (s *Store) RetryAutomationRun(ctx context.Context, run *AutomationRun, cause string) error {
	return s.retryAutomationRun(ctx, run, cause, false)
}

// RetryAutomationRunReplaySafe clears the no-replay boundary only after a
// completed Agent turn proved that no write/execute tool succeeded.
func (s *Store) RetryAutomationRunReplaySafe(ctx context.Context, run *AutomationRun, cause string) error {
	return s.retryAutomationRun(ctx, run, cause, true)
}

func (s *Store) retryAutomationRun(ctx context.Context, run *AutomationRun, cause string, replaySafe bool) error {
	if run == nil || run.ClaimedAt == nil {
		return ErrNotFound
	}
	cause = truncateRunes(cause, 500)
	if run.Attempts >= automationRunMaxAttempts {
		return s.execOne(ctx,
			`UPDATE automation_runs SET status='failed', claimed_at=NULL, available_at=now()+interval '24 hours',
			        outcome=CASE WHEN outcome='' THEN 'failed' ELSE outcome END,
			        last_error=$5, updated_at=now()
			 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND status='processing' AND claimed_at=$4`,
			run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt, cause)
	}
	delay := time.Duration(1<<min(run.Attempts, 6)) * time.Minute
	return s.execOne(ctx,
		`UPDATE automation_runs
		    SET status='pending', claimed_at=NULL, available_at=now()+$5::interval,
		        last_error=$6,
		        action_started=CASE WHEN $7 THEN false ELSE action_started END,
		        result_text=CASE WHEN $7 THEN '' ELSE result_text END,
		        outcome=CASE WHEN $7 THEN '' ELSE outcome END,
		        updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND status='processing' AND claimed_at=$4`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt,
		fmt.Sprintf("%d seconds", int(delay.Seconds())), cause, replaySafe)
}

func AutomationRunCanRetry(run *AutomationRun) bool {
	return run != nil && run.Attempts < automationRunMaxAttempts
}

// ExpireAutomationRuns is a scheduler-wide cleanup pass for occurrences whose
// owning calendar function will no longer visit them.
func (s *Store) ExpireAutomationRuns(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE automation_runs
		    SET status='failed', claimed_at=NULL, completed_at=now(),
		        outcome=CASE WHEN outcome='' THEN 'failed' ELSE outcome END,
		        last_error=CASE WHEN last_error='' THEN 'automation occurrence expired' ELSE last_error END,
		        updated_at=now()
		  WHERE (status='pending' OR (status='processing' AND claimed_at <= $2))
		    AND expires_at IS NOT NULL AND expires_at <= $1`, now, now.Add(-automationRunLease))
	if err != nil {
		return 0, wrapErr(err)
	}
	return tag.RowsAffected(), nil
}

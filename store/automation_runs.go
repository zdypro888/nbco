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
	UpdatedAt     time.Time
}

const automationRunCols = `automation_key, occurrence_key, subject_id, status, attempts, available_at, claimed_at, completed_at, last_error, action_started, result_text, updated_at`

func scanAutomationRun(row interface{ Scan(...any) error }) (*AutomationRun, error) {
	var run AutomationRun
	if err := row.Scan(&run.AutomationKey, &run.OccurrenceKey, &run.SubjectID, &run.Status,
		&run.Attempts, &run.AvailableAt, &run.ClaimedAt, &run.CompletedAt, &run.LastError,
		&run.ActionStarted, &run.ResultText, &run.UpdatedAt); err != nil {
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
func (s *Store) PrepareAutomationResult(ctx context.Context, run *AutomationRun, result string) error {
	if run == nil || run.ClaimedAt == nil {
		return ErrNotFound
	}
	return s.execOne(ctx,
		`UPDATE automation_runs SET result_text=$5, last_error='', updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3
		   AND status='processing' AND claimed_at=$4`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt, truncateRunes(result, 12000))
}

// ClaimAutomationRun 创建并认领一个“自动化+周期+对象”执行单元。完成状态永久去重，
// processing 崩溃后按租约恢复，失败按 available_at 退避。
func (s *Store) ClaimAutomationRun(ctx context.Context, key, occurrence string, subjectID int64, now time.Time) (*AutomationRun, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO automation_runs (automation_key, occurrence_key, subject_id, available_at)
		 VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, key, occurrence, subjectID, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE automation_runs SET status='failed', claimed_at=NULL, completed_at=now(),
		        last_error=CASE WHEN last_error='' THEN 'retry budget exhausted after interrupted claim' ELSE last_error END,
		        updated_at=now()
		  WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND attempts >= $6
		    AND ((status='pending' AND available_at <= $4)
		      OR (status='processing' AND claimed_at <= $5))`,
		key, occurrence, subjectID, now, now.Add(-automationRunLease), automationRunMaxAttempts); err != nil {
		return nil, err
	}
	run, err := scanAutomationRun(tx.QueryRow(ctx,
		`UPDATE automation_runs SET status='processing', claimed_at=$4, attempts=attempts+1, updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3
		   AND attempts < $6 AND available_at <= $4
		   AND (status='pending' OR status='failed' OR (status='processing' AND claimed_at <= $5))
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
		`UPDATE automation_runs SET status='done', claimed_at=NULL, completed_at=now(), last_error='', updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND status='processing' AND claimed_at=$4`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt)
}

func (s *Store) RetryAutomationRun(ctx context.Context, run *AutomationRun, cause string) error {
	if run == nil || run.ClaimedAt == nil {
		return ErrNotFound
	}
	cause = truncateRunes(cause, 500)
	if run.Attempts >= automationRunMaxAttempts {
		return s.execOne(ctx,
			`UPDATE automation_runs SET status='failed', claimed_at=NULL, available_at=now()+interval '24 hours', last_error=$5, updated_at=now()
			 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND status='processing' AND claimed_at=$4`,
			run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt, cause)
	}
	delay := time.Duration(1<<min(run.Attempts, 6)) * time.Minute
	return s.execOne(ctx,
		`UPDATE automation_runs SET status='pending', claimed_at=NULL, available_at=now()+$5::interval, last_error=$6, updated_at=now()
		 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3 AND status='processing' AND claimed_at=$4`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, *run.ClaimedAt,
		fmt.Sprintf("%d seconds", int(delay.Seconds())), cause)
}

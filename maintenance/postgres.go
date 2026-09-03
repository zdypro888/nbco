package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresLedger struct {
	pool *pgxpool.Pool
}

func NewPostgresLedger(pool *pgxpool.Pool) *PostgresLedger {
	return &PostgresLedger{pool: pool}
}

func (l *PostgresLedger) EnsureJobs(ctx context.Context, jobs []Job, now time.Time) error {
	if l == nil || l.pool == nil {
		return errors.New("maintenance database is unavailable")
	}
	for _, job := range jobs {
		_, err := l.pool.Exec(ctx, `
			INSERT INTO maintenance_jobs (name, class, description, interval_seconds, next_run_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (name) DO UPDATE
			SET class=EXCLUDED.class, description=EXCLUDED.description,
			    interval_seconds=EXCLUDED.interval_seconds, updated_at=now()`,
			job.Name, job.Class, job.Description, int64(job.Interval/time.Second), now)
		if err != nil {
			return fmt.Errorf("register %s: %w", job.Name, err)
		}
	}
	return nil
}

func (l *PostgresLedger) Claim(ctx context.Context, job Job, owner, trigger string, dryRun, force bool, now time.Time) (*Claim, error) {
	if l == nil || l.pool == nil {
		return nil, errors.New("maintenance database is unavailable")
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	leaseUntil := now.Add(job.Timeout + time.Minute)
	var name string
	var previousRunID int64
	err = tx.QueryRow(ctx, `
		UPDATE maintenance_jobs
		   SET lease_owner=$2, lease_until=$3,
		       last_status=CASE WHEN $6 THEN last_status ELSE 'running' END,
		       last_started_at=CASE WHEN $6 THEN last_started_at ELSE $4 END,
		       last_error=CASE WHEN $6 THEN last_error ELSE '' END,
		       updated_at=now()
		 WHERE name=$1
		   AND (lease_until IS NULL OR lease_until <= $4)
		   AND ($5 OR next_run_at <= $4)
		 RETURNING name, coalesce(last_run_id, 0)`, job.Name, owner, leaseUntil, now, force, dryRun).Scan(&name, &previousRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if previousRunID > 0 {
		abandoned, err := tx.Exec(ctx, `
			UPDATE maintenance_runs
			   SET status='failed', completed_at=$2, error='maintenance lease expired before completion'
			 WHERE id=$1 AND status='running'`, previousRunID, now)
		if err != nil {
			return nil, err
		}
		if abandoned.RowsAffected() == 1 {
			if _, err := tx.Exec(ctx, `UPDATE maintenance_jobs SET failure_count=failure_count+1 WHERE name=$1`, job.Name); err != nil {
				return nil, err
			}
		}
	}
	var runID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO maintenance_runs (job_name, class, trigger, dry_run, lease_owner, started_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		job.Name, job.Class, trigger, dryRun, owner, now).Scan(&runID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE maintenance_jobs SET last_run_id=$2 WHERE name=$1`, job.Name, runID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &Claim{RunID: runID, Job: job}, nil
}

func (l *PostgresLedger) Finish(ctx context.Context, claim Claim, owner string, dryRun bool, finished time.Time, result Result, runErr error) error {
	if l == nil || l.pool == nil {
		return errors.New("maintenance database is unavailable")
	}
	status := "succeeded"
	errText := ""
	if runErr != nil {
		status = "failed"
		errText = runErr.Error()
	}
	report := marshalResult(result)
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	runTag, err := tx.Exec(ctx, `
		UPDATE maintenance_runs
		   SET status=$2, completed_at=$3, report=$4, error=$5
		 WHERE id=$1 AND status='running' AND lease_owner=$6`,
		claim.RunID, status, finished, report, errText, owner)
	if err != nil {
		return err
	}
	if runTag.RowsAffected() != 1 {
		return errors.New("maintenance run ownership was lost before completion")
	}
	nextRun := finished.Add(claim.Job.Interval)
	if runErr != nil {
		nextRun = finished.Add(min(claim.Job.Interval, 10*time.Minute))
	}
	tag, err := tx.Exec(ctx, `
		UPDATE maintenance_jobs
		   SET lease_owner='', lease_until=NULL,
		       last_status=CASE WHEN $5 THEN last_status ELSE $3 END,
		       last_completed_at=CASE WHEN $5 THEN last_completed_at ELSE $4 END,
		       last_success_at=CASE WHEN $3='succeeded' AND NOT $5 THEN $4 ELSE last_success_at END,
		       last_report=CASE WHEN $5 THEN last_report ELSE $6 END,
		       last_error=CASE WHEN $5 THEN last_error ELSE $7 END,
		       run_count=run_count+1,
		       failure_count=failure_count+CASE WHEN $3='failed' THEN 1 ELSE 0 END,
		       next_run_at=CASE WHEN $5 THEN next_run_at ELSE $8 END,
		       updated_at=now()
		 WHERE name=$1 AND last_run_id=$2 AND lease_owner=$9`,
		claim.Job.Name, claim.RunID, status, finished, dryRun, report, errText, nextRun, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("maintenance lease ownership was lost before completion")
	}
	return tx.Commit(ctx)
}

func (l *PostgresLedger) Status(ctx context.Context, limit int) ([]JobState, []RunRecord, error) {
	if l == nil || l.pool == nil {
		return nil, nil, errors.New("maintenance database is unavailable")
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := l.pool.Query(ctx, `
		SELECT name, class, description, interval_seconds, next_run_at, lease_until,
		       last_status, last_started_at, last_completed_at, last_success_at,
		       last_report, last_error, run_count, failure_count
		  FROM maintenance_jobs ORDER BY name`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	jobs := []JobState{}
	for rows.Next() {
		var item JobState
		var report []byte
		if err := rows.Scan(&item.Name, &item.Class, &item.Description, &item.IntervalSeconds,
			&item.NextRunAt, &item.LeaseUntil, &item.LastStatus, &item.LastStartedAt,
			&item.LastCompletedAt, &item.LastSuccessAt, &report, &item.LastError,
			&item.RunCount, &item.FailureCount); err != nil {
			return nil, nil, err
		}
		_ = json.Unmarshal(report, &item.LastReport)
		jobs = append(jobs, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	runRows, err := l.pool.Query(ctx, `
		SELECT id, job_name, class, trigger, dry_run, status, started_at,
		       completed_at, report, error
		  FROM maintenance_runs ORDER BY started_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, nil, err
	}
	defer runRows.Close()
	runs := []RunRecord{}
	for runRows.Next() {
		var item RunRecord
		var report []byte
		if err := runRows.Scan(&item.ID, &item.JobName, &item.Class, &item.Trigger,
			&item.DryRun, &item.Status, &item.StartedAt, &item.CompletedAt,
			&report, &item.Error); err != nil {
			return nil, nil, err
		}
		_ = json.Unmarshal(report, &item.Report)
		runs = append(runs, item)
	}
	return jobs, runs, runRows.Err()
}

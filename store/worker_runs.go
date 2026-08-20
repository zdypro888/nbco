package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zdypro888/nbco/workerproto"
)

const (
	WorkerRunQueued        = "queued"
	WorkerRunClaimed       = "claimed"
	WorkerRunRetryWait     = "retry_wait"
	WorkerRunAwaitingInput = "awaiting_input"
	WorkerRunCompleted     = "completed"
	WorkerRunCancelled     = "cancelled"
)

// WorkerRun is one durable request for a worker to execute something. TaskID is
// optional: direct commands have an execution lifecycle without pretending to
// be business work, while delegated work links the run to its task.
type WorkerRun struct {
	ID           int64
	TaskID       *int64
	TaskRevision *int64
	LegacyTaskID *int64
	WorkerID     int64
	RequestedBy  int64
	Executor     workerproto.Executor
	Input        json.RawMessage
	Title        string
	Goal         string
	Description  string
	Acceptance   string
	ScopeType    string
	ScopeKey     string
	ScopeTitle   string
	Priority     string
	Status       string
	Outcome      string
	ExitCode     *int
	ClaimID      string
	ClaimedAt    *time.Time
	Attempts     int
	Failures     int
	AvailableAt  time.Time
	LastError    string
	Summary      string
	Lessons      string
	CompletedAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type WorkerRunSpec struct {
	TaskID       *int64
	TaskRevision *int64
	WorkerID     int64
	RequestedBy  int64
	Executor     workerproto.Executor
	Input        json.RawMessage
	Title        string
	Goal         string
	Description  string
	Acceptance   string
	ScopeType    string
	ScopeKey     string
	ScopeTitle   string
	Priority     string
	FileIDs      []int64
}

type WorkerCommandInput struct {
	Command string `json:"command"`
	PTY     bool   `json:"pty"`
}

type WorkerRunProgress struct {
	ID        int64
	RunID     int64
	AuthorID  int64
	Content   string
	CreatedAt time.Time
}

// WorkerRunFinalization identifies one lease-ending request. ID is stable
// across HTTP retries and Hash binds that ID to the exact normalized payload.
type WorkerRunFinalization struct {
	ID   string
	Hash string
}

const (
	workerFinalizationComplete = "complete"
	workerFinalizationFail     = "fail"
	workerFinalizationInput    = "request_input"
)

func normalizeWorkerRunFinalization(value WorkerRunFinalization) (WorkerRunFinalization, bool) {
	value.ID = strings.TrimSpace(value.ID)
	value.Hash = strings.TrimSpace(value.Hash)
	return value, value.ID != "" && value.Hash != ""
}

// lockWorkerRunTx establishes the repository-wide lock order for linked work:
// business task -> worker run -> attempt. Direct runs have no task lock. Every
// path that writes both execution state and task-owned rows must use this order
// so progress/finalization cannot deadlock with task edits or reassignment.
func lockWorkerRunTx(ctx context.Context, tx pgx.Tx, runID, workerID int64) (*int64, error) {
	var taskID *int64
	if err := tx.QueryRow(ctx,
		`SELECT task_id FROM worker_runs WHERE id = $1 AND worker_id = $2`,
		runID, workerID).Scan(&taskID); err != nil {
		return nil, wrapErr(err)
	}
	if taskID != nil {
		var lockedTaskID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id = $1 FOR UPDATE`, *taskID).Scan(&lockedTaskID); err != nil {
			return nil, wrapErr(err)
		}
	}
	var lockedTaskID *int64
	if err := tx.QueryRow(ctx,
		`SELECT task_id FROM worker_runs WHERE id = $1 AND worker_id = $2 FOR UPDATE`,
		runID, workerID).Scan(&lockedTaskID); err != nil {
		return nil, wrapErr(err)
	}
	if (taskID == nil) != (lockedTaskID == nil) || taskID != nil && *taskID != *lockedTaskID {
		return nil, ErrConflict
	}
	return lockedTaskID, nil
}

func workerAttemptWasFinalizedTx(ctx context.Context, tx pgx.Tx, runID, workerID int64, claimID, kind string, finalization WorkerRunFinalization) (bool, error) {
	var storedID, storedKind, storedHash, status string
	err := tx.QueryRow(ctx,
		`SELECT finalization_id, finalization_kind, finalization_hash, status
		 FROM worker_run_attempts
		 WHERE run_id = $1 AND worker_id = $2 AND claim_id = $3
		 FOR UPDATE`, runID, workerID, claimID).
		Scan(&storedID, &storedKind, &storedHash, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if status == "claimed" && storedID == "" {
		return false, nil
	}
	if storedID == "" || storedID != finalization.ID {
		return false, ErrNotFound
	}
	if storedKind != kind || storedHash != finalization.Hash || status == "claimed" {
		return false, ErrConflict
	}
	return true, nil
}

func finalizeWorkerAttemptTx(ctx context.Context, tx pgx.Tx, runID, workerID int64, claimID, status, kind string, finalization WorkerRunFinalization, outcome workerproto.Outcome, exitCode *int, failure, summary, lessons string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE worker_run_attempts SET status = $5, finalization_id = $6,
		   finalization_kind = $7, finalization_hash = $8, outcome = $9,
		   exit_code = $10, error = $11, summary = $12, lessons = $13,
		   finished_at = now(), updated_at = now()
		 WHERE run_id = $1 AND worker_id = $2 AND claim_id = $3 AND status = $4`,
		runID, workerID, claimID, "claimed", status, finalization.ID, kind, finalization.Hash,
		string(outcome), exitCode, failure, summary, lessons)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func taskForWorkerRunTx(ctx context.Context, tx pgx.Tx, run *WorkerRun) (*Task, error) {
	if run == nil || run.TaskID == nil {
		return nil, nil
	}
	return scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1`, *run.TaskID))
}

const workerRunCols = `id, task_id, task_revision, legacy_task_id, worker_id, requested_by, executor, input,
title, goal, description, acceptance, scope_type, scope_key, scope_title, priority,
status, outcome, exit_code, claim_id, claimed_at, attempts, failure_count, available_at, last_error,
summary, lessons, completed_at, created_at, updated_at`

func scanWorkerRun(row interface{ Scan(...any) error }) (*WorkerRun, error) {
	var run WorkerRun
	var executor string
	if err := row.Scan(
		&run.ID, &run.TaskID, &run.TaskRevision, &run.LegacyTaskID, &run.WorkerID, &run.RequestedBy, &executor, &run.Input,
		&run.Title, &run.Goal, &run.Description, &run.Acceptance, &run.ScopeType, &run.ScopeKey, &run.ScopeTitle,
		&run.Priority, &run.Status, &run.Outcome, &run.ExitCode, &run.ClaimID, &run.ClaimedAt, &run.Attempts, &run.Failures,
		&run.AvailableAt, &run.LastError, &run.Summary, &run.Lessons, &run.CompletedAt, &run.CreatedAt,
		&run.UpdatedAt,
	); err != nil {
		return nil, wrapErr(err)
	}
	run.Executor = workerproto.Executor(executor)
	return &run, nil
}

func normalizeWorkerRunSpec(spec *WorkerRunSpec) bool {
	if spec == nil {
		return false
	}
	if spec.Executor == "" {
		spec.Executor = workerproto.ExecutorAgent
	}
	if !spec.Executor.Valid() || spec.WorkerID <= 0 || spec.RequestedBy <= 0 {
		return false
	}
	if spec.TaskID != nil && (spec.TaskRevision == nil || *spec.TaskRevision <= 0) {
		return false
	}
	spec.Title = strings.TrimSpace(spec.Title)
	spec.Goal = strings.TrimSpace(spec.Goal)
	spec.Description = strings.TrimSpace(spec.Description)
	spec.Acceptance = strings.TrimSpace(spec.Acceptance)
	spec.ScopeType = strings.TrimSpace(spec.ScopeType)
	spec.ScopeKey = strings.TrimSpace(spec.ScopeKey)
	spec.ScopeTitle = strings.TrimSpace(spec.ScopeTitle)
	if spec.Title == "" {
		return false
	}
	if spec.ScopeKey == "" {
		if spec.TaskID != nil {
			spec.ScopeType = "task"
			spec.ScopeKey = fmt.Sprintf("task:%d", *spec.TaskID)
		} else {
			spec.ScopeType = "run"
			spec.ScopeKey = fmt.Sprintf("worker:%d", spec.WorkerID)
		}
	}
	if spec.ScopeType == "" {
		spec.ScopeType = "custom"
	}
	if spec.ScopeTitle == "" {
		spec.ScopeTitle = spec.ScopeKey
	}
	if strings.TrimSpace(spec.Priority) == "" {
		spec.Priority = "normal"
	}
	if len(spec.Input) == 0 {
		spec.Input = json.RawMessage(`{}`)
	}
	if spec.Executor == workerproto.ExecutorCommand {
		var input WorkerCommandInput
		if err := json.Unmarshal(spec.Input, &input); err != nil {
			return false
		}
		input.Command = strings.TrimSpace(input.Command)
		if input.Command == "" {
			return false
		}
		spec.Input, _ = json.Marshal(input)
	}
	for _, fileID := range spec.FileIDs {
		if fileID <= 0 {
			return false
		}
	}
	return json.Valid(spec.Input)
}

func (s *Store) CreateWorkerRun(ctx context.Context, spec WorkerRunSpec) (*WorkerRun, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := createWorkerRunTx(ctx, tx, &spec)
	if err != nil {
		return nil, err
	}
	return run, tx.Commit(ctx)
}

func createWorkerRunTx(ctx context.Context, tx pgx.Tx, spec *WorkerRunSpec) (*WorkerRun, error) {
	if !normalizeWorkerRunSpec(spec) {
		return nil, ErrConflict
	}
	var active bool
	if err := tx.QueryRow(ctx,
		`SELECT status = 'active' AND is_worker FROM users WHERE id = $1 FOR SHARE`, spec.WorkerID).Scan(&active); err != nil {
		return nil, wrapErr(err)
	}
	if !active {
		return nil, ErrNotFound
	}
	run, err := scanWorkerRun(tx.QueryRow(ctx,
		`INSERT INTO worker_runs (
		   task_id, task_revision, worker_id, requested_by, executor, input, title, goal, description,
		   acceptance, scope_type, scope_key, scope_title, priority
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 RETURNING `+workerRunCols,
		spec.TaskID, spec.TaskRevision, spec.WorkerID, spec.RequestedBy, string(spec.Executor), spec.Input,
		spec.Title, spec.Goal, spec.Description, spec.Acceptance, spec.ScopeType,
		spec.ScopeKey, spec.ScopeTitle, spec.Priority))
	if err != nil {
		return nil, err
	}
	for _, fileID := range uniquePositiveIDs(spec.FileIDs) {
		tag, err := tx.Exec(ctx,
			`INSERT INTO worker_run_files (run_id, file_id, role, caption, created_by)
			 SELECT $1, id, 'input', '', $3 FROM files WHERE id = $2
			 ON CONFLICT DO NOTHING`, run.ID, fileID, spec.RequestedBy)
		if err != nil {
			return nil, wrapErr(err)
		}
		if tag.RowsAffected() == 0 {
			return nil, ErrNotFound
		}
	}
	return run, nil
}

func uniquePositiveIDs(values []int64) []int64 {
	seen := make(map[int64]bool, len(values))
	out := make([]int64, 0, len(values))
	for _, id := range values {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func defaultWorkerRunSpec(task *Task) WorkerRunSpec {
	return WorkerRunSpec{
		TaskID: &task.ID, TaskRevision: &task.Revision, WorkerID: task.AssigneeID, RequestedBy: task.AssignerID,
		Executor: workerproto.ExecutorAgent, Title: task.Title, Goal: task.Goal,
		Description: task.Description, Acceptance: task.Acceptance, Priority: task.Priority,
		ScopeType: "project", ScopeKey: fmt.Sprintf("project:%d", task.ProjectID),
		ScopeTitle: fmt.Sprintf("Project %d", task.ProjectID),
	}
}

// enqueueTaskWorkerRunTx creates execution only when the assignee is an active
// worker. Human tasks remain pure task rows and never enter the worker queue.
func enqueueTaskWorkerRunTx(ctx context.Context, tx pgx.Tx, task *Task, override *WorkerRunSpec) (*WorkerRun, error) {
	var activeWorker bool
	if err := tx.QueryRow(ctx,
		`SELECT status = 'active' AND is_worker FROM users WHERE id = $1`, task.AssigneeID).Scan(&activeWorker); err != nil {
		return nil, wrapErr(err)
	}
	if !activeWorker {
		return nil, nil
	}
	spec := defaultWorkerRunSpec(task)
	if override != nil {
		if override.Executor != "" {
			spec.Executor = override.Executor
		}
		if len(override.Input) > 0 {
			spec.Input = override.Input
		}
		if strings.TrimSpace(override.ScopeType) != "" {
			spec.ScopeType = override.ScopeType
		}
		if strings.TrimSpace(override.ScopeKey) != "" {
			spec.ScopeKey = override.ScopeKey
		}
		if strings.TrimSpace(override.ScopeTitle) != "" {
			spec.ScopeTitle = override.ScopeTitle
		}
		spec.FileIDs = override.FileIDs
	}
	defaultProjectTitle := fmt.Sprintf("Project %d", task.ProjectID)
	if spec.ScopeType == "project" && spec.ScopeKey == fmt.Sprintf("project:%d", task.ProjectID) &&
		(spec.ScopeTitle == "" || spec.ScopeTitle == defaultProjectTitle || spec.ScopeTitle == spec.ScopeKey) {
		var projectName string
		if err := tx.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, task.ProjectID).Scan(&projectName); err != nil {
			return nil, wrapErr(err)
		}
		if projectName = strings.TrimSpace(projectName); projectName != "" {
			spec.ScopeTitle = projectName
		}
	}
	return createWorkerRunTx(ctx, tx, &spec)
}

func (s *Store) WorkerRunByID(ctx context.Context, id int64) (*WorkerRun, error) {
	return scanWorkerRun(s.pool.QueryRow(ctx, `SELECT `+workerRunCols+` FROM worker_runs WHERE id = $1`, id))
}

// ResolveWorkerRunID accepts the current run id and the pre-0064 task id during
// a rolling upgrade. The exact worker and claim keep the compatibility lookup
// from widening authorization.
func (s *Store) ResolveWorkerRunID(ctx context.Context, candidate, workerID int64, claimID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM worker_runs
		 WHERE (id = $1 OR legacy_task_id = $1) AND worker_id = $2
		   AND status = 'claimed' AND claim_id = $3`, candidate, workerID, claimID).Scan(&id)
	return id, wrapErr(err)
}

// ResolveWorkerRunIDForFinalization also resolves a completed attempt when an
// HTTP response was lost and the worker retries the same finalization ID.
func (s *Store) ResolveWorkerRunIDForFinalization(ctx context.Context, candidate, workerID int64, claimID, finalizationID string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT r.id FROM worker_runs r
		 WHERE (r.id = $1 OR r.legacy_task_id = $1) AND r.worker_id = $2
		   AND (
		     (r.status = 'claimed' AND r.claim_id = $3)
		     OR EXISTS (
		       SELECT 1 FROM worker_run_attempts a
		        WHERE a.run_id = r.id AND a.worker_id = $2 AND a.claim_id = $3
		          AND a.finalization_id = $4 AND a.finalization_id <> ''
		     )
		   )`, candidate, workerID, claimID, strings.TrimSpace(finalizationID)).Scan(&id)
	return id, wrapErr(err)
}

func (s *Store) ActiveWorkerRunForTask(ctx context.Context, taskID int64) (*WorkerRun, error) {
	return scanWorkerRun(s.pool.QueryRow(ctx,
		`SELECT `+workerRunCols+` FROM worker_runs
		 WHERE task_id = $1 AND status IN ('queued','claimed','retry_wait','awaiting_input')
		 ORDER BY id DESC LIMIT 1`, taskID))
}

func (s *Store) LatestWorkerRunForTask(ctx context.Context, taskID int64) (*WorkerRun, error) {
	return scanWorkerRun(s.pool.QueryRow(ctx,
		`SELECT `+workerRunCols+` FROM worker_runs WHERE task_id = $1 ORDER BY id DESC LIMIT 1`, taskID))
}

func (s *Store) LatestWorkerRunsForTasks(ctx context.Context, taskIDs []int64) (map[int64]*WorkerRun, error) {
	out := make(map[int64]*WorkerRun, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (task_id) `+workerRunCols+`
		 FROM worker_runs WHERE task_id = ANY($1)
		 ORDER BY task_id, id DESC`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		run, err := scanWorkerRun(rows)
		if err != nil {
			return nil, err
		}
		if run.TaskID != nil {
			out[*run.TaskID] = run
		}
	}
	return out, rows.Err()
}

// ClaimNextWorkerRun atomically leases one execution. Task dependencies gate
// linked runs, but direct runs have no artificial task row or review state.
func (s *Store) ClaimNextWorkerRun(ctx context.Context, workerID int64) (*WorkerRun, error) {
	staleBefore := time.Now().UTC().Add(-workerClaimTimeout)
	undeliveredBefore := time.Now().UTC().Add(-workerClaimDeliveryTimeout)
	claimID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Polling clients for one worker are serialized independently of business
	// rows. A users-row lock would invert the task -> worker validation order
	// used by task creation/reassignment and can deadlock those writers.
	claimKey := fmt.Sprintf("worker-run-claim:%d", workerID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, claimKey); err != nil {
		return nil, err
	}
	var active bool
	if err := tx.QueryRow(ctx,
		`SELECT status = 'active' AND is_worker FROM users WHERE id = $1`, workerID).Scan(&active); err != nil {
		return nil, wrapErr(err)
	}
	if !active {
		return nil, ErrNotFound
	}
	// Reconcile any orphaned active execution whose business requirement was
	// revised by a writer that did not know about the worker queue. Lock tasks
	// first, then runs and attempts, matching every normal task mutation.
	mismatchTasks, err := tx.Query(ctx,
		`SELECT t.id FROM tasks t
		 JOIN worker_runs r ON r.task_id = t.id
		 WHERE r.worker_id = $1
		   AND r.task_revision IS DISTINCT FROM t.revision
		   AND r.status IN ('queued','claimed','retry_wait','awaiting_input')
		 ORDER BY t.id FOR UPDATE OF t`, workerID)
	if err != nil {
		return nil, err
	}
	for mismatchTasks.Next() {
		var taskID int64
		if err := mismatchTasks.Scan(&taskID); err != nil {
			mismatchTasks.Close()
			return nil, err
		}
	}
	if err := mismatchTasks.Err(); err != nil {
		mismatchTasks.Close()
		return nil, err
	}
	mismatchTasks.Close()
	cancelledRows, err := tx.Query(ctx,
		`UPDATE worker_runs r SET status = 'cancelled', claim_id = '', claimed_at = NULL,
		   last_error = '任务要求版本已变化', completed_at = now(), updated_at = now()
		 FROM tasks t
		 WHERE r.task_id = t.id AND r.worker_id = $1
		   AND r.task_revision IS DISTINCT FROM t.revision
		   AND r.status IN ('queued','claimed','retry_wait','awaiting_input')
		 RETURNING r.id`, workerID)
	if err != nil {
		return nil, err
	}
	var cancelledIDs []int64
	for cancelledRows.Next() {
		var id int64
		if err := cancelledRows.Scan(&id); err != nil {
			cancelledRows.Close()
			return nil, err
		}
		cancelledIDs = append(cancelledIDs, id)
	}
	if err := cancelledRows.Err(); err != nil {
		cancelledRows.Close()
		return nil, err
	}
	cancelledRows.Close()
	if len(cancelledIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE worker_run_attempts SET status = 'cancelled', finished_at = now(), updated_at = now()
			 WHERE run_id = ANY($1) AND status = 'claimed'`, cancelledIDs); err != nil {
			return nil, err
		}
	}

	// The advisory lock serializes pollers for this worker, so the candidate
	// can be discovered without locking a run first. For linked work we then
	// lock its task before revalidating and locking the run.
	candidate, err := scanWorkerRun(tx.QueryRow(ctx,
		`SELECT `+workerRunCols+` FROM worker_runs r
		 WHERE r.worker_id = $1
		   AND (
		     (r.status IN ('queued','retry_wait') AND r.available_at <= now())
		     OR (r.status = 'claimed' AND (
		       r.claimed_at <= $2
		       OR (r.claimed_at <= $3 AND NOT EXISTS (
		         SELECT 1 FROM worker_run_attempts lease
		          WHERE lease.run_id = r.id AND lease.claim_id = r.claim_id
		            AND lease.status = 'claimed' AND lease.heartbeat_at IS NOT NULL
		       ))
		     ))
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM worker_runs active
		      WHERE active.worker_id = $1 AND active.status = 'claimed'
		        AND active.claimed_at > $2 AND active.claim_id <> ''
		        AND (
		          active.claimed_at > $3
		          OR EXISTS (
		            SELECT 1 FROM worker_run_attempts active_lease
		             WHERE active_lease.run_id = active.id AND active_lease.claim_id = active.claim_id
		               AND active_lease.status = 'claimed' AND active_lease.heartbeat_at IS NOT NULL
		          )
		        )
		   )
		   AND (
		     r.task_id IS NULL OR EXISTS (
		       SELECT 1 FROM tasks t
		        WHERE t.id = r.task_id AND t.assignee_id = $1
		          AND t.revision = r.task_revision
		          AND t.status IN ('pending','in_progress')
		          AND NOT EXISTS (
		            SELECT 1 FROM tasks d WHERE d.id = ANY(t.depends_on) AND d.status <> 'accepted'
		          )
		     )
		   )
		 ORDER BY (r.status = 'claimed') DESC, (r.priority = 'high') DESC, r.available_at, r.id
		 LIMIT 1`, workerID, staleBefore, undeliveredBefore))
	if err != nil {
		return nil, err
	}
	if candidate.TaskID != nil {
		var taskID int64
		if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id = $1 FOR UPDATE`, *candidate.TaskID).Scan(&taskID); err != nil {
			return nil, wrapErr(err)
		}
	}
	run, err := scanWorkerRun(tx.QueryRow(ctx,
		`SELECT `+workerRunCols+` FROM worker_runs r
		 WHERE r.id = $3 AND r.worker_id = $1
		   AND (
		     (r.status IN ('queued','retry_wait') AND r.available_at <= now())
		     OR (r.status = 'claimed' AND (
		       r.claimed_at <= $2
		       OR (r.claimed_at <= $4 AND NOT EXISTS (
		         SELECT 1 FROM worker_run_attempts lease
		          WHERE lease.run_id = r.id AND lease.claim_id = r.claim_id
		            AND lease.status = 'claimed' AND lease.heartbeat_at IS NOT NULL
		       ))
		     ))
		   )
		   AND NOT EXISTS (
		     SELECT 1 FROM worker_runs active
		      WHERE active.worker_id = $1 AND active.status = 'claimed'
		        AND active.claimed_at > $2 AND active.claim_id <> ''
		        AND (
		          active.claimed_at > $4
		          OR EXISTS (
		            SELECT 1 FROM worker_run_attempts active_lease
		             WHERE active_lease.run_id = active.id AND active_lease.claim_id = active.claim_id
		               AND active_lease.status = 'claimed' AND active_lease.heartbeat_at IS NOT NULL
		          )
		        )
		   )
		   AND (
		     r.task_id IS NULL OR EXISTS (
		       SELECT 1 FROM tasks t
		        WHERE t.id = r.task_id AND t.assignee_id = $1
		          AND t.revision = r.task_revision
		          AND t.status IN ('pending','in_progress')
		          AND NOT EXISTS (
		            SELECT 1 FROM tasks d WHERE d.id = ANY(t.depends_on) AND d.status <> 'accepted'
		          )
		     )
		   )
		 FOR UPDATE`, workerID, staleBefore, candidate.ID, undeliveredBefore))
	if err != nil {
		return nil, err
	}
	if (candidate.TaskID == nil) != (run.TaskID == nil) || candidate.TaskID != nil && *candidate.TaskID != *run.TaskID {
		return nil, ErrConflict
	}
	if run.Status == WorkerRunClaimed && run.ClaimID != "" {
		if _, err := tx.Exec(ctx,
			`UPDATE worker_run_attempts SET status = 'expired', finished_at = now(), updated_at = now()
			 WHERE run_id = $1 AND claim_id = $2 AND status = 'claimed'`, run.ID, run.ClaimID); err != nil {
			return nil, err
		}
	}
	if run.TaskID != nil {
		tag, err := tx.Exec(ctx,
			`UPDATE tasks SET status = 'in_progress', updated_at = now()
			 WHERE id = $1 AND assignee_id = $2 AND revision = $3
			   AND status IN ('pending','in_progress')`, *run.TaskID, workerID, run.TaskRevision)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, ErrNotFound
		}
	}
	run, err = scanWorkerRun(tx.QueryRow(ctx,
		`UPDATE worker_runs SET status = 'claimed', claim_id = $2, claimed_at = now(),
		   attempts = attempts + 1, available_at = now(), updated_at = now()
		 WHERE id = $1 RETURNING `+workerRunCols, run.ID, claimID))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_run_attempts (run_id, attempt_no, worker_id, claim_id)
		 VALUES ($1,$2,$3,$4)`, run.ID, run.Attempts, workerID, run.ClaimID); err != nil {
		return nil, wrapErr(err)
	}
	if run.ScopeType == "materials" && run.TaskID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE material_cases SET status = 'processing', worker_run_id = $2,
			 last_error = '', updated_at = now()
			 WHERE task_id = $1 AND status IN ('queued','processing')`, *run.TaskID, run.ID); err != nil {
			return nil, wrapErr(err)
		}
	}
	return run, tx.Commit(ctx)
}

func (s *Store) ReleaseWorkerRunClaim(ctx context.Context, runID, workerID int64, claimID string) error {
	if strings.TrimSpace(claimID) == "" {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx,
		`UPDATE worker_runs SET status = 'queued', claim_id = '', claimed_at = NULL,
		   available_at = now(), updated_at = now()
		 WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3`,
		runID, workerID, claimID)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	attemptTag, err := tx.Exec(ctx,
		`UPDATE worker_run_attempts SET status = 'released', finished_at = now(), updated_at = now()
		 WHERE run_id = $1 AND worker_id = $2 AND claim_id = $3 AND status = 'claimed'`, runID, workerID, claimID)
	if err != nil {
		return wrapErr(err)
	}
	if attemptTag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx,
		`UPDATE material_cases SET status = 'queued', updated_at = now()
		 WHERE worker_run_id = $1 AND status = 'processing'`, runID); err != nil {
		return wrapErr(err)
	}
	return tx.Commit(ctx)
}

// HeartbeatWorkerRun renews the exact execution lease. Both the run projection
// and immutable attempt history record the heartbeat in one transaction.
func (s *Store) HeartbeatWorkerRun(ctx context.Context, runID, workerID int64, claimID string) error {
	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx,
		`UPDATE worker_runs SET claimed_at = now(), updated_at = now()
		 WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3`, runID, workerID, claimID)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	attemptTag, err := tx.Exec(ctx,
		`UPDATE worker_run_attempts SET heartbeat_at = now(), updated_at = now()
		 WHERE run_id = $1 AND worker_id = $2 AND claim_id = $3 AND status = 'claimed'`, runID, workerID, claimID)
	if err != nil {
		return wrapErr(err)
	}
	if attemptTag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func (s *Store) AddWorkerRunProgress(ctx context.Context, runID, workerID int64, claimID, requestID, content string) (bool, error) {
	if strings.TrimSpace(claimID) == "" {
		return false, ErrNotFound
	}
	requestID = strings.TrimSpace(requestID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	taskID, err := lockWorkerRunTx(ctx, tx, runID, workerID)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE worker_runs SET claimed_at = now(), updated_at = now()
		 WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3
		`, runID, workerID, claimID); err != nil {
		return false, wrapErr(err)
	}
	attemptTag, err := tx.Exec(ctx,
		`UPDATE worker_run_attempts SET heartbeat_at = now(), updated_at = now()
		 WHERE run_id = $1 AND worker_id = $2 AND claim_id = $3 AND status = 'claimed'`, runID, workerID, claimID)
	if err != nil {
		return false, wrapErr(err)
	}
	if attemptTag.RowsAffected() == 0 {
		return false, ErrNotFound
	}
	progressTag, err := tx.Exec(ctx,
		`INSERT INTO worker_run_progress (run_id, author_id, content, request_id)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (run_id, request_id) WHERE request_id <> '' DO NOTHING`,
		runID, workerID, content, requestID)
	if err != nil {
		return false, wrapErr(err)
	}
	inserted := progressTag.RowsAffected() == 1
	if !inserted && requestID != "" {
		var existing string
		if err := tx.QueryRow(ctx,
			`SELECT content FROM worker_run_progress WHERE run_id=$1 AND request_id=$2`, runID, requestID).
			Scan(&existing); err != nil {
			return false, wrapErr(err)
		}
		if existing != content {
			return false, ErrConflict
		}
	}
	if taskID != nil && inserted {
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1,$2,$3)`,
			*taskID, workerID, content); err != nil {
			return false, wrapErr(err)
		}
	}
	return inserted, tx.Commit(ctx)
}

func (s *Store) RequestWorkerRunInput(ctx context.Context, runID, workerID int64, claimID, content string, finalization WorkerRunFinalization) (*WorkerRun, *Task, bool, error) {
	claimID = strings.TrimSpace(claimID)
	content = strings.TrimSpace(content)
	var ok bool
	if finalization, ok = normalizeWorkerRunFinalization(finalization); claimID == "" || content == "" || !ok {
		return nil, nil, false, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockWorkerRunTx(ctx, tx, runID, workerID); err != nil {
		return nil, nil, false, err
	}
	replayed, err := workerAttemptWasFinalizedTx(ctx, tx, runID, workerID, claimID, workerFinalizationInput, finalization)
	if err != nil {
		return nil, nil, false, err
	}
	if replayed {
		run, err := scanWorkerRun(tx.QueryRow(ctx,
			`SELECT `+workerRunCols+` FROM worker_runs WHERE id = $1 AND worker_id = $2`, runID, workerID))
		if err != nil {
			return nil, nil, false, err
		}
		task, err := taskForWorkerRunTx(ctx, tx, run)
		if err != nil {
			return nil, nil, false, err
		}
		return run, task, true, tx.Commit(ctx)
	}
	if err := finalizeWorkerAttemptTx(ctx, tx, runID, workerID, claimID,
		WorkerRunAwaitingInput, workerFinalizationInput, finalization, "", nil, "", "", ""); err != nil {
		return nil, nil, false, err
	}
	run, err := scanWorkerRun(tx.QueryRow(ctx,
		`UPDATE worker_runs SET status = 'awaiting_input', claim_id = '', claimed_at = NULL,
		   last_error = '', updated_at = now()
		 WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3
		 RETURNING `+workerRunCols, runID, workerID, claimID))
	if err != nil {
		return nil, nil, false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_run_progress (run_id, author_id, content) VALUES ($1,$2,$3)`,
		run.ID, workerID, "❓ 需要补充信息："+content); err != nil {
		return nil, nil, false, wrapErr(err)
	}
	var task *Task
	if run.TaskID != nil {
		task, err = scanTask(tx.QueryRow(ctx,
			`UPDATE tasks SET status = 'awaiting_input', updated_at = now()
			 WHERE id = $1 AND assignee_id = $2 AND revision = $3 AND status = 'in_progress'
			 RETURNING `+taskCols, *run.TaskID, workerID, run.TaskRevision))
		if err != nil {
			return nil, nil, false, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1,$2,$3)`,
			*run.TaskID, workerID, "❓ 需要补充信息："+content); err != nil {
			return nil, nil, false, wrapErr(err)
		}
	}
	if run.ScopeType == "materials" && task != nil {
		if err := setMaterialTaskStateTx(ctx, tx, task.ID, MaterialNeedsInput, content); err != nil {
			return nil, nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	return run, task, false, nil
}

func (s *Store) FailWorkerRun(ctx context.Context, runID, workerID int64, claimID, cause string, finalization WorkerRunFinalization) (*WorkerRun, *Task, bool, error) {
	claimID = strings.TrimSpace(claimID)
	cause = strings.TrimSpace(cause)
	var ok bool
	if finalization, ok = normalizeWorkerRunFinalization(finalization); claimID == "" || cause == "" || !ok {
		return nil, nil, false, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockWorkerRunTx(ctx, tx, runID, workerID); err != nil {
		return nil, nil, false, err
	}
	replayed, err := workerAttemptWasFinalizedTx(ctx, tx, runID, workerID, claimID, workerFinalizationFail, finalization)
	if err != nil {
		return nil, nil, false, err
	}
	if replayed {
		run, err := scanWorkerRun(tx.QueryRow(ctx,
			`SELECT `+workerRunCols+` FROM worker_runs WHERE id = $1 AND worker_id = $2`, runID, workerID))
		if err != nil {
			return nil, nil, false, err
		}
		task, err := taskForWorkerRunTx(ctx, tx, run)
		if err != nil {
			return nil, nil, false, err
		}
		return run, task, true, tx.Commit(ctx)
	}
	current, err := scanWorkerRun(tx.QueryRow(ctx,
		`SELECT `+workerRunCols+` FROM worker_runs
		 WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3 FOR UPDATE`,
		runID, workerID, claimID))
	if err != nil {
		return nil, nil, false, err
	}
	failureCount := current.Failures + 1
	blocked := failureCount >= workerMaxFailures
	retryAt := time.Now().UTC()
	status := WorkerRunAwaitingInput
	if !blocked {
		status = WorkerRunRetryWait
		retryAt = retryAt.Add(workerFailureBackoff(failureCount))
	}
	if err := finalizeWorkerAttemptTx(ctx, tx, runID, workerID, claimID,
		status, workerFinalizationFail, finalization, "", nil, truncateRunes(cause, 2000), "", ""); err != nil {
		return nil, nil, false, err
	}
	run, err := scanWorkerRun(tx.QueryRow(ctx,
		`UPDATE worker_runs SET status = $4, claim_id = '', claimed_at = NULL,
		   failure_count = $5, available_at = $6, last_error = $7, updated_at = now()
		 WHERE id = $1 AND worker_id = $2 AND claim_id = $3 RETURNING `+workerRunCols,
		runID, workerID, claimID, status, failureCount, retryAt, truncateRunes(cause, 2000)))
	if err != nil {
		return nil, nil, false, err
	}
	message := fmt.Sprintf("⚠️ Worker 执行失败（第 %d/%d 次）：%s", failureCount, workerMaxFailures, truncateRunes(cause, 1000))
	if blocked {
		message += "；已暂停，等待发起人处理。"
	} else {
		message += "；将在 " + retryAt.Format(time.RFC3339) + " 后自动重试。"
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_run_progress (run_id, author_id, content) VALUES ($1,$2,$3)`, runID, workerID, message); err != nil {
		return nil, nil, false, wrapErr(err)
	}
	var task *Task
	if run.TaskID != nil {
		taskStatus := TaskPending
		if blocked {
			taskStatus = TaskAwaitingInput
		}
		task, err = scanTask(tx.QueryRow(ctx,
			`UPDATE tasks SET status = $3, updated_at = now()
			 WHERE id = $1 AND assignee_id = $2 AND revision = $4 AND status = 'in_progress'
			 RETURNING `+taskCols, *run.TaskID, workerID, taskStatus, run.TaskRevision))
		if err != nil {
			return nil, nil, false, err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1,$2,$3)`,
			*run.TaskID, workerID, message); err != nil {
			return nil, nil, false, wrapErr(err)
		}
	}
	if run.ScopeType == "materials" && task != nil {
		materialStatus := MaterialQueued
		if run.Status == WorkerRunAwaitingInput {
			materialStatus = MaterialNeedsInput
		}
		if err := setMaterialTaskStateTx(ctx, tx, task.ID, materialStatus, cause); err != nil {
			return nil, nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	return run, task, false, nil
}

func (s *Store) CompleteWorkerRun(ctx context.Context, runID, workerID int64, claimID, summary, lessons string, outcome workerproto.Outcome, exitCode *int, finalization WorkerRunFinalization) (*WorkerRun, *Task, []*Task, bool, error) {
	claimID = strings.TrimSpace(claimID)
	var ok bool
	if finalization, ok = normalizeWorkerRunFinalization(finalization); claimID == "" || !outcome.Valid() || !ok {
		return nil, nil, nil, false, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockWorkerRunTx(ctx, tx, runID, workerID); err != nil {
		return nil, nil, nil, false, err
	}
	replayed, err := workerAttemptWasFinalizedTx(ctx, tx, runID, workerID, claimID, workerFinalizationComplete, finalization)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if replayed {
		run, err := scanWorkerRun(tx.QueryRow(ctx,
			`SELECT `+workerRunCols+` FROM worker_runs WHERE id = $1 AND worker_id = $2`, runID, workerID))
		if err != nil {
			return nil, nil, nil, false, err
		}
		task, err := taskForWorkerRunTx(ctx, tx, run)
		if err != nil {
			return nil, nil, nil, false, err
		}
		return run, task, nil, true, tx.Commit(ctx)
	}
	if err := finalizeWorkerAttemptTx(ctx, tx, runID, workerID, claimID,
		WorkerRunCompleted, workerFinalizationComplete, finalization, outcome, exitCode, "",
		strings.TrimSpace(summary), strings.TrimSpace(lessons)); err != nil {
		return nil, nil, nil, false, err
	}
	run, err := scanWorkerRun(tx.QueryRow(ctx,
		`UPDATE worker_runs SET status = 'completed', outcome = $4, exit_code = $5,
		   claim_id = '', claimed_at = NULL, last_error = '', summary = $6, lessons = $7,
		   completed_at = now(), updated_at = now()
		 WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3
		 RETURNING `+workerRunCols,
		runID, workerID, claimID, string(outcome), exitCode, strings.TrimSpace(summary), strings.TrimSpace(lessons)))
	if err != nil {
		return nil, nil, nil, false, err
	}
	if strings.TrimSpace(summary) != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO worker_run_progress (run_id, author_id, content) VALUES ($1,$2,$3)`,
			run.ID, workerID, "🤖 完成汇报："+strings.TrimSpace(summary)); err != nil {
			return nil, nil, nil, false, wrapErr(err)
		}
	}
	var task *Task
	var chain []*Task
	if run.TaskID != nil {
		task, err = scanTask(tx.QueryRow(ctx,
			`UPDATE tasks SET
			   status = CASE WHEN $3 = 'succeeded' THEN `+successfulTaskCompletionStatusSQL+` ELSE 'done' END,
			   revision = revision + 1,
			   submitted_by = $2, submitted_at = now(), updated_at = now()
			 WHERE id = $1 AND assignee_id = $2 AND revision = $4 AND status = 'in_progress'
			 RETURNING `+taskCols, *run.TaskID, workerID, string(outcome), run.TaskRevision))
		if err != nil {
			return nil, nil, nil, false, err
		}
		if strings.TrimSpace(summary) != "" {
			if _, err := tx.Exec(ctx,
				`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1,$2,$3)`,
				task.ID, workerID, "🤖 完成汇报："+strings.TrimSpace(summary)); err != nil {
				return nil, nil, nil, false, wrapErr(err)
			}
		}
		if task.Status == TaskAccepted {
			chain, err = cascadeUp(ctx, tx, task.ParentID)
			if err != nil {
				return nil, nil, nil, false, err
			}
		}
	}
	if task != nil && strings.TrimSpace(summary) != "" {
		kind := WorkEvidenceDeliverable
		status := WorkEvidenceActive
		if outcome != workerproto.OutcomeSucceeded {
			kind = WorkEvidenceRisk
		} else if task.Status == TaskAccepted {
			status = WorkEvidenceResolved
		}
		workerIDCopy, runIDCopy, taskID, projectID := workerID, run.ID, task.ID, task.ProjectID
		if _, err := upsertWorkEvidence(ctx, tx, WorkEvidenceInput{
			SourceType: "worker_run", SourceKey: strconv.FormatInt(run.ID, 10), Kind: kind,
			Status: status, Title: run.Title, Content: summary, ActorUserID: &workerIDCopy,
			ProjectID: &projectID, TaskID: &taskID, WorkerRunID: &runIDCopy,
			Confidence: 1, EventAt: time.Now().UTC(), CreatedBy: &workerIDCopy,
		}); err != nil {
			return nil, nil, nil, false, err
		}
	}
	if run.ScopeType == "materials" && task != nil {
		materialStatus := MaterialCompleted
		if outcome != workerproto.OutcomeSucceeded {
			materialStatus = MaterialNeedsInput
		}
		if err := setMaterialTaskStateTx(ctx, tx, task.ID, materialStatus, summary); err != nil {
			return nil, nil, nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, false, err
	}
	return run, task, chain, false, nil
}

func cancelActiveWorkerRunsTx(ctx context.Context, tx pgx.Tx, taskID int64, reason string) ([]int64, error) {
	rows, err := tx.Query(ctx,
		`UPDATE worker_runs SET status = 'cancelled', claim_id = '', claimed_at = NULL,
		   last_error = $2, completed_at = now(), updated_at = now()
		 WHERE task_id = $1 AND status IN ('queued','claimed','retry_wait','awaiting_input')
		 RETURNING id`, taskID, truncateRunes(strings.TrimSpace(reason), 2000))
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE worker_run_attempts SET status = 'cancelled', finished_at = now(), updated_at = now()
			 WHERE run_id = ANY($1) AND status = 'claimed'`, ids); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func latestTaskRunSpecTx(ctx context.Context, tx pgx.Tx, task *Task) (WorkerRunSpec, error) {
	spec := defaultWorkerRunSpec(task)
	var executor string
	var input json.RawMessage
	err := tx.QueryRow(ctx,
		`SELECT executor, input, scope_type, scope_key, scope_title
		 FROM worker_runs WHERE task_id = $1 ORDER BY id DESC LIMIT 1`, task.ID).
		Scan(&executor, &input, &spec.ScopeType, &spec.ScopeKey, &spec.ScopeTitle)
	if err == nil {
		spec.Executor = workerproto.Executor(executor)
		spec.Input = input
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return WorkerRunSpec{}, err
	}
	// Task attachments are the business input; worker_run_files is the immutable
	// execution snapshot. Union both so reassignment/rework preserves inputs even
	// for tasks created before worker_runs existed.
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(array_agg(file_id ORDER BY file_id), '{}'::bigint[])
		 FROM (
		   SELECT a.file_id FROM task_attachments a
		    WHERE a.task_id = $1 AND a.file_id IS NOT NULL
		   UNION
		   SELECT rf.file_id FROM worker_run_files rf
		    JOIN worker_runs r ON r.id = rf.run_id
		    WHERE r.task_id = $1 AND rf.role = 'input'
		 ) inputs`, task.ID).Scan(&spec.FileIDs); err != nil {
		return WorkerRunSpec{}, err
	}
	return spec, nil
}

func restartTaskWorkerRunTx(ctx context.Context, tx pgx.Tx, task *Task, reason string) (*WorkerRun, []int64, error) {
	previous, err := latestTaskRunSpecTx(ctx, tx, task)
	if err != nil {
		return nil, nil, err
	}
	cancelled, err := cancelActiveWorkerRunsTx(ctx, tx, task.ID, reason)
	if err != nil {
		return nil, nil, err
	}
	run, err := enqueueTaskWorkerRunTx(ctx, tx, task, &previous)
	if err != nil {
		return nil, nil, err
	}
	materialStatus := MaterialNeedsInput
	var runID *int64
	materialError := strings.TrimSpace(reason)
	if run != nil {
		materialStatus = MaterialQueued
		id := run.ID
		runID = &id
		materialError = ""
	}
	if _, err := tx.Exec(ctx,
		`UPDATE material_cases SET status = $2, worker_run_id = $3, last_error = $4,
		 completed_at = NULL, updated_at = now()
		 WHERE task_id = $1 AND status <> 'ignored'`,
		task.ID, materialStatus, runID, materialError); err != nil {
		return nil, nil, wrapErr(err)
	}
	return run, cancelled, nil
}

func (s *Store) ListWorkerRuns(ctx context.Context, requesterID int64, all bool, scope string, limit int) ([]*WorkerRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	where := "status IN ('queued','claimed','retry_wait','awaiting_input')"
	switch strings.TrimSpace(scope) {
	case "", "queue", "open":
	case "history":
		where = "status IN ('completed','cancelled')"
	case WorkerRunCompleted:
		where = "status = 'completed'"
	case "all":
		where = "true"
	case WorkerRunQueued, WorkerRunClaimed, WorkerRunRetryWait, WorkerRunAwaitingInput, WorkerRunCancelled:
		where = "status = '" + strings.TrimSpace(scope) + "'"
	}
	args := []any{limit}
	if !all {
		where = "(" + where + ") AND requested_by = $2"
		args = append(args, requesterID)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+workerRunCols+` FROM worker_runs WHERE `+where+`
		 ORDER BY CASE status WHEN 'claimed' THEN 0 WHEN 'awaiting_input' THEN 1
		   WHEN 'retry_wait' THEN 2 WHEN 'queued' THEN 3 WHEN 'completed' THEN 4 ELSE 5 END,
		 updated_at DESC LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WorkerRun
	for rows.Next() {
		run, err := scanWorkerRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) CancelWorkerRun(ctx context.Context, runID, actorID int64, superadmin bool, reason string) (*WorkerRun, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	where := "id = $1"
	args := []any{runID, truncateRunes(reason, 2000)}
	if !superadmin {
		where += " AND requested_by = $3"
		args = append(args, actorID)
	}
	run, err := scanWorkerRun(tx.QueryRow(ctx,
		`UPDATE worker_runs SET status = 'cancelled', claim_id = '', claimed_at = NULL,
		   last_error = $2, completed_at = now(), updated_at = now()
		 WHERE `+where+` AND task_id IS NULL
		   AND status IN ('queued','claimed','retry_wait','awaiting_input')
		 RETURNING `+workerRunCols, args...))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE worker_run_attempts SET status = 'cancelled', finished_at = now(), updated_at = now()
		 WHERE run_id = $1 AND status = 'claimed'`, run.ID); err != nil {
		return nil, err
	}
	return run, tx.Commit(ctx)
}

func (s *Store) WorkerRunProgress(ctx context.Context, runID int64) ([]WorkerRunProgress, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, author_id, content, created_at
		 FROM worker_run_progress WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerRunProgress
	for rows.Next() {
		var item WorkerRunProgress
		if err := rows.Scan(&item.ID, &item.RunID, &item.AuthorID, &item.Content, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

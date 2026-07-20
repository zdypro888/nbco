package store

import (
	"context"
	"strings"
	"time"
)

// WorkerSession is a server-owned topic context for a worker CLI. It is not a
// raw remote shell session; it records the right workspace, scope and summary so
// each new PTY task can continue the correct line of work without cross-task
// pollution.
type WorkerSession struct {
	ID                       int64
	WorkerID                 int64
	Engine                   string
	ScopeType                string
	ScopeKey                 string
	Title                    string
	Workdir                  string
	EngineSessionRef         string
	EngineRuntimeFingerprint string
	Summary                  string
	LastRunID                *int64
	LastTaskID               *int64
	UseCount                 int64
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func scanWorkerSession(row interface{ Scan(...any) error }) (*WorkerSession, error) {
	var ws WorkerSession
	err := row.Scan(&ws.ID, &ws.WorkerID, &ws.Engine, &ws.ScopeType, &ws.ScopeKey, &ws.Title,
		&ws.Workdir, &ws.EngineSessionRef, &ws.EngineRuntimeFingerprint, &ws.Summary, &ws.LastRunID, &ws.LastTaskID, &ws.UseCount,
		&ws.CreatedAt, &ws.UpdatedAt)
	return &ws, wrapErr(err)
}

const workerSessionCols = `id, worker_id, engine, scope_type, scope_key, title, workdir, engine_session_ref, engine_runtime_fingerprint, summary, last_run_id, last_task_id, use_count, created_at, updated_at`

// ClaimWorkerSession returns the session for a worker/scope and records that a
// new task is using it. The unique key is the boundary that prevents e.g. HR
// document analysis from resuming the nbco codebase context.
func (s *Store) ClaimWorkerSession(ctx context.Context, workerID int64, engine, scopeType, scopeKey, title string, runID int64, taskID *int64) (*WorkerSession, error) {
	engine = normalizeWorkerSessionPart(engine, "default")
	scopeType = normalizeWorkerSessionPart(scopeType, "project")
	scopeKey = normalizeWorkerSessionPart(scopeKey, "default")
	title = strings.TrimSpace(title)
	if title == "" {
		title = scopeKey
	}
	return scanWorkerSession(s.pool.QueryRow(ctx,
		`INSERT INTO worker_sessions (worker_id, engine, scope_type, scope_key, title, last_run_id, last_task_id, use_count)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 1)
		 ON CONFLICT (worker_id, engine, scope_type, scope_key) DO UPDATE SET
		   title = CASE WHEN worker_sessions.title = '' THEN EXCLUDED.title ELSE worker_sessions.title END,
		   last_run_id = EXCLUDED.last_run_id,
		   last_task_id = EXCLUDED.last_task_id,
		   use_count = worker_sessions.use_count + 1,
		   updated_at = now()
		 RETURNING `+workerSessionCols,
		workerID, engine, scopeType, scopeKey, title, runID, taskID))
}

// UpdateWorkerSession records the latest result summary and optional native CLI
// reference after a task completes.
func (s *Store) UpdateWorkerSession(ctx context.Context, id, workerID, runID int64, taskID *int64, summary, engineRef, runtimeFingerprint, workdir string) error {
	summary = strings.TrimSpace(summary)
	engineRef = strings.TrimSpace(engineRef)
	runtimeFingerprint = normalizeEngineRuntimeFingerprint(runtimeFingerprint)
	workdir = strings.TrimSpace(workdir)
	return s.execOne(ctx,
		`UPDATE worker_sessions SET
		   summary = CASE WHEN $5 <> '' THEN $5 ELSE summary END,
		   engine_session_ref = CASE WHEN $6 <> '' THEN $6 ELSE engine_session_ref END,
		   engine_runtime_fingerprint = CASE WHEN $6 <> '' THEN $7 ELSE engine_runtime_fingerprint END,
		   workdir = CASE WHEN $8 <> '' THEN $8 ELSE workdir END,
		   last_run_id = $3,
		   last_task_id = $4,
		   updated_at = now()
		 WHERE id = $1 AND worker_id = $2`,
		id, workerID, runID, taskID, summary, engineRef, runtimeFingerprint, workdir)
}

// UpdateWorkerSessionForClaim is the worker-control-plane variant: session
// continuity is persisted only while the caller still owns the exact task claim.
// This prevents a stale process from overwriting a newer run's native session.
func (s *Store) UpdateWorkerSessionForClaim(ctx context.Context, id, workerID, runID int64, claimID, summary, engineRef, runtimeFingerprint, workdir string) error {
	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return ErrNotFound
	}
	summary = strings.TrimSpace(summary)
	engineRef = strings.TrimSpace(engineRef)
	runtimeFingerprint = normalizeEngineRuntimeFingerprint(runtimeFingerprint)
	workdir = strings.TrimSpace(workdir)
	return s.execOne(ctx,
		`UPDATE worker_sessions ws SET
		   summary = CASE WHEN $5 <> '' THEN $5 ELSE ws.summary END,
		   engine_session_ref = CASE WHEN $6 <> '' THEN $6 ELSE ws.engine_session_ref END,
		   engine_runtime_fingerprint = CASE WHEN $6 <> '' THEN $7 ELSE ws.engine_runtime_fingerprint END,
		   workdir = CASE WHEN $8 <> '' THEN $8 ELSE ws.workdir END,
		   last_run_id = $3,
		   last_task_id = r.task_id,
		   updated_at = now()
		 FROM worker_runs r
		 WHERE ws.id = $1 AND ws.worker_id = $2
		   AND ws.last_run_id = $3
		   AND r.id = $3 AND r.worker_id = $2
		   AND r.status = 'claimed' AND r.claim_id = $4`,
		id, workerID, runID, claimID, summary, engineRef, runtimeFingerprint, workdir)
}

// UpdateWorkerSessionForFinalization accepts either the live lease or the same
// already-finalized attempt. The latter makes a lost HTTP response retry
// idempotent without allowing another claim to overwrite session continuity.
func (s *Store) UpdateWorkerSessionForFinalization(ctx context.Context, id, workerID, runID int64, claimID, finalizationID, summary, engineRef, runtimeFingerprint, workdir string) error {
	claimID = strings.TrimSpace(claimID)
	finalizationID = strings.TrimSpace(finalizationID)
	if claimID == "" || finalizationID == "" {
		return ErrNotFound
	}
	return s.execOne(ctx,
		`UPDATE worker_sessions ws SET
		   summary = CASE WHEN $6 <> '' THEN $6 ELSE ws.summary END,
		   engine_session_ref = CASE WHEN $7 <> '' THEN $7 ELSE ws.engine_session_ref END,
		   engine_runtime_fingerprint = CASE WHEN $7 <> '' THEN $8 ELSE ws.engine_runtime_fingerprint END,
		   workdir = CASE WHEN $9 <> '' THEN $9 ELSE ws.workdir END,
		   last_run_id = $3,
		   last_task_id = r.task_id,
		   updated_at = now()
		 FROM worker_runs r
		 WHERE ws.id = $1 AND ws.worker_id = $2 AND ws.last_run_id = $3
		   AND r.id = $3 AND r.worker_id = $2
		   AND (
		     (r.status = 'claimed' AND r.claim_id = $4)
		     OR EXISTS (
		       SELECT 1 FROM worker_run_attempts a
		        WHERE a.run_id = r.id AND a.worker_id = $2 AND a.claim_id = $4
		          AND a.finalization_id = $5
		     )
		   )`,
		id, workerID, runID, claimID, finalizationID,
		strings.TrimSpace(summary), strings.TrimSpace(engineRef), normalizeEngineRuntimeFingerprint(runtimeFingerprint), strings.TrimSpace(workdir))
}

func normalizeEngineRuntimeFingerprint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 64 {
		return ""
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	return value
}

func normalizeWorkerSessionPart(v, def string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return def
	}
	return v
}

// LatestWorkerTaskID 返回该 worker 最近活跃会话挂的任务ID（无则 nil）。
// 用于 worker LLM 用量的目标归因：尽力而为，worker 可能在非任务上下文下调用模型。
func (s *Store) LatestWorkerTaskID(ctx context.Context, workerID int64) (*int64, error) {
	var taskID *int64
	err := s.pool.QueryRow(ctx,
		`SELECT last_task_id FROM worker_sessions
		 WHERE worker_id = $1 AND last_task_id IS NOT NULL
		 ORDER BY updated_at DESC LIMIT 1`, workerID).Scan(&taskID)
	if err != nil {
		return nil, wrapErr(err) // ErrNotFound 当无任何带任务的会话
	}
	return taskID, nil
}

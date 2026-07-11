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
	ID               int64
	WorkerID         int64
	Engine           string
	ScopeType        string
	ScopeKey         string
	Title            string
	Workdir          string
	EngineSessionRef string
	Summary          string
	LastTaskID       *int64
	UseCount         int64
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func scanWorkerSession(row interface{ Scan(...any) error }) (*WorkerSession, error) {
	var ws WorkerSession
	err := row.Scan(&ws.ID, &ws.WorkerID, &ws.Engine, &ws.ScopeType, &ws.ScopeKey, &ws.Title,
		&ws.Workdir, &ws.EngineSessionRef, &ws.Summary, &ws.LastTaskID, &ws.UseCount,
		&ws.CreatedAt, &ws.UpdatedAt)
	return &ws, wrapErr(err)
}

const workerSessionCols = `id, worker_id, engine, scope_type, scope_key, title, workdir, engine_session_ref, summary, last_task_id, use_count, created_at, updated_at`

// ClaimWorkerSession returns the session for a worker/scope and records that a
// new task is using it. The unique key is the boundary that prevents e.g. HR
// document analysis from resuming the nbco codebase context.
func (s *Store) ClaimWorkerSession(ctx context.Context, workerID int64, engine, scopeType, scopeKey, title string, taskID int64) (*WorkerSession, error) {
	engine = normalizeWorkerSessionPart(engine, "default")
	scopeType = normalizeWorkerSessionPart(scopeType, "project")
	scopeKey = normalizeWorkerSessionPart(scopeKey, "default")
	title = strings.TrimSpace(title)
	if title == "" {
		title = scopeKey
	}
	return scanWorkerSession(s.pool.QueryRow(ctx,
		`INSERT INTO worker_sessions (worker_id, engine, scope_type, scope_key, title, last_task_id, use_count)
		 VALUES ($1, $2, $3, $4, $5, $6, 1)
		 ON CONFLICT (worker_id, engine, scope_type, scope_key) DO UPDATE SET
		   title = CASE WHEN worker_sessions.title = '' THEN EXCLUDED.title ELSE worker_sessions.title END,
		   last_task_id = EXCLUDED.last_task_id,
		   use_count = worker_sessions.use_count + 1,
		   updated_at = now()
		 RETURNING `+workerSessionCols,
		workerID, engine, scopeType, scopeKey, title, taskID))
}

// UpdateWorkerSession records the latest result summary and optional native CLI
// reference after a task completes.
func (s *Store) UpdateWorkerSession(ctx context.Context, id, workerID, taskID int64, summary, engineRef, workdir string) error {
	summary = strings.TrimSpace(summary)
	engineRef = strings.TrimSpace(engineRef)
	workdir = strings.TrimSpace(workdir)
	return s.execOne(ctx,
		`UPDATE worker_sessions SET
		   summary = CASE WHEN $4 <> '' THEN $4 ELSE summary END,
		   engine_session_ref = CASE WHEN $5 <> '' THEN $5 ELSE engine_session_ref END,
		   workdir = CASE WHEN $6 <> '' THEN $6 ELSE workdir END,
		   last_task_id = $3,
		   updated_at = now()
		 WHERE id = $1 AND worker_id = $2`,
		id, workerID, taskID, summary, engineRef, workdir)
}

// UpdateWorkerSessionForClaim is the worker-control-plane variant: session
// continuity is persisted only while the caller still owns the exact task claim.
// This prevents a stale process from overwriting a newer run's native session.
func (s *Store) UpdateWorkerSessionForClaim(ctx context.Context, id, workerID, taskID int64, claimID, summary, engineRef, workdir string) error {
	claimID = strings.TrimSpace(claimID)
	if claimID == "" {
		return ErrNotFound
	}
	summary = strings.TrimSpace(summary)
	engineRef = strings.TrimSpace(engineRef)
	workdir = strings.TrimSpace(workdir)
	return s.execOne(ctx,
		`UPDATE worker_sessions ws SET
		   summary = CASE WHEN $5 <> '' THEN $5 ELSE ws.summary END,
		   engine_session_ref = CASE WHEN $6 <> '' THEN $6 ELSE ws.engine_session_ref END,
		   workdir = CASE WHEN $7 <> '' THEN $7 ELSE ws.workdir END,
		   last_task_id = $3,
		   updated_at = now()
		 FROM tasks t
		 WHERE ws.id = $1 AND ws.worker_id = $2
		   AND t.id = $3 AND t.assignee_id = $2
		   AND t.status = 'in_progress' AND t.worker_claim_id = $4`,
		id, workerID, taskID, claimID, summary, engineRef, workdir)
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

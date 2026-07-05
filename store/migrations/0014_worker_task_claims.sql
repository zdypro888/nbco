-- Worker task leases: a claimed task must be recoverable if the client process
-- dies before submitting. Nullable columns keep normal human task flow unchanged.
--
-- This supersedes the historical 0010_worker_task_claims.sql filename, which
-- collided numerically with 0010_session_summary_groups.sql. The statements are
-- idempotent so existing databases that already applied the old filename can
-- safely apply this migration record without changing schema twice.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS worker_claimed_by BIGINT REFERENCES users(id);
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS worker_claimed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_tasks_worker_claims
    ON tasks (assignee_id, worker_claimed_at)
    WHERE status = 'in_progress' AND worker_claimed_at IS NOT NULL;

-- Worker task leases: a claimed task must be recoverable if the client process
-- dies before submitting. Nullable columns keep normal human task flow unchanged.
ALTER TABLE tasks ADD COLUMN worker_claimed_by BIGINT REFERENCES users(id);
ALTER TABLE tasks ADD COLUMN worker_claimed_at TIMESTAMPTZ;

CREATE INDEX idx_tasks_worker_claims
    ON tasks (assignee_id, worker_claimed_at)
    WHERE status = 'in_progress' AND worker_claimed_at IS NOT NULL;

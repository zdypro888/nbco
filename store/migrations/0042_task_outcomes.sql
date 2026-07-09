CREATE TABLE IF NOT EXISTS task_outcomes (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    assignee_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reviewer_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    outcome     TEXT NOT NULL CHECK (outcome IN ('accepted', 'rejected')),
    task_kind   TEXT NOT NULL DEFAULT 'general',
    reason      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_task_outcomes_assignee_kind ON task_outcomes(assignee_id, task_kind, id DESC);
CREATE INDEX IF NOT EXISTS idx_task_outcomes_task ON task_outcomes(task_id, id DESC);

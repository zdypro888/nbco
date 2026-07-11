-- Durable delivery and execution state shared by schedulers, events, workers,
-- and dynamic script tools.

CREATE TABLE schedule_deliveries (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    schedule_id     BIGINT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
    occurrence_at   TIMESTAMPTZ NOT NULL,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    mode            TEXT NOT NULL,
    message         TEXT NOT NULL,
    title           TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processing', 'delivered', 'failed', 'cancelled')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    available_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at      TIMESTAMPTZ,
    delivered_at    TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (schedule_id, occurrence_at, user_id)
);
CREATE INDEX idx_schedule_deliveries_due
    ON schedule_deliveries (available_at, id)
    WHERE status IN ('pending', 'processing');

CREATE TABLE automation_runs (
    automation_key TEXT NOT NULL,
    occurrence_key TEXT NOT NULL,
    subject_id     BIGINT NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts       INTEGER NOT NULL DEFAULT 0,
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    last_error     TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (automation_key, occurrence_key, subject_id)
);
CREATE INDEX idx_automation_runs_due
    ON automation_runs (available_at)
    WHERE status IN ('pending', 'processing');

ALTER TABLE events ADD COLUMN status TEXT NOT NULL DEFAULT 'done';
ALTER TABLE events ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN available_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE events ADD COLUMN claimed_at TIMESTAMPTZ;
ALTER TABLE events ADD COLUMN delivery_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE events ALTER COLUMN handled_at DROP NOT NULL;
ALTER TABLE events ALTER COLUMN handled_at DROP DEFAULT;
ALTER TABLE events ADD CONSTRAINT events_status_check
    CHECK (status IN ('pending', 'processing', 'done', 'failed'));
CREATE INDEX idx_events_due ON events (available_at, id)
    WHERE status IN ('pending', 'processing');

ALTER TABLE script_tools ADD COLUMN tested_source_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE script_tools ADD COLUMN last_test_ok BOOLEAN NOT NULL DEFAULT FALSE;
-- Existing enabled rows were never tied to a tested source revision. Require a
-- fresh test under the new lifecycle before loading them again.
UPDATE script_tools SET enabled = FALSE WHERE enabled;

-- Per-turn action failures already live in action_turns with structured traces.
-- They are diagnostics, not reusable company knowledge; retire legacy queue noise.
UPDATE learning_candidates
   SET status = 'rejected', reviewed_at = now(),
       review_note = '归档：动作失败诊断请查看 action_turns，不进入知识学习队列。',
       updated_at = now()
 WHERE status = 'pending' AND source_type = 'action_audit';

ALTER TABLE tasks ADD COLUMN worker_scope_type TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN worker_scope_key TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN worker_scope_title TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN worker_retry_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN worker_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN worker_last_error TEXT NOT NULL DEFAULT '';
UPDATE tasks t
   SET worker_scope_type = 'project',
       worker_scope_key = 'project:' || t.project_id::text,
       worker_scope_title = COALESCE(NULLIF(p.name, ''), 'Project ' || t.project_id::text)
  FROM projects p
 WHERE p.id = t.project_id AND t.worker_scope_key = '';
CREATE INDEX idx_tasks_awaiting_input ON tasks (assigner_id, updated_at)
    WHERE status = 'awaiting_input';
CREATE INDEX idx_tasks_assignee_unfinished_v2 ON tasks (assignee_id, updated_at)
    WHERE status IN ('pending', 'in_progress', 'awaiting_input');
CREATE INDEX idx_tasks_worker_retry ON tasks (assignee_id, worker_retry_at, id)
    WHERE status IN ('pending', 'in_progress');

CREATE TABLE memory_mining_jobs (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id              BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel              TEXT NOT NULL,
    session_id           BIGINT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    user_message_id      BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    assistant_message_id BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    tool_evidence        TEXT NOT NULL DEFAULT '',
    explicit_commit      BOOLEAN NOT NULL DEFAULT FALSE,
    status               TEXT NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts             INTEGER NOT NULL DEFAULT 0,
    available_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at           TIMESTAMPTZ,
    last_error           TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at         TIMESTAMPTZ,
    UNIQUE (session_id, user_message_id)
);
CREATE INDEX idx_memory_mining_jobs_due ON memory_mining_jobs (available_at, id)
    WHERE status IN ('pending', 'processing');

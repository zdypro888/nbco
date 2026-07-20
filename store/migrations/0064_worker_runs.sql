-- A task is a business responsibility and review record. A worker run is a
-- durable execution request. Keeping their state separate prevents executor
-- details, retries and leases from leaking into task acceptance semantics.
ALTER TABLE tasks
    ADD COLUMN revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0);

ALTER TABLE tasks
    ADD CONSTRAINT tasks_status_check
    CHECK (status IN ('pending','in_progress','awaiting_input','done','accepted','split','cancelled'));

CREATE TABLE worker_runs (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id        BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    task_revision  BIGINT,
    legacy_task_id BIGINT UNIQUE,
    worker_id      BIGINT NOT NULL REFERENCES users(id),
    requested_by   BIGINT NOT NULL REFERENCES users(id),
    executor       TEXT NOT NULL DEFAULT 'agent',
    input          JSONB NOT NULL DEFAULT '{}',
    title          TEXT NOT NULL,
    goal           TEXT NOT NULL DEFAULT '',
    description    TEXT NOT NULL DEFAULT '',
    acceptance     TEXT NOT NULL DEFAULT '',
    scope_type     TEXT NOT NULL DEFAULT 'project',
    scope_key      TEXT NOT NULL DEFAULT '',
    scope_title    TEXT NOT NULL DEFAULT '',
    priority       TEXT NOT NULL DEFAULT 'normal',
    status         TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','claimed','retry_wait','awaiting_input','completed','cancelled')),
    outcome        TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','succeeded','failed')),
    exit_code      INTEGER,
    claim_id       TEXT NOT NULL DEFAULT '',
    claimed_at     TIMESTAMPTZ,
    attempts       INTEGER NOT NULL DEFAULT 0,
    failure_count  INTEGER NOT NULL DEFAULT 0,
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error     TEXT NOT NULL DEFAULT '',
    summary        TEXT NOT NULL DEFAULT '',
    lessons        TEXT NOT NULL DEFAULT '',
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (executor IN ('agent','command')),
    CHECK ((task_id IS NULL) = (task_revision IS NULL)),
    CHECK (attempts >= 0 AND failure_count >= 0),
    CHECK (
      (status = 'claimed' AND claim_id <> '' AND claimed_at IS NOT NULL)
      OR (status <> 'claimed' AND claim_id = '' AND claimed_at IS NULL)
    )
);

CREATE UNIQUE INDEX worker_runs_one_active_task
    ON worker_runs(task_id)
    WHERE task_id IS NOT NULL AND status IN ('queued','claimed','retry_wait','awaiting_input');
CREATE INDEX worker_runs_claim_queue
    ON worker_runs(worker_id, available_at, id)
    WHERE status IN ('queued','claimed','retry_wait');
CREATE INDEX worker_runs_task_history ON worker_runs(task_id, id DESC) WHERE task_id IS NOT NULL;
CREATE INDEX worker_runs_requester_history ON worker_runs(requested_by, id DESC);

CREATE TABLE worker_run_attempts (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id              BIGINT NOT NULL REFERENCES worker_runs(id) ON DELETE CASCADE,
    attempt_no          INTEGER NOT NULL CHECK (attempt_no > 0),
    worker_id           BIGINT NOT NULL REFERENCES users(id),
    claim_id            TEXT NOT NULL UNIQUE,
    status              TEXT NOT NULL DEFAULT 'claimed'
                        CHECK (status IN ('claimed','released','expired','retry_wait','awaiting_input','completed','cancelled')),
    finalization_id     TEXT NOT NULL DEFAULT '',
    finalization_kind   TEXT NOT NULL DEFAULT '',
    finalization_hash   TEXT NOT NULL DEFAULT '',
    outcome             TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','succeeded','failed')),
    exit_code           INTEGER,
    error               TEXT NOT NULL DEFAULT '',
    summary             TEXT NOT NULL DEFAULT '',
    lessons             TEXT NOT NULL DEFAULT '',
    claimed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    heartbeat_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, attempt_no),
    CHECK (finalization_kind IN ('','complete','fail','request_input')),
    CHECK (
      (finalization_id = '' AND finalization_kind = '' AND finalization_hash = '')
      OR (finalization_id <> '' AND finalization_kind <> '' AND finalization_hash <> '')
    ),
    CHECK ((status = 'claimed' AND finished_at IS NULL) OR (status <> 'claimed' AND finished_at IS NOT NULL)),
    CHECK ((status = 'completed' AND outcome <> '') OR (status <> 'completed' AND outcome = ''))
);
CREATE UNIQUE INDEX worker_run_attempts_finalization
    ON worker_run_attempts(run_id, finalization_id)
    WHERE finalization_id <> '';
CREATE INDEX worker_run_attempts_run ON worker_run_attempts(run_id, id DESC);

CREATE TABLE worker_run_progress (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     BIGINT NOT NULL REFERENCES worker_runs(id) ON DELETE CASCADE,
    author_id  BIGINT NOT NULL REFERENCES users(id),
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX worker_run_progress_run ON worker_run_progress(run_id, id);

CREATE TABLE worker_run_files (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     BIGINT NOT NULL REFERENCES worker_runs(id) ON DELETE CASCADE,
    file_id    BIGINT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('input','artifact')),
    caption    TEXT NOT NULL DEFAULT '',
    created_by BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, file_id, role)
);
CREATE INDEX worker_run_files_file ON worker_run_files(file_id);

ALTER TABLE worker_sessions
    ADD COLUMN last_run_id BIGINT REFERENCES worker_runs(id) ON DELETE SET NULL;

-- Preserve every legacy worker execution before runtime stops using task
-- leases. legacy_task_id is the rolling-upgrade protocol alias for every old
-- worker response. Rows with worker_command were execution-only records created
-- by the old run_worker_command path, so only those have task_id = NULL.
INSERT INTO worker_runs (
    task_id, task_revision, legacy_task_id, worker_id, requested_by, executor, input,
    title, goal, description, acceptance, scope_type, scope_key, scope_title,
    priority, status, outcome, claim_id, claimed_at, attempts, available_at,
    failure_count, last_error, completed_at, created_at, updated_at
)
SELECT
	CASE WHEN t.worker_command = '' THEN t.id END,
	CASE WHEN t.worker_command = '' THEN t.revision END,
	t.id,
    t.assignee_id,
    t.assigner_id,
    CASE WHEN t.worker_command <> '' THEN 'command' ELSE 'agent' END,
    CASE WHEN t.worker_command <> ''
         THEN jsonb_build_object('command', t.worker_command, 'pty', t.worker_command_pty)
         ELSE '{}'::jsonb END,
    t.title, t.goal, t.description, t.acceptance,
    COALESCE(NULLIF(t.worker_scope_type, ''), 'project'),
    COALESCE(NULLIF(t.worker_scope_key, ''), 'project:' || t.project_id::text),
    COALESCE(NULLIF(t.worker_scope_title, ''), p.name, 'Project ' || t.project_id::text),
    t.priority,
    CASE t.status
      WHEN 'pending' THEN CASE WHEN t.worker_retry_at > now() THEN 'retry_wait' ELSE 'queued' END
      WHEN 'in_progress' THEN CASE WHEN t.worker_claim_id <> '' THEN 'claimed' ELSE 'queued' END
      WHEN 'awaiting_input' THEN 'awaiting_input'
      WHEN 'done' THEN 'completed'
      WHEN 'accepted' THEN 'completed'
      ELSE 'cancelled'
    END,
    '',
    CASE WHEN t.status = 'in_progress' THEN t.worker_claim_id ELSE '' END,
    CASE WHEN t.status = 'in_progress' AND t.worker_claim_id <> ''
         THEN COALESCE(t.worker_claimed_at, t.updated_at) END,
    t.worker_failures + CASE WHEN t.status = 'in_progress' AND t.worker_claim_id <> '' THEN 1 ELSE 0 END,
    COALESCE(t.worker_retry_at, now()),
    t.worker_failures,
    t.worker_last_error,
    CASE WHEN t.status IN ('done','accepted','split','cancelled') THEN t.updated_at END,
    t.created_at,
    t.updated_at
FROM tasks t
JOIN users u ON u.id = t.assignee_id AND u.is_worker
JOIN projects p ON p.id = t.project_id
ON CONFLICT DO NOTHING;

-- Old code serialized pollers, but normalize any manually-corrupted duplicate
-- leases before enforcing that one worker identity executes only one run.
WITH ranked_claims AS (
    SELECT id, row_number() OVER (
        PARTITION BY worker_id ORDER BY claimed_at DESC NULLS LAST, id DESC
    ) AS claim_rank
    FROM worker_runs
    WHERE status = 'claimed'
)
UPDATE worker_runs r
SET status = 'queued', claim_id = '', claimed_at = NULL, available_at = now(),
    last_error = '迁移时释放重复的旧执行租约', updated_at = now()
FROM ranked_claims ranked
WHERE r.id = ranked.id AND ranked.claim_rank > 1;

CREATE UNIQUE INDEX worker_runs_one_claim_per_worker
    ON worker_runs(worker_id)
    WHERE status = 'claimed';

-- Only an active legacy lease can be reconstructed exactly. Older completed
-- attempts remain represented by the aggregate counters on worker_runs.
INSERT INTO worker_run_attempts (
    run_id, attempt_no, worker_id, claim_id, status, claimed_at, updated_at
)
SELECT id, GREATEST(attempts, 1), worker_id, claim_id, 'claimed',
       COALESCE(claimed_at, updated_at), updated_at
FROM worker_runs
WHERE status = 'claimed' AND claim_id <> ''
ON CONFLICT DO NOTHING;

-- Legacy execution history is copied to its run. Linked business tasks retain
-- their original projections; execution-only task rows are removed below.
INSERT INTO worker_run_progress (run_id, author_id, content, created_at)
SELECT r.id, p.author_id, p.content, p.created_at
FROM worker_runs r
JOIN task_progress p ON p.task_id = r.legacy_task_id
WHERE r.legacy_task_id IS NOT NULL;

INSERT INTO worker_run_files (run_id, file_id, role, caption, created_by, created_at)
SELECT r.id, a.file_id, 'input', a.caption, r.requested_by, a.created_at
FROM worker_runs r
JOIN task_attachments a ON a.task_id = COALESCE(r.task_id, r.legacy_task_id)
WHERE a.file_id IS NOT NULL
ON CONFLICT DO NOTHING;

INSERT INTO worker_run_files (run_id, file_id, role, caption, created_by, created_at)
SELECT r.id, a.file_id, 'artifact', a.caption, a.created_by, a.created_at
FROM worker_runs r
JOIN task_artifacts a ON a.task_id = COALESCE(r.task_id, r.legacy_task_id)
ON CONFLICT DO NOTHING;

UPDATE worker_sessions ws
SET last_run_id = r.id,
    last_task_id = CASE WHEN r.task_id IS NOT NULL THEN r.task_id END
FROM worker_runs r
WHERE ws.last_task_id = COALESCE(r.task_id, r.legacy_task_id);

-- A legacy execution-only row was never a business prerequisite. Preserve its
-- execution record and remove it from dependency and parent relationships.
UPDATE tasks t
SET depends_on = array_remove(t.depends_on, r.legacy_task_id)
FROM worker_runs r
WHERE r.task_id IS NULL AND r.legacy_task_id = ANY(t.depends_on);

UPDATE tasks child
SET parent_id = legacy.parent_id
FROM tasks legacy
JOIN worker_runs r ON r.legacy_task_id = legacy.id AND r.task_id IS NULL
WHERE child.parent_id = legacy.id;

DELETE FROM tasks t
USING worker_runs r
WHERE r.task_id IS NULL AND r.legacy_task_id = t.id;

-- The old execution columns intentionally remain for one rollback-compatible
-- release. Runtime code no longer reads or writes them; a later contract
-- migration may remove them after the new protocol has been stable in
-- production and old binaries are no longer valid rollback targets.

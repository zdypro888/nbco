-- One deliverable keeps one task identity. Additional people participate through
-- explicit roles instead of duplicate top-level task rows.

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS cancel_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ;

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS superseded_by BIGINT REFERENCES tasks(id) ON DELETE SET NULL;

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS submitted_by BIGINT REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS submitted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_tasks_superseded_by ON tasks(superseded_by)
    WHERE superseded_by IS NOT NULL;

CREATE TABLE IF NOT EXISTS task_participants (
    task_id    BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('collaborator', 'reviewer', 'watcher')),
    added_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_task_participants_user
    ON task_participants(user_id, role, task_id);

-- Earlier application-level NOT EXISTS checks were not concurrency-safe. Keep
-- the oldest relationship, remove duplicate metadata rows, then enforce the
-- invariant in PostgreSQL.
DELETE FROM task_attachments newer
USING task_attachments older
WHERE newer.id > older.id
  AND newer.task_id = older.task_id
  AND newer.file_id IS NOT NULL
  AND newer.file_id = older.file_id;

DELETE FROM task_attachments newer
USING task_attachments older
WHERE newer.id > older.id
  AND newer.task_id = older.task_id
  AND newer.file_id IS NULL
  AND older.file_id IS NULL
  AND newer.kind = older.kind
  AND newer.file_ref = older.file_ref;

CREATE UNIQUE INDEX IF NOT EXISTS idx_task_attachments_unique_file
    ON task_attachments(task_id, file_id)
    WHERE file_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_task_attachments_unique_ref
    ON task_attachments(task_id, kind, file_ref)
    WHERE file_id IS NULL;

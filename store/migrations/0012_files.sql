CREATE TABLE files (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source        TEXT NOT NULL DEFAULT '',
    original_name TEXT NOT NULL DEFAULT '',
    mime_type     TEXT NOT NULL DEFAULT '',
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    sha256        TEXT NOT NULL,
    storage_path  TEXT NOT NULL,
    created_by    BIGINT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_files_created_by ON files(created_by, id DESC);
CREATE INDEX idx_files_sha256 ON files(sha256);

ALTER TABLE task_attachments ADD COLUMN file_id BIGINT REFERENCES files(id);
CREATE INDEX idx_task_attachments_file ON task_attachments(file_id);

CREATE TABLE task_artifacts (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id    BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    file_id    BIGINT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    claim_id   TEXT NOT NULL DEFAULT '',
    created_by BIGINT NOT NULL REFERENCES users(id),
    caption    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_task_artifacts_task ON task_artifacts(task_id, id);
CREATE INDEX idx_task_artifacts_file ON task_artifacts(file_id);

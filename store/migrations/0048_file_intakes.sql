CREATE TABLE IF NOT EXISTS file_intakes (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source         TEXT NOT NULL,
    external_ref   TEXT NOT NULL DEFAULT '',
    original_name  TEXT NOT NULL,
    mime_type      TEXT NOT NULL DEFAULT '',
    size_bytes     BIGINT NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'saved', 'failed')),
    error_code     TEXT NOT NULL DEFAULT '',
    error_message  TEXT NOT NULL DEFAULT '',
    file_id        BIGINT REFERENCES files(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_file_intakes_user_recent
    ON file_intakes (user_id, id DESC);

CREATE INDEX IF NOT EXISTS idx_file_intakes_pending
    ON file_intakes (id)
    WHERE status = 'pending';

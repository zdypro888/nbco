CREATE TABLE file_content_indexes (
    file_id       BIGINT PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'processing', 'indexed', 'empty', 'unsupported', 'failed')),
    extractor     TEXT NOT NULL DEFAULT '',
    extractor_revision TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0,
    chunk_count   INTEGER NOT NULL DEFAULT 0,
    truncated     BOOLEAN NOT NULL DEFAULT FALSE,
    last_error    TEXT NOT NULL DEFAULT '',
    claim_token   TEXT NOT NULL DEFAULT '',
    claimed_at    TIMESTAMPTZ,
    available_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    indexed_at    TIMESTAMPTZ,
    vector_status TEXT NOT NULL DEFAULT 'pending'
                  CHECK (vector_status IN ('pending', 'processing', 'indexed', 'unavailable', 'failed')),
    vector_model  TEXT NOT NULL DEFAULT '',
    vector_error  TEXT NOT NULL DEFAULT '',
    vector_attempts INTEGER NOT NULL DEFAULT 0,
    vector_claim_token TEXT NOT NULL DEFAULT '',
    vector_claimed_at TIMESTAMPTZ,
    vector_available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    vector_indexed_at TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_file_content_indexes_due
    ON file_content_indexes (available_at, file_id)
    WHERE status IN ('pending', 'failed');

CREATE INDEX idx_file_content_indexes_vector_due
    ON file_content_indexes (vector_available_at, file_id)
    WHERE status = 'indexed' AND vector_status IN ('pending', 'failed', 'unavailable');

CREATE TABLE file_text_chunks (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    file_id     BIGINT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    chunk_index INTEGER NOT NULL,
    content     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (file_id, chunk_index)
);

CREATE INDEX idx_file_text_chunks_file ON file_text_chunks (file_id, chunk_index);

INSERT INTO file_content_indexes (file_id)
SELECT id FROM files
ON CONFLICT (file_id) DO NOTHING;

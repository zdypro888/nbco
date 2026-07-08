CREATE TABLE learning_candidates (
    id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind                   TEXT NOT NULL,
    scope                  TEXT NOT NULL DEFAULT 'global',
    title                  TEXT NOT NULL,
    content                TEXT NOT NULL,
    tags                   TEXT[] NOT NULL DEFAULT '{}',
    evidence               JSONB NOT NULL DEFAULT '{}',
    confidence             REAL NOT NULL DEFAULT 0,
    status                 TEXT NOT NULL DEFAULT 'pending',
    source_type            TEXT NOT NULL DEFAULT '',
    source_ref             TEXT NOT NULL DEFAULT '',
    created_by             BIGINT REFERENCES users(id),
    reviewed_by            BIGINT REFERENCES users(id),
    reviewed_at            TIMESTAMPTZ,
    published_knowledge_id BIGINT REFERENCES knowledge(id),
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_learning_candidates_status ON learning_candidates(status, id DESC);
CREATE INDEX idx_learning_candidates_kind ON learning_candidates(kind, id DESC);
CREATE INDEX idx_learning_candidates_tags ON learning_candidates USING GIN(tags);

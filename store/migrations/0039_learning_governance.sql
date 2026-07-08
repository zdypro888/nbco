ALTER TABLE learning_candidates ADD COLUMN IF NOT EXISTS duplicate_of BIGINT REFERENCES learning_candidates(id);
ALTER TABLE learning_candidates ADD COLUMN IF NOT EXISTS conflict_with BIGINT REFERENCES learning_candidates(id);
ALTER TABLE learning_candidates ADD COLUMN IF NOT EXISTS value_score REAL NOT NULL DEFAULT 0;
ALTER TABLE learning_candidates ADD COLUMN IF NOT EXISTS review_note TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS knowledge_versions (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    knowledge_id BIGINT NOT NULL REFERENCES knowledge(id) ON DELETE CASCADE,
    version      INT NOT NULL,
    title        TEXT NOT NULL,
    content      TEXT NOT NULL,
    tags         TEXT[] NOT NULL DEFAULT '{}',
    kind         TEXT NOT NULL DEFAULT 'fact',
    pinned       BOOLEAN NOT NULL DEFAULT FALSE,
    changed_by   BIGINT REFERENCES users(id),
    change_note  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (knowledge_id, version)
);

CREATE INDEX IF NOT EXISTS idx_knowledge_versions_knowledge ON knowledge_versions(knowledge_id, version DESC);
CREATE INDEX IF NOT EXISTS idx_learning_candidates_dupe ON learning_candidates(duplicate_of) WHERE duplicate_of IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_learning_candidates_conflict ON learning_candidates(conflict_with) WHERE conflict_with IS NOT NULL;

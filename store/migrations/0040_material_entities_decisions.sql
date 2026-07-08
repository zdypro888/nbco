CREATE TABLE IF NOT EXISTS material_entities (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    file_id       BIGINT REFERENCES files(id) ON DELETE SET NULL,
    entity_type   TEXT NOT NULL,
    name          TEXT NOT NULL,
    content       TEXT NOT NULL DEFAULT '',
    evidence      JSONB NOT NULL DEFAULT '{}',
    confidence    REAL NOT NULL DEFAULT 0,
    source_candidate_id BIGINT REFERENCES learning_candidates(id) ON DELETE SET NULL,
    created_by    BIGINT REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_material_entities_type_name ON material_entities(entity_type, lower(name));
CREATE INDEX IF NOT EXISTS idx_material_entities_file ON material_entities(file_id);
CREATE INDEX IF NOT EXISTS idx_material_entities_candidate ON material_entities(source_candidate_id);

CREATE TABLE IF NOT EXISTS decision_items (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    title       TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    ref_type    TEXT NOT NULL DEFAULT '',
    ref_id      BIGINT,
    priority    TEXT NOT NULL DEFAULT 'normal',
    status      TEXT NOT NULL DEFAULT 'open',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_id, kind, ref_type, ref_id)
);

CREATE INDEX IF NOT EXISTS idx_decision_items_owner_status ON decision_items(owner_id, status, priority, id DESC);

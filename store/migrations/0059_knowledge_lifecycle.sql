-- Published memory needs a reversible lifecycle. Deleting stale or conflicting
-- memory destroys audit evidence; leaving it active poisons semantic recall.
ALTER TABLE knowledge
    ADD COLUMN active BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE knowledge_versions
    ADD COLUMN active BOOLEAN NOT NULL DEFAULT TRUE;

CREATE INDEX idx_knowledge_active_kind_id
    ON knowledge (kind, id)
    WHERE active;

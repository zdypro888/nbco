-- Reliable change timestamps let the Qdrant reconciler use incremental
-- keyset scans without delaying mutable records until the next full audit.
ALTER TABLE projects
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE roles
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE schedules
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE schedule_deliveries
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE task_checklist
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE OR REPLACE FUNCTION nbco_touch_updated_at()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER projects_touch_updated_at
    BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();

CREATE TRIGGER roles_touch_updated_at
    BEFORE UPDATE ON roles
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();

CREATE TRIGGER schedule_deliveries_touch_updated_at
    BEFORE UPDATE ON schedule_deliveries
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();

CREATE TRIGGER task_checklist_touch_updated_at
    BEFORE UPDATE ON task_checklist
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();

CREATE INDEX idx_chat_messages_embed_model_id
    ON chat_messages (embed_model, id)
    WHERE btrim(content) <> '';

CREATE INDEX idx_knowledge_embed_model_id
    ON knowledge (embed_model, id);

-- Persist domain facts and their pending side effects in the same PostgreSQL
-- transaction. Transport adapters claim only the topics they own; raw inbound
-- protocol handlers never need to perform an external side effect directly.

CREATE TABLE domain_outbox_events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurrence_key TEXT NOT NULL UNIQUE,
    topic          TEXT NOT NULL,
    payload        JSONB NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts       INTEGER NOT NULL DEFAULT 0,
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at     TIMESTAMPTZ,
    claim_owner    TEXT NOT NULL DEFAULT '',
    completed_at   TIMESTAMPTZ,
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX domain_outbox_events_due_idx
    ON domain_outbox_events (topic, available_at, id)
    WHERE status IN ('pending', 'processing');

CREATE INDEX domain_outbox_events_terminal_idx
    ON domain_outbox_events (updated_at)
    WHERE status IN ('done', 'failed');

CREATE TRIGGER domain_outbox_events_touch_updated_at
    BEFORE UPDATE ON domain_outbox_events
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();

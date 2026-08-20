-- Persist Telegram updates before acknowledging webhook delivery. Processing
-- remains asynchronous, but crashes can only delay an update, not erase it.

CREATE TABLE telegram_inbound_updates (
    update_id    BIGINT PRIMARY KEY,
    payload      JSONB NOT NULL,
    payload_hash TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    attempts     INTEGER NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at   TIMESTAMPTZ,
    claim_owner  TEXT NOT NULL DEFAULT '',
    processed_at TIMESTAMPTZ,
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX telegram_inbound_updates_due_idx
    ON telegram_inbound_updates (available_at, update_id)
    WHERE status IN ('pending', 'processing');

CREATE TRIGGER telegram_inbound_updates_touch_updated_at
    BEFORE UPDATE ON telegram_inbound_updates
    FOR EACH ROW EXECUTE FUNCTION nbco_touch_updated_at();

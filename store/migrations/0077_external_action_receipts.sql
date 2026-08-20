-- Programmatic transport actions (for example Telegram slash commands) do not
-- pass through the durable Agent turn. Claim their source identity before any
-- mutation so an at-least-once inbound update cannot toggle or create twice.
CREATE TABLE external_action_receipts (
    action_key   TEXT PRIMARY KEY,
    kind         TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'started'
                 CHECK (status IN ('started','completed','failed')),
    last_error   TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_external_action_receipts_status
    ON external_action_receipts(status, updated_at DESC);

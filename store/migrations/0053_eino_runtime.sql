-- Durable Eino agent sessions and interruption checkpoints. Chat messages stay
-- in their product-facing tables; this append-only log preserves model/tool
-- context exactly as Eino emitted it so a process restart can resume safely.
CREATE TABLE eino_session_events (
    seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, event_id)
);

CREATE INDEX idx_eino_session_events_session_seq
    ON eino_session_events (session_id, seq);

CREATE INDEX idx_eino_session_events_session_kind_seq
    ON eino_session_events (session_id, kind, seq);

CREATE TABLE eino_checkpoints (
    checkpoint_id TEXT PRIMARY KEY,
    payload BYTEA NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

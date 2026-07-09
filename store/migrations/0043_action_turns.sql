CREATE TABLE IF NOT EXISTS action_turns (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id        BIGINT REFERENCES users(id) ON DELETE CASCADE,
    session_id     BIGINT REFERENCES chat_sessions(id) ON DELETE SET NULL,
    channel        TEXT NOT NULL DEFAULT '',
    user_text_hash TEXT NOT NULL DEFAULT '',
    requires_action BOOLEAN NOT NULL DEFAULT FALSE,
    intent         TEXT NOT NULL DEFAULT '',
    expected_tools TEXT[] NOT NULL DEFAULT '{}',
    evidence       JSONB NOT NULL DEFAULT '{}',
    outcome        TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_action_turns_user ON action_turns(user_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_action_turns_session ON action_turns(session_id, id DESC);

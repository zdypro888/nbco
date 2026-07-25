CREATE TABLE conversation_turns (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_key_hash      TEXT NOT NULL,
    user_id              BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id           BIGINT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    channel              TEXT NOT NULL,
    user_message_id      BIGINT UNIQUE REFERENCES chat_messages(id) ON DELETE SET NULL,
    assistant_message_id BIGINT UNIQUE REFERENCES chat_messages(id) ON DELETE SET NULL,
    status               TEXT NOT NULL DEFAULT 'running'
                         CHECK (status IN ('running', 'completed', 'failed')),
    delivery_status      TEXT NOT NULL DEFAULT 'pending'
                         CHECK (delivery_status IN ('pending', 'delivered', 'failed')),
    result_text          TEXT NOT NULL DEFAULT '',
    engine_session       TEXT NOT NULL DEFAULT '',
    attempts             INTEGER NOT NULL DEFAULT 1,
    delivery_attempts    INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT '',
    started_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at         TIMESTAMPTZ,
    delivered_at         TIMESTAMPTZ,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, channel, source_key_hash)
);

CREATE INDEX idx_conversation_turns_session ON conversation_turns(session_id, id DESC);
CREATE INDEX idx_conversation_turns_running ON conversation_turns(started_at, id)
    WHERE status = 'running';
CREATE INDEX idx_conversation_turns_delivery ON conversation_turns(updated_at, id)
    WHERE status = 'completed' AND delivery_status <> 'delivered';

ALTER TABLE action_turns
    ADD COLUMN conversation_turn_id BIGINT REFERENCES conversation_turns(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX idx_action_turns_conversation_turn
    ON action_turns(conversation_turn_id) WHERE conversation_turn_id IS NOT NULL;

ALTER TABLE ai_usage
    ADD COLUMN conversation_turn_id BIGINT REFERENCES conversation_turns(id) ON DELETE SET NULL;
CREATE UNIQUE INDEX idx_ai_usage_conversation_turn
    ON ai_usage(conversation_turn_id) WHERE conversation_turn_id IS NOT NULL;

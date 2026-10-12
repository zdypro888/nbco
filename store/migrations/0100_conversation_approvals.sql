ALTER TABLE conversation_turns DROP CONSTRAINT conversation_turns_status_check;
ALTER TABLE conversation_turns ADD CONSTRAINT conversation_turns_status_check
    CHECK (status IN ('running','awaiting_approval','completed','failed'));

CREATE TABLE conversation_approvals (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    turn_id BIGINT NOT NULL REFERENCES conversation_turns(id) ON DELETE CASCADE,
    checkpoint_id TEXT NOT NULL,
	consent_key BYTEA NOT NULL CHECK (octet_length(consent_key)=32),
    engine_session TEXT NOT NULL,
    tools JSONB NOT NULL CHECK (jsonb_typeof(tools)='array'),
    decisions JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(decisions)='object'),
    progress JSONB NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','claimed')),
    consent_message_id BIGINT REFERENCES chat_messages(id) ON DELETE SET NULL,
    expires_at TIMESTAMPTZ NOT NULL DEFAULT now() + interval '10 minutes',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX conversation_approvals_pending_turn ON conversation_approvals(turn_id)
    WHERE status='pending';
CREATE UNIQUE INDEX conversation_approvals_pending_checkpoint ON conversation_approvals(checkpoint_id)
    WHERE status='pending';

-- Native grants can only be consumed by the exact authenticated consent message.
-- NULL preserves ordinary cross-turn conversational approval semantics.
ALTER TABLE pending_approvals ADD COLUMN consent_message_id BIGINT REFERENCES chat_messages(id) ON DELETE CASCADE;

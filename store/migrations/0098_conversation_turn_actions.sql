ALTER TABLE conversation_turns
    ADD COLUMN result_actions JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE conversation_turns
    ADD CONSTRAINT conversation_turns_result_actions_array
    CHECK (jsonb_typeof(result_actions) = 'array');

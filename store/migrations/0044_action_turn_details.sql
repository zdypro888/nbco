ALTER TABLE action_turns ADD COLUMN IF NOT EXISTS user_text_excerpt TEXT NOT NULL DEFAULT '';
ALTER TABLE action_turns ADD COLUMN IF NOT EXISTS reply_excerpt TEXT NOT NULL DEFAULT '';
ALTER TABLE action_turns ADD COLUMN IF NOT EXISTS tool_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE action_turns ADD COLUMN IF NOT EXISTS success_tool_count INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_action_turns_outcome ON action_turns(outcome, id DESC);

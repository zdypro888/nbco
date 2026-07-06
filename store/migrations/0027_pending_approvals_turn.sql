-- High-risk tool approval must cross a real user confirmation message.
-- Old pending rows predate this invariant, so discard them rather than letting
-- a stale same-turn approval execute after migration.
DELETE FROM pending_approvals;

ALTER TABLE pending_approvals ADD COLUMN IF NOT EXISTS session_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pending_approvals ADD COLUMN IF NOT EXISTS requested_message_id BIGINT NOT NULL DEFAULT 0;

DROP INDEX IF EXISTS pending_approvals_user_idx;
CREATE INDEX IF NOT EXISTS pending_approvals_user_idx
    ON pending_approvals (user_id, tool, args_hash, session_id, requested_message_id);

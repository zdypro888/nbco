-- A durable tool receipt must be able to recover the original tool result
-- after a process/HTTP response loss. This is especially important for
-- one-time credentials and invite codes that cannot be reconstructed from a
-- hash. Results expire quickly; the no-replay receipt remains longer.
ALTER TABLE external_action_receipts
    ADD COLUMN result_text TEXT NOT NULL DEFAULT '',
    ADD COLUMN result_expires_at TIMESTAMPTZ;

CREATE INDEX idx_external_action_receipts_result_expiry
    ON external_action_receipts(result_expires_at)
    WHERE result_expires_at IS NOT NULL;

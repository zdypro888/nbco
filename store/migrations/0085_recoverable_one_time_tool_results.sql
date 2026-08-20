-- One-time credentials may commit immediately before an Agent process loses
-- the tool result. Retain a short-lived, invocation-scoped recovery value so
-- the exact logical call can reconstruct its output without issuing again.
ALTER TABLE bind_keys
    ADD COLUMN request_key TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX bind_keys_request_once
    ON bind_keys (created_by, request_key)
    WHERE request_key <> '';

ALTER TABLE worker_bind_codes
    ADD COLUMN request_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN recovery_code TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX worker_bind_codes_request_once
    ON worker_bind_codes (created_by, request_key)
    WHERE request_key <> '';

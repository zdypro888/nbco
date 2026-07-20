-- Native CLI sessions capture their original model/provider configuration.
-- Keep that compatibility boundary explicit so a worker does not resume a
-- session after its engine binary, arguments or provider configuration changes.
ALTER TABLE worker_sessions
    ADD COLUMN engine_runtime_fingerprint TEXT NOT NULL DEFAULT ''
    CHECK (engine_runtime_fingerprint = '' OR engine_runtime_fingerprint ~ '^[0-9a-f]{64}$');

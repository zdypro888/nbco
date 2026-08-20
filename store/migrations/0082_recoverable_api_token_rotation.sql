-- API token rotation crosses two external boundaries: PostgreSQL first, then
-- Telegram delivery. Keep the generated plaintext only for the short
-- confirmation window so a lost Telegram response can recover the same token
-- instead of invalidating the old token and losing the replacement forever.

-- Older code serialized replacement only inside one process. Remove any
-- historical duplicates before enforcing the invariant in PostgreSQL.
DELETE FROM api_tokens older
USING api_tokens newer
WHERE older.user_id = newer.user_id
  AND (older.created_at, older.token_hash) < (newer.created_at, newer.token_hash);

CREATE UNIQUE INDEX idx_api_tokens_one_per_user ON api_tokens(user_id);

CREATE TABLE api_token_rotations (
    user_id    BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    candidate  TEXT NOT NULL CHECK (candidate ~ '^[0-9a-f]{48}$'),
    expires_at TIMESTAMPTZ NOT NULL,
    issued_at  TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_token_rotations_expiry ON api_token_rotations(expires_at);

-- Pending confirmations from the old implementation contain only an expiry
-- timestamp and cannot recover a plaintext token. Drop them during migration.
DELETE FROM kv_state WHERE key LIKE 'telegram.pending_api_token:%';

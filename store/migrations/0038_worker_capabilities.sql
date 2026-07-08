CREATE TABLE IF NOT EXISTS worker_capabilities (
    worker_id    BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    engine       TEXT NOT NULL DEFAULT '',
    cli_name     TEXT NOT NULL DEFAULT '',
    cli_version  TEXT NOT NULL DEFAULT '',
    os           TEXT NOT NULL DEFAULT '',
    arch         TEXT NOT NULL DEFAULT '',
    hostname     TEXT NOT NULL DEFAULT '',
    workdir      TEXT NOT NULL DEFAULT '',
    capabilities TEXT[] NOT NULL DEFAULT '{}',
    metadata     JSONB NOT NULL DEFAULT '{}',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_worker_capabilities_caps ON worker_capabilities USING GIN(capabilities);
CREATE INDEX IF NOT EXISTS idx_worker_capabilities_updated ON worker_capabilities(updated_at DESC);

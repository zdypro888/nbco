-- Worker topic sessions: a stable execution context above individual tasks.
-- It lets a worker reuse the right workspace/summary for "nbco codebase",
-- "company materials", or a project without smearing all tasks into one long
-- Claude/Codex conversation.
CREATE TABLE worker_sessions (
    id BIGSERIAL PRIMARY KEY,
    worker_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    engine TEXT NOT NULL DEFAULT '',
    scope_type TEXT NOT NULL DEFAULT '',
    scope_key TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    workdir TEXT NOT NULL DEFAULT '',
    engine_session_ref TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    last_task_id BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    use_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (worker_id, engine, scope_type, scope_key)
);

CREATE INDEX idx_worker_sessions_worker_updated ON worker_sessions (worker_id, updated_at DESC);

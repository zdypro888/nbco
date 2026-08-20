-- Worker builtin Agents retry model requests across transient transport
-- failures. Persist the logical call before crossing the upstream model
-- boundary, then cache its exact response so a lost HTTP response does not
-- cause a second billed/model-dependent execution.
CREATE TABLE worker_llm_calls (
    worker_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id   TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'started'
                 CHECK (status IN ('started','completed','failed')),
    http_status  INTEGER,
    response_body BYTEA,
    last_error   TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (worker_id, request_id)
);

CREATE INDEX idx_worker_llm_calls_terminal_age
    ON worker_llm_calls(updated_at)
    WHERE status IN ('completed','failed');

CREATE TABLE script_tools (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL,
    runtime         TEXT NOT NULL,
    input_schema    JSONB NOT NULL DEFAULT '{}',
    source          TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT FALSE,
    required_action TEXT NOT NULL DEFAULT '',
    created_by      BIGINT NOT NULL REFERENCES users(id),
    last_test_result TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_script_tools_enabled ON script_tools(enabled);

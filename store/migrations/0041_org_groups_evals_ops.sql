CREATE TABLE IF NOT EXISTS org_groups (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    manager_id  BIGINT REFERENCES users(id),
    created_by  BIGINT REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS org_group_members (
    group_id   BIGINT NOT NULL REFERENCES org_groups(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_org_group_members_user ON org_group_members(user_id);

CREATE TABLE IF NOT EXISTS telegram_group_projects (
    chat_id    BIGINT PRIMARY KEY,
    project_id BIGINT REFERENCES projects(id) ON DELETE SET NULL,
    bound_by   BIGINT REFERENCES users(id),
    note       TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_telegram_group_projects_project ON telegram_group_projects(project_id);

CREATE TABLE IF NOT EXISTS conversation_eval_cases (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    channel     TEXT NOT NULL DEFAULT 'telegram',
    user_input  TEXT NOT NULL,
    assertions  JSONB NOT NULL DEFAULT '{}',
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_by  BIGINT REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS conversation_eval_runs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    case_id     BIGINT REFERENCES conversation_eval_cases(id) ON DELETE SET NULL,
    status      TEXT NOT NULL,
    output      TEXT NOT NULL DEFAULT '',
    details     JSONB NOT NULL DEFAULT '{}',
    ran_by      BIGINT REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_eval_cases_enabled ON conversation_eval_cases(enabled, id);
CREATE INDEX IF NOT EXISTS idx_eval_runs_case ON conversation_eval_runs(case_id, id DESC);

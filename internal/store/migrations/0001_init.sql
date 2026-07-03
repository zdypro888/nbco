-- 初始 schema。设计要点：
--   * users.id 为内部主键，IM 身份解耦到 identities（入口皆可换，中枢不可换）
--   * 一切运行状态落库：绑定 key、定时任务、AI 会话 —— 进程无状态可随时重启
--   * 用户停用（status）而非删除，历史数据保留

CREATE TABLE users (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name          TEXT NOT NULL DEFAULT '',
    info          JSONB NOT NULL DEFAULT '{}',
    status        TEXT NOT NULL DEFAULT 'active',
    is_superadmin BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE identities (
    provider    TEXT NOT NULL,
    external_id TEXT NOT NULL,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    chat_ref    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider, external_id)
);
CREATE INDEX idx_identities_user ON identities(user_id);

CREATE TABLE info_fields (
    name       TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE profiles (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    subject_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    author_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    position   INT NOT NULL,
    content    TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (subject_id, author_id, position)
);

CREATE TABLE permissions (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind       TEXT NOT NULL,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL,
    granted_by BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (kind, user_id, action, target)
);

CREATE TABLE projects (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    creator_id  BIGINT NOT NULL REFERENCES users(id),
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    parent_id   BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    assigner_id BIGINT NOT NULL REFERENCES users(id),
    assignee_id BIGINT NOT NULL REFERENCES users(id),
    title       TEXT NOT NULL,
    goal        TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    acceptance  TEXT NOT NULL DEFAULT '',
    priority    TEXT NOT NULL DEFAULT 'normal',
    deadline    TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'pending',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_tasks_assignee_open ON tasks(assignee_id) WHERE status IN ('pending', 'in_progress');
CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_parent ON tasks(parent_id);
CREATE INDEX idx_tasks_assigner ON tasks(assigner_id);

CREATE TABLE task_checklist (
    id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id  BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    position INT NOT NULL,
    item     TEXT NOT NULL,
    done     BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (task_id, position)
);

CREATE TABLE task_progress (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id    BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    author_id  BIGINT NOT NULL,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_progress_task ON task_progress(task_id, id);

CREATE TABLE task_attachments (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    task_id    BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL DEFAULT 'photo',
    file_ref   TEXT NOT NULL,
    caption    TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE schedules (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    message    TEXT NOT NULL,
    fire_at    TIMESTAMPTZ NOT NULL,
    interval_s BIGINT NOT NULL DEFAULT 0,
    status     TEXT NOT NULL DEFAULT 'active',
    last_fired TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_schedules_due ON schedules(fire_at) WHERE status = 'active';

CREATE TABLE roles (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    trigger_desc TEXT NOT NULL DEFAULT '',
    prompt       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_active_roles (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    role_id BIGINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE
);

CREATE TABLE bind_keys (
    key        TEXT PRIMARY KEY,
    created_by BIGINT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_by    BIGINT,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

CREATE TABLE chat_sessions (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel    TEXT NOT NULL DEFAULT 'telegram',
    engine     TEXT NOT NULL,
    engine_ref TEXT NOT NULL DEFAULT '',
    active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_sessions_one_active ON chat_sessions(user_id, channel) WHERE active;

CREATE TABLE chat_messages (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id BIGINT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_messages_session ON chat_messages(session_id, id);

CREATE TABLE audit_log (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL,
    session_id BIGINT,
    tool       TEXT NOT NULL,
    args       JSONB,
    result     TEXT NOT NULL DEFAULT '',
    ok         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_user ON audit_log(user_id, id DESC);

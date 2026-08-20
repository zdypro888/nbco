-- Product operating loop: preserve stable message provenance, connect informal
-- work evidence with canonical work, give uploaded materials an explicit
-- lifecycle, and attribute retrieved knowledge assets to completed turns.

ALTER TABLE chat_messages
    ADD COLUMN provider TEXT NOT NULL DEFAULT '',
    ADD COLUMN external_chat_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN external_message_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN actor_user_id BIGINT REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN external_actor_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN actor_display_name TEXT NOT NULL DEFAULT '',
    ADD COLUMN reply_to_external_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN thread_ref TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_created_at TIMESTAMPTZ,
    ADD COLUMN source_metadata JSONB NOT NULL DEFAULT '{}';

CREATE UNIQUE INDEX idx_chat_messages_external_source
    ON chat_messages(provider, external_chat_ref, external_message_ref)
    WHERE provider <> '' AND external_chat_ref <> '' AND external_message_ref <> '';
CREATE INDEX idx_chat_messages_actor_recent
    ON chat_messages(actor_user_id, id DESC)
    WHERE actor_user_id IS NOT NULL;

-- Telegram webhook retries must resolve to the same intake even if older
-- deployments already recorded duplicate attempts. Retain those rows for
-- audit, but select the best completed attempt as the canonical identity.
ALTER TABLE file_intakes ADD COLUMN canonical BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE file_intakes SET canonical = FALSE WHERE external_ref <> '';
WITH preferred AS (
    SELECT DISTINCT ON (user_id, source, external_ref) id
      FROM file_intakes
     WHERE external_ref <> ''
     ORDER BY user_id, source, external_ref,
              CASE status WHEN 'saved' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END,
              id DESC
)
UPDATE file_intakes intake SET canonical = TRUE
  FROM preferred WHERE intake.id = preferred.id;

CREATE UNIQUE INDEX idx_file_intakes_canonical_source
    ON file_intakes(user_id, source, external_ref)
    WHERE canonical AND external_ref <> '';

-- Existing private user messages have an unambiguous internal actor even though
-- their original provider message identifiers were not retained.
UPDATE chat_messages message
   SET actor_user_id = session.user_id,
       actor_display_name = COALESCE(user_row.name, '')
  FROM chat_sessions session
  JOIN users user_row ON user_row.id = session.user_id
 WHERE message.session_id = session.id
   AND message.role = 'user'
   AND session.channel NOT LIKE '%:group:%';

CREATE TABLE work_evidence (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_type       TEXT NOT NULL,
    source_key        TEXT NOT NULL,
    kind              TEXT NOT NULL DEFAULT 'communication'
                      CHECK (kind IN ('communication', 'summary', 'update', 'decision', 'risk', 'deliverable')),
    status            TEXT NOT NULL DEFAULT 'observed'
                      CHECK (status IN ('observed', 'active', 'resolved', 'superseded', 'ignored')),
    title             TEXT NOT NULL DEFAULT '',
    content           TEXT NOT NULL,
    actor_user_id     BIGINT REFERENCES users(id) ON DELETE SET NULL,
    project_id        BIGINT REFERENCES projects(id) ON DELETE SET NULL,
    task_id           BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    worker_run_id     BIGINT REFERENCES worker_runs(id) ON DELETE SET NULL,
    source_message_id BIGINT REFERENCES chat_messages(id) ON DELETE SET NULL,
    confidence        REAL NOT NULL DEFAULT 1 CHECK (confidence >= 0 AND confidence <= 1),
    event_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata          JSONB NOT NULL DEFAULT '{}',
    created_by        BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_key)
);

CREATE INDEX idx_work_evidence_recent ON work_evidence(event_at DESC, id DESC);
CREATE INDEX idx_work_evidence_project_recent ON work_evidence(project_id, event_at DESC, id DESC);
CREATE INDEX idx_work_evidence_actor_recent ON work_evidence(actor_user_id, event_at DESC, id DESC);
CREATE INDEX idx_work_evidence_actionable ON work_evidence(status, kind, event_at DESC)
    WHERE status IN ('observed', 'active');

CREATE TABLE material_cases (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner_id       BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source         TEXT NOT NULL,
    source_ref     TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    instruction    TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'received'
                   CHECK (status IN ('received', 'queued', 'processing', 'needs_input', 'completed', 'ignored')),
    task_id        BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    worker_run_id  BIGINT REFERENCES worker_runs(id) ON DELETE SET NULL,
    last_error     TEXT NOT NULL DEFAULT '',
    created_by     BIGINT REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ,
    UNIQUE (owner_id, source, source_ref)
);

CREATE TABLE material_case_files (
    case_id    BIGINT NOT NULL REFERENCES material_cases(id) ON DELETE CASCADE,
    file_id    BIGINT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (case_id, file_id)
);

CREATE INDEX idx_material_cases_owner_status ON material_cases(owner_id, status, id DESC);
CREATE INDEX idx_material_cases_task ON material_cases(task_id) WHERE task_id IS NOT NULL;
CREATE INDEX idx_material_case_files_file ON material_case_files(file_id, case_id);

-- Every existing user upload becomes an explicit unresolved material case.
INSERT INTO material_cases (owner_id, source, source_ref, title, created_by, created_at, updated_at)
SELECT f.created_by, f.source, 'legacy-file:' || f.id, f.original_name, f.created_by, f.created_at, f.created_at
  FROM files f
 WHERE f.created_by IS NOT NULL
   AND f.source IN ('telegram', 'api')
   AND NOT EXISTS (SELECT 1 FROM task_attachments a WHERE a.file_id = f.id)
   AND NOT EXISTS (SELECT 1 FROM task_artifacts a WHERE a.file_id = f.id)
   AND NOT EXISTS (SELECT 1 FROM worker_run_files rf WHERE rf.file_id = f.id)
   AND NOT EXISTS (SELECT 1 FROM material_entities e WHERE e.file_id = f.id)
ON CONFLICT DO NOTHING;

INSERT INTO material_case_files (case_id, file_id, created_at)
SELECT c.id, f.id, f.created_at
  FROM files f
  JOIN material_cases c
    ON c.owner_id = f.created_by
   AND c.source = f.source
   AND c.source_ref = 'legacy-file:' || f.id
 WHERE f.source IN ('telegram', 'api')
ON CONFLICT DO NOTHING;

CREATE TABLE conversation_asset_usages (
    conversation_turn_id BIGINT NOT NULL REFERENCES conversation_turns(id) ON DELETE CASCADE,
    knowledge_id         BIGINT NOT NULL REFERENCES knowledge(id) ON DELETE CASCADE,
    phase                TEXT NOT NULL CHECK (phase IN ('injected', 'candidate', 'loaded')),
    turn_outcome         TEXT NOT NULL DEFAULT 'completed'
                         CHECK (turn_outcome IN ('completed', 'action_succeeded', 'partial', 'failed')),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conversation_turn_id, knowledge_id, phase)
);

CREATE INDEX idx_conversation_asset_usage_knowledge
    ON conversation_asset_usages(knowledge_id, phase, created_at DESC);

-- A small, generic smoke suite makes the eval runner useful immediately.
-- Mutating tools are simulated by the runner, so these cases never alter data.
INSERT INTO conversation_eval_cases (name, channel, user_input, assertions)
VALUES
    ('employee_directory_tool_selection', 'telegram',
     '请读取当前员工目录并简洁汇报。',
     '{"required_any_tool_groups":[["list_users","get_users_info","query_data"]],"forbidden_substrings":["<table"],"no_markdown_table":true,"max_tool_calls":12}'::jsonb),
    ('schedule_read_tool_selection', 'telegram',
     '请核实我当前可见的定时自动化，只汇报系统事实。',
     '{"required_any_tool_groups":[["list_schedules","query_data"]],"forbidden_substrings":["<table"],"no_markdown_table":true,"max_tool_calls":12}'::jsonb),
    ('schedule_write_agent_loop', 'telegram',
     '给我创建一个明天上午九点的一次提醒，内容是检查项目进度。',
     '{"required_any_tool_groups":[["schedule_once","schedule_once_push"]],"min_successful_tools":1,"forbidden_substrings":["<table"],"no_markdown_table":true,"max_tool_calls":12,"tool_results":{"schedule_once":"{\"status\":\"ok\",\"message\":\"评测模拟：一次提醒已创建。\"}","schedule_once_push":"{\"status\":\"ok\",\"message\":\"评测模拟：一次提醒已创建。\"}"}}'::jsonb)
ON CONFLICT (name) DO NOTHING;

-- 验收流：done = 已提交待验收，accepted = 验收通过（自派任务提交即通过）。
-- 催办标记：nudged_at 记录最近一次 AI 催办，调度器原子认领防重发。
ALTER TABLE tasks ADD COLUMN nudged_at TIMESTAMPTZ;

-- 知识库：对话与任务中沉淀的可复用结论（决策、流程、方案、约定）。
CREATE TABLE knowledge (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title      TEXT NOT NULL,
    content    TEXT NOT NULL,
    tags       TEXT[] NOT NULL DEFAULT '{}',
    author_id  BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_knowledge_tags ON knowledge USING GIN (tags);

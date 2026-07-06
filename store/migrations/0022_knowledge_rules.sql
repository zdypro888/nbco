-- 知识库规则层（Policy Memory）：kind 区分普通知识（fact）与行为规则（policy），
-- pinned 的规则每轮常驻系统提示，其余规则按语义相关度动态注入。
-- 作用域复用 tags（scope:global / scope:telegram / scope:worker / scope:user:<id>）。
ALTER TABLE knowledge ADD COLUMN kind TEXT NOT NULL DEFAULT 'fact';
ALTER TABLE knowledge ADD COLUMN pinned BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX knowledge_kind_idx ON knowledge (kind) WHERE kind <> 'fact';

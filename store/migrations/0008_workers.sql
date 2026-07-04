-- AI 员工（worker）：装在工作机上的 client，用 PTY 驱动 claude/codex 干活。
-- worker 本质是一个特殊用户——复用整套任务/验收/催办/画像机制，只是执行者是机器。
ALTER TABLE users ADD COLUMN is_worker BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users ADD COLUMN owner_id BIGINT REFERENCES users(id);   -- 监护人：谁创建/负责这个 worker
ALTER TABLE users ADD COLUMN worker_last_seen TIMESTAMPTZ;            -- 最近一次上线心跳
CREATE INDEX idx_users_worker ON users (owner_id) WHERE is_worker;

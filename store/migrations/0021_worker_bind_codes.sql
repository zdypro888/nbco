-- Worker 绑定改一次性兑换码：聊天里只出现短时效绑定码（wbc_ 前缀），
-- 真正的 Worker Access Token 由工作机兑换时才签发，永不进入对话与会话历史。
-- 码以 sha256 哈希落库（与 api_tokens 同规），一次一用，过期即废。
CREATE TABLE worker_bind_codes (
    code_hash  TEXT PRIMARY KEY,
    worker_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_by BIGINT NOT NULL REFERENCES users(id),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX worker_bind_codes_worker_idx ON worker_bind_codes (worker_id);

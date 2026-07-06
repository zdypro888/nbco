-- 通用两段式审批：破坏性工具第一次调用登记待确认动作，与用户确认后以相同参数
-- 再次调用才执行。防单轮冲动执行与提示注入一击即中；对所有渠道生效。
CREATE TABLE pending_approvals (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL,
    tool       TEXT NOT NULL,
    args_hash  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX pending_approvals_user_idx ON pending_approvals (user_id, tool, args_hash);

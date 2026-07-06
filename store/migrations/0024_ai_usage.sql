-- AI 成本计量：每次模型调用的 token 用量流水（对话轮次、压缩轮、worker 内置
-- 智能体管道、文件摘要）。老板要能算清每个 AI 员工、每个任务花了多少。
CREATE TABLE ai_usage (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL,
    session_id    BIGINT,
    kind          TEXT NOT NULL,             -- 渠道/用途：telegram / api / worker_llm / compact / summarize …
    model         TEXT NOT NULL DEFAULT '',
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ai_usage_created_idx ON ai_usage (created_at);

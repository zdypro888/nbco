-- 系统事件审计：领域事件（任务提交待验收/worker 离线/任务过期/前置完成等）
-- 交 AI 处理的留痕，用于可观测与审计——哪些事件被 AI 静默、哪些降级原文、
-- 哪些已通知。事件处理是 fire-and-forget，结果只在这里记录；落库失败不阻断业务。
CREATE TABLE events (
    id BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL,
    decider_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    detail TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT '',   -- handled/skipped/fallback/dropped/send_failed
    reply TEXT NOT NULL DEFAULT '',      -- AI 答复摘要（截断），便于审计
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    handled_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_decider_created ON events (decider_id, created_at DESC);
CREATE INDEX idx_events_kind_created ON events (kind, created_at DESC);

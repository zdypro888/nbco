-- 会话滚动摘要（eino 直连 API 的"自动压缩"）：
-- summary 存较早对话的压缩摘要，summary_upto 是已折叠进摘要的最后一条消息 ID；
-- 重放历史 = 摘要 + summary_upto 之后的消息，早期决定不随硬截断丢失。
ALTER TABLE chat_sessions ADD COLUMN summary TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_sessions ADD COLUMN summary_upto BIGINT NOT NULL DEFAULT 0;
-- 群共享会话按 channel 查活跃会话（channel 形如 telegram:group:<chatID>）。
CREATE INDEX idx_chat_sessions_channel_active ON chat_sessions(channel) WHERE active;

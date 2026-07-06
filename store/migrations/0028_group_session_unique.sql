-- 同一群渠道最多一个活跃会话：两名群成员同时首次 @bot 时，各自事务都看不到
-- 对方未提交的 INSERT，会留下两行 active 会话，后续消息分裂写入两个上下文。
-- 群渠道形如 telegram:group:<chatID> 全局唯一；私聊 channel（如 telegram）
-- 各用户相同，不能按 channel 全局唯一，故只对群加部分唯一索引。
-- 先清历史上已产生的重复活跃行（保留最新一条）。
UPDATE chat_sessions s SET active = FALSE
 WHERE s.active AND s.channel LIKE '%:group:%'
   AND s.id < (SELECT max(id) FROM chat_sessions m WHERE m.channel = s.channel AND m.active);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_one_active_group
    ON chat_sessions (channel) WHERE active AND channel LIKE '%:group:%';

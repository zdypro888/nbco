-- 进程级持久状态（如每日汇总的最后发送日），保证重启不重发。
CREATE TABLE kv_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

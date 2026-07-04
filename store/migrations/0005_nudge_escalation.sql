-- 催办升级：记录累计催办次数，连续多次无响应时通知分配者介入。
ALTER TABLE tasks ADD COLUMN nudge_count INT NOT NULL DEFAULT 0;

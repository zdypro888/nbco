-- 任务截止提醒/过期通知的发送标记：调度器用 UPDATE...RETURNING 原子认领，重启不重发。
ALTER TABLE tasks ADD COLUMN deadline_reminded_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN overdue_notified_at TIMESTAMPTZ;
-- 调度器周期扫开放任务的截止时间。
CREATE INDEX idx_tasks_open_deadline ON tasks (deadline)
  WHERE deadline IS NOT NULL AND status IN ('pending', 'in_progress');

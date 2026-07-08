-- 幂等补齐 milestones 的提醒列：0035 在某些库上以「仅 goals 列」的旧版本跑过一次后，
-- 同名文件不会重放（schema_migrations 按文件名记录）。这里用 IF NOT EXISTS 兜底，
-- 确保所有库最终都有里程碑提醒列，否则 DueMilestone*/MarkMilestone* 每拍报 column 不存在。
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS deadline_reminded_at         TIMESTAMPTZ;
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS overdue_notified_at          TIMESTAMPTZ;
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS deadline_reminder_claimed_at TIMESTAMPTZ;
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS overdue_notice_claimed_at    TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_milestones_open_deadline ON milestones (deadline)
  WHERE deadline IS NOT NULL AND status = 'active';

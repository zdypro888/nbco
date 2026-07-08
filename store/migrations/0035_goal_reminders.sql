-- 目标 / 里程碑截止提醒/过期通知：镜像任务提醒（见 0003/0005/0015）的「原子认领 + ack」模式。
-- goalCols / milestoneCols 不含这 8 列（与 tasks 的提醒列同样不进 Task 结构）——它们是调度器
-- 内部状态，由 DueGoal*/MarkGoal* / DueMilestone*/MarkMilestone* 方法用 UPDATE...RETURNING 认领、独立 ack。

-- goals：战略目标级提醒。
ALTER TABLE goals ADD COLUMN IF NOT EXISTS deadline_reminded_at         TIMESTAMPTZ;
ALTER TABLE goals ADD COLUMN IF NOT EXISTS overdue_notified_at          TIMESTAMPTZ;
ALTER TABLE goals ADD COLUMN IF NOT EXISTS deadline_reminder_claimed_at TIMESTAMPTZ;
ALTER TABLE goals ADD COLUMN IF NOT EXISTS overdue_notice_claimed_at    TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_goals_open_deadline ON goals (deadline)
  WHERE deadline IS NOT NULL AND status = 'active';

-- milestones：里程碑级提醒。里程碑过期更值得点名——它卡住会连带阻塞整个目标的下游。
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS deadline_reminded_at         TIMESTAMPTZ;
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS overdue_notified_at          TIMESTAMPTZ;
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS deadline_reminder_claimed_at TIMESTAMPTZ;
ALTER TABLE milestones ADD COLUMN IF NOT EXISTS overdue_notice_claimed_at    TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_milestones_open_deadline ON milestones (deadline)
  WHERE deadline IS NOT NULL AND status = 'active';

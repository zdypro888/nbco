-- Reminder selection must distinguish "transport occurrence settled" from
-- "channel acknowledged delivery". The former prevents blind replay after an
-- ambiguous boundary; the latter remains truthful product/audit data.
ALTER TABLE tasks
    ADD COLUMN deadline_reminder_attempted_at TIMESTAMPTZ,
    ADD COLUMN overdue_notice_attempted_at TIMESTAMPTZ;

ALTER TABLE goals
    ADD COLUMN deadline_reminder_attempted_at TIMESTAMPTZ,
    ADD COLUMN overdue_notice_attempted_at TIMESTAMPTZ;

ALTER TABLE milestones
    ADD COLUMN deadline_reminder_attempted_at TIMESTAMPTZ,
    ADD COLUMN overdue_notice_attempted_at TIMESTAMPTZ;

UPDATE tasks
SET deadline_reminder_attempted_at = deadline_reminded_at,
    overdue_notice_attempted_at = overdue_notified_at;

UPDATE goals
SET deadline_reminder_attempted_at = deadline_reminded_at,
    overdue_notice_attempted_at = overdue_notified_at;

UPDATE milestones
SET deadline_reminder_attempted_at = deadline_reminded_at,
    overdue_notice_attempted_at = overdue_notified_at;

-- Reliable scheduler delivery leases. Due scans claim work first; sent markers are
-- written only after notifier success. If nbco crashes mid-delivery, the lease
-- expires and the next tick retries instead of silently losing the reminder.
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS delivery_claimed_at TIMESTAMPTZ;

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS deadline_reminder_claimed_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS overdue_notice_claimed_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS nudge_claimed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_schedules_delivery_claim
    ON schedules (fire_at, delivery_claimed_at)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_tasks_deadline_reminder_claim
    ON tasks (deadline, deadline_reminder_claimed_at)
    WHERE status IN ('pending', 'in_progress') AND deadline IS NOT NULL AND deadline_reminded_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_tasks_overdue_notice_claim
    ON tasks (deadline, overdue_notice_claimed_at)
    WHERE status IN ('pending', 'in_progress') AND deadline IS NOT NULL AND overdue_notified_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_tasks_nudge_claim
    ON tasks (nudge_claimed_at)
    WHERE status IN ('pending', 'in_progress') AND overdue_notified_at IS NOT NULL;

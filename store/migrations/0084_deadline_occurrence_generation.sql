-- A deadline value is not a stable occurrence identity: it may be changed
-- away and later restored. Keep a monotonic generation in the database so
-- delivery receipts and claim acknowledgements belong to one exact revision.
ALTER TABLE tasks
    ADD COLUMN deadline_generation BIGINT NOT NULL DEFAULT 1
        CHECK (deadline_generation > 0);

ALTER TABLE goals
    ADD COLUMN deadline_generation BIGINT NOT NULL DEFAULT 1
        CHECK (deadline_generation > 0);

ALTER TABLE milestones
    ADD COLUMN deadline_generation BIGINT NOT NULL DEFAULT 1
        CHECK (deadline_generation > 0);

CREATE OR REPLACE FUNCTION nbco_advance_deadline_generation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.deadline IS DISTINCT FROM OLD.deadline THEN
        NEW.deadline_generation := OLD.deadline_generation + 1;
        NEW.deadline_reminded_at := NULL;
        NEW.overdue_notified_at := NULL;
        NEW.deadline_reminder_attempted_at := NULL;
        NEW.overdue_notice_attempted_at := NULL;
        NEW.deadline_reminder_claimed_at := NULL;
        NEW.overdue_notice_claimed_at := NULL;
        NEW.updated_at := now();
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION nbco_advance_task_deadline_generation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.deadline IS DISTINCT FROM OLD.deadline THEN
        NEW.deadline_generation := OLD.deadline_generation + 1;
        NEW.deadline_reminded_at := NULL;
        NEW.overdue_notified_at := NULL;
        NEW.deadline_reminder_attempted_at := NULL;
        NEW.overdue_notice_attempted_at := NULL;
        NEW.deadline_reminder_claimed_at := NULL;
        NEW.overdue_notice_claimed_at := NULL;
        NEW.nudge_claimed_at := NULL;
        NEW.nudged_at := NULL;
        NEW.nudge_count := 0;
        NEW.nudge_attempt_count := 0;
        NEW.updated_at := now();
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER tasks_advance_deadline_generation
BEFORE UPDATE OF deadline ON tasks
FOR EACH ROW EXECUTE FUNCTION nbco_advance_task_deadline_generation();

CREATE TRIGGER goals_advance_deadline_generation
BEFORE UPDATE OF deadline ON goals
FOR EACH ROW EXECUTE FUNCTION nbco_advance_deadline_generation();

CREATE TRIGGER milestones_advance_deadline_generation
BEFORE UPDATE OF deadline ON milestones
FOR EACH ROW EXECUTE FUNCTION nbco_advance_deadline_generation();

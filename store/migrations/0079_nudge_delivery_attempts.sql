-- A delivery attempt and a confirmed nudge are different facts. Attempts drive
-- stable transport identities across lease reclaims; nudge_count remains the
-- number of messages that the channel actually acknowledged.
ALTER TABLE tasks ADD COLUMN nudge_attempt_count INT NOT NULL DEFAULT 0;
UPDATE tasks SET nudge_attempt_count = nudge_count;
ALTER TABLE tasks ADD CONSTRAINT chk_tasks_nudge_attempt_count
    CHECK (nudge_attempt_count >= nudge_count);

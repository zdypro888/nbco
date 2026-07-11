-- Forward-compatible repair for environments that may have applied an early
-- 0049 build before durable generated output was added. Migrations are
-- immutable once recorded, so keep these guards even though fresh databases
-- already receive the columns from 0049.

ALTER TABLE schedule_deliveries
    ADD COLUMN IF NOT EXISTS result_text TEXT NOT NULL DEFAULT '';

ALTER TABLE automation_runs
    ADD COLUMN IF NOT EXISTS action_started BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE automation_runs
    ADD COLUMN IF NOT EXISTS result_text TEXT NOT NULL DEFAULT '';

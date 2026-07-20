-- Completion behavior is task data, not an inference from the executor fields.
-- Ordinary tasks preserve the existing self-assignment shortcut; creators of
-- deterministic work can explicitly choose auto_accept_on_success.
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS completion_policy TEXT NOT NULL DEFAULT 'self_accept_on_success';

-- Map the legacy representation once. Runtime state transitions never inspect
-- worker_command after this migration.
UPDATE tasks
SET completion_policy = 'auto_accept_on_success'
WHERE worker_command <> ''
  AND completion_policy = 'self_accept_on_success';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tasks_completion_policy_check'
          AND conrelid = 'tasks'::regclass
    ) THEN
        ALTER TABLE tasks
            ADD CONSTRAINT tasks_completion_policy_check
            CHECK (completion_policy IN (
                'review_required',
                'auto_accept_on_success',
                'self_accept_on_success'
            ));
    END IF;
END
$$;

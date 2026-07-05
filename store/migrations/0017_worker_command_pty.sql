-- Direct worker command tasks use ordinary stdout/stderr pipes by default.
-- Set worker_command_pty only when a command explicitly needs terminal behavior.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS worker_command_pty BOOLEAN NOT NULL DEFAULT FALSE;

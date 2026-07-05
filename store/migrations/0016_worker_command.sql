-- Explicit worker command tasks. When worker_command is non-empty, nbco-worker
-- executes it directly in the task workspace via PTY instead of asking an AI CLI
-- to infer the command from natural language.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS worker_command TEXT NOT NULL DEFAULT '';

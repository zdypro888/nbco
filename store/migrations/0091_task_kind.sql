-- Task kind is a structured fact chosen by the Agent at creation time. Keeping
-- it on the task removes language-specific keyword inference from dispatch,
-- outcome learning and proactive communication.
ALTER TABLE tasks ADD COLUMN kind TEXT NOT NULL DEFAULT 'general';

CREATE INDEX idx_tasks_kind ON tasks(kind);

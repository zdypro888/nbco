-- Deterministic worker commands have no human deliverable to review unless a
-- reviewer was explicitly assigned. Older releases left these executions in
-- "done", which made completed commands accumulate in the review queue.
UPDATE tasks AS task
SET status = 'accepted',
    updated_at = now()
WHERE task.status = 'done'
  AND task.worker_command <> ''
  AND EXISTS (
      SELECT 1
      FROM task_progress AS progress
      WHERE progress.task_id = task.id
        AND progress.content LIKE '🤖 完成汇报：命令执行完成，退出码：0%'
  )
  AND NOT EXISTS (
      SELECT 1
      FROM task_participants AS participant
      WHERE participant.task_id = task.id
        AND participant.role = 'reviewer'
  );

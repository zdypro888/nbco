-- ai_usage 加 goal_id 维度：把执行成本归因到战略目标。
-- 仅 worker LLM 路径会尽力填（经 worker_sessions.last_task_id → tasks.milestone_id → milestones.goal_id 解析）；
-- 对话/催办/周报等系统轮次无目标上下文，goal_id 留 NULL。因此该维度只反映「AI 员工执行成本」，
-- 不含老板/员工对话成本——见 ai_usage_stats 工具说明。
ALTER TABLE ai_usage ADD COLUMN IF NOT EXISTS goal_id BIGINT REFERENCES goals(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS ai_usage_goal_created_idx ON ai_usage (goal_id, created_at)
  WHERE goal_id IS NOT NULL;

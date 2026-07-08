-- Goal 目标 / Milestone 里程碑：把模糊战略目标结构化拆解、跟踪进度、周报汇报。
-- 三层 Goal→Milestone→Task：Goal 是公司级战略方向（跨项目），Milestone 是可验收
-- 的关键节点，Task 仍归 Project（执行容器），通过可选 milestone_id 做战略归因
-- （弱关联，SET NULL——删里程碑不毁任务，只是解除归因）。与 Task 拆分树（parent_id）、
-- 依赖（depends_on）正交，不影响既有验收闭环。
--
-- 状态机：active → achieved（达成）/ archived（归档）。不自动从"任务全 accepted"
-- 推导——达成是判断题，由 close_goal/close_milestone 手动设定，进度聚合只读展示。

CREATE TABLE IF NOT EXISTS goals (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    owner_id    BIGINT NOT NULL REFERENCES users(id),   -- 无 ON DELETE：用户停用而非删除（同 projects.creator_id）
    deadline    TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'active',          -- active | achieved | archived
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_goals_owner ON goals (owner_id);
CREATE INDEX IF NOT EXISTS idx_goals_status ON goals (status);

CREATE TABLE IF NOT EXISTS milestones (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    goal_id     BIGINT NOT NULL REFERENCES goals(id) ON DELETE CASCADE, -- 强从属：删目标连里程碑
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    deadline    TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'active',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_milestones_goal ON milestones (goal_id);
CREATE INDEX IF NOT EXISTS idx_milestones_status ON milestones (status);

-- Task 战略归因：可选挂到里程碑。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS milestone_id BIGINT REFERENCES milestones(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_milestone ON tasks (milestone_id);

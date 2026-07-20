package store

import (
	"context"
	"time"
)

// Goal/Milestone 状态：active（推进中）/ achieved（已达成）/ archived（归档）。
// 手动流转（close_goal/close_milestone），不从任务状态自动推导——"达成"是判断题，
// 进度聚合只读展示，辅助人工/AI 在闭环时拍板。
const (
	GoalActive   = "active"
	GoalAchieved = "achieved"
	GoalArchived = "archived"
)

// Goal 战略目标（公司级，跨项目）。
type Goal struct {
	ID          int64
	Title       string
	Description string
	OwnerID     int64
	Deadline    *time.Time
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Milestone 里程碑：Goal 下的可验收关键节点。Task 通过可选 milestone_id 归因到此。
type Milestone struct {
	ID          int64
	GoalID      int64
	Title       string
	Description string
	Deadline    *time.Time
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const goalCols = `id, title, description, owner_id, deadline, status, created_at, updated_at`

func scanGoal(row interface{ Scan(...any) error }) (*Goal, error) {
	var g Goal
	if err := row.Scan(&g.ID, &g.Title, &g.Description, &g.OwnerID, &g.Deadline, &g.Status, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &g, nil
}

const milestoneCols = `id, goal_id, title, description, deadline, status, created_at, updated_at`

func scanMilestone(row interface{ Scan(...any) error }) (*Milestone, error) {
	var m Milestone
	if err := row.Scan(&m.ID, &m.GoalID, &m.Title, &m.Description, &m.Deadline, &m.Status, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &m, nil
}

// --- Goal ---

// CreateGoal 建目标。owner 即创建者（也即后续写操作的权限人）。
func (s *Store) CreateGoal(ctx context.Context, title, description string, ownerID int64, deadline *time.Time) (*Goal, error) {
	return scanGoal(s.pool.QueryRow(ctx,
		`INSERT INTO goals (title, description, owner_id, deadline) VALUES ($1,$2,$3,$4) RETURNING `+goalCols,
		title, description, ownerID, deadline))
}

// GoalByID 取目标；不存在返 ErrNotFound。
func (s *Store) GoalByID(ctx context.Context, id int64) (*Goal, error) {
	return scanGoal(s.pool.QueryRow(ctx, `SELECT `+goalCols+` FROM goals WHERE id = $1`, id))
}

// ListGoals 列目标；activeOnly=true 只看推进中的。
func (s *Store) ListGoals(ctx context.Context, activeOnly bool) ([]Goal, error) {
	q := `SELECT ` + goalCols + ` FROM goals`
	if activeOnly {
		q += ` WHERE status = 'active'`
	}
	q += ` ORDER BY id`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// GoalsOfOwner 负责人名下的目标。
func (s *Store) GoalsOfOwner(ctx context.Context, ownerID int64) ([]Goal, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+goalCols+` FROM goals WHERE owner_id = $1 ORDER BY id`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// UpdateGoal 局部更新（nil 字段保持原值，COALESCE 模式，仿 UpdateTaskContent）。
// 截止时间变更时重置提醒标记，让新截止时间重新获得临近提醒与过期通知。
func (s *Store) UpdateGoal(ctx context.Context, id int64, title, description *string, deadline *time.Time) (*Goal, error) {
	return scanGoal(s.pool.QueryRow(ctx,
		`UPDATE goals SET
		   title = COALESCE($2, title),
		   description = COALESCE($3, description),
		   deadline = COALESCE($4, deadline),
		   deadline_reminded_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE deadline_reminded_at END,
		   overdue_notified_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE overdue_notified_at END,
		   deadline_reminder_claimed_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE deadline_reminder_claimed_at END,
		   overdue_notice_claimed_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE overdue_notice_claimed_at END,
		   updated_at = now()
		 WHERE id = $1 RETURNING `+goalCols,
		id, title, description, deadline))
}

// SetGoalStatus 改目标状态（achieved/archived），仿 SetProjectStatus。
func (s *Store) SetGoalStatus(ctx context.Context, id int64, status string) error {
	return s.execOne(ctx, `UPDATE goals SET status = $2, updated_at = now() WHERE id = $1`, id, status)
}

// --- Milestone ---

func (s *Store) CreateMilestone(ctx context.Context, goalID int64, title, description string, deadline *time.Time) (*Milestone, error) {
	return scanMilestone(s.pool.QueryRow(ctx,
		`INSERT INTO milestones (goal_id, title, description, deadline) VALUES ($1,$2,$3,$4) RETURNING `+milestoneCols,
		goalID, title, description, deadline))
}

func (s *Store) MilestoneByID(ctx context.Context, id int64) (*Milestone, error) {
	return scanMilestone(s.pool.QueryRow(ctx, `SELECT `+milestoneCols+` FROM milestones WHERE id = $1`, id))
}

// MilestonesOfGoal 目标下的里程碑（按 id 升序）。
func (s *Store) MilestonesOfGoal(ctx context.Context, goalID int64) ([]Milestone, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+milestoneCols+` FROM milestones WHERE goal_id = $1 ORDER BY id`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Milestone
	for rows.Next() {
		m, err := scanMilestone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// UpdateMilestone 局部更新（COALESCE 模式）。
// 截止时间变更时重置提醒标记，让新截止时间重新进入临近/过期调度闭环。
func (s *Store) UpdateMilestone(ctx context.Context, id int64, title, description *string, deadline *time.Time) (*Milestone, error) {
	return scanMilestone(s.pool.QueryRow(ctx,
		`UPDATE milestones SET
		   title = COALESCE($2, title),
		   description = COALESCE($3, description),
		   deadline = COALESCE($4, deadline),
		   deadline_reminded_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE deadline_reminded_at END,
		   overdue_notified_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE overdue_notified_at END,
		   deadline_reminder_claimed_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE deadline_reminder_claimed_at END,
		   overdue_notice_claimed_at = CASE WHEN $4::timestamptz IS NOT NULL THEN NULL ELSE overdue_notice_claimed_at END,
		   updated_at = now()
		 WHERE id = $1 RETURNING `+milestoneCols,
		id, title, description, deadline))
}

func (s *Store) SetMilestoneStatus(ctx context.Context, id int64, status string) error {
	return s.execOne(ctx, `UPDATE milestones SET status = $2, updated_at = now() WHERE id = $1`, id, status)
}

// --- 任务与里程碑的关联 ---

// TasksOfMilestone 挂在某里程碑下的任务（按 id 升序）。
func (s *Store) TasksOfMilestone(ctx context.Context, milestoneID int64) ([]*Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE milestone_id = $1 ORDER BY id`, milestoneID)
}

// SetTaskMilestone 绑定/解绑任务到里程碑（milestoneID=nil 解绑）。
func (s *Store) SetTaskMilestone(ctx context.Context, taskID int64, milestoneID *int64) error {
	return s.execOne(ctx, `UPDATE tasks SET milestone_id = $2, updated_at = now() WHERE id = $1`, taskID, milestoneID)
}

// --- 进度聚合（仿 ProjectTaskCounts 的 count(*) FILTER 模式） ---

// MilestoneProgress 单里程碑的任务进度。Total 排除 split（拆分父任务的工作由子任务承载，
// 计入会与子任务重复计分母）。
type MilestoneProgress struct {
	Total    int64 // 非拆分任务
	Open     int64 // pending + in_progress
	Awaiting int64 // done（已提交待验收）
	Accepted int64 // accepted（终态）
}

// MilestoneTaskCounts 各里程碑的任务计数，按 milestone_id GROUP BY。
func (s *Store) MilestoneTaskCounts(ctx context.Context, milestoneIDs []int64) (map[int64]MilestoneProgress, error) {
	m := map[int64]MilestoneProgress{}
	if len(milestoneIDs) == 0 {
		return m, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT milestone_id,
		   count(*) FILTER (WHERE status <> 'split'),
		   count(*) FILTER (WHERE status IN ('pending','in_progress','awaiting_input')),
		   count(*) FILTER (WHERE status = 'done'),
		   count(*) FILTER (WHERE status = 'accepted')
		 FROM tasks WHERE milestone_id = ANY($1) GROUP BY milestone_id`, milestoneIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p MilestoneProgress
		if err := rows.Scan(&id, &p.Total, &p.Open, &p.Awaiting, &p.Accepted); err != nil {
			return nil, err
		}
		m[id] = p
	}
	return m, rows.Err()
}

// GoalMilestoneCounts 各目标的里程碑计数（战略级进度：达成率）。
type GoalMilestoneCounts struct {
	Total    int64
	Achieved int64
}

func (s *Store) GoalMilestoneCounts(ctx context.Context, goalIDs []int64) (map[int64]GoalMilestoneCounts, error) {
	m := map[int64]GoalMilestoneCounts{}
	if len(goalIDs) == 0 {
		return m, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT goal_id, count(*), count(*) FILTER (WHERE status = 'achieved')
		 FROM milestones WHERE goal_id = ANY($1) GROUP BY goal_id`, goalIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c GoalMilestoneCounts
		if err := rows.Scan(&id, &c.Total, &c.Achieved); err != nil {
			return nil, err
		}
		m[id] = c
	}
	return m, rows.Err()
}

// GoalTaskRollup 各目标下所有任务（跨其全部里程碑）的任务进度——找卡点里程碑用。
// JOIN milestones 把任务的 milestone_id 映射回 goal_id。
func (s *Store) GoalTaskRollup(ctx context.Context, goalIDs []int64) (map[int64]MilestoneProgress, error) {
	m := map[int64]MilestoneProgress{}
	if len(goalIDs) == 0 {
		return m, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.goal_id,
		   count(*) FILTER (WHERE t.status <> 'split'),
		   count(*) FILTER (WHERE t.status IN ('pending','in_progress','awaiting_input')),
		   count(*) FILTER (WHERE t.status = 'done'),
		   count(*) FILTER (WHERE t.status = 'accepted')
		 FROM tasks t JOIN milestones m ON t.milestone_id = m.id
		 WHERE m.goal_id = ANY($1) GROUP BY m.goal_id`, goalIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p MilestoneProgress
		if err := rows.Scan(&id, &p.Total, &p.Open, &p.Awaiting, &p.Accepted); err != nil {
			return nil, err
		}
		m[id] = p
	}
	return m, rows.Err()
}

// --- 截止提醒/过期通知（镜像任务的原子认领 + ack 模式，见 store/tasks.go） ---

// goalReminderClaimLease 认领租约：租约内重复认领拿到空，租约过期能重试（重启/崩溃恢复）。
// 与 taskReminderClaimLease 同值，调度器每 30s 一拍、投递在秒级完成。
const goalReminderClaimLease = 10 * time.Minute

func (s *Store) queryGoals(ctx context.Context, sql string, args ...any) ([]Goal, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// DueGoalDeadlineReminders 原子认领「临近截止」目标提醒：active、截止落在 (now, now+window]、未提醒过。
func (s *Store) DueGoalDeadlineReminders(ctx context.Context, now time.Time, window time.Duration) ([]Goal, error) {
	stale := now.Add(-goalReminderClaimLease)
	return s.queryGoals(ctx,
		`UPDATE goals SET deadline_reminder_claimed_at = $1
		 WHERE status = 'active' AND deadline IS NOT NULL
		   AND deadline_reminded_at IS NULL AND deadline > $1 AND deadline <= $2
		   AND (deadline_reminder_claimed_at IS NULL OR deadline_reminder_claimed_at <= $3)
		 RETURNING `+goalCols, now, now.Add(window), stale)
}

// DueGoalOverdueNotices 原子认领「已过期」目标通知：active、截止已过、未通知过。
func (s *Store) DueGoalOverdueNotices(ctx context.Context, now time.Time) ([]Goal, error) {
	stale := now.Add(-goalReminderClaimLease)
	return s.queryGoals(ctx,
		`UPDATE goals SET overdue_notice_claimed_at = $1
		 WHERE status = 'active' AND deadline IS NOT NULL
		   AND overdue_notified_at IS NULL AND deadline <= $1
		   AND (overdue_notice_claimed_at IS NULL OR overdue_notice_claimed_at <= $2)
		 RETURNING `+goalCols, now, stale)
}

// MarkGoalDeadlineReminderSent 投递成功后 ack：写提醒时间、清租约。
func (s *Store) MarkGoalDeadlineReminderSent(ctx context.Context, id int64, sentAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE goals SET deadline_reminded_at = $2, deadline_reminder_claimed_at = NULL, updated_at = now()
		 WHERE id = $1 AND deadline_reminder_claimed_at IS NOT NULL`, id, sentAt)
}

// MarkGoalOverdueNoticeSent 投递成功后 ack：写通知时间、清租约。
func (s *Store) MarkGoalOverdueNoticeSent(ctx context.Context, id int64, sentAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE goals SET overdue_notified_at = $2, overdue_notice_claimed_at = NULL, updated_at = now()
		 WHERE id = $1 AND overdue_notice_claimed_at IS NOT NULL`, id, sentAt)
}

// --- 里程碑截止提醒/过期通知（镜像目标的原子认领 + ack 模式） ---
// 里程碑无独立 owner，提醒投给所属 goal 的 owner（战略负责人）。

func (s *Store) queryMilestones(ctx context.Context, sql string, args ...any) ([]Milestone, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Milestone
	for rows.Next() {
		m, err := scanMilestone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// DueMilestoneDeadlineReminders 原子认领「临近截止」里程碑提醒。
func (s *Store) DueMilestoneDeadlineReminders(ctx context.Context, now time.Time, window time.Duration) ([]Milestone, error) {
	stale := now.Add(-goalReminderClaimLease)
	return s.queryMilestones(ctx,
		`UPDATE milestones SET deadline_reminder_claimed_at = $1
		 WHERE status = 'active' AND deadline IS NOT NULL
		   AND deadline_reminded_at IS NULL AND deadline > $1 AND deadline <= $2
		   AND (deadline_reminder_claimed_at IS NULL OR deadline_reminder_claimed_at <= $3)
		 RETURNING `+milestoneCols, now, now.Add(window), stale)
}

// DueMilestoneOverdueNotices 原子认领「已过期」里程碑通知。
func (s *Store) DueMilestoneOverdueNotices(ctx context.Context, now time.Time) ([]Milestone, error) {
	stale := now.Add(-goalReminderClaimLease)
	return s.queryMilestones(ctx,
		`UPDATE milestones SET overdue_notice_claimed_at = $1
		 WHERE status = 'active' AND deadline IS NOT NULL
		   AND overdue_notified_at IS NULL AND deadline <= $1
		   AND (overdue_notice_claimed_at IS NULL OR overdue_notice_claimed_at <= $2)
		 RETURNING `+milestoneCols, now, stale)
}

func (s *Store) MarkMilestoneDeadlineReminderSent(ctx context.Context, id int64, sentAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE milestones SET deadline_reminded_at = $2, deadline_reminder_claimed_at = NULL, updated_at = now()
		 WHERE id = $1 AND deadline_reminder_claimed_at IS NOT NULL`, id, sentAt)
}

func (s *Store) MarkMilestoneOverdueNoticeSent(ctx context.Context, id int64, sentAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE milestones SET overdue_notified_at = $2, overdue_notice_claimed_at = NULL, updated_at = now()
		 WHERE id = $1 AND overdue_notice_claimed_at IS NOT NULL`, id, sentAt)
}

// --- decompose 用：批量建任务 ---

// CreateMilestoneTasks 单事务批量建任务（decompose_milestone 用）。
// subs 每条由调用方填好 ProjectID/AssignerID/AssigneeID/MilestoneID；depends_on
// 校验同 CreateTask（须为同项目已存在任务）。任一失败整批回滚。
func (s *Store) CreateMilestoneTasks(ctx context.Context, subs []*Task) ([]*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created := make([]*Task, 0, len(subs))
	for _, t := range subs {
		deps, ok := normalizeTaskDeps(t.DependsOn)
		if !ok {
			return nil, ErrNotFound
		}
		if len(deps) > 0 {
			rows, err := tx.Query(ctx,
				`SELECT id FROM tasks WHERE project_id = $1 AND id = ANY($2) FOR SHARE`,
				t.ProjectID, deps)
			if err != nil {
				return nil, err
			}
			seen := map[int64]bool{}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return nil, err
				}
				seen[id] = true
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			rows.Close()
			if len(seen) != len(deps) {
				return nil, ErrNotFound
			}
		}
		row := tx.QueryRow(ctx,
			`INSERT INTO tasks (project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, priority, deadline, depends_on, milestone_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING `+taskCols,
			t.ProjectID, t.ParentID, t.AssignerID, t.AssigneeID, t.Title, t.Goal, t.Description, t.Acceptance, nonEmpty(t.Priority, "normal"), t.Deadline, deps, t.MilestoneID)
		ct, err := scanTask(row)
		if err != nil {
			return nil, err
		}
		if _, err := enqueueTaskWorkerRunTx(ctx, tx, ct, nil); err != nil {
			return nil, err
		}
		created = append(created, ct)
	}
	return created, tx.Commit(ctx)
}

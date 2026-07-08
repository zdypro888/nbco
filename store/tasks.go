package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 项目状态。
const (
	ProjectActive   = "active"
	ProjectArchived = "archived"
)

// 任务状态。流转：pending → in_progress → done（提交待验收）→ accepted（验收通过）。
// 打回：done → in_progress。自派任务（分配者=执行人）提交即 accepted，免自我验收。
const (
	TaskPending    = "pending"
	TaskInProgress = "in_progress"
	TaskDone       = "done"     // 已提交，等待分配者验收
	TaskAccepted   = "accepted" // 验收通过（终态）
	TaskSplit      = "split"    // 已拆分：由子任务承载执行

	taskReminderClaimLease = 10 * time.Minute
)

// Project 项目。
type Project struct {
	ID          int64
	Name        string
	Description string
	CreatorID   int64
	Status      string
	CreatedAt   time.Time
}

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.CreatorID, &p.Status, &p.CreatedAt)
	return &p, wrapErr(err)
}

// Task 任务。Goal 是"为什么做"，Description 是"做什么"，Acceptance 是验收标准。
type Task struct {
	ID               int64
	ProjectID        int64
	ParentID         *int64
	AssignerID       int64
	AssigneeID       int64
	Title            string
	Goal             string
	Description      string
	Acceptance       string
	WorkerCommand    string
	WorkerCommandPTY bool
	Priority         string
	Deadline         *time.Time
	Status           string
	NudgeCount       int64 // 累计 AI 催办次数（有进度后调度器不再催，但计数保留作履历）
	WorkerClaimID    string
	// DependsOn 前置任务 ID：全部 accepted 之前 worker 领不到本任务。
	// 依赖只能指向已存在的任务（新任务 id 恒大于依赖），天然无环。
	DependsOn []int64
	// MilestoneID 可选：战略里程碑归因（与 ParentID 正交——拆分树是执行转移，
	// 里程碑是战略标签）。nil = 无归因；删里程碑时 SET NULL，任务留在原项目继续。
	MilestoneID *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChecklistItem 工作清单条目。
type ChecklistItem struct {
	ID       int64
	TaskID   int64
	Position int
	Item     string
	Done     bool
}

// Progress 进度记录。
type Progress struct {
	ID        int64
	TaskID    int64
	AuthorID  int64
	Content   string
	CreatedAt time.Time
}

// Attachment 任务附件。
type Attachment struct {
	ID        int64
	TaskID    int64
	Kind      string
	FileRef   string
	Caption   string
	CreatedAt time.Time
}

const taskCols = `id, project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, worker_command, worker_command_pty, priority, deadline, status, nudge_count, worker_claim_id, depends_on, milestone_id, created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	if err := row.Scan(&t.ID, &t.ProjectID, &t.ParentID, &t.AssignerID, &t.AssigneeID,
		&t.Title, &t.Goal, &t.Description, &t.Acceptance, &t.WorkerCommand, &t.WorkerCommandPTY, &t.Priority, &t.Deadline,
		&t.Status, &t.NudgeCount, &t.WorkerClaimID, &t.DependsOn, &t.MilestoneID, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &t, nil
}

// --- 项目 ---

// CreateProject 建项目。
func (s *Store) CreateProject(ctx context.Context, name, description string, creatorID int64) (*Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx,
		`INSERT INTO projects (name, description, creator_id) VALUES ($1, $2, $3)
		 RETURNING id, name, description, creator_id, status, created_at`,
		name, description, creatorID).
		Scan(&p.ID, &p.Name, &p.Description, &p.CreatorID, &p.Status, &p.CreatedAt)
	return &p, wrapErr(err)
}

// ProjectByID 取项目。
func (s *Store) ProjectByID(ctx context.Context, id int64) (*Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, description, creator_id, status, created_at FROM projects WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &p.Description, &p.CreatorID, &p.Status, &p.CreatedAt)
	return &p, wrapErr(err)
}

// ProjectsByCreator 我创建的项目。
func (s *Store) ProjectsByCreator(ctx context.Context, creatorID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, creator_id, status, created_at
		 FROM projects WHERE creator_id = $1 ORDER BY id`, creatorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatorID, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

// ProjectsOfAssignee 我有任务的项目（参与视角）。
func (s *Store) ProjectsOfAssignee(ctx context.Context, userID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT p.id, p.name, p.description, p.creator_id, p.status, p.created_at
		 FROM projects p JOIN tasks t ON t.project_id = p.id
		 WHERE t.assignee_id = $1 ORDER BY p.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatorID, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

// ListProjects 全部项目（老板全景/周报用）。
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, description, creator_id, status, created_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatorID, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

// SetProjectStatus 归档/激活项目。
func (s *Store) SetProjectStatus(ctx context.Context, id int64, status string) error {
	return s.execOne(ctx, `UPDATE projects SET status = $2 WHERE id = $1`, id, status)
}

// EnsureWorkerCommandProject 返回超管名下的命令任务项目，不存在则创建。
func (s *Store) EnsureWorkerCommandProject(ctx context.Context, creatorID int64) (*Project, error) {
	const name = "Worker Commands"
	p, err := scanProject(s.pool.QueryRow(ctx,
		`SELECT id, name, description, creator_id, status, created_at FROM projects
		 WHERE creator_id = $1 AND name = $2 AND status = 'active'
		 ORDER BY id LIMIT 1`, creatorID, name))
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return s.CreateProject(ctx, name, "显式 worker 命令任务。", creatorID)
}

// EnsureCompanyIntelligenceProject returns the inbox project for company
// material analysis. Uploaded documents/photos can be routed here, then an
// admin worker extracts structured learning candidates for nbco to publish.
func (s *Store) EnsureCompanyIntelligenceProject(ctx context.Context, creatorID int64) (*Project, error) {
	const name = "Company Intelligence Inbox"
	p, err := scanProject(s.pool.QueryRow(ctx,
		`SELECT id, name, description, creator_id, status, created_at FROM projects
		 WHERE creator_id = $1 AND name = $2 AND status = 'active'
		 ORDER BY id LIMIT 1`, creatorID, name))
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return s.CreateProject(ctx, name, "公司资料、制度、表格、照片等输入的结构化整理入口。", creatorID)
}

// DeleteProject 删除项目及其全部任务（外键级联），并把这些任务从其他项目
// 任务的 depends_on 里剔除（防悬挂 id 被依赖检查当作「已满足」）。
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET depends_on = (
		   SELECT coalesce(array_agg(x), '{}') FROM unnest(depends_on) x
		   WHERE x NOT IN (SELECT id FROM tasks WHERE project_id = $1))
		 WHERE project_id <> $1
		   AND EXISTS (SELECT 1 FROM unnest(depends_on) x JOIN tasks t ON t.id = x WHERE t.project_id = $1)`,
		id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1`, id)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// --- 任务 ---

// CreateTask 建任务。
func (s *Store) CreateTask(ctx context.Context, t *Task) (*Task, error) {
	deps, ok := normalizeTaskDeps(t.DependsOn)
	if !ok {
		return nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
		`INSERT INTO tasks (project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, worker_command, worker_command_pty, priority, deadline, depends_on, milestone_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING `+taskCols,
		t.ProjectID, t.ParentID, t.AssignerID, t.AssigneeID, t.Title, t.Goal, t.Description, t.Acceptance, t.WorkerCommand, t.WorkerCommandPTY, nonEmpty(t.Priority, "normal"), t.Deadline, deps, t.MilestoneID)
	created, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	return created, tx.Commit(ctx)
}

func normalizeTaskDeps(in []int64) ([]int64, bool) {
	if len(in) == 0 {
		return []int64{}, true
	}
	seen := make(map[int64]bool, len(in))
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if id <= 0 {
			return nil, false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, true
}

// TaskByID 取任务。
func (s *Store) TaskByID(ctx context.Context, id int64) (*Task, error) {
	return scanTask(s.pool.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1`, id))
}

// TasksOfAssignee 某人的任务；openOnly 时只取待办/进行中。
func (s *Store) TasksOfAssignee(ctx context.Context, userID int64, openOnly bool) ([]*Task, error) {
	sql := `SELECT ` + taskCols + ` FROM tasks WHERE assignee_id = $1`
	if openOnly {
		sql += ` AND status IN ('pending', 'in_progress')`
	}
	sql += ` ORDER BY id`
	return s.queryTasks(ctx, sql, userID)
}

// TasksOfAssigner 我分配出去的任务（不含拆给自己的）。
func (s *Store) TasksOfAssigner(ctx context.Context, userID int64) ([]*Task, error) {
	return s.queryTasks(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE assigner_id = $1 AND assignee_id <> $1 ORDER BY id`, userID)
}

// TasksOfProject 项目全部任务。
func (s *Store) TasksOfProject(ctx context.Context, projectID int64) ([]*Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE project_id = $1 ORDER BY id`, projectID)
}

// SubTasks 直接子任务。
func (s *Store) SubTasks(ctx context.Context, parentID int64) ([]*Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE parent_id = $1 ORDER BY id`, parentID)
}

// UpdateTaskStatus 更新状态并返回更新后的任务。
func (s *Store) UpdateTaskStatus(ctx context.Context, id int64, status string) (*Task, error) {
	return scanTask(s.pool.QueryRow(ctx,
		`UPDATE tasks
		    SET status = $2,
		        worker_claimed_by = NULL,
		        worker_claimed_at = NULL,
		        worker_claim_id = '',
		        updated_at = now()
		  WHERE id = $1 RETURNING `+taskCols, id, status))
}

// ReassignTask 改派：保留 task 行（连带 task_progress/checklist/attachments 历史），换执行人，
// 状态回到 pending 让新执行人重新领取，清空 worker claim（旧执行人无法再写进度/提交），
// 并重置截止提醒/过期通知标记（新执行人按自己的节奏重新获得提醒）。
// 不动 parent_id/depends_on——改派只换人，不动依赖结构与拆分树。
// 仅允许 pending/in_progress（未提交的任务）：done 已在验收队列、accepted/split 是终态，
// 改派它们会丢失验收记录或破坏拆分树。状态不符返 ErrNotFound（store 层防线，工具层也校验）。
func (s *Store) ReassignTask(ctx context.Context, id, newAssigneeID int64) (*Task, error) {
	return scanTask(s.pool.QueryRow(ctx,
		`UPDATE tasks SET
		   assignee_id = $2,
		   status = 'pending',
		   worker_claimed_by = NULL,
		   worker_claimed_at = NULL,
		   worker_claim_id = '',
		   nudge_count = 0,
		   deadline_reminded_at = NULL,
		   overdue_notified_at = NULL,
		   nudged_at = NULL,
		   deadline_reminder_claimed_at = NULL,
		   overdue_notice_claimed_at = NULL,
		   nudge_claimed_at = NULL,
		   updated_at = now()
		 WHERE id = $1 AND status IN ('pending', 'in_progress') RETURNING `+taskCols, id, newAssigneeID))
}

// UpdateTaskContent 分配者修改任务要素（nil 字段不动）。
// 截止时间变更时重置提醒标记，让新截止时间重新获得临近提醒与过期通知。
func (s *Store) UpdateTaskContent(ctx context.Context, id int64, goal, description, acceptance *string, deadline *time.Time) (*Task, error) {
	return scanTask(s.pool.QueryRow(ctx,
		`UPDATE tasks SET
		   goal = COALESCE($2, goal),
		   description = COALESCE($3, description),
		   acceptance = COALESCE($4, acceptance),
			   deadline = COALESCE($5, deadline),
			   deadline_reminded_at = CASE WHEN $5::timestamptz IS NOT NULL THEN NULL ELSE deadline_reminded_at END,
			   overdue_notified_at  = CASE WHEN $5::timestamptz IS NOT NULL THEN NULL ELSE overdue_notified_at END,
			   deadline_reminder_claimed_at = CASE WHEN $5::timestamptz IS NOT NULL THEN NULL ELSE deadline_reminder_claimed_at END,
			   overdue_notice_claimed_at = CASE WHEN $5::timestamptz IS NOT NULL THEN NULL ELSE overdue_notice_claimed_at END,
			   nudge_claimed_at = CASE WHEN $5::timestamptz IS NOT NULL THEN NULL ELSE nudge_claimed_at END,
			   updated_at = now()
			 WHERE id = $1 RETURNING `+taskCols, id, goal, description, acceptance, deadline))
}

// SubmitTask 执行人提交完成：自派任务（分配者=执行人）免验收直接 accepted 并向上级联；
// 其余置为 done 等待分配者验收。已验收/已拆分的任务不可提交（ErrNotFound）。
func (s *Store) SubmitTask(ctx context.Context, id int64) (*Task, []*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET
			   status = CASE WHEN assigner_id = assignee_id THEN 'accepted' ELSE 'done' END,
			   worker_claimed_by = NULL,
			   worker_claimed_at = NULL,
			   worker_claim_id = '',
			   updated_at = now()
			 WHERE id = $1 AND status IN ('pending', 'in_progress')
			 RETURNING `+taskCols, id))
	if err != nil {
		return nil, nil, err
	}
	var chain []*Task
	if t.Status == TaskAccepted {
		if chain, err = cascadeUp(ctx, tx, t.ParentID); err != nil {
			return nil, nil, err
		}
	}
	return t, chain, tx.Commit(ctx)
}

// SubmitWorkerTask worker 提交：仅当任务仍是该 worker 手上的 in_progress 时才提交，
// 原子避开「分配者同时改需求把任务重置为 pending」的竞态（那时 status 已非
// in_progress，本次提交落空返回 ErrNotFound，旧交付不会被当成完成）。
func (s *Store) SubmitWorkerTask(ctx context.Context, id, workerID int64, claimID, summary string) (*Task, []*Task, error) {
	if strings.TrimSpace(claimID) == "" {
		return nil, nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET
		   status = CASE WHEN assigner_id = assignee_id THEN 'accepted' ELSE 'done' END,
		   worker_claimed_by = NULL,
		   worker_claimed_at = NULL,
		   worker_claim_id = '',
		   updated_at = now()
		 WHERE id = $1 AND assignee_id = $2 AND status = 'in_progress' AND worker_claim_id = $3
		 RETURNING `+taskCols, id, workerID, claimID))
	if err != nil {
		return nil, nil, err
	}
	if summary = strings.TrimSpace(summary); summary != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1, $2, $3)`,
			id, workerID, "🤖 完成汇报："+summary); err != nil {
			return nil, nil, err
		}
	}
	var chain []*Task
	if t.Status == TaskAccepted {
		if chain, err = cascadeUp(ctx, tx, t.ParentID); err != nil {
			return nil, nil, err
		}
	}
	return t, chain, tx.Commit(ctx)
}

// AcceptTask 分配者验收通过：done → accepted，并向上级联。
// 任务不在待验收状态时返回 ErrNotFound。
func (s *Store) AcceptTask(ctx context.Context, id int64) (*Task, []*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks
			    SET status = 'accepted',
			        worker_claimed_by = NULL,
			        worker_claimed_at = NULL,
			        worker_claim_id = '',
			        updated_at = now()
			 WHERE id = $1 AND status = 'done' RETURNING `+taskCols, id))
	if err != nil {
		return nil, nil, err
	}
	chain, err := cascadeUp(ctx, tx, t.ParentID)
	if err != nil {
		return nil, nil, err
	}
	return t, chain, tx.Commit(ctx)
}

// RejectTask 验收打回：done → in_progress，理由写入进度记录。
// 任务不在待验收状态时返回 ErrNotFound。
func (s *Store) RejectTask(ctx context.Context, id, reviewerID int64, reason string) (*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks
			    SET status = 'in_progress',
			        worker_claimed_by = NULL,
			        worker_claimed_at = NULL,
			        worker_claim_id = '',
			        updated_at = now()
			 WHERE id = $1 AND status = 'done' RETURNING `+taskCols, id))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1, $2, $3)`,
		id, reviewerID, "🔍 验收未通过："+reason); err != nil {
		return nil, err
	}
	return t, tx.Commit(ctx)
}

// cascadeUp 子任务验收通过后的向上传播：父任务（split）的全部子任务 accepted 时，
// 父任务转 done（交付物就绪，等待其分配者验收）；父任务是自派任务则直接 accepted
// 并继续向上。返回状态被改变的祖先（自下而上）。
// 注：验收后的子任务被打回不会反向撤销祖先状态（罕见，由分配者人工纠正）。
func cascadeUp(ctx context.Context, tx pgx.Tx, parentID *int64) ([]*Task, error) {
	var chain []*Task
	for parentID != nil {
		// 先锁父行再判子状态：并发验收最后两个兄弟子任务时两个事务在此串行化，
		// 后到者以新语句快照看到先到者已提交的 accepted。不锁的话双方都以旧快照
		// 认为「还有兄弟未完成」，父任务永久卡在 split 且无任何报错。
		if _, err := tx.Exec(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, *parentID); err != nil {
			return nil, err
		}
		p, err := scanTask(tx.QueryRow(ctx,
			`UPDATE tasks SET
				   status = CASE WHEN assigner_id = assignee_id THEN 'accepted' ELSE 'done' END,
				   worker_claimed_by = NULL,
				   worker_claimed_at = NULL,
				   worker_claim_id = '',
				   updated_at = now()
			 WHERE id = $1 AND status = 'split'
			   AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = $1 AND c.status <> 'accepted')
			 RETURNING `+taskCols, *parentID))
		if errors.Is(err, ErrNotFound) {
			break // 还有未验收的兄弟任务，或父状态不是 split
		}
		if err != nil {
			return nil, err
		}
		chain = append(chain, p)
		if p.Status != TaskAccepted {
			break // 父任务等待真人验收，传播止步于此
		}
		parentID = p.ParentID
	}
	return chain, nil
}

// TasksAwaitingReview 我分配出去、已提交待我验收的任务。
func (s *Store) TasksAwaitingReview(ctx context.Context, assignerID int64) ([]*Task, error) {
	return s.queryTasks(ctx,
		`SELECT `+taskCols+` FROM tasks
		 WHERE assigner_id = $1 AND assignee_id <> $1 AND status = 'done' ORDER BY updated_at`, assignerID)
}

// DueNudges 原子认领需要 AI 催办的任务：已发过期通知，且距最近一次
// 「过期通知 / 催办 / 进度更新」超过 interval 的开放任务。
func (s *Store) DueNudges(ctx context.Context, now time.Time, interval time.Duration) ([]*Task, error) {
	stale := now.Add(-taskReminderClaimLease)
	return s.queryTasks(ctx,
		`UPDATE tasks SET nudge_claimed_at = $1 WHERE id IN (
		   SELECT t.id FROM tasks t
		   LEFT JOIN LATERAL (
		     SELECT max(created_at) AS at FROM task_progress p WHERE p.task_id = t.id
		   ) prog ON TRUE
		   WHERE t.status IN ('pending', 'in_progress')
		     AND t.overdue_notified_at IS NOT NULL
		     AND (t.nudge_claimed_at IS NULL OR t.nudge_claimed_at <= $3)
		     AND GREATEST(t.overdue_notified_at,
		                  COALESCE(t.nudged_at, t.overdue_notified_at),
		                  COALESCE(prog.at, t.overdue_notified_at)) <= $2
		 ) RETURNING `+taskCols, now, now.Add(-interval), stale)
}

// DueDeadlineReminders 原子认领「临近截止」提醒：开放任务、截止落在 (now, now+window]、未成功提醒过。
func (s *Store) DueDeadlineReminders(ctx context.Context, now time.Time, window time.Duration) ([]*Task, error) {
	stale := now.Add(-taskReminderClaimLease)
	return s.queryTasks(ctx,
		`UPDATE tasks SET deadline_reminder_claimed_at = $1
		 WHERE status IN ('pending', 'in_progress') AND deadline IS NOT NULL
		   AND deadline_reminded_at IS NULL AND deadline > $1 AND deadline <= $2
		   AND (deadline_reminder_claimed_at IS NULL OR deadline_reminder_claimed_at <= $3)
		 RETURNING `+taskCols, now, now.Add(window), stale)
}

// DueOverdueNotices 原子认领「已过期」通知：开放任务、截止已过、未成功通知过。
func (s *Store) DueOverdueNotices(ctx context.Context, now time.Time) ([]*Task, error) {
	stale := now.Add(-taskReminderClaimLease)
	return s.queryTasks(ctx,
		`UPDATE tasks SET overdue_notice_claimed_at = $1
		 WHERE status IN ('pending', 'in_progress') AND deadline IS NOT NULL
		   AND overdue_notified_at IS NULL AND deadline <= $1
		   AND (overdue_notice_claimed_at IS NULL OR overdue_notice_claimed_at <= $2)
		 RETURNING `+taskCols, now, stale)
}

func (s *Store) MarkDeadlineReminderSent(ctx context.Context, id int64, sentAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE tasks SET deadline_reminded_at = $2, deadline_reminder_claimed_at = NULL, updated_at = now()
		 WHERE id = $1 AND deadline_reminder_claimed_at IS NOT NULL`, id, sentAt)
}

func (s *Store) MarkOverdueNoticeSent(ctx context.Context, id int64, sentAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE tasks SET overdue_notified_at = $2, overdue_notice_claimed_at = NULL, updated_at = now()
		 WHERE id = $1 AND overdue_notice_claimed_at IS NOT NULL`, id, sentAt)
}

func (s *Store) MarkNudgeSent(ctx context.Context, id int64, sentAt time.Time) error {
	return s.execOne(ctx,
		`UPDATE tasks SET nudged_at = $2, nudge_claimed_at = NULL, nudge_count = nudge_count + 1, updated_at = now()
		 WHERE id = $1 AND nudge_claimed_at IS NOT NULL`, id, sentAt)
}

// TaskStats 全局任务统计（老板摘要用）。
type TaskStats struct {
	Open      int64 // 待处理 + 进行中
	Overdue   int64 // 其中已过截止时间
	Awaiting  int64 // 已提交待验收
	DoneSince int64 // 自给定时刻以来验收通过
}

// GlobalTaskStats 全局任务统计。
func (s *Store) GlobalTaskStats(ctx context.Context, doneSince time.Time) (*TaskStats, error) {
	var st TaskStats
	err := s.pool.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE status IN ('pending','in_progress')),
		   count(*) FILTER (WHERE status IN ('pending','in_progress') AND deadline IS NOT NULL AND deadline < now()),
		   count(*) FILTER (WHERE status = 'done'),
		   count(*) FILTER (WHERE status = 'accepted' AND updated_at >= $1)
		 FROM tasks`, doneSince).Scan(&st.Open, &st.Overdue, &st.Awaiting, &st.DoneSince)
	return &st, err
}

// AssigneeStats 某人的任务履历统计（画像与分配决策的原料）。
type AssigneeStats struct {
	Open                 int64 // 手上开放任务
	OverdueNow           int64 // 其中已过截止
	Awaiting             int64 // 已提交待验收
	Accepted             int64 // 验收通过总数
	AcceptedWithDeadline int64 // 其中有截止时间的
	AcceptedOnTime       int64 // 其中截止前通过的
}

// StatsOfAssignee 某执行人的任务统计。按时率以验收时间（updated_at）对比截止时间近似。
func (s *Store) StatsOfAssignee(ctx context.Context, userID int64) (*AssigneeStats, error) {
	var st AssigneeStats
	err := s.pool.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE status IN ('pending','in_progress')),
		   count(*) FILTER (WHERE status IN ('pending','in_progress') AND deadline IS NOT NULL AND deadline < now()),
		   count(*) FILTER (WHERE status = 'done'),
		   count(*) FILTER (WHERE status = 'accepted'),
		   count(*) FILTER (WHERE status = 'accepted' AND deadline IS NOT NULL),
		   count(*) FILTER (WHERE status = 'accepted' AND deadline IS NOT NULL AND updated_at <= deadline)
		 FROM tasks WHERE assignee_id = $1`, userID).
		Scan(&st.Open, &st.OverdueNow, &st.Awaiting, &st.Accepted, &st.AcceptedWithDeadline, &st.AcceptedOnTime)
	return &st, err
}

// ProjectCounts 单个项目的任务计数。
type ProjectCounts struct {
	Open     int64
	Awaiting int64
	Accepted int64
}

// ProjectTaskCounts 各项目的任务计数（全景视图用）。
func (s *Store) ProjectTaskCounts(ctx context.Context) (map[int64]ProjectCounts, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT project_id,
		   count(*) FILTER (WHERE status IN ('pending','in_progress')),
		   count(*) FILTER (WHERE status = 'done'),
		   count(*) FILTER (WHERE status = 'accepted')
		 FROM tasks GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]ProjectCounts{}
	for rows.Next() {
		var id int64
		var c ProjectCounts
		if err := rows.Scan(&id, &c.Open, &c.Awaiting, &c.Accepted); err != nil {
			return nil, err
		}
		m[id] = c
	}
	return m, rows.Err()
}

// OverdueTasks 已过期的开放任务（按截止时间升序，最多 limit 条）。
func (s *Store) OverdueTasks(ctx context.Context, limit int) ([]*Task, error) {
	return s.queryTasks(ctx,
		`SELECT `+taskCols+` FROM tasks
		 WHERE status IN ('pending','in_progress') AND deadline IS NOT NULL AND deadline < now()
		 ORDER BY deadline LIMIT $1`, limit)
}

// SplitTask 拆分任务：原任务置为 split，并在同一事务中创建子任务。
// 返回创建的子任务。若原任务状态已不是 pending/in_progress 则失败。
func (s *Store) SplitTask(ctx context.Context, parentID int64, subs []*Task) ([]*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'split', worker_claimed_by = NULL, worker_claimed_at = NULL, worker_claim_id = '', updated_at = now()
			 WHERE id = $1 AND status IN ('pending', 'in_progress')`, parentID)
	if err != nil {
		return nil, wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	created := make([]*Task, 0, len(subs))
	for _, t := range subs {
		row := tx.QueryRow(ctx,
			`INSERT INTO tasks (project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, worker_command, worker_command_pty, priority, deadline, milestone_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+taskCols,
			t.ProjectID, parentID, t.AssignerID, t.AssigneeID, t.Title, t.Goal, t.Description, t.Acceptance, t.WorkerCommand, t.WorkerCommandPTY, nonEmpty(t.Priority, "normal"), t.Deadline, t.MilestoneID)
		ct, err := scanTask(row)
		if err != nil {
			return nil, err
		}
		created = append(created, ct)
	}
	return created, tx.Commit(ctx)
}

// DeleteTask 删除任务（子任务级联）。
func (s *Store) DeleteTask(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// 把被删任务从其他任务的 depends_on 里剔除：语义上「前置被取消 = 不再等待」，
	// 同时防悬挂 id——依赖检查的 NOT EXISTS 对已消失的前置恒为真，下游会在前置
	// 从未完成的情况下被 worker 领走。
	if _, err := tx.Exec(ctx,
		`UPDATE tasks SET depends_on = array_remove(depends_on, $1) WHERE $1 = ANY(depends_on)`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func (s *Store) queryTasks(ctx context.Context, sql string, args ...any) ([]*Task, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ts []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		ts = append(ts, t)
	}
	return ts, rows.Err()
}

// --- 清单 / 进度 / 附件 ---

// ReplaceChecklist 整体替换清单。
func (s *Store) ReplaceChecklist(ctx context.Context, taskID int64, items []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM task_checklist WHERE task_id = $1`, taskID); err != nil {
		return err
	}
	for i, item := range items {
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_checklist (task_id, position, item) VALUES ($1, $2, $3)`, taskID, i, item); err != nil {
			return wrapErr(err)
		}
	}
	return tx.Commit(ctx)
}

// Checklist 取清单。
func (s *Store) Checklist(ctx context.Context, taskID int64) ([]ChecklistItem, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, task_id, position, item, done FROM task_checklist WHERE task_id = $1 ORDER BY position`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ChecklistItem
	for rows.Next() {
		var it ChecklistItem
		if err := rows.Scan(&it.ID, &it.TaskID, &it.Position, &it.Item, &it.Done); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// ToggleChecklist 勾选/取消清单条目（按位置）。
func (s *Store) ToggleChecklist(ctx context.Context, taskID int64, position int, done bool) error {
	return s.execOne(ctx,
		`UPDATE task_checklist SET done = $3 WHERE task_id = $1 AND position = $2`, taskID, position, done)
}

// AddProgress 添加进度记录。
func (s *Store) AddProgress(ctx context.Context, taskID, authorID int64, content string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1, $2, $3)`, taskID, authorID, content)
	return wrapErr(err)
}

// AddWorkerProgress 仅当任务仍由该 worker 以同一 claim 持有时写进度，防止
// 旧 worker 进程在任务被重置/重领后继续污染历史。
func (s *Store) AddWorkerProgress(ctx context.Context, taskID, workerID int64, claimID, content string) error {
	if strings.TrimSpace(claimID) == "" {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO task_progress (task_id, author_id, content)
		 SELECT id, $2, $4 FROM tasks
		 WHERE id = $1 AND assignee_id = $2 AND status = 'in_progress' AND worker_claim_id = $3`,
		taskID, workerID, claimID, content)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ProgressOf 取任务进度记录。
func (s *Store) ProgressOf(ctx context.Context, taskID int64) ([]Progress, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, task_id, author_id, content, created_at FROM task_progress WHERE task_id = $1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Progress
	for rows.Next() {
		var p Progress
		if err := rows.Scan(&p.ID, &p.TaskID, &p.AuthorID, &p.Content, &p.CreatedAt); err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}

// AddAttachment 挂附件。
func (s *Store) AddAttachment(ctx context.Context, a Attachment) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO task_attachments (task_id, kind, file_ref, caption) VALUES ($1, $2, $3, $4)`,
		a.TaskID, a.Kind, a.FileRef, a.Caption)
	return wrapErr(err)
}

// AttachmentsOf 取任务附件。
func (s *Store) AttachmentsOf(ctx context.Context, taskID int64) ([]Attachment, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, task_id, kind, file_ref, caption, created_at FROM task_attachments WHERE task_id = $1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var as []Attachment
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.ID, &a.TaskID, &a.Kind, &a.FileRef, &a.Caption, &a.CreatedAt); err != nil {
			return nil, err
		}
		as = append(as, a)
	}
	return as, rows.Err()
}

func nonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ReadyDependents 任务 acceptedID 验收通过后，找出因此「全部前置就绪」的下游
// 待办任务（编排触发点：唤醒 worker / 通知分配者）。
func (s *Store) ReadyDependents(ctx context.Context, acceptedID int64) ([]*Task, error) {
	return s.queryTasks(ctx,
		`SELECT `+taskCols+` FROM tasks t
		 WHERE $1 = ANY(t.depends_on) AND t.status = 'pending'
		   AND NOT EXISTS (SELECT 1 FROM tasks d WHERE d.id = ANY(t.depends_on) AND d.status <> 'accepted')
		 ORDER BY t.id`, acceptedID)
}

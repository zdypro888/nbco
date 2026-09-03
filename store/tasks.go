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
// Worker 缺少必要信息时进入 awaiting_input；分配者补充任务内容后回到 pending。
// 打回：done → in_progress。执行租约、重试和结果保存在 worker_runs，不属于任务状态。
const (
	TaskPending       = "pending"
	TaskInProgress    = "in_progress"
	TaskAwaitingInput = "awaiting_input" // worker 等待分配者补充信息，不持有执行 claim
	TaskDone          = "done"           // 已提交，等待分配者验收
	TaskAccepted      = "accepted"       // 验收通过（终态）
	TaskSplit         = "split"          // 已拆分：由子任务承载执行
	TaskCancelled     = "cancelled"      // 已取消（终态，可由 superseded_by 指向替代任务）

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
	ID                 int64
	ProjectID          int64
	ParentID           *int64
	AssignerID         int64
	AssigneeID         int64
	Title              string
	Goal               string
	Description        string
	Acceptance         string
	Kind               string
	Priority           string
	Deadline           *time.Time
	DeadlineGeneration int64
	Status             string
	Revision           int64 // 业务要求版本；执行只能提交其认领时对应的版本
	NudgeCount         int64 // 渠道已确认送达的 AI 催办次数
	NudgeAttemptCount  int64 // 已结算的投递尝试次数（含失败/结果不确定）
	// DependsOn 前置任务 ID：全部 accepted 之前 worker 领不到本任务。
	// 依赖只能指向已存在的任务（新任务 id 恒大于依赖），天然无环。
	DependsOn []int64
	// MilestoneID 可选：战略里程碑归因（与 ParentID 正交——拆分树是执行转移，
	// 里程碑是战略标签）。nil = 无归因；删里程碑时 SET NULL，任务留在原项目继续。
	MilestoneID  *int64
	SubmittedBy  *int64
	SubmittedAt  *time.Time
	CancelReason string
	CancelledAt  *time.Time
	SupersededBy *int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
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

const taskCols = `id, project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, kind, priority, deadline, deadline_generation, status, revision, nudge_count, nudge_attempt_count, depends_on, milestone_id, submitted_by, submitted_at, cancel_reason, cancelled_at, superseded_by, created_at, updated_at`

// successfulTaskCompletionStatusSQL is the business review rule. Work assigned
// to somebody else requires review; self-owned work can close directly unless
// an explicit reviewer participates. Executor representation is absent.
const successfulTaskCompletionStatusSQL = `CASE
  WHEN NOT EXISTS (
         SELECT 1 FROM task_participants tp
          WHERE tp.task_id = tasks.id AND tp.role = 'reviewer'
       )
	   AND assigner_id = assignee_id
    THEN 'accepted'
  ELSE 'done'
END`

func taskColsWithAlias(alias string) string {
	cols := strings.Split(taskCols, ", ")
	for i := range cols {
		cols[i] = alias + "." + cols[i]
	}
	return strings.Join(cols, ", ")
}

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	if err := row.Scan(&t.ID, &t.ProjectID, &t.ParentID, &t.AssignerID, &t.AssigneeID,
		&t.Title, &t.Goal, &t.Description, &t.Acceptance, &t.Kind, &t.Priority, &t.Deadline,
		&t.DeadlineGeneration, &t.Status, &t.Revision, &t.NudgeCount, &t.NudgeAttemptCount, &t.DependsOn, &t.MilestoneID,
		&t.SubmittedBy, &t.SubmittedAt, &t.CancelReason, &t.CancelledAt, &t.SupersededBy,
		&t.CreatedAt, &t.UpdatedAt); err != nil {
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
			 WHERE t.assignee_id = $1
			    OR EXISTS (SELECT 1 FROM task_participants tp
			               WHERE tp.task_id = t.id AND tp.user_id = $1)
			 ORDER BY p.id`, userID)
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

// EnsureWorkerOperationsProject returns the durable inbox for worker command,
// agent, skill and workflow tasks. Older installations used "Worker Commands";
// rename that project in place so task history keeps the same stable project ID.
func (s *Store) EnsureWorkerOperationsProject(ctx context.Context, creatorID int64) (*Project, error) {
	const name = "Worker Operations"
	p, err := scanProject(s.pool.QueryRow(ctx,
		`SELECT id, name, description, creator_id, status, created_at FROM projects
		 WHERE creator_id = $1 AND name = ANY($2) AND status = 'active'
		 ORDER BY CASE WHEN name = $3 THEN 0 ELSE 1 END, id LIMIT 1`,
		creatorID, []string{name, "Worker Commands"}, name))
	if err == nil {
		if p.Name != name {
			if _, updateErr := s.pool.Exec(ctx,
				`UPDATE projects SET name = $2, description = $3 WHERE id = $1`,
				p.ID, name, "AI worker 命令、Agent、Skill 与工作流任务。"); updateErr != nil {
				return nil, updateErr
			}
			p.Name = name
			p.Description = "AI worker 命令、Agent、Skill 与工作流任务。"
		}
		return p, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return s.CreateProject(ctx, name, "AI worker 命令、Agent、Skill 与工作流任务。", creatorID)
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
	return s.createTask(ctx, t, nil)
}

// CreateTaskWithWorkerRun creates a business task and its initial execution in
// one transaction. It is only needed when the caller wants a non-default scope
// or executor; ordinary worker-assigned tasks use CreateTask.
func (s *Store) CreateTaskWithWorkerRun(ctx context.Context, t *Task, spec WorkerRunSpec) (*Task, error) {
	return s.createTask(ctx, t, &spec)
}

func (s *Store) createTask(ctx context.Context, t *Task, spec *WorkerRunSpec) (*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := createTaskTx(ctx, tx, t, spec)
	if err != nil {
		return nil, err
	}
	return created, tx.Commit(ctx)
}

func createTaskTx(ctx context.Context, tx pgx.Tx, t *Task, runSpec *WorkerRunSpec) (*Task, error) {
	t.Title = strings.TrimSpace(t.Title)
	t.Goal = strings.TrimSpace(t.Goal)
	t.Description = strings.TrimSpace(t.Description)
	t.Acceptance = strings.TrimSpace(t.Acceptance)
	t.Kind = NormalizeTaskKind(t.Kind)
	if t.Kind == "" {
		t.Kind = TaskKindGeneral
	}
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
		`INSERT INTO tasks (project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, kind, priority, deadline, depends_on, milestone_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+taskCols,
		t.ProjectID, t.ParentID, t.AssignerID, t.AssigneeID, t.Title, t.Goal, t.Description, t.Acceptance,
		t.Kind, nonEmpty(t.Priority, "normal"), t.Deadline, deps, t.MilestoneID)
	created, err := scanTask(row)
	if err != nil {
		return nil, err
	}
	if _, err := enqueueTaskWorkerRunTx(ctx, tx, created, runSpec); err != nil {
		return nil, err
	}
	return created, nil
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

// TasksOfAssignee 某人的相关任务。openOnly 只返回本人负责/协作的待办；完整列表
// 还包含其验收和关注的任务，便于从通知之外重新找到只读任务。
func (s *Store) TasksOfAssignee(ctx context.Context, userID int64, openOnly bool) ([]*Task, error) {
	sql := `SELECT ` + taskColsWithAlias("t") + ` FROM tasks t
			WHERE (t.assignee_id = $1 OR EXISTS (
			  SELECT 1 FROM task_participants tp
			  WHERE tp.task_id = t.id AND tp.user_id = $1`
	if openOnly {
		sql += ` AND tp.role = 'collaborator'`
	}
	sql += `
			))`
	if openOnly {
		sql += ` AND t.status IN ('pending', 'in_progress', 'awaiting_input')`
	}
	sql += ` ORDER BY t.id`
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

// TaskQueue 返回全局任务队列。scope:
//   - queue/open: pending + in_progress + awaiting_input（仍可执行或等待输入）
//   - review: done（已停止执行，等待验收）
//   - history: accepted + split + cancelled（终态）
//   - all: 全部
//   - 具体状态：pending/in_progress/awaiting_input/done/accepted/split/cancelled
func (s *Store) TaskQueue(ctx context.Context, scope string, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	where := "status IN ('pending','in_progress','awaiting_input')"
	switch strings.TrimSpace(scope) {
	case "", "queue", "open":
	case "review":
		where = "status = 'done'"
	case "history":
		where = "status IN ('accepted','split','cancelled')"
	case "all":
		where = "true"
	case TaskPending, TaskInProgress, TaskAwaitingInput, TaskDone, TaskAccepted, TaskSplit, TaskCancelled:
		where = "status = $2"
	default:
		where = "status IN ('pending','in_progress','awaiting_input')"
	}
	sql := `SELECT ` + taskCols + ` FROM tasks WHERE ` + where + `
		ORDER BY
		  CASE status
		    WHEN 'done' THEN 0
		    WHEN 'awaiting_input' THEN 1
		    WHEN 'in_progress' THEN 2
		    WHEN 'pending' THEN 3
		    WHEN 'accepted' THEN 4
		    WHEN 'split' THEN 5
		    WHEN 'cancelled' THEN 6
		    ELSE 7
		  END,
		  COALESCE(deadline, 'infinity'::timestamptz),
		  updated_at DESC
		LIMIT $1`
	if strings.Contains(where, "$2") {
		return s.queryTasks(ctx, sql, limit, strings.TrimSpace(scope))
	}
	return s.queryTasks(ctx, sql, limit)
}

// SubTasks 直接子任务。
func (s *Store) SubTasks(ctx context.Context, parentID int64) ([]*Task, error) {
	return s.queryTasks(ctx, `SELECT `+taskCols+` FROM tasks WHERE parent_id = $1 ORDER BY id`, parentID)
}

// UpdateTaskStatus 更新状态并返回更新后的任务。
func (s *Store) UpdateTaskStatus(ctx context.Context, id int64, status string) (*Task, error) {
	// Generic status writes are intentionally limited to reversible execution
	// states. Submission, review, split and cancellation have dedicated atomic
	// operations with their own side effects.
	if status != TaskPending && status != TaskInProgress {
		return nil, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanTask(tx.QueryRow(ctx,
		`SELECT `+taskCols+` FROM tasks
		 WHERE id = $1 AND status NOT IN ('accepted','split','cancelled') FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	if current.Status == status {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return current, nil
	}
	task, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET status = $2, revision = revision + 1,
		   submitted_by = NULL, submitted_at = NULL, updated_at = now()
		 WHERE id = $1 AND revision = $3
		 RETURNING `+taskCols, id, status, current.Revision))
	if err != nil {
		return nil, err
	}
	if _, _, err := restartTaskWorkerRunTx(ctx, tx, task, "任务状态被重新打开"); err != nil {
		return nil, err
	}
	return task, tx.Commit(ctx)
}

// ReassignTask 改派：保留 task 行（连带 task_progress/checklist/attachments 历史），换执行人，
// 状态回到 pending 让新执行人重新领取，清空 worker claim（旧执行人无法再写进度/提交），
// 并重置截止提醒/过期通知标记（新执行人按自己的节奏重新获得提醒）。
// 不动 parent_id/depends_on——改派只换人，不动依赖结构与拆分树。
// 仅允许 pending/in_progress（未提交的任务）：done 已在验收队列、accepted/split 是终态，
// 改派它们会丢失验收记录或破坏拆分树。状态不符返 ErrNotFound（store 层防线，工具层也校验）。
func (s *Store) ReassignTask(ctx context.Context, id, newAssigneeID int64) (*Task, error) {
	return s.ReassignTaskWithProgress(ctx, id, newAssigneeID, 0, "")
}

// ReassignTaskWithProgress performs the ownership change, participant cleanup,
// and optional audit progress write in one transaction. A participant role is
// never allowed to survive when that same user becomes the primary assignee:
// in particular, a former reviewer must not be able to review their own work.
func (s *Store) ReassignTaskWithProgress(ctx context.Context, id, newAssigneeID, actorID int64, note string) (*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanTask(tx.QueryRow(ctx,
		`SELECT `+taskCols+` FROM tasks
		 WHERE id = $1 AND status IN ('pending','in_progress','awaiting_input') FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	if current.AssigneeID == newAssigneeID {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return current, nil
	}
	task, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET
			   assignee_id = $2,
			   status = 'pending',
			   revision = revision + 1,
			   submitted_by = NULL,
			   submitted_at = NULL,
		   nudge_count = 0,
		   nudge_attempt_count = 0,
		   deadline_reminded_at = NULL,
		   overdue_notified_at = NULL,
		   deadline_reminder_attempted_at = NULL,
		   overdue_notice_attempted_at = NULL,
		   nudged_at = NULL,
		   deadline_reminder_claimed_at = NULL,
		   overdue_notice_claimed_at = NULL,
		   nudge_claimed_at = NULL,
		   updated_at = now()
			 WHERE id = $1 AND revision = $3 RETURNING `+taskCols, id, newAssigneeID, current.Revision))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM task_participants WHERE task_id = $1 AND user_id = $2`, id, newAssigneeID); err != nil {
		return nil, err
	}
	if note = strings.TrimSpace(note); note != "" && actorID > 0 {
		tag, err := tx.Exec(ctx,
			`INSERT INTO task_progress (task_id, author_id, content)
			 SELECT $1, id, $3 FROM users WHERE id = $2`,
			id, actorID, note)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, ErrNotFound
		}
	}
	if _, _, err := restartTaskWorkerRunTx(ctx, tx, task, "任务已改派"); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return task, nil
}

// UpdateTaskContent 分配者修改任务要素（nil 字段不动）。
// 截止时间变更时重置提醒标记，让新截止时间重新获得临近提醒与过期通知。
func (s *Store) UpdateTaskContent(ctx context.Context, id int64, goal, description, acceptance *string, deadline *time.Time) (*Task, error) {
	return s.updateTaskContent(ctx, id, goal, description, acceptance, nil, deadline)
}

// UpdateTaskContentWithKind also updates the structured dispatch/outcome
// category. Keeping it optional preserves callers that only change prose.
func (s *Store) UpdateTaskContentWithKind(ctx context.Context, id int64, goal, description, acceptance, kind *string, deadline *time.Time) (*Task, error) {
	return s.updateTaskContent(ctx, id, goal, description, acceptance, kind, deadline)
}

func (s *Store) updateTaskContent(ctx context.Context, id int64, goal, description, acceptance, kind *string, deadline *time.Time) (*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanTask(tx.QueryRow(ctx,
		`SELECT `+taskColsWithAlias("t")+` FROM tasks t
		 WHERE t.id = $1 AND t.status IN ('pending','in_progress','awaiting_input') FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	trim := func(value *string) *string {
		if value == nil {
			return nil
		}
		normalized := strings.TrimSpace(*value)
		return &normalized
	}
	goal = trim(goal)
	description = trim(description)
	acceptance = trim(acceptance)
	if kind != nil {
		if !IsTaskKind(*kind) {
			return nil, ErrConflict
		}
		normalized := NormalizeTaskKind(*kind)
		kind = &normalized
	}
	changed := goal != nil && *goal != current.Goal ||
		description != nil && *description != current.Description ||
		acceptance != nil && *acceptance != current.Acceptance ||
		kind != nil && *kind != current.Kind ||
		deadline != nil && !sameOptionalTime(deadline, current.Deadline)
	if !changed {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return current, nil
	}
	var isWorker bool
	if err := tx.QueryRow(ctx, `SELECT is_worker FROM users WHERE id = $1`, current.AssigneeID).Scan(&isWorker); err != nil {
		return nil, wrapErr(err)
	}
	task, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks t SET
			   goal = COALESCE($2, t.goal),
			   description = COALESCE($3, t.description),
			   acceptance = COALESCE($4, t.acceptance),
			   kind = COALESCE($5, t.kind),
			   status = CASE WHEN $7::boolean THEN 'pending' ELSE t.status END,
			   revision = t.revision + 1,
			   deadline = COALESCE($6, t.deadline),
			   nudge_claimed_at = CASE WHEN $6::timestamptz IS NOT NULL AND $6::timestamptz IS DISTINCT FROM t.deadline THEN NULL ELSE t.nudge_claimed_at END,
			   nudged_at = CASE WHEN $6::timestamptz IS NOT NULL AND $6::timestamptz IS DISTINCT FROM t.deadline THEN NULL ELSE t.nudged_at END,
			   nudge_count = CASE WHEN $6::timestamptz IS NOT NULL AND $6::timestamptz IS DISTINCT FROM t.deadline THEN 0 ELSE t.nudge_count END,
			   nudge_attempt_count = CASE WHEN $6::timestamptz IS NOT NULL AND $6::timestamptz IS DISTINCT FROM t.deadline THEN 0 ELSE t.nudge_attempt_count END,
			   updated_at = now()
			 WHERE t.id = $1 AND t.status IN ('pending', 'in_progress', 'awaiting_input')
			 RETURNING `+taskColsWithAlias("t"), id, goal, description, acceptance, kind, deadline, isWorker))
	if err != nil {
		return nil, err
	}
	if isWorker {
		if _, _, err := restartTaskWorkerRunTx(ctx, tx, task, "任务要求已更新"); err != nil {
			return nil, err
		}
	}
	return task, tx.Commit(ctx)
}

// SubmitTask 兼容旧调用：按责任人身份提交。
func (s *Store) SubmitTask(ctx context.Context, id int64) (*Task, []*Task, error) {
	return s.SubmitTaskBy(ctx, id, 0)
}

// SubmitTaskBy 由责任人或协作者提交完成。委派给他人的工作进入验收，
// 自己负责且没有独立验收人的工作直接归档。actorID=0 兼容旧调用。
func (s *Store) SubmitTaskBy(ctx context.Context, id, actorID int64) (*Task, []*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Acquire the task row before evaluating reviewer participation. A single
	// UPDATE statement can wait behind ReplaceTaskParticipants while retaining
	// its older statement snapshot, then miss the newly committed reviewer.
	current, err := scanTask(tx.QueryRow(ctx,
		`SELECT `+taskCols+` FROM tasks
		  WHERE id = $1 AND status IN ('pending', 'in_progress')
		    AND ($2::bigint = 0 OR assignee_id = $2 OR EXISTS (
		      SELECT 1 FROM task_participants tp
		       WHERE tp.task_id = tasks.id AND tp.user_id = $2 AND tp.role = 'collaborator'
		    ))
		  FOR UPDATE`, id, actorID))
	if err != nil {
		return nil, nil, err
	}
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET
				   status = `+successfulTaskCompletionStatusSQL+`,
				   revision = revision + 1,
				   submitted_by = CASE WHEN $2::bigint > 0 THEN $2 ELSE assignee_id END,
				   submitted_at = now(),
			   updated_at = now()
			 WHERE id = $1 AND revision = $3
			 RETURNING `+taskCols, id, actorID, current.Revision))
	if err != nil {
		return nil, nil, err
	}
	if _, err := cancelActiveWorkerRunsTx(ctx, tx, t.ID, "任务已由参与者提交"); err != nil {
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
			        revision = revision + 1,
			        updated_at = now()
			 WHERE id = $1 AND status = 'done' RETURNING `+taskCols, id))
	if err != nil {
		return nil, nil, err
	}
	chain, err := cascadeUp(ctx, tx, t.ParentID)
	if err != nil {
		return nil, nil, err
	}
	acceptedIDs := make([]int64, 1, len(chain)+1)
	acceptedIDs[0] = t.ID
	for _, item := range chain {
		acceptedIDs = append(acceptedIDs, item.ID)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE work_evidence SET status = 'resolved', updated_at = now()
		  WHERE task_id = ANY($1::bigint[]) AND status = 'active'`, acceptedIDs); err != nil {
		return nil, nil, err
	}
	return t, chain, tx.Commit(ctx)
}

// RejectTask 验收打回：真人执行人回 in_progress；worker 任务回 pending，
// 并创建一条新的执行记录。理由写入进度记录。
// 任务不在待验收状态时返回 ErrNotFound。
func (s *Store) RejectTask(ctx context.Context, id, reviewerID int64, reason string) (*Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks t
				    SET status = CASE WHEN u.is_worker THEN 'pending' ELSE 'in_progress' END,
				        revision = t.revision + 1,
				        submitted_by = NULL,
				        submitted_at = NULL,
			        updated_at = now()
			 FROM users u
			 WHERE t.id = $1 AND t.status = 'done' AND u.id = t.assignee_id
			 RETURNING `+taskColsWithAlias("t"), id))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1, $2, $3)`,
		id, reviewerID, "🔍 验收未通过："+reason); err != nil {
		return nil, err
	}
	if _, _, err := restartTaskWorkerRunTx(ctx, tx, t, "验收打回："+strings.TrimSpace(reason)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE work_evidence SET status = 'active', updated_at = now()
		  WHERE task_id = $1 AND status <> 'ignored'`, t.ID); err != nil {
		return nil, err
	}
	return t, tx.Commit(ctx)
}

// cascadeUp 子任务验收通过后的向上传播：父任务（split）的全部子任务 accepted 时，
// 父任务按责任关系进入 done 或 accepted。返回状态被改变的祖先（自下而上）。
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
				   status = `+successfulTaskCompletionStatusSQL+`,
				   revision = revision + 1,
				   updated_at = now()
			 WHERE id = $1 AND status = 'split'
			   AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id = $1 AND c.status NOT IN ('accepted','cancelled'))
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

// replayCascadeChain reconstructs the durable result of cascadeUp after an
// idempotent caller retry. It never mutates state: only ancestors whose stored
// status proves that propagation crossed that boundary are returned.
func replayCascadeChain(ctx context.Context, tx pgx.Tx, parentID *int64) ([]*Task, error) {
	var chain []*Task
	for parentID != nil {
		parent, err := scanTask(tx.QueryRow(ctx,
			`SELECT `+taskCols+` FROM tasks WHERE id = $1`, *parentID))
		if errors.Is(err, ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		if parent.Status != TaskAccepted && parent.Status != TaskDone {
			break
		}
		chain = append(chain, parent)
		if parent.Status != TaskAccepted {
			break
		}
		parentID = parent.ParentID
	}
	return chain, nil
}

// TasksAwaitingReview 我分配出去、已提交待我验收的任务。
func (s *Store) TasksAwaitingReview(ctx context.Context, assignerID int64) ([]*Task, error) {
	return s.queryTasks(ctx,
		`SELECT `+taskColsWithAlias("t")+` FROM tasks t
		 WHERE t.status = 'done' AND (
		   (t.assigner_id = $1 AND t.assignee_id <> $1)
		   OR EXISTS (SELECT 1 FROM task_participants tp
		              WHERE tp.task_id = t.id AND tp.user_id = $1 AND tp.role = 'reviewer')
		 ) ORDER BY t.updated_at`, assignerID)
}

// DueNudges 原子认领需要 AI 催办的任务：已发过期通知，且距最近一次
// 「过期通知 / 催办 / 进度更新」超过 interval 的开放任务。
func (s *Store) DueNudges(ctx context.Context, now time.Time, interval time.Duration) ([]*Task, error) {
	stale := now.Add(-taskReminderClaimLease)
	return s.queryTasks(ctx,
		`WITH due AS (
		   SELECT t.id FROM tasks t
		   JOIN users u ON u.id = t.assignee_id
		   LEFT JOIN LATERAL (
		     SELECT max(created_at) AS at FROM task_progress p WHERE p.task_id = t.id
		   ) prog ON TRUE
		   WHERE t.status IN ('pending', 'in_progress')
		     AND u.status = 'active' AND NOT u.is_worker
		     AND t.overdue_notified_at IS NOT NULL
		     AND (t.nudge_claimed_at IS NULL OR t.nudge_claimed_at <= $3)
		     AND GREATEST(t.overdue_notified_at,
		                  COALESCE(t.nudged_at, t.overdue_notified_at),
		                  COALESCE(prog.at, t.overdue_notified_at)) <= $2
		   ORDER BY t.id LIMIT 128 FOR UPDATE OF t SKIP LOCKED
		 )
		 UPDATE tasks t SET nudge_claimed_at = $1
		 FROM due WHERE t.id = due.id RETURNING `+taskColsWithAlias("t"), now, now.Add(-interval), stale)
}

// DueDeadlineReminders 原子认领「临近截止」提醒：开放任务、截止落在 (now, now+window]、本轮尚未尝试。
func (s *Store) DueDeadlineReminders(ctx context.Context, now time.Time, window time.Duration) ([]*Task, error) {
	stale := now.Add(-taskReminderClaimLease)
	return s.queryTasks(ctx,
		`WITH due AS (
		   SELECT id FROM tasks
		    WHERE status IN ('pending', 'in_progress') AND deadline IS NOT NULL
		      AND deadline_reminder_attempted_at IS NULL AND deadline > $1 AND deadline <= $2
		      AND (deadline_reminder_claimed_at IS NULL OR deadline_reminder_claimed_at <= $3)
		    ORDER BY deadline, id LIMIT 128 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE tasks t SET deadline_reminder_claimed_at = $1
		 FROM due WHERE t.id = due.id RETURNING `+taskColsWithAlias("t"), now, now.Add(window), stale)
}

// DueOverdueNotices 原子认领「已过期」通知：开放任务、截止已过、本轮尚未尝试。
func (s *Store) DueOverdueNotices(ctx context.Context, now time.Time) ([]*Task, error) {
	stale := now.Add(-taskReminderClaimLease)
	return s.queryTasks(ctx,
		`WITH due AS (
		   SELECT id FROM tasks
		    WHERE status IN ('pending', 'in_progress') AND deadline IS NOT NULL
		      AND overdue_notice_attempted_at IS NULL AND deadline <= $1
		      AND (overdue_notice_claimed_at IS NULL OR overdue_notice_claimed_at <= $2)
		    ORDER BY deadline, id LIMIT 128 FOR UPDATE SKIP LOCKED
		 )
		 UPDATE tasks t SET overdue_notice_claimed_at = $1
		 FROM due WHERE t.id = due.id RETURNING `+taskColsWithAlias("t"), now, stale)
}

func (s *Store) MarkDeadlineReminderSent(ctx context.Context, id, generation int64, sentAt time.Time) error {
	return s.MarkDeadlineReminderAttempt(ctx, id, generation, sentAt, true)
}

func (s *Store) MarkDeadlineReminderAttempt(ctx context.Context, id, generation int64, attemptedAt time.Time, delivered bool) error {
	return s.execOne(ctx,
		`UPDATE tasks
		 SET deadline_reminder_attempted_at=$3,
		     deadline_reminded_at=CASE WHEN $4 THEN $3 ELSE deadline_reminded_at END,
		     deadline_reminder_claimed_at=NULL, updated_at=now()
		 WHERE id=$1 AND deadline_generation=$2 AND deadline_reminder_claimed_at IS NOT NULL`, id, generation, attemptedAt, delivered)
}

func (s *Store) MarkOverdueNoticeSent(ctx context.Context, id, generation int64, sentAt time.Time) error {
	return s.MarkOverdueNoticeAttempt(ctx, id, generation, sentAt, true)
}

func (s *Store) MarkOverdueNoticeAttempt(ctx context.Context, id, generation int64, attemptedAt time.Time, delivered bool) error {
	return s.execOne(ctx,
		`UPDATE tasks
		 SET overdue_notice_attempted_at=$3,
		     overdue_notified_at=CASE WHEN $4 THEN $3 ELSE overdue_notified_at END,
		     overdue_notice_claimed_at=NULL, updated_at=now()
		 WHERE id=$1 AND deadline_generation=$2 AND overdue_notice_claimed_at IS NOT NULL`, id, generation, attemptedAt, delivered)
}

func (s *Store) MarkNudgeSent(ctx context.Context, id, generation int64, sentAt time.Time) error {
	return s.MarkNudgeAttempt(ctx, id, generation, sentAt, true)
}

// MarkNudgeAttempt settles one claimed transport occurrence. Failed or
// uncertain deliveries advance only the attempt sequence, allowing a later
// scheduled attempt to use a fresh key without inflating successful nudges.
func (s *Store) MarkNudgeAttempt(ctx context.Context, id, generation int64, attemptedAt time.Time, delivered bool) error {
	return s.execOne(ctx,
		`UPDATE tasks
		    SET nudged_at=$3, nudge_claimed_at=NULL,
		        nudge_attempt_count=nudge_attempt_count+1,
		        nudge_count=nudge_count+CASE WHEN $4 THEN 1 ELSE 0 END,
		        updated_at=now()
		  WHERE id=$1 AND deadline_generation=$2 AND nudge_claimed_at IS NOT NULL`, id, generation, attemptedAt, delivered)
}

// TaskStats 全局任务统计（老板摘要用）。
type TaskStats struct {
	Open          int64 // 待处理 + 进行中 + 待补充信息
	AwaitingInput int64 // 其中等待分配者补充信息
	Overdue       int64 // 开放任务中已过截止时间
	Awaiting      int64 // 已提交待验收
	DoneSince     int64 // 自给定时刻以来验收通过
}

// GlobalTaskStats 全局任务统计。
func (s *Store) GlobalTaskStats(ctx context.Context, doneSince time.Time) (*TaskStats, error) {
	var st TaskStats
	err := s.pool.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE status IN ('pending','in_progress','awaiting_input')),
		   count(*) FILTER (WHERE status = 'awaiting_input'),
		   count(*) FILTER (WHERE status IN ('pending','in_progress','awaiting_input') AND deadline IS NOT NULL AND deadline < now()),
		   count(*) FILTER (WHERE status = 'done'),
		   count(*) FILTER (WHERE status = 'accepted' AND updated_at >= $1)
		 FROM tasks`, doneSince).Scan(&st.Open, &st.AwaitingInput, &st.Overdue, &st.Awaiting, &st.DoneSince)
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
		   count(*) FILTER (WHERE status IN ('pending','in_progress','awaiting_input')),
		   count(*) FILTER (WHERE status IN ('pending','in_progress','awaiting_input') AND deadline IS NOT NULL AND deadline < now()),
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
			   count(*) FILTER (WHERE status IN ('pending','in_progress','awaiting_input')),
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
		 WHERE status IN ('pending','in_progress','awaiting_input') AND deadline IS NOT NULL AND deadline < now()
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
	parent, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET status = 'split', revision = revision + 1, updated_at = now()
		 WHERE id = $1 AND status IN ('pending', 'in_progress', 'awaiting_input')
		 RETURNING `+taskCols, parentID))
	if err != nil {
		return nil, err
	}
	parentSpec, err := latestTaskRunSpecTx(ctx, tx, parent)
	if err != nil {
		return nil, err
	}
	if _, err := cancelActiveWorkerRunsTx(ctx, tx, parentID, "任务已拆分"); err != nil {
		return nil, err
	}
	created := make([]*Task, 0, len(subs))
	for _, t := range subs {
		row := tx.QueryRow(ctx,
			`INSERT INTO tasks (project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, kind, priority, deadline, milestone_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING `+taskCols,
			t.ProjectID, parentID, t.AssignerID, t.AssigneeID, t.Title, t.Goal, t.Description, t.Acceptance,
			nonEmpty(NormalizeTaskKind(t.Kind), parent.Kind), nonEmpty(t.Priority, "normal"), t.Deadline, t.MilestoneID)
		ct, err := scanTask(row)
		if err != nil {
			return nil, err
		}
		if _, err := enqueueTaskWorkerRunTx(ctx, tx, ct, &parentSpec); err != nil {
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
	rows, err := tx.Query(ctx,
		`WITH RECURSIVE task_tree AS (
		   SELECT id FROM tasks WHERE id = $1
		   UNION ALL
		   SELECT child.id FROM tasks child JOIN task_tree parent ON child.parent_id = parent.id
		 ) SELECT id FROM task_tree`, id)
	if err != nil {
		return err
	}
	var deletedIDs []int64
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return err
		}
		deletedIDs = append(deletedIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(deletedIDs) == 0 {
		return ErrNotFound
	}

	// 把整棵被删任务树从其他任务的 depends_on 里剔除：语义上「前置被取消 = 不再等待」，
	// 同时防悬挂 id——依赖检查的 NOT EXISTS 对已消失的前置恒为真，下游会在前置
	// 从未完成的情况下被 worker 领走。
	if _, err := tx.Exec(ctx,
		`UPDATE tasks
		    SET depends_on = ARRAY(SELECT dep FROM unnest(depends_on) dep WHERE NOT dep = ANY($1::bigint[])),
		        updated_at = now()
		  WHERE depends_on && $1::bigint[]`, deletedIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE decision_items SET status = 'closed', updated_at = now()
		  WHERE status = 'open' AND ref_type = 'task' AND ref_id = ANY($1::bigint[])`, deletedIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE work_evidence SET status = 'ignored', updated_at = now()
		  WHERE task_id = ANY($1::bigint[]) AND status NOT IN ('resolved','ignored')`, deletedIDs); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE material_cases SET status = 'ignored', last_error = '关联任务已删除',
		  completed_at = now(), updated_at = now()
		  WHERE task_id = ANY($1::bigint[]) AND status <> 'ignored'`, deletedIDs); err != nil {
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

// CancelTask keeps the task and its history while removing it from active
// queues. When supersededBy is set, downstream dependencies are atomically
// rewired to the replacement task; otherwise the cancelled prerequisite is
// removed. Accepted and split tasks cannot be cancelled.
func (s *Store) CancelTask(ctx context.Context, id int64, reason string, supersededBy *int64) (*Task, []*Task, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, nil, err
	}
	if task.Status == TaskAccepted || task.Status == TaskSplit || task.Status == TaskCancelled {
		return nil, nil, ErrNotFound
	}
	if supersededBy != nil {
		if *supersededBy <= 0 || *supersededBy == task.ID {
			return nil, nil, ErrConflict
		}
		replacement, err := scanTask(tx.QueryRow(ctx,
			`SELECT `+taskCols+` FROM tasks WHERE id = $1 FOR SHARE`, *supersededBy))
		if err != nil {
			return nil, nil, err
		}
		if replacement.ProjectID != task.ProjectID || replacement.Status == TaskCancelled ||
			nullableIDKey(replacement.ParentID) != nullableIDKey(task.ParentID) {
			return nil, nil, ErrConflict
		}
		// Preserve the replacement task's existing roles, then carry over active
		// human participants and the old human owner. Worker ownership remains an
		// independent execution unit and is never converted into collaboration.
		existingRows, err := tx.Query(ctx, `SELECT user_id FROM task_participants WHERE task_id = $1`, replacement.ID)
		if err != nil {
			return nil, nil, err
		}
		existing := map[int64]bool{}
		for existingRows.Next() {
			var userID int64
			if err := existingRows.Scan(&userID); err != nil {
				existingRows.Close()
				return nil, nil, err
			}
			existing[userID] = true
		}
		if err := existingRows.Err(); err != nil {
			existingRows.Close()
			return nil, nil, err
		}
		existingRows.Close()

		participantRows, err := tx.Query(ctx,
			`SELECT p.user_id, p.role
			   FROM task_participants p
			   JOIN users u ON u.id = p.user_id
			  WHERE p.task_id = $1 AND u.status = 'active' AND NOT u.is_worker`, task.ID)
		if err != nil {
			return nil, nil, err
		}
		var migrated []TaskParticipantInput
		for participantRows.Next() {
			var input TaskParticipantInput
			if err := participantRows.Scan(&input.UserID, &input.Role); err != nil {
				participantRows.Close()
				return nil, nil, err
			}
			if !existing[input.UserID] {
				existing[input.UserID] = true
				migrated = append(migrated, input)
			}
		}
		if err := participantRows.Err(); err != nil {
			participantRows.Close()
			return nil, nil, err
		}
		participantRows.Close()
		if task.AssigneeID != replacement.AssigneeID && !existing[task.AssigneeID] {
			var activeHuman bool
			if err := tx.QueryRow(ctx,
				`SELECT status = 'active' AND NOT is_worker FROM users WHERE id = $1`, task.AssigneeID).Scan(&activeHuman); err != nil {
				return nil, nil, wrapErr(err)
			}
			if activeHuman {
				migrated = append(migrated, TaskParticipantInput{UserID: task.AssigneeID, Role: TaskParticipantCollaborator})
			}
		}
		if _, err := upsertTaskParticipantsTx(ctx, tx, replacement, migrated, task.AssignerID); err != nil {
			if !errors.Is(err, ErrWorkerTaskParticipant) {
				return nil, nil, err
			}
			// A worker-owned replacement cannot share collaborators. Preserve
			// reviewer/watcher roles while leaving human execution on its old record.
			filtered := migrated[:0]
			for _, input := range migrated {
				if input.Role != TaskParticipantCollaborator {
					filtered = append(filtered, input)
				}
			}
			if _, err := upsertTaskParticipantsTx(ctx, tx, replacement, filtered, task.AssignerID); err != nil {
				return nil, nil, err
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_attachments (task_id, kind, file_ref, caption, file_id)
			 SELECT $2, kind, file_ref, caption, file_id FROM task_attachments WHERE task_id = $1
			 ON CONFLICT DO NOTHING`, task.ID, replacement.ID); err != nil {
			return nil, nil, err
		}
	}

	rows, err := tx.Query(ctx, `SELECT id, depends_on FROM tasks WHERE $1 = ANY(depends_on) FOR UPDATE`, task.ID)
	if err != nil {
		return nil, nil, err
	}
	type dependent struct {
		id   int64
		deps []int64
	}
	var dependents []dependent
	for rows.Next() {
		var item dependent
		if err := rows.Scan(&item.id, &item.deps); err != nil {
			rows.Close()
			return nil, nil, err
		}
		dependents = append(dependents, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()
	for _, item := range dependents {
		deps := make([]int64, 0, len(item.deps))
		for _, dep := range item.deps {
			if dep != task.ID {
				deps = append(deps, dep)
				continue
			}
			if supersededBy != nil && *supersededBy != item.id {
				// Dependencies are constrained to older task IDs. Preserve that
				// invariant while rewiring so replacement cannot create a cycle.
				if *supersededBy >= item.id {
					return nil, nil, ErrConflict
				}
				deps = append(deps, *supersededBy)
			}
		}
		deps, ok := normalizeTaskDeps(deps)
		if !ok {
			return nil, nil, ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET depends_on = $2, updated_at = now() WHERE id = $1`, item.id, deps); err != nil {
			return nil, nil, err
		}
	}

	cancelled, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET
		   status = 'cancelled', cancel_reason = $2, cancelled_at = now(), superseded_by = $3,
		   revision = revision + 1,
		   updated_at = now()
		 WHERE id = $1
		 RETURNING `+taskCols, task.ID, reason, supersededBy))
	if err != nil {
		return nil, nil, err
	}
	if _, err := cancelActiveWorkerRunsTx(ctx, tx, task.ID, reason); err != nil {
		return nil, nil, err
	}
	evidenceStatus := WorkEvidenceIgnored
	if supersededBy != nil {
		evidenceStatus = WorkEvidenceSuperseded
	}
	if _, err := tx.Exec(ctx,
		`UPDATE work_evidence SET status = $2, updated_at = now()
		  WHERE task_id = $1 AND status NOT IN ('resolved','ignored')`, task.ID, evidenceStatus); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE material_cases SET status = 'ignored', last_error = $2,
		  completed_at = now(), updated_at = now()
		  WHERE task_id = $1 AND status <> 'ignored'`, task.ID, reason); err != nil {
		return nil, nil, err
	}
	chain, err := cascadeUp(ctx, tx, task.ParentID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return cancelled, chain, nil
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

// LatestProgressForTasks 批量读取每个任务最新一条进度，供队列和验收视图展示
// 执行证据；单条批量查询避免列表页按任务产生 N+1 查询。
func (s *Store) LatestProgressForTasks(ctx context.Context, taskIDs []int64) (map[int64]Progress, error) {
	out := make(map[int64]Progress, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (task_id) id, task_id, author_id, content, created_at
		   FROM task_progress
		  WHERE task_id = ANY($1)
		  ORDER BY task_id, id DESC`, taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var progress Progress
		if err := rows.Scan(&progress.ID, &progress.TaskID, &progress.AuthorID, &progress.Content, &progress.CreatedAt); err != nil {
			return nil, err
		}
		out[progress.TaskID] = progress
	}
	return out, rows.Err()
}

// AddAttachment 挂附件。
func (s *Store) AddAttachment(ctx context.Context, a Attachment) error {
	_, err := s.AddAttachmentOnce(ctx, a)
	return err
}

func lockTaskForInputChangeTx(ctx context.Context, tx pgx.Tx, taskID int64) (*Task, error) {
	return scanTask(tx.QueryRow(ctx,
		`SELECT `+taskCols+` FROM tasks
		 WHERE id = $1 AND status IN ('pending','in_progress','awaiting_input')
		 FOR UPDATE`, taskID))
}

func reviseTaskForInputChangeTx(ctx context.Context, tx pgx.Tx, current *Task) (*Task, error) {
	var isWorker bool
	if err := tx.QueryRow(ctx, `SELECT is_worker FROM users WHERE id = $1`, current.AssigneeID).Scan(&isWorker); err != nil {
		return nil, wrapErr(err)
	}
	task, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET revision = revision + 1,
		   status = CASE WHEN $3 THEN 'pending' ELSE status END,
		   updated_at = now()
		 WHERE id = $1 AND revision = $2 RETURNING `+taskCols,
		current.ID, current.Revision, isWorker))
	if err != nil {
		return nil, err
	}
	if isWorker {
		if _, _, err := restartTaskWorkerRunTx(ctx, tx, task, "任务输入附件已更新"); err != nil {
			return nil, err
		}
	}
	return task, nil
}

func (s *Store) AddAttachmentOnce(ctx context.Context, a Attachment) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := lockTaskForInputChangeTx(ctx, tx, a.TaskID)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO task_attachments (task_id, kind, file_ref, caption)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT DO NOTHING`,
		a.TaskID, a.Kind, a.FileRef, a.Caption)
	if err != nil {
		return false, wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if _, err := reviseTaskForInputChangeTx(ctx, tx, current); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
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

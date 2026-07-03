package store

import (
	"context"
	"time"
)

// 项目状态。
const (
	ProjectActive   = "active"
	ProjectArchived = "archived"
)

// 任务状态。
const (
	TaskPending    = "pending"
	TaskInProgress = "in_progress"
	TaskDone       = "done"
	TaskSplit      = "split" // 已拆分：由子任务承载执行
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

// Task 任务。Goal 是"为什么做"，Description 是"做什么"，Acceptance 是验收标准。
type Task struct {
	ID          int64
	ProjectID   int64
	ParentID    *int64
	AssignerID  int64
	AssigneeID  int64
	Title       string
	Goal        string
	Description string
	Acceptance  string
	Priority    string
	Deadline    *time.Time
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
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

const taskCols = `id, project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, priority, deadline, status, created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	if err := row.Scan(&t.ID, &t.ProjectID, &t.ParentID, &t.AssignerID, &t.AssigneeID,
		&t.Title, &t.Goal, &t.Description, &t.Acceptance, &t.Priority, &t.Deadline,
		&t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
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

// SetProjectStatus 归档/激活项目。
func (s *Store) SetProjectStatus(ctx context.Context, id int64, status string) error {
	return s.execOne(ctx, `UPDATE projects SET status = $2 WHERE id = $1`, id, status)
}

// DeleteProject 删除项目及其全部任务（外键级联）。
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	return s.execOne(ctx, `DELETE FROM projects WHERE id = $1`, id)
}

// --- 任务 ---

// CreateTask 建任务。
func (s *Store) CreateTask(ctx context.Context, t *Task) (*Task, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO tasks (project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, priority, deadline)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+taskCols,
		t.ProjectID, t.ParentID, t.AssignerID, t.AssigneeID, t.Title, t.Goal, t.Description, t.Acceptance, nonEmpty(t.Priority, "normal"), t.Deadline)
	return scanTask(row)
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
		`UPDATE tasks SET status = $2, updated_at = now() WHERE id = $1 RETURNING `+taskCols, id, status))
}

// UpdateTaskContent 分配者修改任务要素（nil 字段不动）。
func (s *Store) UpdateTaskContent(ctx context.Context, id int64, goal, description, acceptance *string, deadline *time.Time) (*Task, error) {
	return scanTask(s.pool.QueryRow(ctx,
		`UPDATE tasks SET
		   goal = COALESCE($2, goal),
		   description = COALESCE($3, description),
		   acceptance = COALESCE($4, acceptance),
		   deadline = COALESCE($5, deadline),
		   updated_at = now()
		 WHERE id = $1 RETURNING `+taskCols, id, goal, description, acceptance, deadline))
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
		`UPDATE tasks SET status = 'split', updated_at = now()
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
			`INSERT INTO tasks (project_id, parent_id, assigner_id, assignee_id, title, goal, description, acceptance, priority, deadline)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+taskCols,
			t.ProjectID, parentID, t.AssignerID, t.AssigneeID, t.Title, t.Goal, t.Description, t.Acceptance, nonEmpty(t.Priority, "normal"), t.Deadline)
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
	return s.execOne(ctx, `DELETE FROM tasks WHERE id = $1`, id)
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

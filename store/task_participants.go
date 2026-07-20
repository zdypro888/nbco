package store

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	TaskParticipantCollaborator = "collaborator"
	TaskParticipantReviewer     = "reviewer"
	TaskParticipantWatcher      = "watcher"
)

var ErrWorkerTaskParticipant = errors.New("ai worker 不能作为共享任务参与者")

type TaskParticipantInput struct {
	UserID int64
	Role   string
}

type TaskParticipant struct {
	TaskID   int64  `json:"task_id"`
	UserID   int64  `json:"user_id"`
	UserName string `json:"user_name"`
	Role     string `json:"role"`
	AddedBy  *int64 `json:"added_by,omitempty"`
}

type TaskAccess struct {
	Role          string
	CanView       bool
	CanContribute bool
	CanReview     bool
	CanManage     bool
}

type TaskCreateResult struct {
	Task                *Task
	Created             bool
	Participants        []TaskParticipant
	ChangedParticipants []TaskParticipant
	UpdatedFields       []string
}

// CreateOrMergeTask creates one task identity for one open deliverable. An
// equivalent request with another human assignee adds that person as a
// collaborator instead of creating a duplicate row. allowParallel is the
// explicit escape hatch for genuinely independent deliverables.
func (s *Store) CreateOrMergeTask(ctx context.Context, t *Task, participants []TaskParticipantInput, addedBy int64, allowParallel bool) (*TaskCreateResult, error) {
	if t == nil {
		return nil, ErrNotFound
	}
	t.Title = strings.TrimSpace(t.Title)
	t.Goal = strings.TrimSpace(t.Goal)
	t.Description = strings.TrimSpace(t.Description)
	t.Acceptance = strings.TrimSpace(t.Acceptance)
	normalizeTaskWorkerScope(t)
	if !normalizeTaskCompletionPolicy(t) {
		return nil, ErrConflict
	}
	t.Priority = nonEmpty(strings.TrimSpace(t.Priority), "normal")
	deps, ok := normalizeTaskDeps(t.DependsOn)
	if !ok {
		return nil, ErrNotFound
	}
	t.DependsOn = deps

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var task *Task
	var updatedFields []string
	created := false
	if !allowParallel {
		key := strings.Join([]string{
			strconv.FormatInt(t.ProjectID, 10), nullableIDKey(t.ParentID), strconv.FormatInt(t.AssignerID, 10),
			t.Title, t.Goal, t.Description, t.Acceptance,
		}, "\x1f")
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
			return nil, err
		}
		task, err = scanTask(tx.QueryRow(ctx,
			`SELECT `+taskCols+` FROM tasks
				 WHERE project_id = $1 AND parent_id IS NOT DISTINCT FROM $2
				   AND title = $3 AND goal = $4 AND description = $5 AND acceptance = $6
				   AND assigner_id = $7
				   AND status IN ('pending','in_progress','awaiting_input','done')
				 ORDER BY id LIMIT 1 FOR UPDATE`,
			t.ProjectID, t.ParentID, t.Title, t.Goal, t.Description, t.Acceptance, t.AssignerID))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if task == nil {
		task, err = createTaskTx(ctx, tx, t)
		if err != nil {
			return nil, err
		}
		created = true
	} else {
		task, updatedFields, err = mergeTaskConstraintsTx(ctx, tx, task, t)
		if err != nil {
			return nil, err
		}
		if task.AssigneeID != t.AssigneeID {
			participants = append([]TaskParticipantInput{{UserID: t.AssigneeID, Role: TaskParticipantCollaborator}}, participants...)
		}
	}

	changed, err := upsertTaskParticipantsTx(ctx, tx, task, participants, addedBy)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	all, err := s.TaskParticipants(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	changedSet := make(map[int64]bool, len(changed))
	for _, p := range changed {
		changedSet[p.UserID] = true
	}
	changed = changed[:0]
	for _, p := range all {
		if changedSet[p.UserID] {
			changed = append(changed, p)
		}
	}
	return &TaskCreateResult{
		Task: task, Created: created, Participants: all,
		ChangedParticipants: changed, UpdatedFields: updatedFields,
	}, nil
}

// mergeTaskConstraintsTx preserves the single-deliverable identity while
// deterministically merging operational constraints. Duplicate assignments may
// tighten priority/deadline and add older same-project prerequisites. Fields
// that would change the execution identity are rejected instead of being
// silently discarded.
func mergeTaskConstraintsTx(ctx context.Context, tx pgx.Tx, current, incoming *Task) (*Task, []string, error) {
	if current.WorkerCommand != incoming.WorkerCommand ||
		current.WorkerCommandPTY != incoming.WorkerCommandPTY ||
		current.CompletionPolicy != incoming.CompletionPolicy ||
		current.WorkerScopeType != incoming.WorkerScopeType ||
		current.WorkerScopeKey != incoming.WorkerScopeKey {
		return nil, nil, ErrConflict
	}
	if current.MilestoneID != nil && incoming.MilestoneID != nil && *current.MilestoneID != *incoming.MilestoneID {
		return nil, nil, ErrConflict
	}

	priority, ok := stricterTaskPriority(current.Priority, incoming.Priority)
	if !ok {
		return nil, nil, ErrConflict
	}
	deadline := current.Deadline
	if incoming.Deadline != nil && (deadline == nil || incoming.Deadline.Before(*deadline)) {
		value := *incoming.Deadline
		deadline = &value
	}
	deps, changedDeps, err := mergeTaskDependenciesTx(ctx, tx, current, incoming.DependsOn)
	if err != nil {
		return nil, nil, err
	}
	milestoneID := current.MilestoneID
	if milestoneID == nil && incoming.MilestoneID != nil {
		value := *incoming.MilestoneID
		milestoneID = &value
	}

	updated := make([]string, 0, 4)
	if priority != current.Priority {
		updated = append(updated, "priority")
	}
	if !sameOptionalTime(deadline, current.Deadline) {
		updated = append(updated, "deadline")
	}
	if changedDeps {
		updated = append(updated, "depends_on")
	}
	if !sameOptionalID(milestoneID, current.MilestoneID) {
		updated = append(updated, "milestone_id")
	}
	if len(updated) == 0 {
		return current, nil, nil
	}
	merged, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET priority = $2, deadline = $3, depends_on = $4,
		                    milestone_id = $5, updated_at = now()
		  WHERE id = $1 RETURNING `+taskCols,
		current.ID, priority, deadline, deps, milestoneID))
	return merged, updated, err
}

func stricterTaskPriority(a, b string) (string, bool) {
	rank := map[string]int{"low": 0, "normal": 1, "high": 2}
	a = nonEmpty(strings.TrimSpace(a), "normal")
	b = nonEmpty(strings.TrimSpace(b), "normal")
	ar, aok := rank[a]
	br, bok := rank[b]
	if !aok || !bok {
		return "", false
	}
	if br > ar {
		return b, true
	}
	return a, true
}

func mergeTaskDependenciesTx(ctx context.Context, tx pgx.Tx, current *Task, incoming []int64) ([]int64, bool, error) {
	merged := append([]int64(nil), current.DependsOn...)
	seen := make(map[int64]bool, len(merged)+len(incoming))
	for _, id := range merged {
		seen[id] = true
	}
	var added []int64
	for _, id := range incoming {
		if seen[id] {
			continue
		}
		// Task creation naturally enforces this invariant. Preserve it on a
		// later merge so a newer task can never become an ancestor dependency.
		if id <= 0 || id >= current.ID {
			return nil, false, ErrConflict
		}
		seen[id] = true
		added = append(added, id)
		merged = append(merged, id)
	}
	if len(added) == 0 {
		return merged, false, nil
	}
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE project_id = $1 AND id = ANY($2)`,
		current.ProjectID, added).Scan(&count); err != nil {
		return nil, false, err
	}
	if count != len(added) {
		return nil, false, ErrNotFound
	}
	return merged, true, nil
}

func sameOptionalID(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func sameOptionalTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

func nullableIDKey(id *int64) string {
	if id == nil {
		return "-"
	}
	return strconv.FormatInt(*id, 10)
}

func validTaskParticipantRole(role string) bool {
	switch role {
	case TaskParticipantCollaborator, TaskParticipantReviewer, TaskParticipantWatcher:
		return true
	default:
		return false
	}
}

func normalizeParticipantInputs(task *Task, in []TaskParticipantInput) ([]TaskParticipantInput, error) {
	byUser := make(map[int64]string, len(in))
	for _, item := range in {
		item.Role = strings.TrimSpace(item.Role)
		if item.UserID <= 0 || !validTaskParticipantRole(item.Role) {
			return nil, ErrNotFound
		}
		if item.UserID == task.AssigneeID || item.UserID == task.AssignerID {
			continue
		}
		byUser[item.UserID] = item.Role
	}
	ids := make([]int64, 0, len(byUser))
	for id := range byUser {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]TaskParticipantInput, 0, len(ids))
	for _, id := range ids {
		out = append(out, TaskParticipantInput{UserID: id, Role: byUser[id]})
	}
	return out, nil
}

func validateTaskParticipantUsersTx(ctx context.Context, tx pgx.Tx, task *Task, in []TaskParticipantInput) error {
	if len(in) == 0 {
		return nil
	}
	var ownerIsWorker bool
	if err := tx.QueryRow(ctx, `SELECT is_worker FROM users WHERE id = $1 AND status = 'active'`, task.AssigneeID).Scan(&ownerIsWorker); err != nil {
		return wrapErr(err)
	}
	if ownerIsWorker {
		for _, item := range in {
			if item.Role == TaskParticipantCollaborator {
				return ErrWorkerTaskParticipant
			}
		}
	}
	ids := make([]int64, 0, len(in))
	for _, item := range in {
		ids = append(ids, item.UserID)
	}
	rows, err := tx.Query(ctx, `SELECT id, status, is_worker FROM users WHERE id = ANY($1) FOR SHARE`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		var status string
		var worker bool
		if err := rows.Scan(&id, &status, &worker); err != nil {
			return err
		}
		if status != UserActive {
			return ErrNotFound
		}
		if worker {
			return ErrWorkerTaskParticipant
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(seen) != len(ids) {
		return ErrNotFound
	}
	return nil
}

func upsertTaskParticipantsTx(ctx context.Context, tx pgx.Tx, task *Task, in []TaskParticipantInput, addedBy int64) ([]TaskParticipant, error) {
	normalized, err := normalizeParticipantInputs(task, in)
	if err != nil {
		return nil, err
	}
	if err := validateTaskParticipantUsersTx(ctx, tx, task, normalized); err != nil {
		return nil, err
	}
	var addedByArg any
	if addedBy > 0 {
		addedByArg = addedBy
	}
	changed := make([]TaskParticipant, 0, len(normalized))
	for _, item := range normalized {
		var p TaskParticipant
		err := tx.QueryRow(ctx,
			`INSERT INTO task_participants (task_id, user_id, role, added_by)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (task_id, user_id) DO UPDATE
			 SET role = EXCLUDED.role, added_by = EXCLUDED.added_by, updated_at = now()
			 WHERE task_participants.role IS DISTINCT FROM EXCLUDED.role
			    OR task_participants.added_by IS DISTINCT FROM EXCLUDED.added_by
			 RETURNING task_id, user_id, role, added_by`,
			task.ID, item.UserID, item.Role, addedByArg).
			Scan(&p.TaskID, &p.UserID, &p.Role, &p.AddedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		changed = append(changed, p)
	}
	return changed, nil
}

// ReplaceTaskParticipants atomically replaces all non-owner participation
// roles. The assigner and primary assignee keep their implicit access.
func (s *Store) ReplaceTaskParticipants(ctx context.Context, taskID, addedBy int64, in []TaskParticipantInput) ([]TaskParticipant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = $1 FOR UPDATE`, taskID))
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeParticipantInputs(task, in)
	if err != nil {
		return nil, err
	}
	if err := validateTaskParticipantUsersTx(ctx, tx, task, normalized); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(normalized))
	for _, item := range normalized {
		ids = append(ids, item.UserID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM task_participants WHERE task_id = $1 AND NOT (user_id = ANY($2::bigint[]))`, taskID, ids); err != nil {
		return nil, err
	}
	if _, err := upsertTaskParticipantsTx(ctx, tx, task, normalized, addedBy); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.TaskParticipants(ctx, taskID)
}

func (s *Store) TaskParticipants(ctx context.Context, taskID int64) ([]TaskParticipant, error) {
	byTask, err := s.TaskParticipantsForTasks(ctx, []int64{taskID})
	if err != nil {
		return nil, err
	}
	return byTask[taskID], nil
}

func (s *Store) TaskParticipantsForTasks(ctx context.Context, taskIDs []int64) (map[int64][]TaskParticipant, error) {
	out := make(map[int64][]TaskParticipant, len(taskIDs))
	if len(taskIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT p.task_id, p.user_id, u.name, p.role, p.added_by
		 FROM task_participants p JOIN users u ON u.id = p.user_id
		 WHERE p.task_id = ANY($1)
		 ORDER BY p.task_id, CASE p.role WHEN 'collaborator' THEN 0 WHEN 'reviewer' THEN 1 ELSE 2 END, p.user_id`,
		taskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p TaskParticipant
		if err := rows.Scan(&p.TaskID, &p.UserID, &p.UserName, &p.Role, &p.AddedBy); err != nil {
			return nil, err
		}
		out[p.TaskID] = append(out[p.TaskID], p)
	}
	return out, rows.Err()
}

func (s *Store) TaskAccessForUser(ctx context.Context, task *Task, userID int64, superadmin bool) (TaskAccess, error) {
	if task == nil || userID <= 0 {
		return TaskAccess{}, nil
	}
	if superadmin {
		return TaskAccess{CanView: true, CanContribute: true, CanReview: true, CanManage: true}, nil
	}
	access := TaskAccess{
		CanView:       task.AssigneeID == userID || task.AssignerID == userID,
		CanContribute: task.AssigneeID == userID,
		CanReview:     task.AssignerID == userID,
		CanManage:     task.AssignerID == userID,
	}
	if access.CanContribute && access.CanReview {
		return access, nil
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT role FROM task_participants WHERE task_id = $1 AND user_id = $2), '')`,
		task.ID, userID).Scan(&access.Role); err != nil {
		return TaskAccess{}, err
	}
	switch access.Role {
	case TaskParticipantCollaborator:
		access.CanView = true
		access.CanContribute = true
	case TaskParticipantReviewer:
		access.CanView = true
		access.CanReview = true
	case TaskParticipantWatcher:
		access.CanView = true
	}
	return access, nil
}

func (s *Store) TaskParticipantIDs(ctx context.Context, taskID int64, roles ...string) ([]int64, error) {
	roleSet := make(map[string]bool, len(roles))
	for _, role := range roles {
		if validTaskParticipantRole(role) {
			roleSet[role] = true
		}
	}
	participants, err := s.TaskParticipants(ctx, taskID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(participants))
	for _, p := range participants {
		if len(roleSet) == 0 || roleSet[p.Role] {
			ids = append(ids, p.UserID)
		}
	}
	return ids, nil
}

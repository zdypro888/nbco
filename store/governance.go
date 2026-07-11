package store

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type WorkerCapability struct {
	WorkerID     int64
	Engine       string
	CLIName      string
	CLIVersion   string
	OS           string
	Arch         string
	Hostname     string
	Workdir      string
	Capabilities []string
	Metadata     json.RawMessage
	UpdatedAt    time.Time
}

type WorkerCapabilityInput struct {
	WorkerID     int64
	Engine       string
	CLIName      string
	CLIVersion   string
	OS           string
	Arch         string
	Hostname     string
	Workdir      string
	Capabilities []string
	Metadata     json.RawMessage
}

const workerCapabilityCols = `worker_id, engine, cli_name, cli_version, os, arch, hostname, workdir, capabilities, metadata, updated_at`

func scanWorkerCapability(row interface{ Scan(...any) error }) (*WorkerCapability, error) {
	var c WorkerCapability
	if err := row.Scan(&c.WorkerID, &c.Engine, &c.CLIName, &c.CLIVersion, &c.OS, &c.Arch, &c.Hostname, &c.Workdir, &c.Capabilities, &c.Metadata, &c.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &c, nil
}

func (s *Store) UpsertWorkerCapability(ctx context.Context, in WorkerCapabilityInput) (*WorkerCapability, error) {
	if in.OS == "" {
		in.OS = runtime.GOOS
	}
	if in.Arch == "" {
		in.Arch = runtime.GOARCH
	}
	if len(in.Metadata) == 0 {
		in.Metadata = json.RawMessage(`{}`)
	}
	in.Capabilities = normalizeStringList(in.Capabilities)
	return scanWorkerCapability(s.pool.QueryRow(ctx,
		`INSERT INTO worker_capabilities
		   (worker_id, engine, cli_name, cli_version, os, arch, hostname, workdir, capabilities, metadata)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 ON CONFLICT (worker_id) DO UPDATE SET
		   engine = EXCLUDED.engine,
		   cli_name = EXCLUDED.cli_name,
		   cli_version = EXCLUDED.cli_version,
		   os = EXCLUDED.os,
		   arch = EXCLUDED.arch,
		   hostname = EXCLUDED.hostname,
		   workdir = EXCLUDED.workdir,
		   capabilities = EXCLUDED.capabilities,
		   metadata = EXCLUDED.metadata,
		   updated_at = now()
		 RETURNING `+workerCapabilityCols,
		in.WorkerID, in.Engine, in.CLIName, in.CLIVersion, in.OS, in.Arch, in.Hostname, in.Workdir, in.Capabilities, in.Metadata))
}

func (s *Store) WorkerCapabilityByID(ctx context.Context, workerID int64) (*WorkerCapability, error) {
	return scanWorkerCapability(s.pool.QueryRow(ctx,
		`SELECT `+workerCapabilityCols+` FROM worker_capabilities WHERE worker_id = $1`, workerID))
}

func (s *Store) WorkerCapabilities(ctx context.Context, workerIDs []int64) (map[int64]*WorkerCapability, error) {
	out := map[int64]*WorkerCapability{}
	if len(workerIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+workerCapabilityCols+` FROM worker_capabilities WHERE worker_id = ANY($1)`, workerIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanWorkerCapability(rows)
		if err != nil {
			return nil, err
		}
		out[c.WorkerID] = c
	}
	return out, rows.Err()
}

type KnowledgeVersion struct {
	ID          int64
	KnowledgeID int64
	Version     int
	Title       string
	Content     string
	Tags        []string
	Kind        string
	Pinned      bool
	ChangedBy   *int64
	ChangeNote  string
	CreatedAt   time.Time
}

const knowledgeVersionCols = `id, knowledge_id, version, title, content, tags, kind, pinned, changed_by, change_note, created_at`

func scanKnowledgeVersion(row interface{ Scan(...any) error }) (*KnowledgeVersion, error) {
	var v KnowledgeVersion
	if err := row.Scan(&v.ID, &v.KnowledgeID, &v.Version, &v.Title, &v.Content, &v.Tags, &v.Kind, &v.Pinned, &v.ChangedBy, &v.ChangeNote, &v.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &v, nil
}

func snapshotKnowledgeRow(ctx context.Context, q pgx.Tx, id int64, changedBy *int64, note string) error {
	k, err := scanKnowledge(q.QueryRow(ctx, `SELECT `+knowledgeCols+` FROM knowledge WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	var next int
	if err := q.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM knowledge_versions WHERE knowledge_id = $1`, id).Scan(&next); err != nil {
		return err
	}
	_, err = q.Exec(ctx,
		`INSERT INTO knowledge_versions (knowledge_id, version, title, content, tags, kind, pinned, changed_by, change_note)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (knowledge_id, version) DO NOTHING`,
		id, next, k.Title, k.Content, k.Tags, k.Kind, k.Pinned, changedBy, note)
	return err
}

func (s *Store) KnowledgeVersions(ctx context.Context, knowledgeID int64, limit int) ([]*KnowledgeVersion, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+knowledgeVersionCols+` FROM knowledge_versions WHERE knowledge_id = $1 ORDER BY version DESC LIMIT $2`,
		knowledgeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*KnowledgeVersion
	for rows.Next() {
		v, err := scanKnowledgeVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) RollbackKnowledge(ctx context.Context, knowledgeID int64, version int, changedBy int64) (*Knowledge, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	v, err := scanKnowledgeVersion(tx.QueryRow(ctx,
		`SELECT `+knowledgeVersionCols+` FROM knowledge_versions WHERE knowledge_id = $1 AND version = $2`,
		knowledgeID, version))
	if err != nil {
		return nil, err
	}
	by := changedBy
	if err := snapshotKnowledgeRow(ctx, tx, knowledgeID, &by, fmt.Sprintf("rollback to version %d", version)); err != nil {
		return nil, err
	}
	k, err := updateKnowledgeRow(ctx, tx, knowledgeID, &v.Title, &v.Content, v.Tags)
	if err != nil {
		return nil, err
	}
	return k, tx.Commit(ctx)
}

type MaterialEntity struct {
	ID                int64
	FileID            *int64
	EntityType        string
	Name              string
	Content           string
	Evidence          json.RawMessage
	Confidence        float32
	SourceCandidateID *int64
	CreatedBy         *int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const materialEntityCols = `id, file_id, entity_type, name, content, evidence, confidence, source_candidate_id, created_by, created_at, updated_at`

func scanMaterialEntity(row interface{ Scan(...any) error }) (*MaterialEntity, error) {
	var e MaterialEntity
	if err := row.Scan(&e.ID, &e.FileID, &e.EntityType, &e.Name, &e.Content, &e.Evidence, &e.Confidence, &e.SourceCandidateID, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &e, nil
}

func (s *Store) CreateMaterialEntity(ctx context.Context, e MaterialEntity) (*MaterialEntity, error) {
	if len(e.Evidence) == 0 {
		e.Evidence = json.RawMessage(`{}`)
	}
	return scanMaterialEntity(s.pool.QueryRow(ctx,
		`INSERT INTO material_entities (file_id, entity_type, name, content, evidence, confidence, source_candidate_id, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 RETURNING `+materialEntityCols,
		e.FileID, e.EntityType, e.Name, e.Content, e.Evidence, e.Confidence, e.SourceCandidateID, e.CreatedBy))
}

func (s *Store) ListMaterialEntities(ctx context.Context, entityType string, limit int) ([]*MaterialEntity, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	sql := `SELECT ` + materialEntityCols + ` FROM material_entities WHERE TRUE`
	args := []any{}
	if strings.TrimSpace(entityType) != "" {
		args = append(args, strings.TrimSpace(entityType))
		sql += ` AND entity_type = $1`
	}
	args = append(args, limit)
	sql += ` ORDER BY id DESC LIMIT $` + fmt.Sprint(len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MaterialEntity
	for rows.Next() {
		e, err := scanMaterialEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type DecisionItem struct {
	ID        int64
	OwnerID   int64
	Kind      string
	Title     string
	Detail    string
	RefType   string
	RefID     *int64
	Priority  string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const decisionItemCols = `id, owner_id, kind, title, detail, ref_type, ref_id, priority, status, created_at, updated_at`

func scanDecisionItem(row interface{ Scan(...any) error }) (*DecisionItem, error) {
	var d DecisionItem
	if err := row.Scan(&d.ID, &d.OwnerID, &d.Kind, &d.Title, &d.Detail, &d.RefType, &d.RefID, &d.Priority, &d.Status, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &d, nil
}

// UpsertDecisionItem 插入或刷新决策项。冲突键 (owner_id, kind, ref_type, ref_id)：
// 已 closed 的项不会被重开（status 仅在仍是 open 时保持 open）。
// 注意：ref_id 为 NULL 时唯一约束不生效（Postgres NULL≠NULL），会插重复行——
// 所有调用方目前都传非 nil ref_id；新增调用方若无需 ref，应自行保证幂等或改用非空哨兵值。
func (s *Store) UpsertDecisionItem(ctx context.Context, d DecisionItem) (*DecisionItem, error) {
	if d.Priority == "" {
		d.Priority = "normal"
	}
	if d.Status == "" {
		d.Status = "open"
	}
	return scanDecisionItem(s.pool.QueryRow(ctx,
		`INSERT INTO decision_items (owner_id, kind, title, detail, ref_type, ref_id, priority, status)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (owner_id, kind, ref_type, ref_id) DO UPDATE SET
		   title = EXCLUDED.title,
		   detail = EXCLUDED.detail,
		   priority = EXCLUDED.priority,
		   status = CASE WHEN decision_items.status = 'open' THEN 'open' ELSE decision_items.status END,
		   updated_at = now()
		 RETURNING `+decisionItemCols,
		d.OwnerID, d.Kind, d.Title, d.Detail, d.RefType, d.RefID, d.Priority, d.Status))
}

func (s *Store) ListDecisionItems(ctx context.Context, ownerID int64, status string, limit int) ([]*DecisionItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if status == "" {
		status = "open"
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+decisionItemCols+` FROM decision_items
		  WHERE owner_id = $1 AND status = $2
		  ORDER BY (priority = 'high') DESC, id DESC LIMIT $3`, ownerID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DecisionItem
	for rows.Next() {
		d, err := scanDecisionItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) BuildDecisionQueue(ctx context.Context, ownerID int64) (int, error) {
	count := 0
	reviews, err := s.TasksAwaitingReview(ctx, ownerID)
	if err != nil {
		return count, err
	}
	for _, t := range reviews {
		id := t.ID
		if _, err := s.UpsertDecisionItem(ctx, DecisionItem{
			OwnerID: ownerID, Kind: "review", Title: "验收任务：" + t.Title,
			Detail: "任务已提交待验收，需要通过或打回。", RefType: "task", RefID: &id, Priority: "high",
		}); err != nil {
			return count, err
		}
		count++
	}
	overdue, err := s.OverdueTasks(ctx, 30)
	if err != nil {
		return count, err
	}
	for _, t := range overdue {
		if t.AssignerID != ownerID {
			continue
		}
		id := t.ID
		if _, err := s.UpsertDecisionItem(ctx, DecisionItem{
			OwnerID: ownerID, Kind: "overdue_task", Title: "处理过期任务：" + t.Title,
			Detail: "任务已过截止时间，建议调整期限、改派或要求汇报。", RefType: "task", RefID: &id, Priority: "high",
		}); err != nil {
			return count, err
		}
		count++
	}
	// 孤儿任务：执行人已停用（吊销/宕机），任务无人接手。提醒分配者改派。
	orphaned, err := s.OrphanedTasks(ctx)
	if err != nil {
		return count, err
	}
	for _, t := range orphaned {
		if t.AssignerID != ownerID {
			continue
		}
		id := t.ID
		if _, err := s.UpsertDecisionItem(ctx, DecisionItem{
			OwnerID: ownerID, Kind: "orphaned_task", Title: "改派孤儿任务：" + t.Title,
			Detail:  "执行人已停用，任务无人接手。建议用 reassign_task 改派给在线的 AI 员工。",
			RefType: "task", RefID: &id, Priority: "high",
		}); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// OrphanedTasks 查「执行人已停用」的开放任务（pending/in_progress）——吊销/宕机的 worker
// 名下无人接手的任务。按 assigner 聚合返回，供调度/决策队列提醒分配者改派。
func (s *Store) OrphanedTasks(ctx context.Context) ([]*Task, error) {
	return s.queryTasks(ctx,
		`SELECT `+taskColsWithAlias("t")+` FROM tasks t
		 JOIN users u ON u.id = t.assignee_id
		 WHERE t.status IN ('pending', 'in_progress', 'awaiting_input') AND u.status <> 'active'
		 ORDER BY t.id`)
}

// CloseDecisionItem 关闭决策项（status='closed'）。改派/验收完成后用，避免已处理项留在队列。
func (s *Store) CloseDecisionItem(ctx context.Context, id, ownerID int64) error {
	return s.execOne(ctx,
		`UPDATE decision_items SET status = 'closed', updated_at = now()
		 WHERE id = $1 AND owner_id = $2 AND status = 'open'`, id, ownerID)
}

// CloseDecisionsByRef 关闭某 owner 名下指向指定 ref 的全部 open 决策项。
// 改派成功后用（ref_type='task', ref_id=任务ID）——把 orphaned_task/overdue_task 等
// 同指该任务的项一并关掉。返回关闭行数。
func (s *Store) CloseDecisionsByRef(ctx context.Context, ownerID int64, refType string, refID int64) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE decision_items SET status = 'closed', updated_at = now()
		 WHERE owner_id = $1 AND ref_type = $2 AND ref_id = $3 AND status = 'open'`,
		ownerID, refType, refID)
	if err != nil {
		return 0, wrapErr(err)
	}
	return tag.RowsAffected(), nil
}

// CloseDecisionsByKindRef 只关指定 kind 的决策项（改派用：只关 orphaned_task，保留仍有效的 overdue_task）。
func (s *Store) CloseDecisionsByKindRef(ctx context.Context, ownerID int64, kind, refType string, refID int64) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE decision_items SET status = 'closed', updated_at = now()
		 WHERE owner_id = $1 AND kind = $2 AND ref_type = $3 AND ref_id = $4 AND status = 'open'`,
		ownerID, kind, refType, refID)
	if err != nil {
		return 0, wrapErr(err)
	}
	return tag.RowsAffected(), nil
}

type OrgGroup struct {
	ID          int64
	Name        string
	Description string
	ManagerID   *int64
	CreatedBy   *int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const orgGroupCols = `id, name, description, manager_id, created_by, created_at, updated_at`

func scanOrgGroup(row interface{ Scan(...any) error }) (*OrgGroup, error) {
	var g OrgGroup
	if err := row.Scan(&g.ID, &g.Name, &g.Description, &g.ManagerID, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &g, nil
}

func (s *Store) CreateOrgGroup(ctx context.Context, name, desc string, managerID, createdBy *int64) (*OrgGroup, error) {
	return scanOrgGroup(s.pool.QueryRow(ctx,
		`INSERT INTO org_groups (name, description, manager_id, created_by)
		 VALUES ($1,$2,$3,$4) RETURNING `+orgGroupCols, name, desc, managerID, createdBy))
}

func (s *Store) ListOrgGroups(ctx context.Context, limit int) ([]*OrgGroup, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+orgGroupCols+` FROM org_groups ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*OrgGroup
	for rows.Next() {
		g, err := scanOrgGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) AddOrgGroupMember(ctx context.Context, groupID, userID int64, role string) error {
	if role == "" {
		role = "member"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO org_group_members (group_id, user_id, role) VALUES ($1,$2,$3)
		 ON CONFLICT (group_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		groupID, userID, role)
	return err
}

func (s *Store) BindTelegramGroupProject(ctx context.Context, chatID, projectID, boundBy int64, note string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO telegram_group_projects (chat_id, project_id, bound_by, note)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (chat_id) DO UPDATE SET project_id = EXCLUDED.project_id, bound_by = EXCLUDED.bound_by, note = EXCLUDED.note, updated_at = now()`,
		chatID, projectID, boundBy, note)
	return err
}

func (s *Store) TelegramGroupProject(ctx context.Context, chatID int64) (*Project, error) {
	var projectID int64
	if err := s.pool.QueryRow(ctx, `SELECT project_id FROM telegram_group_projects WHERE chat_id = $1 AND project_id IS NOT NULL`, chatID).Scan(&projectID); err != nil {
		return nil, wrapErr(err)
	}
	return s.ProjectByID(ctx, projectID)
}

type ConversationEvalCase struct {
	ID         int64
	Name       string
	Channel    string
	UserInput  string
	Assertions json.RawMessage
	Enabled    bool
	CreatedBy  *int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const conversationEvalCaseCols = `id, name, channel, user_input, assertions, enabled, created_by, created_at, updated_at`

func scanConversationEvalCase(row interface{ Scan(...any) error }) (*ConversationEvalCase, error) {
	var c ConversationEvalCase
	if err := row.Scan(&c.ID, &c.Name, &c.Channel, &c.UserInput, &c.Assertions, &c.Enabled, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &c, nil
}

func (s *Store) CreateConversationEvalCase(ctx context.Context, c ConversationEvalCase) (*ConversationEvalCase, error) {
	if c.Channel == "" {
		c.Channel = "telegram"
	}
	if len(c.Assertions) == 0 {
		c.Assertions = json.RawMessage(`{}`)
	}
	return scanConversationEvalCase(s.pool.QueryRow(ctx,
		`INSERT INTO conversation_eval_cases (name, channel, user_input, assertions, enabled, created_by)
		 VALUES ($1,$2,$3,$4,$5,$6)
		 RETURNING `+conversationEvalCaseCols,
		c.Name, c.Channel, c.UserInput, c.Assertions, c.Enabled, c.CreatedBy))
}

func (s *Store) ListConversationEvalCases(ctx context.Context, enabledOnly bool, limit int) ([]*ConversationEvalCase, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	sql := `SELECT ` + conversationEvalCaseCols + ` FROM conversation_eval_cases`
	args := []any{}
	if enabledOnly {
		sql += ` WHERE enabled`
	}
	args = append(args, limit)
	sql += ` ORDER BY id DESC LIMIT $` + fmt.Sprint(len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ConversationEvalCase
	for rows.Next() {
		c, err := scanConversationEvalCase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func normalizeStringList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(strings.ToLower(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

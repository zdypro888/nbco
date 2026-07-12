package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DataSource describes one curated, permission-aware read model exposed to AI.
// It is intentionally not a database table catalog: credentials, raw storage
// paths, model secrets, and migration state never enter this surface.
type DataSource struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Fields         []string `json:"fields"`
	SuperadminOnly bool     `json:"superadmin_only,omitempty"`
}

type DataReadQuery struct {
	Source  string
	Terms   []string
	Filters map[string]string
	// EntityIDs 是语义索引返回的稳定 ID 候选。它只用于内部回读；权限条件
	// 仍在 source query 内先执行，不能借此读取不可见记录。
	EntityIDs []string
	Limit     int
	Offset    int
}

type dataSourceDef struct {
	DataSource
	query          string
	order          string
	semanticID     string
	semanticFields []string
}

var dataSourceOrder = []string{
	"users", "identities", "profiles", "permissions", "workers",
	"projects", "tasks", "files", "schedules", "knowledge",
	"goals", "milestones", "campaigns", "decisions", "action_turns",
	"telegram_groups", "material_entities", "audit_activity",
}

var dataSourceDefs = map[string]dataSourceDef{
	"users": {
		DataSource: DataSource{
			Name: "users", Description: "成员目录；所有人可见稳定用户ID和名称，动态 info 仅本人、超管或持有 view_self_intro 授权者可见。",
			Fields: []string{"user_id", "name", "status", "is_superadmin", "is_worker", "owner_id", "worker_last_seen", "info", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'user_id', u.id, 'name', u.name, 'status', u.status,
			'is_superadmin', u.is_superadmin, 'is_worker', u.is_worker,
			'owner_id', CASE WHEN $2 OR u.id = $1 OR u.owner_id = $1 THEN u.owner_id END,
			'worker_last_seen', CASE WHEN $2 OR u.id = $1 OR u.owner_id = $1 THEN u.worker_last_seen END,
			'info', CASE WHEN $2 OR u.id = $1 OR EXISTS (
				SELECT 1 FROM permissions p
				 WHERE p.kind = 'active' AND p.user_id = $1 AND p.action = 'view_self_intro'
				   AND p.target IN ('_all', u.id::text)
			) THEN u.info ELSE '{}'::jsonb END,
			'created_at', u.created_at
		) AS item, u.created_at AS sort_at, u.id AS sort_id FROM users u`,
		// info 具有字段级可见性，不能进入全局向量；否则普通用户可能通过
		// 语义命中顺序推断隐藏手机号等字段与某个公开姓名的关联。
		semanticID: "user_id", semanticFields: []string{"name", "status"},
	},
	"identities": {
		DataSource: DataSource{
			Name: "identities", Description: "IM身份与稳定用户ID绑定；普通用户只能看自己的绑定，超管可看全部。",
			Fields: []string{"provider", "external_id", "user_id", "chat_ref"},
		},
		query: `SELECT jsonb_build_object(
			'provider', i.provider, 'external_id', i.external_id,
			'user_id', i.user_id, 'chat_ref', i.chat_ref
		) AS item, NULL::timestamptz AS sort_at, i.user_id AS sort_id
		FROM identities i WHERE $2 OR i.user_id = $1`,
	},
	"profiles": {
		DataSource: DataSource{
			Name: "profiles", Description: "成员画像条目；按双向画像权限逐行裁剪。",
			Fields: []string{"profile_id", "subject_id", "author_id", "position", "content", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'profile_id', p.id, 'subject_id', p.subject_id, 'author_id', p.author_id,
			'position', p.position, 'content', p.content, 'updated_at', p.updated_at
		) AS item, p.updated_at AS sort_at, p.id AS sort_id
		FROM profiles p
		WHERE $2 OR p.author_id = $1
		   OR (p.author_id = p.subject_id AND EXISTS (
			 SELECT 1 FROM permissions g
			  WHERE g.kind = 'active' AND g.user_id = $1 AND g.action = 'view_self_intro'
			    AND g.target IN ('_all', p.subject_id::text)
		   ))
		   OR EXISTS (
			 SELECT 1 FROM permissions g
			  WHERE g.kind = 'passive' AND g.user_id = p.subject_id
			    AND g.action IN ('view_profile:_all', 'view_profile:' || p.author_id::text)
			    AND g.target IN ('_all', $1::text)
		   )`,
		semanticID: "profile_id", semanticFields: []string{"position", "content"},
	},
	"permissions": {
		DataSource: DataSource{
			Name: "permissions", Description: "权限授权记录；普通用户只看自己的权限或自己授出的权限，超管可看全部。",
			Fields: []string{"permission_id", "kind", "user_id", "action", "target", "granted_by", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'permission_id', p.id, 'kind', p.kind, 'user_id', p.user_id,
			'action', p.action, 'target', p.target, 'granted_by', p.granted_by,
			'created_at', p.created_at
		) AS item, p.created_at AS sort_at, p.id AS sort_id
		FROM permissions p WHERE $2 OR p.user_id = $1 OR p.granted_by = $1`,
	},
	"workers": {
		DataSource: DataSource{
			Name: "workers", Description: "AI Worker状态；普通用户只看自己的worker，超管可看全部。",
			Fields: []string{"worker_id", "name", "status", "owner_id", "is_superadmin", "last_seen", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'worker_id', u.id, 'name', u.name, 'status', u.status,
			'owner_id', u.owner_id, 'is_superadmin', u.is_superadmin,
			'last_seen', u.worker_last_seen, 'created_at', u.created_at
		) AS item, COALESCE(u.worker_last_seen, u.created_at) AS sort_at, u.id AS sort_id
		FROM users u WHERE u.is_worker AND ($2 OR u.owner_id = $1 OR u.id = $1)`,
		semanticID: "worker_id", semanticFields: []string{"name", "status"},
	},
	"projects": {
		DataSource: DataSource{
			Name: "projects", Description: "项目；普通用户只看自己创建或参与任务的项目，超管可看全部。",
			Fields: []string{"project_id", "name", "description", "creator_id", "status", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'project_id', p.id, 'name', p.name, 'description', p.description,
			'creator_id', p.creator_id, 'status', p.status, 'created_at', p.created_at
			) AS item, p.created_at AS sort_at, p.id AS sort_id
			FROM projects p WHERE $2 OR p.creator_id = $1 OR EXISTS (
				SELECT 1 FROM tasks t WHERE t.project_id = p.id
				 AND (t.assigner_id = $1 OR t.assignee_id = $1 OR EXISTS (
				      SELECT 1 FROM task_participants tp
				       WHERE tp.task_id = t.id AND tp.user_id = $1
				 ))
			)`,
		semanticID: "project_id", semanticFields: []string{"name", "description", "status"},
	},
	"tasks": {
		DataSource: DataSource{
			Name: "tasks", Description: "任务全字段读模型；普通用户只看自己分配或执行的任务，超管可看全部。",
			Fields: []string{"task_id", "project_id", "parent_id", "assigner_id", "assignee_id", "title", "goal", "description", "acceptance", "priority", "deadline", "status", "depends_on", "milestone_id", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'task_id', t.id, 'project_id', t.project_id, 'parent_id', t.parent_id,
			'assigner_id', t.assigner_id, 'assignee_id', t.assignee_id,
			'title', t.title, 'goal', t.goal, 'description', t.description,
			'acceptance', t.acceptance, 'priority', t.priority, 'deadline', t.deadline,
			'status', t.status, 'depends_on', t.depends_on, 'milestone_id', t.milestone_id,
			'created_at', t.created_at, 'updated_at', t.updated_at
		) AS item, t.updated_at AS sort_at, t.id AS sort_id
			FROM tasks t WHERE $2 OR t.assigner_id = $1 OR t.assignee_id = $1
			 OR EXISTS (SELECT 1 FROM task_participants tp
			             WHERE tp.task_id = t.id AND tp.user_id = $1)`,
		semanticID: "task_id", semanticFields: []string{"title", "goal", "description", "acceptance", "priority", "status"},
	},
	"files": {
		DataSource: DataSource{
			Name: "files", Description: "文件元数据；不暴露物理路径或哈希，普通用户只看自己上传或自己任务引用的文件。",
			Fields: []string{"file_id", "source", "original_name", "mime_type", "size_bytes", "created_by", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'file_id', f.id, 'source', f.source, 'original_name', f.original_name,
			'mime_type', f.mime_type, 'size_bytes', f.size_bytes,
			'created_by', f.created_by, 'created_at', f.created_at
		) AS item, f.created_at AS sort_at, f.id AS sort_id
			FROM files f WHERE $2 OR f.created_by = $1
			 OR EXISTS (SELECT 1 FROM task_attachments a JOIN tasks t ON t.id = a.task_id
			             WHERE a.file_id = f.id AND (
			               t.assigner_id = $1 OR t.assignee_id = $1 OR EXISTS (
			                 SELECT 1 FROM task_participants tp
			                  WHERE tp.task_id = t.id AND tp.user_id = $1
			               )))
			 OR EXISTS (SELECT 1 FROM task_artifacts a JOIN tasks t ON t.id = a.task_id
			             WHERE a.file_id = f.id AND (
			               t.assigner_id = $1 OR t.assignee_id = $1 OR EXISTS (
			                 SELECT 1 FROM task_participants tp
			                  WHERE tp.task_id = t.id AND tp.user_id = $1
			               )))`,
		semanticID: "file_id", semanticFields: []string{"original_name", "mime_type", "source"},
	},
	"schedules": {
		DataSource: DataSource{
			Name: "schedules", Description: "定时任务和自动化；普通用户只看发给自己或自己创建的条目。",
			Fields: []string{"schedule_id", "title", "kind", "message", "fire_at", "interval_s", "status", "last_fired", "target", "mode", "daily_at", "weekdays", "created_by", "source_kind", "source_key", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'schedule_id', s.id, 'title', s.title, 'kind', s.kind, 'message', s.message,
			'fire_at', s.fire_at, 'interval_s', s.interval_s, 'status', s.status,
			'last_fired', s.last_fired, 'target', s.target, 'mode', s.mode,
			'daily_at', s.daily_at, 'weekdays', s.weekdays, 'created_by', s.created_by,
			'source_kind', s.source_kind, 'source_key', s.source_key, 'created_at', s.created_at
		) AS item, s.fire_at AS sort_at, s.id AS sort_id
		FROM schedules s WHERE $2 OR s.user_id = $1 OR s.created_by = $1`,
		order: "ASC", semanticID: "schedule_id",
		semanticFields: []string{"title", "kind", "message", "status", "target", "mode", "source_kind"},
	},
	"knowledge": {
		DataSource: DataSource{
			Name: "knowledge", Description: "共享事实和skill；策略规则仅超管可全量读取。",
			Fields: []string{"knowledge_id", "title", "content", "tags", "author_id", "kind", "pinned", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'knowledge_id', k.id, 'title', k.title, 'content', k.content,
			'tags', k.tags, 'author_id', k.author_id, 'kind', k.kind,
			'pinned', k.pinned, 'created_at', k.created_at, 'updated_at', k.updated_at
		) AS item, k.updated_at AS sort_at, k.id AS sort_id
		FROM knowledge k WHERE $2 OR k.kind <> 'policy'`,
		semanticID: "knowledge_id",
	},
	"goals": {
		DataSource: DataSource{
			Name: "goals", Description: "战略目标；普通用户只看自己负责或有参与任务归因的目标。",
			Fields: []string{"goal_id", "title", "description", "owner_id", "deadline", "status", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'goal_id', g.id, 'title', g.title, 'description', g.description,
			'owner_id', g.owner_id, 'deadline', g.deadline, 'status', g.status,
			'created_at', g.created_at, 'updated_at', g.updated_at
		) AS item, g.updated_at AS sort_at, g.id AS sort_id
		FROM goals g WHERE $2 OR g.owner_id = $1 OR EXISTS (
			SELECT 1 FROM milestones m JOIN tasks t ON t.milestone_id = m.id
			 WHERE m.goal_id = g.id AND (t.assigner_id = $1 OR t.assignee_id = $1)
		)`,
		semanticID: "goal_id", semanticFields: []string{"title", "description", "status"},
	},
	"milestones": {
		DataSource: DataSource{
			Name: "milestones", Description: "战略里程碑；继承所属目标的可见范围。",
			Fields: []string{"milestone_id", "goal_id", "title", "description", "deadline", "status", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'milestone_id', m.id, 'goal_id', m.goal_id, 'title', m.title,
			'description', m.description, 'deadline', m.deadline, 'status', m.status,
			'created_at', m.created_at, 'updated_at', m.updated_at
		) AS item, m.updated_at AS sort_at, m.id AS sort_id
		FROM milestones m JOIN goals g ON g.id = m.goal_id
		WHERE $2 OR g.owner_id = $1 OR EXISTS (
			SELECT 1 FROM tasks t WHERE t.milestone_id = m.id
			 AND (t.assigner_id = $1 OR t.assignee_id = $1)
		)`,
		semanticID: "milestone_id", semanticFields: []string{"title", "description", "status"},
	},
	"campaigns": {
		DataSource: DataSource{
			Name: "campaigns", Description: "资料收集活动及完成统计；普通用户只看自己创建或自己作为目标的活动。",
			Fields: []string{"campaign_id", "title", "instruction", "required_fields", "status", "created_by", "total", "completed", "pending", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'campaign_id', c.id, 'title', c.title, 'instruction', c.instruction,
			'required_fields', c.required_fields, 'status', c.status, 'created_by', c.created_by,
			'total', (SELECT count(*) FROM data_collection_campaign_targets x WHERE x.campaign_id = c.id),
			'completed', (SELECT count(*) FROM data_collection_campaign_targets x WHERE x.campaign_id = c.id AND x.status = 'completed'),
			'pending', (SELECT count(*) FROM data_collection_campaign_targets x WHERE x.campaign_id = c.id AND x.status = 'pending'),
			'created_at', c.created_at, 'updated_at', c.updated_at
		) AS item, c.updated_at AS sort_at, c.id AS sort_id
		FROM data_collection_campaigns c WHERE $2 OR c.created_by = $1 OR EXISTS (
			SELECT 1 FROM data_collection_campaign_targets x
			 WHERE x.campaign_id = c.id AND x.user_id = $1
		)`,
		semanticID: "campaign_id", semanticFields: []string{"title", "instruction", "required_fields", "status"},
	},
	"decisions": {
		DataSource: DataSource{
			Name: "decisions", Description: "决策队列；普通用户只看自己的队列，超管可看全部。",
			Fields: []string{"decision_id", "owner_id", "kind", "title", "detail", "ref_type", "ref_id", "priority", "status", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'decision_id', d.id, 'owner_id', d.owner_id, 'kind', d.kind,
			'title', d.title, 'detail', d.detail, 'ref_type', d.ref_type,
			'ref_id', d.ref_id, 'priority', d.priority, 'status', d.status,
			'created_at', d.created_at, 'updated_at', d.updated_at
		) AS item, d.updated_at AS sort_at, d.id AS sort_id
		FROM decision_items d WHERE $2 OR d.owner_id = $1`,
		semanticID: "decision_id", semanticFields: []string{"kind", "title", "detail", "ref_type", "status"},
	},
	"action_turns": {
		DataSource: DataSource{
			Name: "action_turns", Description: "AI动作轮次与工具证据；普通用户只看自己的轮次，超管可看全部。",
			Fields: []string{"turn_id", "user_id", "session_id", "channel", "user_text", "reply", "requires_action", "intent", "expected_tools", "evidence", "outcome", "tool_count", "success_tool_count", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'turn_id', a.id, 'user_id', a.user_id, 'session_id', a.session_id,
			'channel', a.channel, 'user_text', a.user_text_excerpt, 'reply', a.reply_excerpt,
			'requires_action', a.requires_action, 'intent', a.intent,
			'expected_tools', a.expected_tools, 'evidence', a.evidence,
			'outcome', a.outcome, 'tool_count', a.tool_count,
			'success_tool_count', a.success_tool_count, 'created_at', a.created_at
		) AS item, a.created_at AS sort_at, a.id AS sort_id
		FROM action_turns a WHERE $2 OR a.user_id = $1`,
		semanticID:     "turn_id",
		semanticFields: []string{"user_text", "reply", "intent", "expected_tools", "evidence", "outcome"},
	},
	"telegram_groups": {
		DataSource: DataSource{
			Name: "telegram_groups", Description: "Telegram群接入状态；仅超管可读。",
			Fields:         []string{"chat_id", "title", "type", "status", "listen", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT k.value::jsonb AS item,
			COALESCE((k.value::jsonb ->> 'updated_at')::timestamptz, now()) AS sort_at,
			COALESCE((k.value::jsonb ->> 'chat_id')::bigint, 0) AS sort_id
		FROM kv_state k WHERE $2 AND k.key LIKE 'telegram.group:%' AND k.value <> ''`,
		semanticID: "chat_id", semanticFields: []string{"title", "type", "status"},
	},
	"material_entities": {
		DataSource: DataSource{
			Name: "material_entities", Description: "从资料中提取的结构化实体和证据；仅超管可读。",
			Fields:         []string{"entity_id", "file_id", "entity_type", "name", "content", "evidence", "confidence", "created_by", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'entity_id', e.id, 'file_id', e.file_id, 'entity_type', e.entity_type,
			'name', e.name, 'content', e.content, 'evidence', e.evidence,
			'confidence', e.confidence, 'created_by', e.created_by,
			'created_at', e.created_at, 'updated_at', e.updated_at
		) AS item, e.updated_at AS sort_at, e.id AS sort_id
		FROM material_entities e WHERE $2`,
		semanticID: "entity_id", semanticFields: []string{"entity_type", "name", "content", "evidence"},
	},
	"audit_activity": {
		DataSource: DataSource{
			Name: "audit_activity", Description: "全局工具调用事实流水；仅超管可读，凭据已在写入时脱敏。",
			Fields:         []string{"audit_id", "user_id", "session_id", "tool", "args", "result", "ok", "created_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'audit_id', a.id, 'user_id', a.user_id, 'session_id', a.session_id,
			'tool', a.tool, 'args', a.args, 'result', a.result,
			'ok', a.ok, 'created_at', a.created_at
		) AS item, a.created_at AS sort_at, a.id AS sort_id
		FROM audit_log a WHERE $2`,
		semanticID: "audit_id", semanticFields: []string{"tool", "args", "result", "ok"},
	},
}

func DataSources(isSuperadmin bool) []DataSource {
	out := make([]DataSource, 0, len(dataSourceOrder))
	for _, name := range dataSourceOrder {
		def := dataSourceDefs[name]
		if def.SuperadminOnly && !isSuperadmin {
			continue
		}
		out = append(out, def.DataSource)
	}
	return out
}

// SemanticDocument is a curated, non-secret representation used only to build
// the external semantic index. Source rows remain authoritative in PostgreSQL.
type SemanticDocument struct {
	Source   string
	EntityID string
	Content  string
}

// SemanticDataSources lists text-bearing curated read models. Exact-only data
// such as identity bindings and permission edges deliberately stays in SQL.
func SemanticDataSources() []string {
	out := make([]string, 0, len(dataSourceOrder))
	for _, name := range dataSourceOrder {
		if def := dataSourceDefs[name]; def.semanticID != "" && len(def.semanticFields) > 0 {
			out = append(out, name)
		}
	}
	return out
}

// DataSourceIDField returns the stable ID field for semantic round-tripping.
func DataSourceIDField(source string) (string, bool) {
	def, ok := dataSourceDefs[strings.TrimSpace(source)]
	return def.semanticID, ok && def.semanticID != ""
}

// DataRowEntityID extracts the stable entity ID without decoding JSON numbers
// through float64 (Telegram chat IDs and future IDs may exceed 53 bits).
func DataRowEntityID(source string, row json.RawMessage) (string, bool) {
	field, ok := DataSourceIDField(source)
	if !ok {
		return "", false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(row, &object); err != nil {
		return "", false
	}
	return semanticJSONScalar(object[field])
}

// SemanticDocuments returns one stable page from a superadmin-visible curated
// read model. The exported content contains only fields already allowed by the
// AI read catalog; credentials, storage paths, hashes, and migration state are
// never part of these definitions.
func (s *Store) SemanticDocuments(ctx context.Context, source string, offset, limit int) ([]SemanticDocument, error) {
	name := strings.TrimSpace(source)
	def, ok := dataSourceDefs[name]
	if !ok || def.semanticID == "" || len(def.semanticFields) == 0 {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(`WITH visible AS (%s)
		 SELECT item FROM visible ORDER BY sort_id ASC, sort_at ASC NULLS LAST LIMIT $3 OFFSET $4`, def.query),
		int64(0), true, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SemanticDocument, 0, limit)
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, ok := DataRowEntityID(name, raw)
		if !ok || id == "" {
			continue
		}
		content, err := semanticDocumentText(raw, def.semanticFields)
		if err != nil {
			return nil, err
		}
		out = append(out, SemanticDocument{Source: name, EntityID: id, Content: content})
	}
	return out, rows.Err()
}

func semanticDocumentText(raw json.RawMessage, fields []string) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, field := range fields {
		value := object[field]
		if len(value) == 0 || string(value) == "null" || string(value) == `""` || string(value) == "{}" || string(value) == "[]" {
			continue
		}
		text := strings.TrimSpace(string(value))
		var decoded string
		if err := json.Unmarshal(value, &decoded); err == nil {
			text = strings.TrimSpace(decoded)
		}
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", field, text)
	}
	return strings.TrimSpace(b.String()), nil
}

func semanticJSONScalar(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		return text, text != ""
	}
	text = strings.TrimSpace(string(raw))
	if text == "" || strings.ContainsAny(text, "{}[]") {
		return "", false
	}
	return text, true
}

// ReadData executes a structured query over one curated read model. Visibility
// is embedded in each source query before search and filters are applied, so a
// filter cannot turn into a side channel for hidden rows or fields.
func (s *Store) ReadData(ctx context.Context, userID int64, isSuperadmin bool, q DataReadQuery) ([]json.RawMessage, error) {
	name := strings.TrimSpace(q.Source)
	def, ok := dataSourceDefs[name]
	if !ok || (def.SuperadminOnly && !isSuperadmin) {
		return nil, ErrNotFound
	}
	q.Terms = cleanWorkspaceFilterValues(q.Terms, nil, 8)
	q.EntityIDs = cleanWorkspaceFilterValues(q.EntityIDs, nil, 500)
	if len(q.EntityIDs) > 0 && def.semanticID == "" {
		return nil, fmt.Errorf("数据源 %s 不支持稳定ID候选回读", name)
	}
	maxLimit := 100
	if len(q.EntityIDs) > 0 {
		maxLimit = 500
	}
	if q.Limit <= 0 {
		q.Limit = 30
	} else if q.Limit > maxLimit {
		q.Limit = maxLimit
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	if q.Offset > 10_000 {
		q.Offset = 10_000
	}
	fieldSet := make(map[string]bool, len(def.Fields))
	for _, field := range def.Fields {
		fieldSet[field] = true
	}
	if len(q.Filters) > 20 {
		return nil, fmt.Errorf("单次最多使用 20 个精确过滤字段")
	}
	filterKeys := make([]string, 0, len(q.Filters))
	for key := range q.Filters {
		if !fieldSet[key] {
			return nil, fmt.Errorf("数据源 %s 不支持字段 %s；可用字段：%s", name, key, strings.Join(def.Fields, ", "))
		}
		filterKeys = append(filterKeys, key)
	}
	sort.Strings(filterKeys)

	args := []any{userID, isSuperadmin, q.Terms}
	var sql strings.Builder
	fmt.Fprintf(&sql, "WITH caller AS (SELECT $1::bigint AS user_id, $2::boolean AS is_superadmin), visible AS (%s) SELECT item FROM visible WHERE ", def.query)
	sql.WriteString(`(cardinality($3::text[]) = 0 OR EXISTS (
		 SELECT 1 FROM unnest($3::text[]) AS q(term)
		 WHERE strpos(lower(visible.item::text), lower(q.term)) > 0
	))`)
	if len(q.EntityIDs) > 0 {
		args = append(args, q.EntityIDs)
		fmt.Fprintf(&sql, " AND visible.item ->> '%s' = ANY($%d::text[])", def.semanticID, len(args))
	}
	for _, key := range filterKeys {
		value := q.Filters[key]
		if runes := []rune(value); len(runes) > 1000 {
			value = string(runes[:1000])
		}
		args = append(args, value)
		fmt.Fprintf(&sql, " AND visible.item ->> '%s' = $%d", key, len(args))
	}
	order := strings.ToUpper(strings.TrimSpace(def.order))
	if order != "ASC" {
		order = "DESC"
	}
	args = append(args, q.Limit, q.Offset)
	fmt.Fprintf(&sql, " ORDER BY sort_at %s NULLS LAST, sort_id DESC LIMIT $%d OFFSET $%d", order, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, sql.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]json.RawMessage, 0, q.Limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, append(json.RawMessage(nil), raw...))
	}
	return out, rows.Err()
}

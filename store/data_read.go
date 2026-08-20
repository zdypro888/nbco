package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
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
	"users", "identities", "profiles", "permissions", "workers", "worker_sessions", "worker_capabilities",
	"roles", "org_groups", "projects", "tasks", "task_updates",
	"files", "file_intakes", "file_chunks", "schedules", "deliveries", "notification_deliveries", "external_action_receipts",
	"domain_outbox_events", "telegram_inbound_updates", "telegram_delivery_parts", "worker_llm_calls", "knowledge", "learning_candidates",
	"goals", "milestones", "campaigns", "decisions", "events", "conversation_turns", "action_turns", "chat_messages",
	"telegram_groups", "material_entities", "script_tools", "eval_cases", "audit_activity",
}

var dataSourceDefs = map[string]dataSourceDef{
	"users": {
		DataSource: DataSource{
			Name: "users", Description: "成员目录；所有人可见稳定用户ID和名称，动态 info 仅本人、超管或持有 view_self_intro 授权者可见。",
			Fields: []string{"user_id", "name", "status", "is_superadmin", "is_worker", "owner_id", "worker_last_seen", "info", "created_at"},
		},
		query: `WITH RECURSIVE ` + effectiveActiveGrantsCTE + `
		SELECT jsonb_build_object(
			'user_id', u.id, 'name', u.name, 'status', u.status,
			'is_superadmin', u.is_superadmin, 'is_worker', u.is_worker,
			'owner_id', CASE WHEN $2 OR u.id = $1 OR u.owner_id = $1 THEN u.owner_id END,
			'worker_last_seen', CASE WHEN $2 OR u.id = $1 OR u.owner_id = $1 THEN u.worker_last_seen END,
			'info', CASE WHEN $2 OR u.id = $1 OR EXISTS (
				SELECT 1 FROM effective_active p
				 WHERE p.user_id = $1 AND p.action = 'view_self_intro'
				   AND p.target IN ('_all', u.id::text)
			) THEN u.info ELSE '{}'::jsonb END,
			'created_at', u.created_at
		) AS item, u.updated_at AS sort_at, u.id AS sort_id FROM users u`,
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
		query: `WITH RECURSIVE ` + effectiveActiveGrantsCTE + `
		SELECT jsonb_build_object(
			'profile_id', p.id, 'subject_id', p.subject_id, 'author_id', p.author_id,
			'position', p.position, 'content', p.content, 'updated_at', p.updated_at
		) AS item, p.updated_at AS sort_at, p.id AS sort_id
		FROM profiles p
		WHERE $2 OR p.author_id = $1
		   OR (p.author_id = p.subject_id AND EXISTS (
			 SELECT 1 FROM effective_active g
			  WHERE g.user_id = $1 AND g.action = 'view_self_intro'
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
			Name: "permissions", Description: "当前有效权限委托；普通用户只看自己的权限或自己授出的权限，超管可看全部。",
			Fields: []string{"permission_id", "kind", "user_id", "action", "target", "granted_by", "created_at"},
		},
		query: `WITH RECURSIVE ` + effectiveActiveGrantsCTE + `
		SELECT jsonb_build_object(
			'permission_id', p.id, 'kind', p.kind, 'user_id', p.user_id,
			'action', p.action, 'target', p.target, 'granted_by', p.granted_by,
			'created_at', p.created_at
		) AS item, p.created_at AS sort_at, p.id AS sort_id
		FROM permissions p
		WHERE (p.kind = 'passive' OR EXISTS (SELECT 1 FROM effective_active e WHERE e.id = p.id))
		  AND ($2 OR p.user_id = $1 OR p.granted_by = $1)`,
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
		) AS item, u.updated_at AS sort_at, u.id AS sort_id
		FROM users u WHERE u.is_worker AND ($2 OR u.owner_id = $1 OR u.id = $1)`,
		semanticID: "worker_id", semanticFields: []string{"name", "status"},
	},
	"worker_sessions": {
		DataSource: DataSource{
			Name: "worker_sessions", Description: "Worker 持久工作主题与摘要；普通用户只看自己或自己名下 Worker。",
			Fields: []string{"worker_session_id", "worker_id", "worker_name", "engine", "scope_type", "scope_key", "title", "summary", "last_task_id", "use_count", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'worker_session_id', ws.id, 'worker_id', ws.worker_id, 'worker_name', w.name,
			'engine', ws.engine, 'scope_type', ws.scope_type, 'scope_key', ws.scope_key,
			'title', ws.title, 'summary', ws.summary, 'last_task_id', ws.last_task_id,
			'use_count', ws.use_count, 'updated_at', ws.updated_at
		) AS item, GREATEST(ws.updated_at, w.updated_at) AS sort_at, ws.id AS sort_id
		FROM worker_sessions ws JOIN users w ON w.id = ws.worker_id
		WHERE $2 OR ws.worker_id = $1 OR w.owner_id = $1`,
		semanticID: "worker_session_id", semanticFields: []string{"worker_name", "engine", "scope_type", "scope_key", "title", "summary"},
	},
	"worker_capabilities": {
		DataSource: DataSource{
			Name: "worker_capabilities", Description: "Worker 上报的执行环境和能力；省略主机路径与自由元数据。",
			Fields: []string{"worker_id", "worker_name", "engine", "cli_name", "cli_version", "os", "arch", "capabilities", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'worker_id', c.worker_id, 'worker_name', w.name, 'engine', c.engine,
			'cli_name', c.cli_name, 'cli_version', c.cli_version,
			'os', c.os, 'arch', c.arch, 'capabilities', c.capabilities,
			'updated_at', c.updated_at
		) AS item, GREATEST(c.updated_at, w.updated_at) AS sort_at, c.worker_id AS sort_id
		FROM worker_capabilities c JOIN users w ON w.id = c.worker_id
		WHERE $2 OR c.worker_id = $1 OR w.owner_id = $1`,
		semanticID: "worker_id", semanticFields: []string{"worker_name", "engine", "cli_name", "cli_version", "os", "arch", "capabilities"},
	},
	"roles": {
		DataSource: DataSource{
			Name: "roles", Description: "可激活的岗位角色与工作方法。",
			Fields: []string{"role_id", "name", "trigger_desc", "prompt", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'role_id', r.id, 'name', r.name, 'trigger_desc', r.trigger_desc,
			'prompt', r.prompt, 'created_at', r.created_at, 'updated_at', r.updated_at
		) AS item, r.updated_at AS sort_at, r.id AS sort_id FROM roles r`,
		semanticID: "role_id", semanticFields: []string{"name", "trigger_desc", "prompt"},
	},
	"org_groups": {
		DataSource: DataSource{
			Name: "org_groups", Description: "组织分组及成员角色；普通用户只看自己参与、管理或创建的组。",
			Fields: []string{"group_id", "name", "description", "manager_id", "created_by", "members", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'group_id', g.id, 'name', g.name, 'description', g.description,
			'manager_id', g.manager_id, 'created_by', g.created_by,
			'members', COALESCE((
				SELECT jsonb_agg(jsonb_build_object('user_id', m.user_id, 'name', u.name, 'role', m.role) ORDER BY u.name, m.user_id)
				  FROM org_group_members m JOIN users u ON u.id = m.user_id WHERE m.group_id = g.id
			), '[]'::jsonb),
			'created_at', g.created_at, 'updated_at', g.updated_at
		) AS item, g.updated_at AS sort_at, g.id AS sort_id
		FROM org_groups g
		WHERE $2 OR g.manager_id = $1 OR g.created_by = $1
		 OR EXISTS (SELECT 1 FROM org_group_members m WHERE m.group_id = g.id AND m.user_id = $1)`,
		semanticID: "group_id", semanticFields: []string{"name", "description", "members"},
	},
	"projects": {
		DataSource: DataSource{
			Name: "projects", Description: "项目；普通用户只看自己创建或参与任务的项目，超管可看全部。",
			Fields: []string{"project_id", "name", "description", "creator_id", "status", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'project_id', p.id, 'name', p.name, 'description', p.description,
			'creator_id', p.creator_id, 'status', p.status,
			'created_at', p.created_at, 'updated_at', p.updated_at
			) AS item, p.updated_at AS sort_at, p.id AS sort_id
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
	"task_updates": {
		DataSource: DataSource{
			Name: "task_updates", Description: "任务清单、过程汇报、附件说明、产物说明和验收结果的统一事实流。",
			Fields: []string{"update_id", "task_id", "kind", "author_id", "content", "done", "outcome", "task_kind", "reason", "file_id", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'update_id', 'progress:' || p.id::text, 'task_id', p.task_id,
			'kind', 'progress', 'author_id', p.author_id, 'content', p.content,
			'created_at', p.created_at
		) AS item, p.created_at AS sort_at, p.id * 10 + 1 AS sort_id
		FROM task_progress p JOIN tasks t ON t.id = p.task_id
		WHERE $2 OR t.assigner_id = $1 OR t.assignee_id = $1
		 OR EXISTS (SELECT 1 FROM task_participants tp WHERE tp.task_id = t.id AND tp.user_id = $1)
		UNION ALL
		SELECT jsonb_build_object(
			'update_id', 'checklist:' || c.id::text, 'task_id', c.task_id,
			'kind', 'checklist', 'content', c.item, 'done', c.done,
			'created_at', c.updated_at
		), c.updated_at, c.id * 10 + 2
		FROM task_checklist c JOIN tasks t ON t.id = c.task_id
		WHERE $2 OR t.assigner_id = $1 OR t.assignee_id = $1
		 OR EXISTS (SELECT 1 FROM task_participants tp WHERE tp.task_id = t.id AND tp.user_id = $1)
		UNION ALL
		SELECT jsonb_build_object(
			'update_id', 'outcome:' || o.id::text, 'task_id', o.task_id,
			'kind', 'outcome', 'author_id', o.reviewer_id, 'outcome', o.outcome,
			'task_kind', o.task_kind, 'reason', o.reason, 'created_at', o.created_at
		), o.created_at, o.id * 10 + 3
		FROM task_outcomes o JOIN tasks t ON t.id = o.task_id
		WHERE $2 OR t.assigner_id = $1 OR t.assignee_id = $1
		 OR EXISTS (SELECT 1 FROM task_participants tp WHERE tp.task_id = t.id AND tp.user_id = $1)
		UNION ALL
		SELECT jsonb_build_object(
			'update_id', 'attachment:' || a.id::text, 'task_id', a.task_id,
			'kind', 'attachment', 'content', a.caption, 'file_id', a.file_id,
			'created_at', a.created_at
		), a.created_at, a.id * 10 + 4
		FROM task_attachments a JOIN tasks t ON t.id = a.task_id
		WHERE $2 OR t.assigner_id = $1 OR t.assignee_id = $1
		 OR EXISTS (SELECT 1 FROM task_participants tp WHERE tp.task_id = t.id AND tp.user_id = $1)
		UNION ALL
		SELECT jsonb_build_object(
			'update_id', 'artifact:' || a.id::text, 'task_id', a.task_id,
			'kind', 'artifact', 'author_id', a.created_by, 'content', a.caption,
			'file_id', a.file_id, 'created_at', a.created_at
		), a.created_at, a.id * 10 + 5
		FROM task_artifacts a JOIN tasks t ON t.id = a.task_id
		WHERE $2 OR t.assigner_id = $1 OR t.assignee_id = $1
		 OR EXISTS (SELECT 1 FROM task_participants tp WHERE tp.task_id = t.id AND tp.user_id = $1)`,
		semanticID: "update_id", semanticFields: []string{"kind", "content", "done", "outcome", "task_kind", "reason"},
	},
	"files": {
		DataSource: DataSource{
			Name: "files", Description: "文件元数据；不暴露物理路径或哈希，普通用户只看自己上传或自己任务引用的文件。",
			Fields: []string{"file_id", "source", "original_name", "mime_type", "size_bytes", "created_by", "content_index_status", "content_chunk_count", "content_index_truncated", "vector_index_status", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'file_id', f.id, 'source', f.source, 'original_name', f.original_name,
			'mime_type', f.mime_type, 'size_bytes', f.size_bytes,
			'created_by', f.created_by,
			'content_index_status', COALESCE(fi.status, 'pending'),
			'content_chunk_count', COALESCE(fi.chunk_count, 0),
			'content_index_truncated', COALESCE(fi.truncated, FALSE),
			'vector_index_status', COALESCE(fi.vector_status, 'pending'),
			'created_at', f.created_at
		) AS item, f.created_at AS sort_at, f.id AS sort_id
			FROM files f LEFT JOIN file_content_indexes fi ON fi.file_id = f.id
			WHERE $2 OR f.created_by = $1
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
	"file_intakes": {
		DataSource: DataSource{
			Name: "file_intakes", Description: "每次文件接收尝试及真实结果；普通用户只看自己的接收记录。",
			Fields: []string{"intake_id", "user_id", "source", "original_name", "mime_type", "size_bytes", "status", "error_code", "error_message", "file_id", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'intake_id', i.id, 'user_id', i.user_id, 'source', i.source,
			'original_name', i.original_name, 'mime_type', i.mime_type,
			'size_bytes', i.size_bytes, 'status', i.status,
			'error_code', i.error_code, 'error_message', i.error_message,
			'file_id', i.file_id, 'created_at', i.created_at, 'updated_at', i.updated_at
		) AS item, i.updated_at AS sort_at, i.id AS sort_id
		FROM file_intakes i WHERE $2 OR i.user_id = $1`,
		semanticID: "intake_id", semanticFields: []string{"source", "original_name", "mime_type", "status", "error_code", "error_message"},
	},
	"file_chunks": {
		DataSource: DataSource{
			Name: "file_chunks", Description: "文件正文的可检索分块；命中后保留文件ID和分块位置，权限继承原文件。",
			Fields: []string{"chunk_id", "file_id", "original_name", "mime_type", "chunk_index", "content", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'chunk_id', c.id, 'file_id', f.id, 'original_name', f.original_name,
			'mime_type', f.mime_type, 'chunk_index', c.chunk_index,
			'content', c.content, 'created_at', c.created_at
		) AS item, c.created_at AS sort_at, c.id AS sort_id
		FROM file_text_chunks c JOIN files f ON f.id = c.file_id
		WHERE $2 OR f.created_by = $1
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
		// File chunks have a durable extraction/vector queue that owns their
		// payload and retry state. Keep the stable ID for semantic round-trips,
		// but never let the generic reconciler overwrite those points.
		semanticID: "chunk_id",
	},
	"schedules": {
		DataSource: DataSource{
			Name: "schedules", Description: "定时任务和自动化；普通用户只看发给自己或自己创建的条目。",
			Fields: []string{"schedule_id", "title", "kind", "message", "fire_at", "interval_s", "status", "last_fired", "target", "targets_viewer", "delivery_enabled", "recipient_policy", "mode", "daily_at", "weekdays", "created_by", "source_kind", "source_key", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
				'schedule_id', s.id, 'title', s.title, 'kind', s.kind, 'message', s.message,
				'fire_at', s.fire_at, 'interval_s', s.interval_s, 'status', s.status,
				'last_fired', s.last_fired, 'target', s.target,
				'targets_viewer', (s.user_id = $1 OR s.target = $1::text OR
				  (s.target = '_all' AND (s.status = 'active' OR EXISTS (
				    SELECT 1 FROM schedule_deliveries td WHERE td.schedule_id = s.id AND td.user_id = $1
				  )))),
				'delivery_enabled', CASE WHEN s.recipient_policy = 'mandatory' THEN true ELSE coalesce(
				  (SELECT p.enabled FROM schedule_delivery_preferences p WHERE p.user_id = $1 AND p.schedule_id = s.id),
				  (SELECT p.enabled FROM schedule_delivery_preferences p WHERE p.user_id = $1 AND p.schedule_id = 0),
				  true) END,
				'recipient_policy', s.recipient_policy,
				'mode', s.mode,
				'daily_at', s.daily_at, 'weekdays', s.weekdays, 'created_by', s.created_by,
				'source_kind', s.source_kind, 'source_key', s.source_key,
				'created_at', s.created_at, 'updated_at', s.updated_at
			) AS item, s.updated_at AS sort_at, s.id AS sort_id
			FROM schedules s
			WHERE $2 OR s.user_id = $1 OR s.created_by = $1 OR s.target = $1::text OR
			  (s.target = '_all' AND (s.status = 'active' OR EXISTS (
			    SELECT 1 FROM schedule_deliveries vd WHERE vd.schedule_id = s.id AND vd.user_id = $1
			  )))`,
		order: "ASC", semanticID: "schedule_id",
		semanticFields: []string{"title", "kind", "message", "status", "target", "mode", "source_kind"},
	},
	"deliveries": {
		DataSource: DataSource{
			Name: "deliveries", Description: "定时消息的逐人投递结果；普通用户看发给自己或自己创建的投递。",
			Fields: []string{"delivery_id", "schedule_id", "user_id", "mode", "title", "message", "result_text", "status", "attempts", "last_error", "occurrence_at", "delivered_at", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'delivery_id', d.id, 'schedule_id', d.schedule_id, 'user_id', d.user_id,
			'mode', d.mode, 'title', d.title, 'message', d.message,
			'result_text', d.result_text, 'status', d.status, 'attempts', d.attempts,
			'last_error', d.last_error, 'occurrence_at', d.occurrence_at,
			'delivered_at', d.delivered_at, 'created_at', d.created_at, 'updated_at', d.updated_at
		) AS item, d.updated_at AS sort_at, d.id AS sort_id
		FROM schedule_deliveries d JOIN schedules s ON s.id = d.schedule_id
		WHERE $2 OR d.user_id = $1 OR s.created_by = $1`,
		semanticID: "delivery_id", semanticFields: []string{"mode", "title", "message", "result_text", "status", "last_error"},
	},
	"notification_deliveries": {
		DataSource: DataSource{
			Name: "notification_deliveries", Description: "外部通知的逐次幂等投递边界；仅超级管理员可读，用于核对已确认、失败和结果不确定的消息。",
			Fields:         []string{"delivery_key", "user_id", "status", "attempts", "started_at", "delivered_at", "last_error", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'delivery_key', d.delivery_key, 'user_id', d.user_id,
			'status', d.status, 'attempts', d.attempts,
			'started_at', d.started_at, 'delivered_at', d.delivered_at,
			'last_error', d.last_error, 'created_at', d.created_at, 'updated_at', d.updated_at
		) AS item, d.updated_at AS sort_at,
		  hashtextextended(d.delivery_key, 0) AS sort_id
			FROM notification_deliveries d WHERE $2`,
	},
	"external_action_receipts": {
		DataSource: DataSource{
			Name: "external_action_receipts", Description: "外部渠道直达动作的幂等执行凭据；仅超级管理员可读，用于核对命令是否已领取、完成或中断。",
			Fields:         []string{"action_key", "kind", "status", "last_error", "started_at", "completed_at", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'action_key', r.action_key, 'kind', r.kind, 'status', r.status,
			'last_error', r.last_error, 'started_at', r.started_at,
			'completed_at', r.completed_at, 'created_at', r.created_at, 'updated_at', r.updated_at
		) AS item, r.updated_at AS sort_at,
		  hashtextextended(r.action_key, 0) AS sort_id
		FROM external_action_receipts r WHERE $2`,
	},
	"domain_outbox_events": {
		DataSource: DataSource{
			Name: "domain_outbox_events", Description: "领域状态转换产生的可靠副作用队列；仅超级管理员可读，用于核对事实是否已进入外部投递链路。",
			Fields:         []string{"event_id", "occurrence_key", "topic", "status", "attempts", "available_at", "claimed_at", "claim_owner", "completed_at", "last_error", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'event_id', e.id, 'occurrence_key', e.occurrence_key, 'topic', e.topic,
			'status', e.status, 'attempts', e.attempts,
			'available_at', e.available_at, 'claimed_at', e.claimed_at,
			'claim_owner', e.claim_owner, 'completed_at', e.completed_at,
			'last_error', e.last_error, 'created_at', e.created_at, 'updated_at', e.updated_at
		) AS item, e.updated_at AS sort_at, e.id AS sort_id
		FROM domain_outbox_events e WHERE $2`,
	},
	"telegram_inbound_updates": {
		DataSource: DataSource{
			Name: "telegram_inbound_updates", Description: "Telegram 入站持久队列的处理状态；仅超级管理员可读，不暴露消息正文。",
			Fields:         []string{"update_id", "status", "attempts", "available_at", "claimed_at", "claim_owner", "processed_at", "last_error", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'update_id', u.update_id, 'status', u.status, 'attempts', u.attempts,
			'available_at', u.available_at, 'claimed_at', u.claimed_at,
			'claim_owner', u.claim_owner, 'processed_at', u.processed_at,
			'last_error', u.last_error, 'created_at', u.created_at, 'updated_at', u.updated_at
		) AS item, u.updated_at AS sort_at, u.update_id AS sort_id
		FROM telegram_inbound_updates u WHERE $2`,
	},
	"telegram_delivery_parts": {
		DataSource: DataSource{
			Name: "telegram_delivery_parts", Description: "Telegram 长消息逐分片投递回执；仅超级管理员可读，用于定位部分送达和不确定结果。",
			Fields:         []string{"delivery_key", "part_index", "part_count", "chat_id", "status", "telegram_message_id", "delivered_at", "last_error", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'delivery_key', p.delivery_key, 'part_index', p.part_index,
			'part_count', p.part_count, 'chat_id', p.chat_id, 'status', p.status,
			'telegram_message_id', p.telegram_message_id, 'delivered_at', p.delivered_at,
			'last_error', p.last_error, 'created_at', p.created_at, 'updated_at', p.updated_at
		) AS item, p.updated_at AS sort_at,
		  hashtextextended(p.delivery_key || ':' || p.part_index::text, 0) AS sort_id
		FROM telegram_delivery_parts p WHERE $2`,
	},
	"worker_llm_calls": {
		DataSource: DataSource{
			Name: "worker_llm_calls", Description: "Worker 内置智能体的模型请求回执；仅超级管理员可读，不暴露请求或响应正文。",
			Fields:         []string{"worker_id", "request_id", "status", "http_status", "last_error", "started_at", "completed_at", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'worker_id', c.worker_id, 'request_id', c.request_id, 'status', c.status,
			'http_status', c.http_status, 'last_error', c.last_error,
			'started_at', c.started_at, 'completed_at', c.completed_at,
			'created_at', c.created_at, 'updated_at', c.updated_at
		) AS item, c.updated_at AS sort_at,
		  hashtextextended(c.worker_id::text || ':' || c.request_id, 0) AS sort_id
		FROM worker_llm_calls c WHERE $2`,
	},
	"knowledge": {
		DataSource: DataSource{
			Name: "knowledge", Description: "共享事实和skill；策略规则仅超管可全量读取。",
			Fields: []string{"knowledge_id", "title", "content", "tags", "author_id", "kind", "pinned", "active", "created_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'knowledge_id', k.id, 'title', k.title, 'content', k.content,
			'tags', k.tags, 'author_id', k.author_id, 'kind', k.kind,
			'pinned', k.pinned, 'active', k.active, 'created_at', k.created_at, 'updated_at', k.updated_at
		) AS item, k.updated_at AS sort_at, k.id AS sort_id
		FROM knowledge k WHERE $2 OR k.kind <> 'policy'`,
		semanticID: "knowledge_id",
	},
	"learning_candidates": {
		DataSource: DataSource{
			Name: "learning_candidates", Description: "待治理或已发布的学习候选；仅超级管理员可读。",
			Fields:         []string{"candidate_id", "kind", "scope", "title", "content", "tags", "evidence", "confidence", "status", "source_type", "source_ref", "value_score", "review_note", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'candidate_id', c.id, 'kind', c.kind, 'scope', c.scope,
			'title', c.title, 'content', c.content, 'tags', c.tags,
			'evidence', c.evidence, 'confidence', c.confidence, 'status', c.status,
			'source_type', c.source_type, 'source_ref', c.source_ref,
			'value_score', c.value_score, 'review_note', c.review_note,
			'created_at', c.created_at, 'updated_at', c.updated_at
		) AS item, c.updated_at AS sort_at, c.id AS sort_id
		FROM learning_candidates c WHERE $2`,
		semanticID: "candidate_id", semanticFields: []string{"kind", "scope", "title", "content", "tags", "evidence", "status", "source_type", "review_note"},
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
	"events": {
		DataSource: DataSource{
			Name: "events", Description: "领域事件及 AI 处理结果；普通用户只看由自己决策的事件。",
			Fields: []string{"event_id", "source_key", "kind", "decider_id", "detail", "notification_required", "status", "outcome", "reply", "delivery_mode", "attempts", "last_error", "created_at", "handled_at"},
		},
		query: `SELECT jsonb_build_object(
			'event_id', e.id, 'source_key', e.source_key, 'kind', e.kind, 'decider_id', e.decider_id,
			'detail', e.detail, 'notification_required', e.notification_required,
			'status', e.status, 'outcome', e.outcome,
			'reply', e.reply, 'delivery_mode', e.delivery_mode,
			'attempts', e.attempts, 'last_error', e.last_error,
			'created_at', e.created_at, 'handled_at', e.handled_at
		) AS item, COALESCE(e.handled_at, e.created_at) AS sort_at, e.id AS sort_id
		FROM events e WHERE $2 OR e.decider_id = $1`,
		semanticID: "event_id", semanticFields: []string{"kind", "detail", "status", "outcome", "reply", "delivery_mode", "last_error"},
	},
	"conversation_turns": {
		DataSource: DataSource{
			Name: "conversation_turns", Description: "交互 Agent 轮次的执行与渠道交付生命周期；普通用户只看自己的轮次，超管可看全部。",
			Fields: []string{"conversation_turn_id", "user_id", "session_id", "channel", "user_message_id", "assistant_message_id", "status", "delivery_status", "result", "attempts", "delivery_attempts", "last_error", "started_at", "completed_at", "delivered_at", "updated_at"},
		},
		query: `SELECT jsonb_build_object(
			'conversation_turn_id', t.id, 'user_id', t.user_id, 'session_id', t.session_id,
			'channel', t.channel, 'user_message_id', t.user_message_id,
			'assistant_message_id', t.assistant_message_id, 'status', t.status,
			'delivery_status', t.delivery_status, 'result', t.result_text,
			'attempts', t.attempts, 'delivery_attempts', t.delivery_attempts,
			'last_error', t.last_error, 'started_at', t.started_at,
			'completed_at', t.completed_at, 'delivered_at', t.delivered_at,
			'updated_at', t.updated_at
		) AS item, t.updated_at AS sort_at, t.id AS sort_id
		FROM conversation_turns t WHERE $2 OR t.user_id = $1`,
	},
	"action_turns": {
		DataSource: DataSource{
			Name: "action_turns", Description: "AI动作轮次与工具证据；普通用户只看自己的轮次，超管可看全部。",
			Fields: []string{"turn_id", "conversation_turn_id", "user_id", "session_id", "channel", "user_text", "reply", "requires_action", "intent", "expected_tools", "evidence", "outcome", "tool_count", "success_tool_count", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'turn_id', a.id, 'conversation_turn_id', a.conversation_turn_id,
			'user_id', a.user_id, 'session_id', a.session_id,
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
	"chat_messages": {
		DataSource: DataSource{
			Name: "chat_messages", Description: "逐条聊天事实；普通用户只读自己的私聊会话，超级管理员可跨会话检索。",
			Fields: []string{"chat_message_id", "session_id", "session_user_id", "channel", "role", "content", "context_eligible", "previous_role", "previous_content", "created_at"},
		},
		query: `SELECT jsonb_build_object(
			'chat_message_id', m.id, 'session_id', m.session_id,
			'session_user_id', cs.user_id, 'channel', cs.channel,
			'role', m.role, 'content', m.content, 'context_eligible', m.context_eligible,
			'previous_role', COALESCE(prev.role, ''),
			'previous_content', left(COALESCE(prev.content, ''), 1200),
			'created_at', m.created_at
		) AS item, m.created_at AS sort_at, m.id AS sort_id
		FROM chat_messages m JOIN chat_sessions cs ON cs.id = m.session_id
		LEFT JOIN LATERAL (
			SELECT p.role, p.content FROM chat_messages p
			 WHERE p.session_id = m.session_id AND p.id < m.id
			 ORDER BY p.id DESC LIMIT 1
		) prev ON TRUE
		WHERE cs.channel NOT LIKE 'internal:%'
		  AND ($2 OR (cs.user_id = $1 AND strpos(cs.channel, ':group:') = 0))`,
		// Chat messages use the immediate message-index pipeline so their
		// permission payload is preserved. A stable ID still lets query_data
		// reuse that index and re-read the authoritative row here.
		semanticID: "chat_message_id",
	},
	"telegram_groups": {
		DataSource: DataSource{
			Name: "telegram_groups", Description: "Telegram群接入状态；仅超管可读。",
			Fields:         []string{"chat_id", "title", "type", "status", "listen", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT k.value::jsonb AS item,
			COALESCE((k.value::jsonb ->> 'updated_at')::timestamptz, 'epoch'::timestamptz) AS sort_at,
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
	"script_tools": {
		DataSource: DataSource{
			Name: "script_tools", Description: "动态脚本工具的定义与测试状态；仅超级管理员可读，源码和测试哈希不进入向量。",
			Fields:         []string{"script_tool_id", "name", "description", "runtime", "input_schema", "enabled", "required_action", "last_test_result", "last_test_ok", "created_by", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'script_tool_id', s.id, 'name', s.name, 'description', s.description,
			'runtime', s.runtime, 'input_schema', s.input_schema,
			'enabled', s.enabled, 'required_action', s.required_action,
			'last_test_result', s.last_test_result, 'last_test_ok', s.last_test_ok,
			'created_by', s.created_by, 'created_at', s.created_at, 'updated_at', s.updated_at
		) AS item, s.updated_at AS sort_at, s.id AS sort_id
		FROM script_tools s WHERE $2`,
		semanticID: "script_tool_id", semanticFields: []string{"name", "description", "runtime", "input_schema", "enabled", "required_action", "last_test_result"},
	},
	"eval_cases": {
		DataSource: DataSource{
			Name: "eval_cases", Description: "对话回归用例与最近运行；仅超级管理员可读。",
			Fields:         []string{"eval_case_id", "name", "channel", "user_input", "assertions", "enabled", "created_by", "created_at", "updated_at"},
			SuperadminOnly: true,
		},
		query: `SELECT jsonb_build_object(
			'eval_case_id', c.id, 'name', c.name, 'channel', c.channel,
			'user_input', c.user_input, 'assertions', c.assertions,
			'enabled', c.enabled, 'created_by', c.created_by,
			'created_at', c.created_at, 'updated_at', c.updated_at
		) AS item, c.updated_at AS sort_at, c.id AS sort_id
		FROM conversation_eval_cases c WHERE $2`,
		semanticID: "eval_case_id", semanticFields: []string{"name", "channel", "user_input", "assertions", "enabled"},
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
	SortAt   time.Time
	SortID   int64
}

// SemanticCursor is a stable keyset position inside a curated read model.
// Using (sort_at, sort_id) avoids OFFSET's increasingly expensive rescans and
// remains deterministic when many rows share a timestamp.
type SemanticCursor struct {
	SortAt time.Time
	SortID int64
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
func (s *Store) SemanticDocuments(ctx context.Context, source string, after *SemanticCursor, changedSince *time.Time, limit int) ([]SemanticDocument, error) {
	name := strings.TrimSpace(source)
	def, ok := dataSourceDefs[name]
	if !ok || def.semanticID == "" || len(def.semanticFields) == 0 {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := []any{int64(0), true}
	var predicates []string
	if changedSince != nil && !changedSince.IsZero() {
		args = append(args, changedSince.UTC())
		predicates = append(predicates, fmt.Sprintf("COALESCE(sort_at, 'epoch'::timestamptz) >= $%d", len(args)))
	}
	if after != nil {
		args = append(args, after.SortAt.UTC(), after.SortID)
		predicates = append(predicates, fmt.Sprintf(
			"(COALESCE(sort_at, 'epoch'::timestamptz), sort_id) > ($%d::timestamptz, $%d::bigint)",
			len(args)-1, len(args)))
	}
	where := ""
	if len(predicates) > 0 {
		where = " WHERE " + strings.Join(predicates, " AND ")
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx,
		fmt.Sprintf(`WITH caller AS (SELECT $1::bigint AS user_id, $2::boolean AS is_superadmin), visible AS (%s)
			 SELECT item, COALESCE(sort_at, 'epoch'::timestamptz), sort_id FROM visible%s
			 ORDER BY COALESCE(sort_at, 'epoch'::timestamptz) ASC, sort_id ASC LIMIT $%d`, def.query, where, len(args)),
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SemanticDocument, 0, limit)
	for rows.Next() {
		var raw json.RawMessage
		var sortAt time.Time
		var sortID int64
		if err := rows.Scan(&raw, &sortAt, &sortID); err != nil {
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
		out = append(out, SemanticDocument{Source: name, EntityID: id, Content: content, SortAt: sortAt, SortID: sortID})
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

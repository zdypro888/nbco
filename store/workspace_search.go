package store

import (
	"context"
	"strings"
	"time"
)

// WorkspaceResource is a permission-filtered reference in the shared resource
// catalog. Kind is one of task, file, or project.
type WorkspaceResource struct {
	Kind      string
	ID        int64
	Name      string
	State     string
	CreatedAt time.Time
}

// WorkspaceCandidateFilter describes literal catalog retrieval planned by the
// AI layer. Terms are OR-ed literal substrings; Kinds is an optional allowlist.
// The store deliberately does not infer semantic similarity or object type.
type WorkspaceCandidateFilter struct {
	Terms []string
	Kinds []string
	Limit int
}

// WorkspaceCandidates returns resources the caller may access. The AI layer
// plans terms and kinds and resolves ambiguity; this method is only the
// permission-enforcing data plane.
func (s *Store) WorkspaceCandidates(ctx context.Context, userID int64, isSuperadmin bool, filter WorkspaceCandidateFilter) ([]WorkspaceResource, error) {
	filter.Terms = cleanWorkspaceFilterValues(filter.Terms, nil, 8)
	filter.Kinds = cleanWorkspaceFilterValues(filter.Kinds, map[string]bool{
		"task": true, "file": true, "project": true,
	}, 3)
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 30
	}
	rows, err := s.pool.Query(ctx, `
		WITH resources AS (
			SELECT 'task'::text AS kind, t.id, t.title AS name, t.status AS state, t.created_at
			  FROM tasks t
			 WHERE $2::boolean OR t.assigner_id = $1 OR t.assignee_id = $1
			    OR EXISTS (SELECT 1 FROM task_participants tp
			                WHERE tp.task_id = t.id AND tp.user_id = $1)
			UNION ALL
			SELECT 'file'::text, f.id, f.original_name, 'saved'::text, f.created_at
			  FROM files f
			 WHERE ($2::boolean OR f.created_by = $1
			        OR EXISTS (
			             SELECT 1 FROM task_attachments a JOIN tasks t ON t.id = a.task_id
			             WHERE a.file_id = f.id AND (
			               t.assigner_id = $1 OR t.assignee_id = $1 OR EXISTS (
			                 SELECT 1 FROM task_participants tp
			                  WHERE tp.task_id = t.id AND tp.user_id = $1
			               ))
			        ) OR EXISTS (
			             SELECT 1 FROM task_artifacts a JOIN tasks t ON t.id = a.task_id
			              WHERE a.file_id = f.id AND (
			                t.assigner_id = $1 OR t.assignee_id = $1 OR EXISTS (
			                  SELECT 1 FROM task_participants tp
			                   WHERE tp.task_id = t.id AND tp.user_id = $1
			                ))
			        ))
			UNION ALL
			SELECT 'project'::text, p.id, p.name, p.status, p.created_at
			  FROM projects p
			 WHERE ($2::boolean OR p.creator_id = $1 OR EXISTS (
			       SELECT 1 FROM tasks t WHERE t.project_id = p.id
			        AND (t.assigner_id = $1 OR t.assignee_id = $1 OR EXISTS (
			          SELECT 1 FROM task_participants tp
			           WHERE tp.task_id = t.id AND tp.user_id = $1
			        ))
			   ))
		)
		SELECT kind, id, name, state, created_at
		  FROM resources r
		 WHERE (cardinality($3::text[]) = 0 OR r.kind = ANY($3::text[]))
		   AND (cardinality($4::text[]) = 0 OR EXISTS (
		         SELECT 1 FROM unnest($4::text[]) AS q(term)
		          WHERE strpos(lower(r.name), lower(q.term)) > 0
		       ))
		 ORDER BY created_at DESC, kind, id DESC
		 LIMIT $5`, userID, isSuperadmin, filter.Kinds, filter.Terms, filter.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]WorkspaceResource, 0, filter.Limit)
	for rows.Next() {
		var item WorkspaceResource
		if err := rows.Scan(&item.Kind, &item.ID, &item.Name, &item.State, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func cleanWorkspaceFilterValues(values []string, allow map[string]bool, limit int) []string {
	if limit <= 0 {
		return nil
	}
	out := make([]string, 0, min(len(values), limit))
	seen := make(map[string]bool, min(len(values), limit))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsRune(value, '\x00') || (allow != nil && !allow[value]) {
			continue
		}
		if runes := []rune(value); len(runes) > 200 {
			value = string(runes[:200])
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) == limit {
			break
		}
	}
	return out
}

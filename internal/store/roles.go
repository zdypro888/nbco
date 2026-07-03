package store

import (
	"context"
	"time"
)

// Role 可激活的 AI 角色/Skill。
type Role struct {
	ID          int64
	Name        string
	TriggerDesc string
	Prompt      string
	CreatedAt   time.Time
}

// CreateRole 建角色。
func (s *Store) CreateRole(ctx context.Context, name, triggerDesc, prompt string) (*Role, error) {
	var r Role
	err := s.pool.QueryRow(ctx,
		`INSERT INTO roles (name, trigger_desc, prompt) VALUES ($1, $2, $3)
		 RETURNING id, name, trigger_desc, prompt, created_at`, name, triggerDesc, prompt).
		Scan(&r.ID, &r.Name, &r.TriggerDesc, &r.Prompt, &r.CreatedAt)
	return &r, wrapErr(err)
}

// UpdateRole 更新角色（空串字段不动）。
func (s *Store) UpdateRole(ctx context.Context, name, triggerDesc, prompt string) error {
	return s.execOne(ctx,
		`UPDATE roles SET
		   trigger_desc = CASE WHEN $2 = '' THEN trigger_desc ELSE $2 END,
		   prompt = CASE WHEN $3 = '' THEN prompt ELSE $3 END
		 WHERE name = $1`, name, triggerDesc, prompt)
}

// DeleteRole 删角色。
func (s *Store) DeleteRole(ctx context.Context, name string) error {
	return s.execOne(ctx, `DELETE FROM roles WHERE name = $1`, name)
}

// ListRoles 列出全部角色。
func (s *Store) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, trigger_desc, prompt, created_at FROM roles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var roles []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.TriggerDesc, &r.Prompt, &r.CreatedAt); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

// ActivateRole 激活角色（替换旧激活）。
func (s *Store) ActivateRole(ctx context.Context, userID int64, roleName string) error {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO user_active_roles (user_id, role_id)
		 SELECT $1, id FROM roles WHERE name = $2
		 ON CONFLICT (user_id) DO UPDATE SET role_id = EXCLUDED.role_id`, userID, roleName)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound // 角色名不存在
	}
	return nil
}

// DeactivateRole 取消激活。
func (s *Store) DeactivateRole(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_active_roles WHERE user_id = $1`, userID)
	return err
}

// ActiveRole 用户当前激活的角色；未激活返回 ErrNotFound。
func (s *Store) ActiveRole(ctx context.Context, userID int64) (*Role, error) {
	var r Role
	err := s.pool.QueryRow(ctx,
		`SELECT r.id, r.name, r.trigger_desc, r.prompt, r.created_at
		 FROM user_active_roles ar JOIN roles r ON r.id = ar.role_id
		 WHERE ar.user_id = $1`, userID).
		Scan(&r.ID, &r.Name, &r.TriggerDesc, &r.Prompt, &r.CreatedAt)
	return &r, wrapErr(err)
}

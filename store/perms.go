package store

import (
	"context"
	"time"
)

// Grant 一条权限授予记录。
//
// Kind=active：User 能对 Target 做 Action（主动权限）。
// Kind=passive：Target 能对 User 做 Action（被动权限，挂在被作用者身上）。
// Target 是用户 ID 的十进制文本，或 "_all"。
type Grant struct {
	ID        int64
	Kind      string
	UserID    int64
	Action    string
	Target    string
	GrantedBy int64
	CreatedAt time.Time
}

// 权限维度。
const (
	KindActive  = "active"
	KindPassive = "passive"
)

// TargetAll 通配目标。
const TargetAll = "_all"

// GrantPerm 授权（幂等：重复授予返回 ErrConflict）。
func (s *Store) GrantPerm(ctx context.Context, g Grant) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO permissions (kind, user_id, action, target, granted_by) VALUES ($1, $2, $3, $4, $5)`,
		g.Kind, g.UserID, g.Action, g.Target, g.GrantedBy)
	return wrapErr(err)
}

// RevokePerm 撤销。
func (s *Store) RevokePerm(ctx context.Context, kind string, userID int64, action, target string) error {
	return s.execOne(ctx,
		`DELETE FROM permissions WHERE kind = $1 AND user_id = $2 AND action = $3 AND target = $4`,
		kind, userID, action, target)
}

// PermsOf 取某用户的全部权限（两个维度）。
func (s *Store) PermsOf(ctx context.Context, userID int64) ([]Grant, error) {
	return s.queryGrants(ctx,
		`SELECT id, kind, user_id, action, target, granted_by, created_at
		 FROM permissions WHERE user_id = $1 ORDER BY id`, userID)
}

// PassivePermsToward 取「谁对 subject 有被动授权」之外，还需要 _all 通配：
// 返回挂在 subject 身上的全部被动授权记录。
func (s *Store) PassivePermsToward(ctx context.Context, subjectID int64) ([]Grant, error) {
	return s.queryGrants(ctx,
		`SELECT id, kind, user_id, action, target, granted_by, created_at
		 FROM permissions WHERE kind = 'passive' AND user_id = $1 ORDER BY id`, subjectID)
}

func (s *Store) queryGrants(ctx context.Context, sql string, args ...any) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.Kind, &g.UserID, &g.Action, &g.Target, &g.GrantedBy, &g.CreatedAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

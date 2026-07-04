package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// 用户状态。
const (
	UserActive   = "active"
	UserDisabled = "disabled"
)

// User 一名成员。Info 是动态字段值（字段定义见 info_fields）。
type User struct {
	ID           int64
	Name         string
	Info         map[string]string
	Status       string
	IsSuperadmin bool
	CreatedAt    time.Time
}

// Identity 一条 IM 身份绑定。ChatRef 是该渠道的通知投递地址。
type Identity struct {
	Provider   string
	ExternalID string
	UserID     int64
	ChatRef    string
}

const userCols = `id, name, info, status, is_superadmin, created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var info []byte
	if err := row.Scan(&u.ID, &u.Name, &info, &u.Status, &u.IsSuperadmin, &u.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	if err := json.Unmarshal(info, &u.Info); err != nil {
		return nil, err
	}
	return &u, nil
}

// HasSuperadmin 系统中是否已有活跃超管。
func (s *Store) HasSuperadmin(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE is_superadmin AND status = 'active')`).Scan(&exists)
	return exists, err
}

// bootstrapLockKey /superadmin 引导的事务咨询锁（防两人同时抢首任超管）。
const bootstrapLockKey = 7767001

// BootstrapSuperadmin 首任超管引导：系统尚无活跃超管时，把发起者建为超管并绑定身份；
// 已有超管返回 ErrConflict。
func (s *Store) BootstrapSuperadmin(ctx context.Context, name string, ident Identity) (*User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockNoSuperadmin(ctx, tx); err != nil {
		return nil, err
	}
	u, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (name, is_superadmin) VALUES ($1, TRUE) RETURNING `+userCols, name))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO identities (provider, external_id, user_id, chat_ref) VALUES ($1, $2, $3, $4)`,
		ident.Provider, ident.ExternalID, u.ID, ident.ChatRef); err != nil {
		return nil, wrapErr(err)
	}
	return u, tx.Commit(ctx)
}

// PromoteFirstSuperadmin 已绑定用户的首任超管引导：无活跃超管时晋升该用户；
// 已有超管返回 ErrConflict。
func (s *Store) PromoteFirstSuperadmin(ctx context.Context, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockNoSuperadmin(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE users SET is_superadmin = TRUE, updated_at = now() WHERE id = $1`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockNoSuperadmin 取事务咨询锁并确认当前没有活跃超管（有则 ErrConflict）。
func lockNoSuperadmin(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapLockKey); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE is_superadmin AND status = 'active')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrConflict
	}
	return nil
}

// CreateUser 建用户并绑定身份（一个事务）。
func (s *Store) CreateUser(ctx context.Context, name string, superadmin bool, ident Identity) (*User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row := tx.QueryRow(ctx,
		`INSERT INTO users (name, is_superadmin) VALUES ($1, $2) RETURNING `+userCols, name, superadmin)
	u, err := scanUser(row)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO identities (provider, external_id, user_id, chat_ref) VALUES ($1, $2, $3, $4)`,
		ident.Provider, ident.ExternalID, u.ID, ident.ChatRef); err != nil {
		return nil, wrapErr(err)
	}
	return u, tx.Commit(ctx)
}

// UserByID 按内部 ID 取用户。
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// UserByIdentity 按 IM 身份取用户。
func (s *Store) UserByIdentity(ctx context.Context, provider, externalID string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users u JOIN identities i ON i.user_id = u.id
		 WHERE i.provider = $1 AND i.external_id = $2`, provider, externalID))
}

// ListUsers 列出全部用户（含停用，调用方自行过滤展示）。
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpdateUserName 改名。
func (s *Store) UpdateUserName(ctx context.Context, id int64, name string) error {
	return s.execOne(ctx, `UPDATE users SET name = $2, updated_at = now() WHERE id = $1`, id, name)
}

// UpdateUserInfo 合并动态字段值（value 为空串表示删除该字段值）。
func (s *Store) UpdateUserInfo(ctx context.Context, id int64, kv map[string]string) error {
	u, err := s.UserByID(ctx, id)
	if err != nil {
		return err
	}
	if u.Info == nil {
		u.Info = map[string]string{}
	}
	for k, v := range kv {
		if v == "" {
			delete(u.Info, k)
		} else {
			u.Info[k] = v
		}
	}
	raw, err := json.Marshal(u.Info)
	if err != nil {
		return err
	}
	return s.execOne(ctx, `UPDATE users SET info = $2, updated_at = now() WHERE id = $1`, id, raw)
}

// SetUserStatus 停用/启用。
func (s *Store) SetUserStatus(ctx context.Context, id int64, status string) error {
	return s.execOne(ctx, `UPDATE users SET status = $2, updated_at = now() WHERE id = $1`, id, status)
}

// ChatRef 取用户在某渠道的通知投递地址。
func (s *Store) ChatRef(ctx context.Context, userID int64, provider string) (string, error) {
	var ref string
	err := s.pool.QueryRow(ctx,
		`SELECT chat_ref FROM identities WHERE user_id = $1 AND provider = $2`, userID, provider).Scan(&ref)
	return ref, wrapErr(err)
}

// --- 动态字段定义 ---

// AddInfoField 添加字段定义。
func (s *Store) AddInfoField(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO info_fields (name) VALUES ($1)`, name)
	return wrapErr(err)
}

// RemoveInfoField 移除字段定义（不清除已有值，历史保留）。
func (s *Store) RemoveInfoField(ctx context.Context, name string) error {
	return s.execOne(ctx, `DELETE FROM info_fields WHERE name = $1`, name)
}

// ListInfoFields 列出字段定义。
func (s *Store) ListInfoFields(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM info_fields ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// execOne 执行必须恰好影响一行的语句；零行视为 ErrNotFound。
func (s *Store) execOne(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

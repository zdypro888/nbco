package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// BindKey 真人员工一次性邀请：落库、带过期、一次性。
type BindKey struct {
	Key         string
	CreatedBy   int64
	InvitedName string
	InvitedRole string
	Note        string
	ExpiresAt   time.Time
	UsedBy      *int64
	UsedAt      *time.Time
	CreatedAt   time.Time
}

// CreateBindKey 生成真人员工入职码（兼容旧调用；新入口用 CreateBindInvite）。
func (s *Store) CreateBindKey(ctx context.Context, createdBy int64, ttl time.Duration) (*BindKey, error) {
	return s.CreateBindInvite(ctx, createdBy, ttl, "", "", "")
}

// CreateBindInvite 生成真人员工一次性邀请。
func (s *Store) CreateBindInvite(ctx context.Context, createdBy int64, ttl time.Duration, invitedName, invitedRole, note string) (*BindKey, error) {
	key, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	invitedName = strings.TrimSpace(invitedName)
	invitedRole = strings.TrimSpace(invitedRole)
	note = strings.TrimSpace(note)
	var bk BindKey
	err = s.pool.QueryRow(ctx,
		`INSERT INTO bind_keys (key, created_by, invited_name, invited_role, note, expires_at) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING key, created_by, invited_name, invited_role, note, expires_at, used_by, used_at, created_at`,
		key, createdBy, invitedName, invitedRole, note, time.Now().UTC().Add(ttl)).
		Scan(&bk.Key, &bk.CreatedBy, &bk.InvitedName, &bk.InvitedRole, &bk.Note, &bk.ExpiresAt, &bk.UsedBy, &bk.UsedAt, &bk.CreatedAt)
	return &bk, wrapErr(err)
}

// BindUserWithKey 单事务完成入职：锁定并校验 Key → 建用户 → 绑身份 → 标记 Key 已用。
// 返回新用户与邀请人 ID（事件总线据此让邀请人的 AI 决定要不要通知/安排入职）。
// Key 无效/已用/过期返回 ErrNotFound，且不会留下任何半开账号。
func (s *Store) BindUserWithKey(ctx context.Context, key, name string, ident Identity) (*User, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var createdBy int64
	var invitedName, invitedRole string
	if err := tx.QueryRow(ctx,
		`SELECT created_by, invited_name, invited_role FROM bind_keys
		 WHERE key = $1 AND used_by IS NULL AND expires_at > now() FOR UPDATE`, key).
		Scan(&createdBy, &invitedName, &invitedRole); err != nil {
		return nil, 0, wrapErr(err)
	}
	if invitedName = strings.TrimSpace(invitedName); invitedName != "" {
		name = invitedName
	}
	info := "{}"
	if invitedRole = strings.TrimSpace(invitedRole); invitedRole != "" {
		info = `{"role": ` + quoteJSONString(invitedRole) + `}`
	}
	u, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (name, info, is_superadmin) VALUES ($1, $2::jsonb, FALSE) RETURNING `+userCols, name, info))
	if err != nil {
		return nil, 0, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO identities (provider, external_id, user_id, chat_ref) VALUES ($1, $2, $3, $4)`,
		ident.Provider, ident.ExternalID, u.ID, ident.ChatRef); err != nil {
		return nil, 0, wrapErr(err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE bind_keys SET used_by = $2, used_at = now() WHERE key = $1`, key, u.ID); err != nil {
		return nil, 0, err
	}
	return u, createdBy, tx.Commit(ctx)
}

func quoteJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// CancelBindKeys 作废某人生成的全部未用 Key。
func (s *Store) CancelBindKeys(ctx context.Context, createdBy int64) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM bind_keys WHERE created_by = $1 AND used_by IS NULL`, createdBy)
	return err
}

// --- API Token（哈希存储）---

// IssueAPIToken 生成 API token 并替换该用户旧 token；返回明文（仅此一次可见）。
func (s *Store) IssueAPIToken(ctx context.Context, userID int64) (string, error) {
	plain, err := randomHex(24)
	if err != nil {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM api_tokens WHERE user_id = $1`, userID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_tokens (token_hash, user_id) VALUES ($1, $2)`, hashToken(plain), userID); err != nil {
		return "", wrapErr(err)
	}
	return plain, tx.Commit(ctx)
}

// RevokeAPIToken 撤销该用户 token。
func (s *Store) RevokeAPIToken(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM api_tokens WHERE user_id = $1`, userID)
	return err
}

// UserByAPIToken 按明文 token 验证并取用户（哈希索引查询）。
// 列必须带 u. 前缀：api_tokens 也有 created_at，裸列名会歧义报错。
func (s *Store) UserByAPIToken(ctx context.Context, plain string) (*User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT u.id, u.name, u.info, u.status, u.is_superadmin, u.is_worker, u.owner_id, u.worker_last_seen, u.created_at
		 FROM users u JOIN api_tokens t ON t.user_id = u.id
		 WHERE t.token_hash = $1`, hashToken(plain)))
}

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

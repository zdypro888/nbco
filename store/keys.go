package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type APITokenStatus struct {
	Exists    bool
	CreatedAt time.Time
}

// APITokenRotation is a short-lived recoverable token replacement. Candidate
// is deleted after Telegram confirms delivery or when ExpiresAt passes.
type APITokenRotation struct {
	Candidate string
	ExpiresAt time.Time
	IssuedAt  *time.Time
}

// BindKey 真人员工一次性邀请：落库、带过期、一次性。
type BindKey struct {
	Key         string
	CreatedBy   int64
	RequestKey  string
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
	return s.CreateBindInviteForRequest(ctx, createdBy, ttl, invitedName, invitedRole, note, "")
}

const bindKeyCols = `key, created_by, request_key, invited_name, invited_role, note, expires_at, used_by, used_at, created_at`

func scanBindKey(row interface{ Scan(...any) error }) (*BindKey, error) {
	var bk BindKey
	if err := row.Scan(&bk.Key, &bk.CreatedBy, &bk.RequestKey, &bk.InvitedName, &bk.InvitedRole,
		&bk.Note, &bk.ExpiresAt, &bk.UsedBy, &bk.UsedAt, &bk.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &bk, nil
}

// CreateBindInviteForRequest returns the original invitation when the same
// runtime-owned request is recovered. The key itself is already stored for
// redemption, so no second credential is issued.
func (s *Store) CreateBindInviteForRequest(ctx context.Context, createdBy int64, ttl time.Duration, invitedName, invitedRole, note, requestKey string) (*BindKey, error) {
	key, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	invitedName = strings.TrimSpace(invitedName)
	invitedRole = strings.TrimSpace(invitedRole)
	note = strings.TrimSpace(note)
	requestKey = strings.TrimSpace(requestKey)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if requestKey != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "bind-invite:"+requestKey); err != nil {
			return nil, err
		}
		existing, lookupErr := scanBindKey(tx.QueryRow(ctx,
			`SELECT `+bindKeyCols+` FROM bind_keys WHERE created_by=$1 AND request_key=$2`, createdBy, requestKey))
		switch {
		case lookupErr == nil:
			if existing.InvitedName != invitedName || existing.InvitedRole != invitedRole || existing.Note != note {
				return nil, ErrConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return existing, nil
		case !errors.Is(lookupErr, ErrNotFound):
			return nil, lookupErr
		}
	}
	bk, err := scanBindKey(tx.QueryRow(ctx,
		`INSERT INTO bind_keys (key, created_by, request_key, invited_name, invited_role, note, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+bindKeyCols,
		key, createdBy, requestKey, invitedName, invitedRole, note, time.Now().UTC().Add(ttl)))
	if err != nil {
		return nil, err
	}
	return bk, tx.Commit(ctx)
}

func (s *Store) BindInviteByRequest(ctx context.Context, createdBy int64, requestKey string) (*BindKey, error) {
	requestKey = strings.TrimSpace(requestKey)
	if requestKey == "" {
		return nil, ErrNotFound
	}
	return scanBindKey(s.pool.QueryRow(ctx,
		`SELECT `+bindKeyCols+` FROM bind_keys
		 WHERE created_by=$1 AND request_key=$2 AND expires_at > now()`, createdBy, requestKey))
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
	return s.IssueAPITokenCandidate(ctx, userID, plain)
}

// IssueAPITokenCandidate replaces a user's API token with a caller-owned
// candidate. Repeating the same candidate returns the same plaintext without
// rotating again, which makes response-loss recovery deterministic.
func (s *Store) IssueAPITokenCandidate(ctx context.Context, userID int64, plain string) (string, error) {
	plain = strings.ToLower(strings.TrimSpace(plain))
	decoded, err := hex.DecodeString(plain)
	if err != nil || len(decoded) != 24 {
		return "", ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := issueAPITokenCandidateTx(ctx, tx, userID, plain); err != nil {
		return "", err
	}
	return plain, tx.Commit(ctx)
}

func issueAPITokenCandidateTx(ctx context.Context, tx pgx.Tx, userID int64, plain string) error {
	// The unique index is the data invariant; the advisory lock avoids a
	// delete/insert race and lets an exact replay observe the current hash.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('api-token:' || $1::bigint::text, 0))`, userID); err != nil {
		return err
	}
	wantedHash := hashToken(plain)
	var alreadyCurrent bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM api_tokens WHERE user_id=$1 AND token_hash=$2)`,
		userID, wantedHash).Scan(&alreadyCurrent); err != nil {
		return err
	}
	if alreadyCurrent {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_tokens WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_tokens (token_hash, user_id) VALUES ($1, $2)`, wantedHash, userID); err != nil {
		return wrapErr(err)
	}
	return nil
}

// BeginAPITokenRotation creates a pending candidate or returns the existing
// unexpired one. Repeating /token new therefore never changes an in-flight
// confirmation.
func (s *Store) BeginAPITokenRotation(ctx context.Context, userID int64, ttl time.Duration) (*APITokenRotation, error) {
	if userID <= 0 || ttl <= 0 {
		return nil, ErrNotFound
	}
	candidate, err := randomHex(24)
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	var rotation APITokenRotation
	err = s.pool.QueryRow(ctx,
		`INSERT INTO api_token_rotations (user_id, candidate, expires_at)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (user_id) DO UPDATE
		 SET candidate = CASE WHEN api_token_rotations.expires_at <= now() THEN EXCLUDED.candidate ELSE api_token_rotations.candidate END,
		     expires_at = CASE WHEN api_token_rotations.expires_at <= now() THEN EXCLUDED.expires_at ELSE api_token_rotations.expires_at END,
		     issued_at = CASE WHEN api_token_rotations.expires_at <= now() THEN NULL ELSE api_token_rotations.issued_at END,
		     created_at = CASE WHEN api_token_rotations.expires_at <= now() THEN now() ELSE api_token_rotations.created_at END
		 RETURNING candidate, expires_at, issued_at`,
		userID, candidate, expiresAt).Scan(&rotation.Candidate, &rotation.ExpiresAt, &rotation.IssuedAt)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rotation, nil
}

// ConfirmAPITokenRotation issues the pending candidate. A retry after a
// process or network failure returns the same token and does not rotate again.
func (s *Store) ConfirmAPITokenRotation(ctx context.Context, userID int64) (*APITokenRotation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var rotation APITokenRotation
	if err := tx.QueryRow(ctx,
		`SELECT candidate, expires_at, issued_at
		 FROM api_token_rotations
		 WHERE user_id=$1 AND expires_at > now()
		 FOR UPDATE`, userID).
		Scan(&rotation.Candidate, &rotation.ExpiresAt, &rotation.IssuedAt); err != nil {
		return nil, wrapErr(err)
	}
	if err := issueAPITokenCandidateTx(ctx, tx, userID, rotation.Candidate); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx,
		`UPDATE api_token_rotations
		 SET issued_at=COALESCE(issued_at, now())
		 WHERE user_id=$1 AND candidate=$2 AND expires_at > now()
		 RETURNING candidate, expires_at, issued_at`, userID, rotation.Candidate).
		Scan(&rotation.Candidate, &rotation.ExpiresAt, &rotation.IssuedAt); err != nil {
		return nil, wrapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &rotation, nil
}

// AcknowledgeAPITokenRotation deletes plaintext only after the transport has
// confirmed delivery. The candidate match cannot delete a newer rotation.
func (s *Store) AcknowledgeAPITokenRotation(ctx context.Context, userID int64, candidate string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM api_token_rotations WHERE user_id=$1 AND candidate=$2`,
		userID, strings.ToLower(strings.TrimSpace(candidate)))
	return err
}

func (s *Store) CancelAPITokenRotation(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM api_token_rotations WHERE user_id=$1`, userID)
	return err
}

func (s *Store) DeleteExpiredAPITokenRotations(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM api_token_rotations WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// APITokenStatus 返回用户是否已有 API token。明文不可逆哈希存储，因此不能查询原文。
func (s *Store) APITokenStatus(ctx context.Context, userID int64) (*APITokenStatus, error) {
	var st APITokenStatus
	err := s.pool.QueryRow(ctx,
		`SELECT created_at FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, userID).
		Scan(&st.CreatedAt)
	if err != nil {
		if err := wrapErr(err); errors.Is(err, ErrNotFound) {
			return &st, nil
		}
		return nil, wrapErr(err)
	}
	st.Exists = true
	return &st, nil
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

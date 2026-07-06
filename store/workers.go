package store

import (
	"context"
	"strings"
	"time"
)

const workerClaimTimeout = 3 * time.Hour

// Worker 绑定码：create_worker 只把这个短时效一次性码带回对话，真正的
// Worker Access Token 由工作机兑换时才签发——长期凭据永不进入聊天与会话历史。
const (
	WorkerBindCodePrefix = "wbc_"
	workerBindCodeTTL    = 24 * time.Hour
)

// CreateWorker 建一个 AI worker 用户并签发一次性绑定码（非 access token）。
// worker 无 IM 身份；工作机用 nbco-worker bind/bootstrap 拿绑定码兑换 token 后认证。
// owner 为监护人。
func (s *Store) CreateWorker(ctx context.Context, name string, ownerID int64) (*User, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	u, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (name, is_worker, owner_id) VALUES ($1, TRUE, $2) RETURNING `+userCols, name, ownerID))
	if err != nil {
		return nil, "", err
	}
	code, err := newWorkerBindCode()
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_bind_codes (code_hash, worker_id, created_by, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(code), u.ID, ownerID, time.Now().UTC().Add(workerBindCodeTTL)); err != nil {
		return nil, "", wrapErr(err)
	}
	return u, code, tx.Commit(ctx)
}

func newWorkerBindCode() (string, error) {
	raw, err := randomHex(12)
	if err != nil {
		return "", err
	}
	return WorkerBindCodePrefix + raw, nil
}

// NewWorkerBindCode 给已有 worker 补发绑定码（旧码作废，一台机器一码）。
// 已生效的 access token 不受影响，直到新码被兑换才被替换。
// 目标非 worker 或已停用返回 ErrNotFound。
func (s *Store) NewWorkerBindCode(ctx context.Context, workerID, createdBy int64) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT TRUE FROM users WHERE id = $1 AND is_worker AND status = 'active'`, workerID).Scan(&ok); err != nil {
		return "", wrapErr(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM worker_bind_codes WHERE worker_id = $1`, workerID); err != nil {
		return "", err
	}
	code, err := newWorkerBindCode()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_bind_codes (code_hash, worker_id, created_by, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(code), workerID, createdBy, time.Now().UTC().Add(workerBindCodeTTL)); err != nil {
		return "", wrapErr(err)
	}
	return code, tx.Commit(ctx)
}

// RedeemWorkerBindCode 用绑定码兑换 Worker Access Token（一次一用）：
// 锁定并校验码 → 撤旧 token → 签发新 token → 删掉该 worker 的所有绑定码。
// 码无效/过期/worker 已停用返回 ErrNotFound。顺带清掉全表过期码。
func (s *Store) RedeemWorkerBindCode(ctx context.Context, code string) (*User, string, error) {
	if !strings.HasPrefix(code, WorkerBindCodePrefix) {
		return nil, "", ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM worker_bind_codes WHERE expires_at <= now()`); err != nil {
		return nil, "", err
	}
	var workerID int64
	if err := tx.QueryRow(ctx,
		`SELECT worker_id FROM worker_bind_codes WHERE code_hash = $1 AND expires_at > now() FOR UPDATE`,
		hashToken(code)).Scan(&workerID); err != nil {
		return nil, "", wrapErr(err)
	}
	u, err := scanUser(tx.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1 AND is_worker AND status = 'active'`, workerID))
	if err != nil {
		return nil, "", err
	}
	plain, err := randomHex(24)
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_tokens WHERE user_id = $1`, workerID); err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_tokens (token_hash, user_id) VALUES ($1, $2)`, hashToken(plain), workerID); err != nil {
		return nil, "", wrapErr(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM worker_bind_codes WHERE worker_id = $1`, workerID); err != nil {
		return nil, "", err
	}
	return u, plain, tx.Commit(ctx)
}

// ListWorkers 列出监护人名下的 worker（superadmin 传 0 看全部）。
func (s *Store) ListWorkers(ctx context.Context, ownerID int64) ([]*User, error) {
	sql := `SELECT ` + userCols + ` FROM users WHERE is_worker`
	args := []any{}
	if ownerID != 0 {
		sql += ` AND owner_id = $1`
		args = append(args, ownerID)
	}
	sql += ` ORDER BY id`
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ws []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		ws = append(ws, u)
	}
	return ws, rows.Err()
}

// WorkerHeartbeat 刷新 worker 上线时间（每次拉任务时调）。
func (s *Store) WorkerHeartbeat(ctx context.Context, workerID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET worker_last_seen = now() WHERE id = $1`, workerID)
	return err
}

// ClaimNextTask 原子认领 worker 的下一个待办任务：取最早的 pending 或未持有
// claim 的返工 in_progress 置为 in_progress。
// 无任务返回 ErrNotFound。SKIP LOCKED 防多进程重复认领。
// 已被 worker 认领但超时未提交的 in_progress 任务会被回收，避免 client 崩溃后永久卡住。
func (s *Store) ClaimNextTask(ctx context.Context, workerID int64) (*Task, error) {
	staleBefore := time.Now().Add(-workerClaimTimeout)
	claimID, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	return scanTask(s.pool.QueryRow(ctx,
		`UPDATE tasks
		    SET status = 'in_progress',
		        worker_claimed_by = $1,
		        worker_claimed_at = now(),
		        worker_claim_id = $3,
		        updated_at = now()
			 WHERE id = (
			   SELECT id FROM tasks
			   WHERE assignee_id = $1
			     AND (
			       status = 'pending'
			       OR (status = 'in_progress' AND worker_claimed_at IS NULL)
			       OR (status = 'in_progress' AND worker_claimed_at IS NOT NULL AND worker_claimed_at <= $2)
			     )
			   ORDER BY
			     (status = 'in_progress') DESC,
			     (priority = 'high') DESC,
			     COALESCE(deadline, 'infinity'),
			     id
			   LIMIT 1 FOR UPDATE SKIP LOCKED
			 ) RETURNING `+taskCols, workerID, staleBefore, claimID))
}

// RevokeWorker 停用 worker 并撤销其 token（历史任务保留）。目标非 worker 时 ErrNotFound。
func (s *Store) RevokeWorker(ctx context.Context, workerID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1 AND is_worker`, workerID)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_tokens WHERE user_id = $1`, workerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM worker_bind_codes WHERE worker_id = $1`, workerID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

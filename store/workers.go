package store

import (
	"context"
	"strings"
	"time"
)

const (
	workerClaimTimeout = 3 * time.Hour
	workerMaxFailures  = 5
)

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

// ListAdminWorkers lists active workers that are explicitly promoted to
// system-admin workers. Pass ownerID to restrict to one owner's workers; 0 means
// all owners and should only be used by superadmin-level views.
func (s *Store) ListAdminWorkers(ctx context.Context, ownerID int64) ([]*User, error) {
	sql := `SELECT ` + userCols + ` FROM users
		 WHERE is_worker AND is_superadmin AND status = 'active'`
	args := []any{}
	if ownerID != 0 {
		sql += ` AND owner_id = $1`
		args = append(args, ownerID)
	}
	sql += ` ORDER BY COALESCE(worker_last_seen, '-infinity'::timestamptz) DESC, id`
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

// SetWorkerAdmin promotes or demotes a worker's system-admin capability. It
// reuses users.is_superadmin so all target-level checks keep one meaning.
func (s *Store) SetWorkerAdmin(ctx context.Context, workerID int64, admin bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET is_superadmin = $2, updated_at = now()
		 WHERE id = $1 AND is_worker`,
		workerID, admin)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// WorkerHeartbeat 刷新 worker 上线时间（每次拉任务时调）。
func (s *Store) WorkerHeartbeat(ctx context.Context, workerID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET worker_last_seen = now() WHERE id = $1`, workerID)
	return err
}

func workerFailureBackoff(attempt int) time.Duration {
	delays := [...]time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	if attempt <= 0 {
		return delays[0]
	}
	if attempt > len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempt-1]
}

// RevokeWorker 停用 worker 并撤销其 token（历史任务保留）。目标非 worker 时 ErrNotFound。
// 同事务内取消该 worker 的执行记录，并把未完成业务任务重置为 pending，
// 让管理者能够改派而不丢失历史。返回重置的任务数。
func (s *Store) RevokeWorker(ctx context.Context, workerID int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Follow the same task -> run -> attempt lock order as normal task edits.
	// Business history remains pending for reassignment; direct runs are simply
	// cancelled because they have no artificial task row.
	rows, err := tx.Query(ctx,
		`SELECT id FROM tasks
		 WHERE assignee_id = $1 AND status IN ('pending', 'in_progress', 'awaiting_input')
		 ORDER BY id FOR UPDATE`, workerID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var taskID int64
		if err := rows.Scan(&taskID); err != nil {
			rows.Close()
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	// Worker validation during task creation takes a shared users-row lock while
	// already holding the task. Disable only after the task locks above, keeping
	// that order consistent and preventing revoke/create deadlocks.
	tag, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled', updated_at = now() WHERE id = $1 AND is_worker`, workerID)
	if err != nil {
		return 0, wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return 0, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM api_tokens WHERE user_id = $1`, workerID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM worker_bind_codes WHERE worker_id = $1`, workerID); err != nil {
		return 0, err
	}
	rtag, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'pending', revision = revision + 1, updated_at = now()
		  WHERE assignee_id = $1 AND status IN ('pending', 'in_progress', 'awaiting_input')`, workerID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE worker_runs SET status = 'cancelled', claim_id = '', claimed_at = NULL,
		   last_error = 'worker 已停用', completed_at = now(), updated_at = now()
		 WHERE worker_id = $1 AND status IN ('queued','claimed','retry_wait','awaiting_input')`, workerID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE worker_run_attempts SET status = 'cancelled', finished_at = now(), updated_at = now()
		 WHERE worker_id = $1 AND status = 'claimed'`, workerID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return rtag.RowsAffected(), nil
}

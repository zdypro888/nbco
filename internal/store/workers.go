package store

import (
	"context"
	"time"
)

const workerClaimTimeout = 3 * time.Hour

// CreateWorker 建一个 AI 员工用户并签发常驻 API token（明文仅此一次返回）。
// worker 无 IM 身份，靠 token 认证；owner 为监护人。
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
	plain, err := randomHex(24)
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_tokens (token_hash, user_id) VALUES ($1, $2)`, hashToken(plain), u.ID); err != nil {
		return nil, "", wrapErr(err)
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

// ClaimNextTask 原子认领 worker 的下一个待办任务：取最早的 pending 置为 in_progress。
// 无任务返回 ErrNotFound。SKIP LOCKED 防多进程重复认领。
// 已被 worker 认领但超时未提交的 in_progress 任务会被回收，避免 client 崩溃后永久卡住。
func (s *Store) ClaimNextTask(ctx context.Context, workerID int64) (*Task, error) {
	staleBefore := time.Now().Add(-workerClaimTimeout)
	return scanTask(s.pool.QueryRow(ctx,
		`UPDATE tasks
		    SET status = 'in_progress',
		        worker_claimed_by = $1,
		        worker_claimed_at = now(),
		        updated_at = now()
			 WHERE id = (
			   SELECT id FROM tasks
			   WHERE assignee_id = $1
			     AND (
			       status = 'pending'
			       OR (status = 'in_progress' AND worker_claimed_at IS NOT NULL AND worker_claimed_at <= $2)
			     )
			   ORDER BY
			     (status = 'in_progress') DESC,
			     (priority = 'high') DESC,
			     COALESCE(deadline, 'infinity'),
			     id
			   LIMIT 1 FOR UPDATE SKIP LOCKED
			 ) RETURNING `+taskCols, workerID, staleBefore))
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
	return tx.Commit(ctx)
}

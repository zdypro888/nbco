package store

import (
	"context"
	"fmt"
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One worker identity owns one local workspace/CLI at a time. Locking the
	// worker row makes concurrent pollers serialize before they inspect claims.
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT TRUE FROM users WHERE id=$1 AND is_worker AND status='active' FOR UPDATE`, workerID).Scan(&exists); err != nil {
		return nil, wrapErr(err)
	}
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks
			    SET status = 'in_progress',
			        worker_claimed_by = $1,
			        worker_claimed_at = now(),
			        worker_claim_id = $3,
			        worker_retry_at = NULL,
			        updated_at = now()
			 WHERE id = (
			   SELECT t.id FROM tasks t
			   WHERE t.assignee_id = $1
				     AND (
				       t.status = 'pending'
				       OR (t.status = 'in_progress' AND t.worker_claimed_at IS NULL)
				       OR (t.status = 'in_progress' AND t.worker_claimed_at IS NOT NULL AND t.worker_claimed_at <= $2)
				     )
				     AND (t.worker_retry_at IS NULL OR t.worker_retry_at <= now())
				     AND NOT EXISTS (
				       SELECT 1 FROM tasks active
				        WHERE active.assignee_id = $1
				          AND active.status = 'in_progress'
				          AND active.worker_claimed_at IS NOT NULL
				          AND active.worker_claimed_at > $2
				          AND active.worker_claim_id <> ''
				     )
			     -- 依赖编排：前置任务未全部验收通过前不可领取
			     AND NOT EXISTS (
			       SELECT 1 FROM tasks d WHERE d.id = ANY(t.depends_on) AND d.status <> 'accepted'
			     )
			   ORDER BY
			     (status = 'in_progress') DESC,
			     (priority = 'high') DESC,
			     COALESCE(deadline, 'infinity'),
			     id
			   LIMIT 1 FOR UPDATE SKIP LOCKED
				 ) RETURNING `+taskCols, workerID, staleBefore, claimID))
	if err != nil {
		return nil, err
	}
	return t, tx.Commit(ctx)
}

// FailWorkerTask records a genuine execution failure and releases the claim
// with durable exponential backoff. Repeated failures pause the task for human
// intervention instead of spinning forever or silently holding a three-hour
// claim lease.
func (s *Store) FailWorkerTask(ctx context.Context, taskID, workerID int64, claimID, cause string) (*Task, error) {
	claimID = strings.TrimSpace(claimID)
	cause = strings.TrimSpace(cause)
	if claimID == "" || cause == "" {
		return nil, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var failures int
	if err := tx.QueryRow(ctx,
		`SELECT worker_failures FROM tasks
		  WHERE id=$1 AND assignee_id=$2 AND status='in_progress' AND worker_claim_id=$3
		  FOR UPDATE`, taskID, workerID, claimID).Scan(&failures); err != nil {
		return nil, wrapErr(err)
	}
	failures++
	blocked := failures >= workerMaxFailures
	var retryAt *time.Time
	if !blocked {
		next := time.Now().UTC().Add(workerFailureBackoff(failures))
		retryAt = &next
	}
	t, err := scanTask(tx.QueryRow(ctx,
		`UPDATE tasks SET
		   status = CASE WHEN $5 THEN 'awaiting_input' ELSE 'pending' END,
		   worker_claimed_by = NULL,
		   worker_claimed_at = NULL,
		   worker_claim_id = '',
		   worker_retry_at = $4,
		   worker_failures = $6,
		   worker_last_error = $7,
		   updated_at = now()
		 WHERE id=$1 AND assignee_id=$2 AND status='in_progress' AND worker_claim_id=$3
		 RETURNING `+taskCols,
		taskID, workerID, claimID, retryAt, blocked, failures, truncateRunes(cause, 2000)))
	if err != nil {
		return nil, err
	}
	message := fmt.Sprintf("⚠️ Worker 执行失败（第 %d/%d 次）：%s", failures, workerMaxFailures, truncateRunes(cause, 1000))
	if blocked {
		message += "；已暂停，等待分配者处理。"
	} else if retryAt != nil {
		message += "；将在 " + retryAt.Format(time.RFC3339) + " 后自动重试。"
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO task_progress (task_id, author_id, content) VALUES ($1,$2,$3)`,
		taskID, workerID, message); err != nil {
		return nil, wrapErr(err)
	}
	return t, tx.Commit(ctx)
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

// ReleaseWorkerTaskClaim 清掉一次已领取但尚未交付给 worker 的 claim，使任务立即可重领。
// 不改 status：返工任务本来就是 in_progress，pending 任务领取时也已进入 in_progress；
// 关键是清空 claim 字段，避免交付失败后等 3 小时租约超时。
func (s *Store) ReleaseWorkerTaskClaim(ctx context.Context, taskID, workerID int64, claimID string) error {
	if strings.TrimSpace(claimID) == "" {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks
		    SET worker_claimed_by = NULL,
		        worker_claimed_at = NULL,
		        worker_claim_id = '',
		        updated_at = now()
		  WHERE id = $1 AND assignee_id = $2 AND status = 'in_progress' AND worker_claim_id = $3`,
		taskID, workerID, claimID)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeWorker 停用 worker 并撤销其 token（历史任务保留）。目标非 worker 时 ErrNotFound。
// 同事务内把该 worker 名下「未完成」的任务（pending/in_progress/awaiting_input）重置为 pending 并清空
// worker claim——否则它们会永远停在已禁用的 assignee 名下无人恢复（ClaimNextTask 只认领
// assignee_id 匹配自己的任务，nudgePass 对禁用 assignee 直接跳过）。返回重置的任务数。
func (s *Store) RevokeWorker(ctx context.Context, workerID int64) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE id = $1 AND is_worker`, workerID)
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
	// 重置未完成任务：回 pending、清 claim，让分配者可改派或其他机制接管。
	rtag, err := tx.Exec(ctx,
		`UPDATE tasks SET status = 'pending',
		    worker_claimed_by = NULL, worker_claimed_at = NULL, worker_claim_id = '',
		    worker_retry_at = NULL, worker_failures = 0, worker_last_error = '',
		    updated_at = now()
		  WHERE assignee_id = $1 AND status IN ('pending', 'in_progress', 'awaiting_input')`, workerID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return rtag.RowsAffected(), nil
}

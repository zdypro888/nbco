package store

import (
	"context"
	"time"
)

// approvalTTL 待确认动作的有效期：够一轮「AI 登记 → 用户确认 → AI 重调」，
// 又不至于让旧确认在很久以后被翻出来执行。
const approvalTTL = 10 * time.Minute

// CreatePendingApproval 登记一个待确认动作，返回编号。顺带清理全表过期行。
func (s *Store) CreatePendingApproval(ctx context.Context, userID int64, tool, argsHash string) (int64, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM pending_approvals WHERE expires_at <= now()`); err != nil {
		return 0, err
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO pending_approvals (user_id, tool, args_hash, expires_at) VALUES ($1, $2, $3, $4) RETURNING id`,
		userID, tool, argsHash, time.Now().UTC().Add(approvalTTL)).Scan(&id)
	return id, wrapErr(err)
}

// ConsumePendingApproval 核销匹配的待确认动作（同人、同工具、同参数、未过期）。
// 返回是否命中；命中即删除（一次一用）。
func (s *Store) ConsumePendingApproval(ctx context.Context, userID int64, tool, argsHash string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM pending_approvals
		 WHERE id = (SELECT id FROM pending_approvals
		             WHERE user_id = $1 AND tool = $2 AND args_hash = $3 AND expires_at > now()
		             ORDER BY id LIMIT 1)`, userID, tool, argsHash)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

package store

import (
	"context"
	"time"
)

// approvalTTL 待确认动作的有效期：够一轮「AI 登记 → 用户确认 → AI 重调」，
// 又不至于让旧确认在很久以后被翻出来执行。
const approvalTTL = 10 * time.Minute

// PendingApproval 是一条仍在有效期内、等待用户二次确认的高危工具调用。
// args 只存哈希，列表页不能也不应展示原始敏感参数。
type PendingApproval struct {
	ID                 int64     `json:"id"`
	UserID             int64     `json:"user_id"`
	UserName           string    `json:"user_name"`
	Tool               string    `json:"tool"`
	SessionID          int64     `json:"session_id"`
	RequestedMessageID int64     `json:"requested_message_id"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// CreatePendingApproval 登记一个待确认动作，返回编号。顺带清理全表过期行。
// requestedMessageID 是触发登记的用户消息；执行时必须来自同会话更晚的用户消息。
func (s *Store) CreatePendingApproval(ctx context.Context, userID int64, tool, argsHash string, sessionID, requestedMessageID int64) (int64, error) {
	if _, err := s.pool.Exec(ctx, `DELETE FROM pending_approvals WHERE expires_at <= now()`); err != nil {
		return 0, err
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM pending_approvals
		  WHERE user_id = $1 AND tool = $2 AND args_hash = $3 AND session_id = $4`,
		userID, tool, argsHash, sessionID); err != nil {
		return 0, err
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO pending_approvals (user_id, tool, args_hash, session_id, requested_message_id, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		userID, tool, argsHash, sessionID, requestedMessageID, time.Now().UTC().Add(approvalTTL)).Scan(&id)
	return id, wrapErr(err)
}

// ConsumePendingApproval 核销匹配的待确认动作（同人、同工具、同参数、同会话、
// 未过期，且当前用户消息晚于登记消息）。
// 返回是否命中；命中即删除（一次一用）。
func (s *Store) ConsumePendingApproval(ctx context.Context, userID int64, tool, argsHash string, sessionID, currentMessageID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM pending_approvals
		 WHERE id = (SELECT id FROM pending_approvals
		             WHERE user_id = $1 AND tool = $2 AND args_hash = $3
		               AND session_id = $4 AND requested_message_id < $5 AND expires_at > now()
		             ORDER BY id LIMIT 1)`, userID, tool, argsHash, sessionID, currentMessageID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CleanupExpiredPendingApprovals 删除过期审批，避免控制台或巡检看到假待办。
func (s *Store) CleanupExpiredPendingApprovals(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM pending_approvals WHERE expires_at <= now()`)
	return err
}

// ListPendingApprovals 列出当前仍有效的待确认动作。读取前先清过期项，保证
// “待确认”数量代表真实可确认动作，而不是历史残留。
func (s *Store) ListPendingApprovals(ctx context.Context, limit int) ([]PendingApproval, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if err := s.CleanupExpiredPendingApprovals(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.user_id, coalesce(u.name, ''), p.tool, p.session_id, p.requested_message_id, p.expires_at
		   FROM pending_approvals p
		   JOIN users u ON u.id = p.user_id
		  WHERE p.expires_at > now()
		  ORDER BY p.expires_at ASC, p.id ASC
		  LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingApproval{}
	for rows.Next() {
		var p PendingApproval
		if err := rows.Scan(&p.ID, &p.UserID, &p.UserName, &p.Tool, &p.SessionID, &p.RequestedMessageID, &p.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

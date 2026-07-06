package store

import (
	"context"
	"time"
)

// AIUsage 一次模型调用的用量流水。
type AIUsage struct {
	UserID       int64
	SessionID    *int64
	Kind         string // telegram / api / worker_llm / compact / summarize …
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// RecordAIUsage 记一笔用量（尽力而为：失败由调用方记日志，不阻断业务）。
func (s *Store) RecordAIUsage(ctx context.Context, u AIUsage) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ai_usage (user_id, session_id, kind, model, input_tokens, output_tokens)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		u.UserID, u.SessionID, u.Kind, u.Model, u.InputTokens, u.OutputTokens)
	return err
}

// AIUsageTotal 时间段内的用量合计。
type AIUsageTotal struct {
	Calls        int64
	InputTokens  int64
	OutputTokens int64
}

// AIUsageSince 自 since 起的总用量。
func (s *Store) AIUsageSince(ctx context.Context, since time.Time) (AIUsageTotal, error) {
	var t AIUsageTotal
	err := s.pool.QueryRow(ctx,
		`SELECT count(*), coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0)
		 FROM ai_usage WHERE created_at >= $1`, since).
		Scan(&t.Calls, &t.InputTokens, &t.OutputTokens)
	return t, err
}

// AIUsageByUser 一名用户的用量小计。
type AIUsageByUser struct {
	UserID       int64
	Name         string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
}

// AIUsageByUserSince 自 since 起按用户分组的用量（多者在前）。
func (s *Store) AIUsageByUserSince(ctx context.Context, since time.Time) ([]AIUsageByUser, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT u.user_id, coalesce(us.name, '用户'||u.user_id), count(*),
		        coalesce(sum(u.input_tokens),0), coalesce(sum(u.output_tokens),0)
		 FROM ai_usage u LEFT JOIN users us ON us.id = u.user_id
		 WHERE u.created_at >= $1
		 GROUP BY u.user_id, us.name
		 ORDER BY sum(u.input_tokens)+sum(u.output_tokens) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIUsageByUser
	for rows.Next() {
		var r AIUsageByUser
		if err := rows.Scan(&r.UserID, &r.Name, &r.Calls, &r.InputTokens, &r.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

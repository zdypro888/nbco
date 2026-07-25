package store

import (
	"context"
	"time"
)

// AIUsage 一次模型调用的用量流水。
type AIUsage struct {
	ConversationTurnID *int64
	UserID             int64
	SessionID          *int64
	Kind               string // telegram / api / worker_llm / compact / summarize …
	Model              string
	InputTokens        int64
	OutputTokens       int64
	GoalID             *int64 // 可选：经 worker 任务链解析出的战略目标（仅 worker_llm 尽力填，其余为 nil）
}

// RecordAIUsage 记一笔用量（尽力而为：失败由调用方记日志，不阻断业务）。
func (s *Store) RecordAIUsage(ctx context.Context, u AIUsage) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ai_usage
		   (user_id, session_id, kind, model, input_tokens, output_tokens, goal_id, conversation_turn_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		u.UserID, u.SessionID, u.Kind, u.Model, u.InputTokens, u.OutputTokens, u.GoalID,
		u.ConversationTurnID)
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

// AIUsageByGoal 各战略目标的执行成本小计（仅含 worker_llm 已归因的；goal_id 为 NULL 的不在此列）。
type AIUsageByGoal struct {
	GoalID       int64
	Title        string
	Calls        int64
	InputTokens  int64
	OutputTokens int64
}

// AIUsageByGoalSince 自 since 起按目标分组的 worker 执行成本（多者在前）。
// 只反映 AI 员工执行成本，不含对话/催办等系统轮次——见 ai_usage_stats 工具说明。
func (s *Store) AIUsageByGoalSince(ctx context.Context, since time.Time) ([]AIUsageByGoal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT u.goal_id, coalesce(g.title, '目标'||u.goal_id), count(*),
		        coalesce(sum(u.input_tokens),0), coalesce(sum(u.output_tokens),0)
		 FROM ai_usage u LEFT JOIN goals g ON g.id = u.goal_id
		 WHERE u.created_at >= $1 AND u.goal_id IS NOT NULL
		 GROUP BY u.goal_id, g.title
		 ORDER BY sum(u.input_tokens)+sum(u.output_tokens) DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIUsageByGoal
	for rows.Next() {
		var r AIUsageByGoal
		if err := rows.Scan(&r.GoalID, &r.Title, &r.Calls, &r.InputTokens, &r.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GoalIDOfTask 解析一个任务所属的战略目标（经 milestone_id → milestones.goal_id）。
// 任务不存在返 ErrNotFound；任务存在但未挂里程碑返 (nil, nil)。
func (s *Store) GoalIDOfTask(ctx context.Context, taskID int64) (*int64, error) {
	var milestoneID *int64
	err := s.pool.QueryRow(ctx, `SELECT milestone_id FROM tasks WHERE id = $1`, taskID).Scan(&milestoneID)
	if err != nil {
		return nil, wrapErr(err) // ErrNotFound 当任务不存在
	}
	if milestoneID == nil {
		return nil, nil
	}
	var goalID *int64
	err = s.pool.QueryRow(ctx, `SELECT goal_id FROM milestones WHERE id = $1`, *milestoneID).Scan(&goalID)
	if err != nil {
		return nil, wrapErr(err)
	}
	return goalID, nil
}

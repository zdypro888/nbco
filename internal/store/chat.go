package store

import (
	"context"
	"encoding/json"
	"time"
)

// ChatSession AI 会话。每个 (user, channel) 至多一个活跃会话。
type ChatSession struct {
	ID        int64
	UserID    int64
	Channel   string
	Engine    string
	EngineRef string // claudecli 的 CLI session id
	Active    bool
	CreatedAt time.Time
}

// ChatMessage 会话内一条文本消息。
type ChatMessage struct {
	ID        int64
	SessionID int64
	Role      string
	Content   string
	CreatedAt time.Time
}

const sessionCols = `id, user_id, channel, engine, engine_ref, active, created_at`

func scanSession(row interface{ Scan(...any) error }) (*ChatSession, error) {
	var cs ChatSession
	if err := row.Scan(&cs.ID, &cs.UserID, &cs.Channel, &cs.Engine, &cs.EngineRef, &cs.Active, &cs.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &cs, nil
}

// ActiveSession 取活跃会话；没有则 ErrNotFound。
func (s *Store) ActiveSession(ctx context.Context, userID int64, channel string) (*ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE user_id = $1 AND channel = $2 AND active`, userID, channel))
}

// StartSession 关闭旧活跃会话并开新会话（一个事务）。
func (s *Store) StartSession(ctx context.Context, userID int64, channel, engine string) (*ChatSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE chat_sessions SET active = FALSE, updated_at = now()
		 WHERE user_id = $1 AND channel = $2 AND active`, userID, channel); err != nil {
		return nil, err
	}
	cs, err := scanSession(tx.QueryRow(ctx,
		`INSERT INTO chat_sessions (user_id, channel, engine) VALUES ($1, $2, $3) RETURNING `+sessionCols,
		userID, channel, engine))
	if err != nil {
		return nil, err
	}
	return cs, tx.Commit(ctx)
}

// SetSessionEngineRef 记录引擎侧会话标识（claudecli 首轮返回）。
func (s *Store) SetSessionEngineRef(ctx context.Context, sessionID int64, ref string) error {
	return s.execOne(ctx,
		`UPDATE chat_sessions SET engine_ref = $2, updated_at = now() WHERE id = $1`, sessionID, ref)
}

// AppendMessage 追加消息。
func (s *Store) AppendMessage(ctx context.Context, sessionID int64, role, content string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO chat_messages (session_id, role, content) VALUES ($1, $2, $3)`, sessionID, role, content)
	return wrapErr(err)
}

// MessagesOf 会话消息（升序），limit<=0 取全部。
func (s *Store) MessagesOf(ctx context.Context, sessionID int64, limit int) ([]ChatMessage, error) {
	sql := `SELECT id, session_id, role, content, created_at FROM chat_messages WHERE session_id = $1 ORDER BY id`
	args := []any{sessionID}
	if limit > 0 {
		// 取最近 limit 条但保持升序。
		sql = `SELECT id, session_id, role, content, created_at FROM (
		         SELECT id, session_id, role, content, created_at FROM chat_messages
		         WHERE session_id = $1 ORDER BY id DESC LIMIT $2
		       ) sub ORDER BY id`
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// --- 审计 ---

// Audit 写一条审计记录（尽力而为：失败不阻断业务，调用方记日志即可）。
func (s *Store) Audit(ctx context.Context, userID int64, sessionID *int64, tool string, args json.RawMessage, result string, ok bool) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_log (user_id, session_id, tool, args, result, ok) VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, sessionID, tool, args, result, ok)
	return err
}

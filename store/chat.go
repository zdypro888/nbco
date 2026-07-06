package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ChatSession AI 会话。每个 (user, channel) 至多一个活跃会话；
// 群共享会话（channel 形如 telegram:group:<chatID>）则每个 channel 至多一个活跃。
type ChatSession struct {
	ID        int64
	UserID    int64
	Channel   string
	Engine    string
	EngineRef string // 引擎侧会话标识（当前保留字段）
	Active    bool
	CreatedAt time.Time
	// 滚动摘要压缩：Summary 是较早对话的压缩摘要，SummaryUpto 是已折叠的
	// 最后一条消息 ID。重放历史 = 摘要 + SummaryUpto 之后的消息。
	Summary     string
	SummaryUpto int64
}

// ChatMessage 会话内一条文本消息。
type ChatMessage struct {
	ID        int64
	SessionID int64
	Role      string
	Content   string
	CreatedAt time.Time
}

const sessionCols = `id, user_id, channel, engine, engine_ref, active, created_at, summary, summary_upto`

func scanSession(row interface{ Scan(...any) error }) (*ChatSession, error) {
	var cs ChatSession
	if err := row.Scan(&cs.ID, &cs.UserID, &cs.Channel, &cs.Engine, &cs.EngineRef, &cs.Active, &cs.CreatedAt,
		&cs.Summary, &cs.SummaryUpto); err != nil {
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

// SetSessionEngineRef 记录引擎侧会话标识。
func (s *Store) SetSessionEngineRef(ctx context.Context, sessionID int64, ref string) error {
	return s.execOne(ctx,
		`UPDATE chat_sessions SET engine_ref = $2, updated_at = now() WHERE id = $1`, sessionID, ref)
}

// SessionByID 取会话。
func (s *Store) SessionByID(ctx context.Context, id int64) (*ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE id = $1`, id))
}

// ActiveSessionByChannel 按渠道取活跃会话（群共享会话）；没有则 ErrNotFound。
func (s *Store) ActiveSessionByChannel(ctx context.Context, channel string) (*ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE channel = $1 AND active ORDER BY id DESC LIMIT 1`, channel))
}

// StartGroupSession 开群共享会话：按 channel（而非 user）关闭旧活跃会话。
// userID 只做创建者记录。
func (s *Store) StartGroupSession(ctx context.Context, userID int64, channel, engine string) (*ChatSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE chat_sessions SET active = FALSE, updated_at = now()
		 WHERE channel = $1 AND active`, channel); err != nil {
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

// UpdateSessionSummary 更新滚动摘要与折叠位点（只允许位点前进，防并发压缩回退）。
func (s *Store) UpdateSessionSummary(ctx context.Context, sessionID int64, summary string, upto int64) error {
	return s.execOne(ctx,
		`UPDATE chat_sessions SET summary = $2, summary_upto = $3, updated_at = now()
		 WHERE id = $1 AND summary_upto < $3`, sessionID, summary, upto)
}

// AppendMessage 追加消息，返回消息 ID（情景记忆按 ID 异步补 embedding）。
func (s *Store) AppendMessage(ctx context.Context, sessionID int64, role, content string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO chat_messages (session_id, role, content) VALUES ($1, $2, $3) RETURNING id`,
		sessionID, role, content).Scan(&id)
	return id, wrapErr(err)
}

// --- 会话情景记忆（episodic memory）：消息级语义检索 ---

// minMemorableLen 短于该字节数的消息不值得嵌入（「在」「好的」之类寒暄）。
const minMemorableLen = 8

// SetMessageEmbedding 落一条消息的向量与模型标签。
func (s *Store) SetMessageEmbedding(ctx context.Context, id int64, model string, vec []float32) error {
	return s.execOne(ctx,
		`UPDATE chat_messages SET embedding = $2, embed_model = $3 WHERE id = $1`, id, vec, model)
}

// MessagesNeedingEmbeddingAfter 从 id 游标后取尚未按当前模型嵌入的消息（回填用）。
// 只嵌有记忆价值的：长度达标即可，user/assistant 都要（决定可能出现在任一侧）。
func (s *Store) MessagesNeedingEmbeddingAfter(ctx context.Context, model string, afterID int64, limit int) ([]ChatMessage, error) {
	return s.queryMessages(ctx,
		`SELECT id, session_id, role, content, created_at FROM chat_messages
		 WHERE embed_model <> $1 AND id > $2 AND length(content) >= $3
		 ORDER BY id LIMIT $4`, model, afterID, minMemorableLen, limit)
}

// MessageVec 一条消息的 id + 向量（语义检索候选）。
type MessageVec struct {
	ID        int64
	Embedding []float32
}

// EmbeddedMessagesOfUser 取某用户名下会话中已按指定模型嵌入的消息向量。
// 只搜自己名下的会话：情景记忆是个人视角，不跨权限泄露他人对话。
func (s *Store) EmbeddedMessagesOfUser(ctx context.Context, model string, userID int64) ([]MessageVec, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.embedding FROM chat_messages m
		 JOIN chat_sessions cs ON cs.id = m.session_id
		 WHERE cs.user_id = $1 AND m.embed_model = $2 AND m.embedding IS NOT NULL`, userID, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageVec
	for rows.Next() {
		var mv MessageVec
		if err := rows.Scan(&mv.ID, &mv.Embedding); err != nil {
			return nil, err
		}
		out = append(out, mv)
	}
	return out, rows.Err()
}

// SearchMessagesOfUser 词法检索某用户名下会话的消息（新的在前）。
func (s *Store) SearchMessagesOfUser(ctx context.Context, userID int64, query string, limit int) ([]ChatMessage, error) {
	terms := searchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	args := []any{userID}
	var conds []string
	for _, t := range terms {
		args = append(args, "%"+escapeLike(t)+"%")
		conds = append(conds, fmt.Sprintf("m.content ILIKE $%d", len(args)))
	}
	args = append(args, limit)
	return s.queryMessages(ctx, fmt.Sprintf(
		`SELECT m.id, m.session_id, m.role, m.content, m.created_at FROM chat_messages m
		 JOIN chat_sessions cs ON cs.id = m.session_id
		 WHERE cs.user_id = $1 AND (%s)
		 ORDER BY m.id DESC LIMIT $%d`, strings.Join(conds, " OR "), len(args)), args...)
}

// MessagesByIDs 按 id 批量取消息，保持传入顺序（语义排序后回取详情）。
func (s *Store) MessagesByIDs(ctx context.Context, ids []int64) ([]ChatMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ms, err := s.queryMessages(ctx,
		`SELECT id, session_id, role, content, created_at FROM chat_messages WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]ChatMessage, len(ms))
	for _, m := range ms {
		byID[m.ID] = m
	}
	out := make([]ChatMessage, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Store) queryMessages(ctx context.Context, sql string, args ...any) ([]ChatMessage, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ms []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, rows.Err()
}

// MessagesOf 会话消息（升序），limit<=0 取全部。
func (s *Store) MessagesOf(ctx context.Context, sessionID int64, limit int) ([]ChatMessage, error) {
	return s.MessagesAfter(ctx, sessionID, 0, limit)
}

// MessagesAfter 会话中 ID 大于 afterID 的消息（升序），limit>0 时取其中最近 limit 条。
// 滚动摘要压缩用：afterID = 会话的 summary_upto。
func (s *Store) MessagesAfter(ctx context.Context, sessionID, afterID int64, limit int) ([]ChatMessage, error) {
	sql := `SELECT id, session_id, role, content, created_at FROM chat_messages
	        WHERE session_id = $1 AND id > $2 ORDER BY id`
	args := []any{sessionID, afterID}
	if limit > 0 {
		// 取最近 limit 条但保持升序。
		sql = `SELECT id, session_id, role, content, created_at FROM (
		         SELECT id, session_id, role, content, created_at FROM chat_messages
		         WHERE session_id = $1 AND id > $2 ORDER BY id DESC LIMIT $3
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

// OldestMessagesAfter 会话中 ID 大于 afterID 的【最早】limit 条消息（升序）。
// 压缩用：折叠最旧的一批未折叠消息。
func (s *Store) OldestMessagesAfter(ctx context.Context, sessionID, afterID int64, limit int) ([]ChatMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, session_id, role, content, created_at FROM chat_messages
		 WHERE session_id = $1 AND id > $2 ORDER BY id LIMIT $3`, sessionID, afterID, limit)
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

// CountMessagesAfter 会话中 ID 大于 afterID 的消息条数（未折叠消息数）。
func (s *Store) CountMessagesAfter(ctx context.Context, sessionID, afterID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_messages WHERE session_id = $1 AND id > $2`, sessionID, afterID).Scan(&n)
	return n, err
}

// --- 审计 ---

// Audit 写一条审计记录（尽力而为：失败不阻断业务，调用方记日志即可）。
func (s *Store) Audit(ctx context.Context, userID int64, sessionID *int64, tool string, args json.RawMessage, result string, ok bool) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_log (user_id, session_id, tool, args, result, ok) VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, sessionID, tool, args, result, ok)
	return err
}

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ChatSession AI 会话。每个 (user, channel) 至多一个活跃会话；
// 群共享会话（channel 形如 telegram:group:<chatID>）则每个 channel 至多一个活跃。
type ChatSession struct {
	ID      int64
	UserID  int64
	Channel string
	Engine  string
	// EngineRef is retained for legacy rows and non-chat callers. Interactive
	// chat now scopes Eino sessions to conversation_turns and ignores this field.
	EngineRef string
	Active    bool
	CreatedAt time.Time
	// 滚动摘要压缩：Summary 是较早对话的压缩摘要，SummaryUpto 是已折叠的
	// 最后一条消息 ID。重放历史 = 摘要 + SummaryUpto 之后的消息。
	Summary     string
	SummaryUpto int64
}

// ChatMessage 会话内一条文本消息。
type ChatMessage struct {
	ID                 int64
	SessionID          int64
	Role               string
	Content            string
	Provider           string
	ExternalChatRef    string
	ExternalMessageRef string
	ActorUserID        *int64
	ExternalActorRef   string
	ActorDisplayName   string
	ReplyToExternalRef string
	ThreadRef          string
	SourceCreatedAt    *time.Time
	SourceMetadata     json.RawMessage
	CreatedAt          time.Time
}

const nonGroupChannelSQL = `NOT (
	split_part(cs.channel, ':', 2) = 'group'
	AND split_part(cs.channel, ':', 3) <> ''
)`

// MessageEnvelope is provider provenance for one human-originated message.
// Stable identifiers live here; display names remain point-in-time labels.
type MessageEnvelope struct {
	Provider           string
	ExternalChatRef    string
	ExternalMessageRef string
	ActorUserID        *int64
	ExternalActorRef   string
	ActorDisplayName   string
	ReplyToExternalRef string
	ThreadRef          string
	SourceCreatedAt    *time.Time
	Metadata           json.RawMessage
}

const chatMessageCols = `id, session_id, role, content, provider, external_chat_ref,
external_message_ref, actor_user_id, external_actor_ref, actor_display_name,
reply_to_external_ref, thread_ref, source_created_at, source_metadata, created_at`

const chatMessageColsM = `m.id, m.session_id, m.role, m.content, m.provider, m.external_chat_ref,
m.external_message_ref, m.actor_user_id, m.external_actor_ref, m.actor_display_name,
m.reply_to_external_ref, m.thread_ref, m.source_created_at, m.source_metadata, m.created_at`

func scanChatMessage(row interface{ Scan(...any) error }) (*ChatMessage, error) {
	var m ChatMessage
	if err := row.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Provider, &m.ExternalChatRef,
		&m.ExternalMessageRef, &m.ActorUserID, &m.ExternalActorRef, &m.ActorDisplayName,
		&m.ReplyToExternalRef, &m.ThreadRef, &m.SourceCreatedAt, &m.SourceMetadata, &m.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &m, nil
}

// EventAt is the authoritative business timestamp. Provider-originated events
// keep their signed source time; locally created messages use insertion time.
func (m ChatMessage) EventAt() time.Time {
	if m.SourceCreatedAt != nil && !m.SourceCreatedAt.IsZero() {
		return *m.SourceCreatedAt
	}
	return m.CreatedAt
}

// IsGroupChannel reports whether a gateway channel uses the shared-group
// namespace. Gateways may vary by provider; provider:group:reference is the
// stable protocol contract used by sessions and semantic permission scopes.
func IsGroupChannel(channel string) bool {
	provider, rest, ok := strings.Cut(strings.TrimSpace(channel), ":")
	if !ok || provider == "" {
		return false
	}
	kind, reference, ok := strings.Cut(rest, ":")
	return ok && kind == "group" && strings.TrimSpace(reference) != ""
}

// IsInternalChannel identifies scheduler/runtime sessions that are execution
// plumbing rather than human conversation history.
func IsInternalChannel(channel string) bool {
	return strings.HasPrefix(strings.TrimSpace(channel), "internal:")
}

func (s *Store) ChatMessageByID(ctx context.Context, id int64) (*ChatMessage, error) {
	return scanChatMessage(s.pool.QueryRow(ctx,
		`SELECT `+chatMessageCols+` FROM chat_messages WHERE id = $1`, id))
}

// ChannelMessagePage 是某个渠道在绝对时间范围内的消息页。查询跨越该渠道的
// 历史会话，因此 /new 只重置模型上下文，不会让群消息事实从审计查询里消失。
type ChannelMessagePage struct {
	Messages []ChatMessage
	Total    int64
	// NextCursor 非零表示还有更早消息；传给 ListChannelMessagesPage 可继续读取。
	NextCursor int64
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

// LatestSessionByChannel returns the newest session regardless of active state.
// Gateways use it to restore a durable group transcript after administrative
// cleanup or a failed reset left the channel without an active session.
func (s *Store) LatestSessionByChannel(ctx context.Context, channel string) (*ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE channel = $1 ORDER BY id DESC LIMIT 1`, channel))
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

// AppendMessageWithEnvelope idempotently records provider-originated messages.
// Webhook retries return the existing row; edited payloads refresh the content
// and display snapshot without changing the stable source identity.
func (s *Store) AppendMessageWithEnvelope(ctx context.Context, sessionID int64, role, content string, envelope MessageEnvelope) (int64, error) {
	provider := strings.TrimSpace(envelope.Provider)
	chatRef := strings.TrimSpace(envelope.ExternalChatRef)
	messageRef := strings.TrimSpace(envelope.ExternalMessageRef)
	if provider == "" || chatRef == "" || messageRef == "" {
		return s.AppendMessage(ctx, sessionID, role, content)
	}
	metadata := envelope.Metadata
	if len(metadata) == 0 || !json.Valid(metadata) {
		metadata = json.RawMessage(`{}`)
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO chat_messages (
		   session_id, role, content, provider, external_chat_ref, external_message_ref,
		   actor_user_id, external_actor_ref, actor_display_name, reply_to_external_ref,
		   thread_ref, source_created_at, source_metadata
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		 ON CONFLICT (provider, external_chat_ref, external_message_ref)
		   WHERE provider <> '' AND external_chat_ref <> '' AND external_message_ref <> ''
		 DO UPDATE SET content = EXCLUDED.content,
		               actor_user_id = COALESCE(EXCLUDED.actor_user_id, chat_messages.actor_user_id),
		               external_actor_ref = EXCLUDED.external_actor_ref,
		               actor_display_name = EXCLUDED.actor_display_name,
		               reply_to_external_ref = EXCLUDED.reply_to_external_ref,
		               thread_ref = EXCLUDED.thread_ref,
		               source_created_at = EXCLUDED.source_created_at,
		               source_metadata = EXCLUDED.source_metadata
		 RETURNING id`,
		sessionID, role, content, provider, chatRef, messageRef, envelope.ActorUserID,
		strings.TrimSpace(envelope.ExternalActorRef), strings.TrimSpace(envelope.ActorDisplayName),
		strings.TrimSpace(envelope.ReplyToExternalRef), strings.TrimSpace(envelope.ThreadRef),
		envelope.SourceCreatedAt, metadata).Scan(&id)
	return id, wrapErr(err)
}

// SetMessageEnvelope attaches provider provenance to a message created by the
// durable conversation-turn transaction.
func (s *Store) SetMessageEnvelope(ctx context.Context, messageID int64, envelope MessageEnvelope) error {
	metadata := envelope.Metadata
	if len(metadata) == 0 || !json.Valid(metadata) {
		metadata = json.RawMessage(`{}`)
	}
	return s.execOne(ctx,
		`UPDATE chat_messages
		    SET provider = $2, external_chat_ref = $3, external_message_ref = $4,
		        actor_user_id = $5, external_actor_ref = $6, actor_display_name = $7,
		        reply_to_external_ref = $8, thread_ref = $9, source_created_at = $10,
		        source_metadata = $11
		  WHERE id = $1`, messageID, strings.TrimSpace(envelope.Provider),
		strings.TrimSpace(envelope.ExternalChatRef), strings.TrimSpace(envelope.ExternalMessageRef),
		envelope.ActorUserID, strings.TrimSpace(envelope.ExternalActorRef),
		strings.TrimSpace(envelope.ActorDisplayName), strings.TrimSpace(envelope.ReplyToExternalRef),
		strings.TrimSpace(envelope.ThreadRef), envelope.SourceCreatedAt, metadata)
}

// --- 会话情景记忆（episodic memory）：消息级语义检索 ---

// SetMessageEmbedding 落一条消息的向量与模型标签。
func (s *Store) SetMessageEmbedding(ctx context.Context, id int64, model string, vec []float32) error {
	return s.execOne(ctx,
		`UPDATE chat_messages SET embedding = $2, embed_model = $3 WHERE id = $1`, id, vec, model)
}

// MarkMessageVectorIndexed records successful external-vector indexing while
// keeping PostgreSQL free of duplicate vector payloads.
func (s *Store) MarkMessageVectorIndexed(ctx context.Context, id int64, model string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE chat_messages SET embedding = NULL, embed_model = $2
		  WHERE id = $1 AND (embedding IS NOT NULL OR embed_model IS DISTINCT FROM $2)`, id, model)
	return wrapErr(err)
}

// ClearLegacyMessageEmbeddings removes vector payloads left by the legacy
// PostgreSQL index after a complete external-index reconciliation.
func (s *Store) ClearLegacyMessageEmbeddings(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_messages SET embedding = NULL WHERE embedding IS NOT NULL`)
	return wrapErr(err)
}

// MessageSemanticDocument carries the permission scope needed as Qdrant
// payload metadata. Message content remains authoritative in PostgreSQL.
type MessageSemanticDocument struct {
	ChatMessage
	UserID          int64
	Channel         string
	PreviousRole    string
	PreviousContent string
}

// ContextContent is the retrieval representation of a message. PostgreSQL
// keeps the original message untouched; only semantic hits receive the nearby
// context that made them match in the first place.
func (m MessageSemanticDocument) ContextContent() string {
	current := strings.TrimSpace(m.Content)
	previous := strings.TrimSpace(m.PreviousContent)
	if previous == "" {
		return current
	}
	return fmt.Sprintf("current_%s: %s\nprevious_%s: %s",
		strings.TrimSpace(m.Role), current,
		strings.TrimSpace(m.PreviousRole), previous)
}

func (s *Store) MessageSemanticDocumentByID(ctx context.Context, id int64) (*MessageSemanticDocument, error) {
	doc, err := scanMessageSemanticDocument(s.pool.QueryRow(ctx,
		`SELECT `+chatMessageColsM+`, cs.user_id, cs.channel,
		        COALESCE(prev.role, ''), COALESCE(prev.content, '')
		   FROM chat_messages m JOIN chat_sessions cs ON cs.id = m.session_id
		   LEFT JOIN LATERAL (
		       SELECT p.role, p.content FROM chat_messages p
		        WHERE p.session_id = m.session_id AND p.id < m.id AND p.context_eligible
		        ORDER BY p.id DESC LIMIT 1
		   ) prev ON TRUE
		  WHERE m.id = $1 AND m.context_eligible`, id))
	if err != nil {
		return nil, wrapErr(err)
	}
	return doc, nil
}

func scanMessageSemanticDocument(row interface{ Scan(...any) error }) (*MessageSemanticDocument, error) {
	var doc MessageSemanticDocument
	err := row.Scan(&doc.ID, &doc.SessionID, &doc.Role, &doc.Content, &doc.Provider, &doc.ExternalChatRef,
		&doc.ExternalMessageRef, &doc.ActorUserID, &doc.ExternalActorRef, &doc.ActorDisplayName,
		&doc.ReplyToExternalRef, &doc.ThreadRef, &doc.SourceCreatedAt, &doc.SourceMetadata, &doc.CreatedAt,
		&doc.UserID, &doc.Channel, &doc.PreviousRole, &doc.PreviousContent)
	return &doc, err
}

// SemanticMessagesAfter scans every non-empty message for Qdrant
// reconciliation rather than trusting the legacy embed_model marker. Even a
// one-character acknowledgement can be relevant when reconstructing a
// decision or delivery timeline.
func (s *Store) SemanticMessagesAfter(ctx context.Context, afterID int64, limit int) ([]MessageSemanticDocument, error) {
	return s.querySemanticMessages(ctx, `m.id > $1`, afterID, limit)
}

// SemanticMessagesNeedingIndexAfter consumes the durable external-index
// marker. A separate periodic reconciliation intentionally ignores this marker
// so a restored or cleared Qdrant collection still self-heals.
func (s *Store) SemanticMessagesNeedingIndexAfter(ctx context.Context, marker string, afterID int64, limit int) ([]MessageSemanticDocument, error) {
	return s.querySemanticMessages(ctx, `m.embed_model <> $1 AND m.id > $2`, marker, afterID, limit)
}

func (s *Store) querySemanticMessages(ctx context.Context, predicate string, args ...any) ([]MessageSemanticDocument, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+chatMessageColsM+`, cs.user_id, cs.channel,
		        COALESCE(prev.role, ''), COALESCE(prev.content, '')
		   FROM chat_messages m JOIN chat_sessions cs ON cs.id = m.session_id
		   LEFT JOIN LATERAL (
		       SELECT p.role, p.content FROM chat_messages p
		        WHERE p.session_id = m.session_id AND p.id < m.id AND p.context_eligible
		        ORDER BY p.id DESC LIMIT 1
		   ) prev ON TRUE
		  WHERE `+predicate+` AND m.context_eligible AND btrim(m.content) <> '' AND cs.channel NOT LIKE 'internal:%'
		  ORDER BY m.id LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageSemanticDocument
	for rows.Next() {
		doc, err := scanMessageSemanticDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *doc)
	}
	return out, rows.Err()
}

func (s *Store) SemanticMessageIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.id FROM chat_messages m JOIN chat_sessions cs ON cs.id = m.session_id
		 WHERE m.context_eligible AND btrim(m.content) <> '' AND cs.channel NOT LIKE 'internal:%' ORDER BY m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MessagesNeedingEmbeddingAfter 从 id 游标后取尚未按当前模型嵌入的非空消息
// （回填用）。user/assistant 都要，事实和确认可能出现在任一侧。
func (s *Store) MessagesNeedingEmbeddingAfter(ctx context.Context, model string, afterID int64, limit int) ([]MessageSemanticDocument, error) {
	return s.querySemanticMessages(ctx,
		`(m.embed_model <> $1 OR m.embedding IS NULL) AND m.id > $2`, model, afterID, limit)
}

// MessageVec 一条消息的 id + 向量（语义检索候选）。
type MessageVec struct {
	ID        int64
	Embedding []float32
}

type ChatMessageIndexStats struct {
	Total   int64 `json:"total"`
	Indexed int64 `json:"indexed"`
	Pending int64 `json:"pending"`
}

func (s *Store) ChatMessageIndexStats(ctx context.Context, marker string) (ChatMessageIndexStats, error) {
	var stats ChatMessageIndexStats
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE btrim(m.content) <> ''),
		        count(*) FILTER (WHERE btrim(m.content) <> '' AND m.embed_model = $1)
		   FROM chat_messages m JOIN chat_sessions cs ON cs.id = m.session_id
		  WHERE cs.channel NOT LIKE 'internal:%' AND m.context_eligible`, marker).Scan(&stats.Total, &stats.Indexed)
	stats.Pending = max(0, stats.Total-stats.Indexed)
	return stats, err
}

// embeddedMessagesCap 单次语义检索加载的向量上限（最近优先）：消息随年月无限
// 增长，全量加载迟早把单次查询拖到几百 MB；两万条 ≈ 覆盖数月对话，足够
// 「上个月聊过什么」场景。更早的历史词法检索仍可命中。
const embeddedMessagesCap = 20000

// EmbeddedMessagesOfUser 取某用户名下会话中已按指定模型嵌入的消息向量（最近优先）。
// 只搜自己名下的会话：情景记忆是个人视角，不跨权限泄露他人对话。
func (s *Store) EmbeddedMessagesOfUser(ctx context.Context, model string, userID int64) ([]MessageVec, error) {
	return s.embeddedMessagesOfUser(ctx, model, userID, "")
}

// EmbeddedUserMessagesOfUser is the PostgreSQL-vector fallback used by
// automatic recall. Assistant outputs remain searchable explicitly, but are
// not treated as authoritative memory candidates.
func (s *Store) EmbeddedUserMessagesOfUser(ctx context.Context, model string, userID int64) ([]MessageVec, error) {
	return s.embeddedMessagesOfUser(ctx, model, userID, "user")
}

func (s *Store) embeddedMessagesOfUser(ctx context.Context, model string, userID int64, role string) ([]MessageVec, error) {
	rolePredicate := ""
	args := []any{userID, model, embeddedMessagesCap}
	if role != "" {
		args = append(args, role)
		rolePredicate = " AND m.role = $4"
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.id, m.embedding FROM chat_messages m
		 JOIN chat_sessions cs ON cs.id = m.session_id
		 WHERE cs.user_id = $1 AND `+nonGroupChannelSQL+` AND cs.channel NOT LIKE 'internal:%'
		   AND m.context_eligible AND m.embed_model = $2 AND m.embedding IS NOT NULL
		   `+rolePredicate+`
		 ORDER BY m.id DESC LIMIT $3`, args...)
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
	return s.searchMessagesOfUser(ctx, userID, query, limit, "")
}

// SearchUserMessagesOfUser restricts automatic memory candidates to what the
// human actually said. Explicit history tools still use SearchMessagesOfUser
// and can inspect both sides of the transcript.
func (s *Store) SearchUserMessagesOfUser(ctx context.Context, userID int64, query string, limit int) ([]ChatMessage, error) {
	return s.searchMessagesOfUser(ctx, userID, query, limit, "user")
}

func (s *Store) searchMessagesOfUser(ctx context.Context, userID int64, query string, limit int, role string) ([]ChatMessage, error) {
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
	rolePredicate := ""
	if role != "" {
		args = append(args, role)
		rolePredicate = fmt.Sprintf(" AND m.role = $%d", len(args))
	}
	args = append(args, limit)
	return s.queryMessages(ctx, fmt.Sprintf(
		`SELECT `+chatMessageColsM+` FROM chat_messages m
		 JOIN chat_sessions cs ON cs.id = m.session_id
		 WHERE cs.user_id = $1 AND `+nonGroupChannelSQL+` AND cs.channel NOT LIKE 'internal:%%'
		   AND m.context_eligible%s AND (%s)
		 ORDER BY m.id DESC LIMIT $%d`, rolePredicate, strings.Join(conds, " OR "), len(args)), args...)
}

// SearchMessagesOfChannel is the lexical side of shared-group retrieval.
func (s *Store) SearchMessagesOfChannel(ctx context.Context, channel, query string, limit int) ([]ChatMessage, error) {
	return s.searchMessagesOfChannel(ctx, channel, query, limit, "")
}

func (s *Store) SearchUserMessagesOfChannel(ctx context.Context, channel, query string, limit int) ([]ChatMessage, error) {
	return s.searchMessagesOfChannel(ctx, channel, query, limit, "user")
}

func (s *Store) searchMessagesOfChannel(ctx context.Context, channel, query string, limit int, role string) ([]ChatMessage, error) {
	terms := searchTerms(query)
	if len(terms) == 0 || !IsGroupChannel(channel) {
		return nil, nil
	}
	args := []any{strings.TrimSpace(channel)}
	var conds []string
	for _, term := range terms {
		args = append(args, "%"+escapeLike(term)+"%")
		conds = append(conds, fmt.Sprintf("m.content ILIKE $%d", len(args)))
	}
	rolePredicate := ""
	if role != "" {
		args = append(args, role)
		rolePredicate = fmt.Sprintf(" AND m.role = $%d", len(args))
	}
	args = append(args, limit)
	return s.queryMessages(ctx, fmt.Sprintf(
		`SELECT `+chatMessageColsM+` FROM chat_messages m
		 JOIN chat_sessions cs ON cs.id = m.session_id
		 WHERE cs.channel = $1 AND m.context_eligible%s AND (%s)
		 ORDER BY m.id DESC LIMIT $%d`, rolePredicate, strings.Join(conds, " OR "), len(args)), args...)
}

// MessagesByIDs 按 id 批量取消息，保持传入顺序（语义排序后回取详情）。
func (s *Store) MessagesByIDs(ctx context.Context, ids []int64) ([]ChatMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	docs, err := s.messageSemanticDocumentsByIDs(ctx, ids, 0, "", "")
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]ChatMessage, len(docs))
	for _, doc := range docs {
		message := doc.ChatMessage
		message.Content = doc.ContextContent()
		byID[message.ID] = message
	}
	out := make([]ChatMessage, 0, len(ids))
	for _, id := range ids {
		if m, ok := byID[id]; ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// MessagesByIDsForUser re-checks ownership and the private-session scope after
// a vector hit. Group sessions keep their creator in user_id for attribution,
// so ownership alone is not enough to authorize private-memory retrieval.
func (s *Store) MessagesByIDsForUser(ctx context.Context, userID int64, ids []int64) ([]ChatMessage, error) {
	if len(ids) == 0 || userID <= 0 {
		return nil, nil
	}
	docs, err := s.messageSemanticDocumentsByIDs(ctx, ids, userID, "", "")
	if err != nil {
		return nil, err
	}
	return orderedMessages(ids, docs, true), nil
}

func (s *Store) UserMessagesByIDsForUser(ctx context.Context, userID int64, ids []int64) ([]ChatMessage, error) {
	if len(ids) == 0 || userID <= 0 {
		return nil, nil
	}
	docs, err := s.messageSemanticDocumentsByIDs(ctx, ids, userID, "", "user")
	if err != nil {
		return nil, err
	}
	return orderedMessages(ids, docs, false), nil
}

// MessagesByIDsForChannel re-checks the shared channel after a vector hit.
func (s *Store) MessagesByIDsForChannel(ctx context.Context, channel string, ids []int64) ([]ChatMessage, error) {
	if len(ids) == 0 || !IsGroupChannel(channel) {
		return nil, nil
	}
	docs, err := s.messageSemanticDocumentsByIDs(ctx, ids, 0, strings.TrimSpace(channel), "")
	if err != nil {
		return nil, err
	}
	return orderedMessages(ids, docs, true), nil
}

func (s *Store) UserMessagesByIDsForChannel(ctx context.Context, channel string, ids []int64) ([]ChatMessage, error) {
	if len(ids) == 0 || !IsGroupChannel(channel) {
		return nil, nil
	}
	docs, err := s.messageSemanticDocumentsByIDs(ctx, ids, 0, strings.TrimSpace(channel), "user")
	if err != nil {
		return nil, err
	}
	return orderedMessages(ids, docs, false), nil
}

func orderedMessages(ids []int64, docs []MessageSemanticDocument, includeContext bool) []ChatMessage {
	byID := make(map[int64]ChatMessage, len(docs))
	for _, doc := range docs {
		message := doc.ChatMessage
		if includeContext {
			message.Content = doc.ContextContent()
		}
		byID[message.ID] = message
	}
	out := make([]ChatMessage, 0, len(ids))
	for _, id := range ids {
		if message, ok := byID[id]; ok {
			out = append(out, message)
		}
	}
	return out
}

func (s *Store) messageSemanticDocumentsByIDs(ctx context.Context, ids []int64, userID int64, channel, role string) ([]MessageSemanticDocument, error) {
	args := []any{ids}
	predicates := []string{"cs.channel NOT LIKE 'internal:%'", "m.context_eligible"}
	if userID > 0 {
		args = append(args, userID)
		predicates = append(predicates, fmt.Sprintf("cs.user_id = $%d AND %s", len(args), nonGroupChannelSQL))
	}
	if channel != "" {
		args = append(args, channel)
		predicates = append(predicates, fmt.Sprintf("cs.channel = $%d", len(args)))
	}
	if role != "" {
		args = append(args, role)
		predicates = append(predicates, fmt.Sprintf("m.role = $%d", len(args)))
	}
	scopePredicate := " AND " + strings.Join(predicates, " AND ")
	rows, err := s.pool.Query(ctx,
		`SELECT `+chatMessageColsM+`, cs.user_id, cs.channel,
		        COALESCE(prev.role, ''), COALESCE(prev.content, '')
		   FROM chat_messages m JOIN chat_sessions cs ON cs.id = m.session_id
		   LEFT JOIN LATERAL (
		       SELECT p.role, p.content FROM chat_messages p
		        WHERE p.session_id = m.session_id AND p.id < m.id AND p.context_eligible
		        ORDER BY p.id DESC LIMIT 1
		   ) prev ON TRUE
		  WHERE m.id = ANY($1)`+scopePredicate, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageSemanticDocument
	for rows.Next() {
		doc, err := scanMessageSemanticDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *doc)
	}
	return out, rows.Err()
}

// ListChannelMessages 按渠道读取事件时间位于 [from, to) 的消息，返回最新 limit
// 条并按写入顺序排列。Total 是该范围完整数量，调用方可据此明确是否发生截断。
func (s *Store) ListChannelMessages(ctx context.Context, channel string, from, to time.Time, limit int) (ChannelMessagePage, error) {
	return s.ListChannelMessagesPage(ctx, channel, from, to, 0, limit)
}

// ListChannelMessagesPage 使用稳定消息 ID 游标向更早记录翻页。Total 是当前
// 游标之前仍符合时间范围的完整数量，不依赖 OFFSET，因此新消息不会造成跳页。
func (s *Store) ListChannelMessagesPage(ctx context.Context, channel string, from, to time.Time, beforeID int64, limit int) (ChannelMessagePage, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return ChannelMessagePage{}, nil
	}
	if from.IsZero() {
		from = time.Unix(0, 0).UTC()
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if !to.After(from) {
		return ChannelMessagePage{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if beforeID < 0 {
		beforeID = 0
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+chatMessageColsM+`, count(*) OVER ()
		   FROM chat_messages m
		   JOIN chat_sessions cs ON cs.id = m.session_id
		  WHERE cs.channel = $1
		    AND COALESCE(m.source_created_at, m.created_at) >= $2
		    AND COALESCE(m.source_created_at, m.created_at) < $3
		    AND ($4::bigint = 0 OR m.id < $4)
		  ORDER BY m.id DESC
		  LIMIT $5`, channel, from, to, beforeID, limit)
	if err != nil {
		return ChannelMessagePage{}, err
	}
	defer rows.Close()
	page := ChannelMessagePage{Messages: []ChatMessage{}}
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &m.Provider, &m.ExternalChatRef,
			&m.ExternalMessageRef, &m.ActorUserID, &m.ExternalActorRef, &m.ActorDisplayName,
			&m.ReplyToExternalRef, &m.ThreadRef, &m.SourceCreatedAt, &m.SourceMetadata, &m.CreatedAt,
			&page.Total); err != nil {
			return ChannelMessagePage{}, err
		}
		page.Messages = append(page.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return ChannelMessagePage{}, err
	}
	for left, right := 0, len(page.Messages)-1; left < right; left, right = left+1, right-1 {
		page.Messages[left], page.Messages[right] = page.Messages[right], page.Messages[left]
	}
	if page.Total > int64(len(page.Messages)) && len(page.Messages) > 0 {
		page.NextCursor = page.Messages[0].ID
	}
	return page, nil
}

func (s *Store) queryMessages(ctx context.Context, sql string, args ...any) ([]ChatMessage, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ms []ChatMessage
	for rows.Next() {
		m, err := scanChatMessage(rows)
		if err != nil {
			return nil, err
		}
		ms = append(ms, *m)
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
	sql := `SELECT ` + chatMessageCols + ` FROM chat_messages
	        WHERE session_id = $1 AND id > $2 AND context_eligible ORDER BY id`
	args := []any{sessionID, afterID}
	reverse := false
	if limit > 0 {
		// 先取最近 limit 条，扫描后恢复为会话顺序。
		sql = `SELECT ` + chatMessageCols + ` FROM chat_messages
		         WHERE session_id = $1 AND id > $2 AND context_eligible ORDER BY id DESC LIMIT $3`
		args = append(args, limit)
		reverse = true
	}
	msgs, err := s.queryMessages(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if reverse {
		slices.Reverse(msgs)
	}
	return msgs, nil
}

// MessagesBefore returns the most recent context-eligible messages after the
// summary boundary and strictly before one message. Interactive turns use it
// after atomically appending the current user message, so that message cannot
// be replayed both as History and UserText.
func (s *Store) MessagesBefore(ctx context.Context, sessionID, afterID, beforeID int64, limit int) ([]ChatMessage, error) {
	query := `SELECT ` + chatMessageCols + ` FROM chat_messages
	          WHERE session_id = $1 AND id > $2 AND id < $3 AND context_eligible
	          ORDER BY id`
	args := []any{sessionID, afterID, beforeID}
	reverse := false
	if limit > 0 {
		query = `SELECT ` + chatMessageCols + ` FROM chat_messages
		           WHERE session_id = $1 AND id > $2 AND id < $3 AND context_eligible
		           ORDER BY id DESC LIMIT $4`
		args = append(args, limit)
		reverse = true
	}
	msgs, err := s.queryMessages(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if reverse {
		slices.Reverse(msgs)
	}
	return msgs, nil
}

// OldestMessagesAfter 会话中 ID 大于 afterID 的【最早】limit 条消息（升序）。
// 压缩用：折叠最旧的一批未折叠消息。
func (s *Store) OldestMessagesAfter(ctx context.Context, sessionID, afterID int64, limit int) ([]ChatMessage, error) {
	return s.queryMessages(ctx,
		`SELECT `+chatMessageCols+` FROM chat_messages
		 WHERE session_id = $1 AND id > $2 AND context_eligible ORDER BY id LIMIT $3`, sessionID, afterID, limit)
}

// CountMessagesAfter 会话中 ID 大于 afterID 的消息条数（未折叠消息数）。
func (s *Store) CountMessagesAfter(ctx context.Context, sessionID, afterID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_messages WHERE session_id = $1 AND id > $2 AND context_eligible`, sessionID, afterID).Scan(&n)
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

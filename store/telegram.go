package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"
)

const KVTelegramGroupPrefix = "telegram.group:"
const KVTelegramGroupListenPrefix = "tg_listen:"
const kvTelegramGroupMembershipPrefix = "telegram.bot_group_membership:"
const KVTelegramGroupLastMessagePrefix = "telegram.group.last_message:"
const KVTelegramGroupSeenMemberPrefix = "telegram.group.seen_member:"
const KVTelegramGroupAutoInvitePrefix = "telegram.group.auto_invite:"
const KVTelegramGroupMonitorPrefix = "telegram.group_monitor:"
const KVTelegramPendingEmployeeInvitePrefix = "telegram.pending_employee_invite:"

// TelegramGroupState 记录 bot 与 Telegram 群的接入事实。
// 这是系统事实状态，不是聊天记忆；AI 回答群接入问题时应以它为准。
type TelegramGroupState struct {
	ChatID    int64     `json:"chat_id"`
	Title     string    `json:"title"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Listen    bool      `json:"listen"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TelegramGroupSeenMember struct {
	ChatID    int64     `json:"chat_id"`
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	Username  string    `json:"username"`
	IsBot     bool      `json:"is_bot"`
	LastSeen  time.Time `json:"last_seen"`
	LastText  string    `json:"last_text,omitempty"`
	MessageID int       `json:"message_id,omitempty"`
}

type TelegramPendingEmployeeInvite struct {
	TelegramUserID int64     `json:"telegram_user_id"`
	GroupChatID    int64     `json:"group_chat_id"`
	Key            string    `json:"key"`
	Name           string    `json:"name"`
	CreatedBy      int64     `json:"created_by"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// TelegramGroupMonitor 保存群事件监控配置、持久批次游标和分析租约。
// Buffer 仅为兼容旧数据保留，新消息正文统一从群共享事实流读取。
type TelegramGroupMonitor struct {
	ChatID            int64     `json:"chat_id"`
	Enabled           bool      `json:"enabled"`
	GroupTitle        string    `json:"group_title,omitempty"`
	Instruction       string    `json:"instruction,omitempty"`
	NotifyUserID      int64     `json:"notify_user_id"`
	CreatedBy         int64     `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	LastCheckedAt     time.Time `json:"last_checked_at,omitempty"`
	LastNotifiedAt    time.Time `json:"last_notified_at,omitempty"`
	BatchStartedAt    time.Time `json:"batch_started_at,omitempty"`
	PendingCount      int       `json:"pending_count,omitempty"`
	AnalysisOwner     string    `json:"analysis_owner,omitempty"`
	AnalysisStartedAt time.Time `json:"analysis_started_at,omitempty"`
	AnalysisThrough   time.Time `json:"analysis_through,omitempty"`
	AnalysisFailures  int       `json:"analysis_failures,omitempty"`
	Buffer            []string  `json:"buffer,omitempty"`
}

func telegramGroupKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupPrefix, chatID)
}

func telegramGroupMembershipKey(chatID int64) string {
	return fmt.Sprintf("%s%d", kvTelegramGroupMembershipPrefix, chatID)
}

func TelegramGroupListenKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupListenPrefix, chatID)
}

func TelegramGroupAutoInviteKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupAutoInvitePrefix, chatID)
}

func TelegramGroupMonitorKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupMonitorPrefix, chatID)
}

func TelegramPendingEmployeeInviteKey(tgUserID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramPendingEmployeeInvitePrefix, tgUserID)
}

func telegramGroupLastMessageKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupLastMessagePrefix, chatID)
}

func telegramGroupSeenMemberKey(chatID, userID int64) string {
	return fmt.Sprintf("%s%d:%d", KVTelegramGroupSeenMemberPrefix, chatID, userID)
}

func (s *Store) SaveTelegramGroupState(ctx context.Context, st TelegramGroupState) error {
	if st.ChatID == 0 {
		return nil
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.SetKV(ctx, telegramGroupKey(st.ChatID), string(raw))
}

// TransitionTelegramGroupMembership atomically records whether the bot is an
// active member of a group. Telegram emits both my_chat_member and a service
// message for one join; only the first observer of an inactive -> active
// transition receives joined=true. Existing deployments are initialized from
// their persisted group state, so a later role update does not look like a new
// join.
func (s *Store) TransitionTelegramGroupMembership(ctx context.Context, st TelegramGroupState, active bool) (joined bool, err error) {
	if st.ChatID == 0 {
		return false, nil
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	membershipKey := telegramGroupMembershipKey(st.ChatID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, membershipKey); err != nil {
		return false, err
	}

	var previous TelegramGroupState
	previousFound := false
	var previousRaw string
	err = tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key = $1`, telegramGroupKey(st.ChatID)).Scan(&previousRaw)
	if err == nil {
		if err := json.Unmarshal([]byte(previousRaw), &previous); err != nil {
			return false, fmt.Errorf("解析 Telegram 群状态: %w", err)
		}
		previousFound = true
	} else if wrapErr(err) != ErrNotFound {
		return false, err
	}

	var marker string
	markerFound := false
	err = tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key = $1`, membershipKey).Scan(&marker)
	if err == nil {
		markerFound = true
	} else if wrapErr(err) != ErrNotFound {
		return false, err
	}

	previousActive := marker == "1"
	if !markerFound && previousFound {
		previousActive = telegramGroupStatusActive(previous.Status)
	}
	joined = active && !previousActive

	listen := false
	if active {
		listen = joined
		if !joined {
			var listenRaw string
			err = tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key = $1`, TelegramGroupListenKey(st.ChatID)).Scan(&listenRaw)
			switch {
			case err == nil:
				listen = listenRaw == "1"
			case wrapErr(err) == ErrNotFound:
				listen = previousFound && previous.Listen
			default:
				return false, err
			}
		}
	}
	st.Listen = listen
	raw, err := json.Marshal(st)
	if err != nil {
		return false, err
	}
	marker = ""
	listenValue := ""
	if active {
		marker = "1"
	}
	if listen {
		listenValue = "1"
	}
	for key, value := range map[string]string{
		membershipKey:                     marker,
		TelegramGroupListenKey(st.ChatID): listenValue,
		telegramGroupKey(st.ChatID):       string(raw),
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO kv_state (key, value) VALUES ($1, $2)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return joined, nil
}

func telegramGroupStatusActive(status string) bool {
	switch status {
	case "member", "administrator", "creator", "owner", "restricted":
		return true
	default:
		return false
	}
}

func (s *Store) TelegramGroupState(ctx context.Context, chatID int64) (*TelegramGroupState, error) {
	raw, err := s.GetKV(ctx, telegramGroupKey(chatID))
	if err != nil || raw == "" {
		if err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	var st TelegramGroupState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) ListTelegramGroupStates(ctx context.Context, limit int) ([]TelegramGroupState, error) {
	fetchLimit := 1000
	if limit > fetchLimit {
		fetchLimit = limit
	}
	pairs, err := s.ListKVPrefix(ctx, KVTelegramGroupPrefix, fetchLimit)
	if err != nil {
		return nil, err
	}
	out := make([]TelegramGroupState, 0, len(pairs))
	for _, p := range pairs {
		var st TelegramGroupState
		if err := json.Unmarshal([]byte(p.Value), &st); err != nil {
			continue
		}
		if st.ChatID != 0 {
			out = append(out, st)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) SaveTelegramGroupLastMessage(ctx context.Context, chatID int64, messageID int) error {
	if chatID == 0 || messageID <= 0 {
		return nil
	}
	return s.SetKV(ctx, telegramGroupLastMessageKey(chatID), strconv.Itoa(messageID))
}

func (s *Store) TelegramGroupLastMessage(ctx context.Context, chatID int64) (int, error) {
	raw, err := s.GetKV(ctx, telegramGroupLastMessageKey(chatID))
	if err != nil {
		return 0, err
	}
	if raw == "" {
		return 0, ErrNotFound
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return 0, ErrNotFound
	}
	return id, nil
}

func (s *Store) SaveTelegramGroupSeenMember(ctx context.Context, m TelegramGroupSeenMember) error {
	if m.ChatID == 0 || m.UserID == 0 {
		return nil
	}
	if m.LastSeen.IsZero() {
		m.LastSeen = time.Now()
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.SetKV(ctx, telegramGroupSeenMemberKey(m.ChatID, m.UserID), string(raw))
}

func (s *Store) ListTelegramGroupSeenMembers(ctx context.Context, chatID int64, limit int) ([]TelegramGroupSeenMember, error) {
	fetchLimit := 1000
	if limit > fetchLimit {
		fetchLimit = limit
	}
	pairs, err := s.ListKVPrefix(ctx, fmt.Sprintf("%s%d:", KVTelegramGroupSeenMemberPrefix, chatID), fetchLimit)
	if err != nil {
		return nil, err
	}
	out := make([]TelegramGroupSeenMember, 0, len(pairs))
	for _, p := range pairs {
		var m TelegramGroupSeenMember
		if err := json.Unmarshal([]byte(p.Value), &m); err != nil {
			continue
		}
		if m.ChatID == chatID && m.UserID != 0 {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) SaveTelegramPendingEmployeeInvite(ctx context.Context, inv TelegramPendingEmployeeInvite) error {
	if inv.TelegramUserID == 0 || inv.Key == "" {
		return nil
	}
	raw, err := json.Marshal(inv)
	if err != nil {
		return err
	}
	return s.SetKV(ctx, TelegramPendingEmployeeInviteKey(inv.TelegramUserID), string(raw))
}

func (s *Store) TelegramPendingEmployeeInvite(ctx context.Context, tgUserID int64) (*TelegramPendingEmployeeInvite, error) {
	raw, err := s.GetKV(ctx, TelegramPendingEmployeeInviteKey(tgUserID))
	if err != nil || raw == "" {
		if err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	var inv TelegramPendingEmployeeInvite
	if err := json.Unmarshal([]byte(raw), &inv); err != nil {
		return nil, err
	}
	if !inv.ExpiresAt.IsZero() && time.Now().After(inv.ExpiresAt) {
		_ = s.ClearTelegramPendingEmployeeInvite(ctx, tgUserID)
		return nil, ErrNotFound
	}
	return &inv, nil
}

func (s *Store) ClearTelegramPendingEmployeeInvite(ctx context.Context, tgUserID int64) error {
	return s.SetKV(ctx, TelegramPendingEmployeeInviteKey(tgUserID), "")
}

func (s *Store) SaveTelegramGroupMonitor(ctx context.Context, mon TelegramGroupMonitor) error {
	if mon.ChatID == 0 {
		return nil
	}
	normalizeTelegramGroupMonitor(&mon)
	raw, err := json.Marshal(mon)
	if err != nil {
		return err
	}
	return s.SetKV(ctx, TelegramGroupMonitorKey(mon.ChatID), string(raw))
}

// UpdateTelegramGroupMonitor 用 PostgreSQL 行锁串行化监控状态的读改写，防止配置、
// 新消息和分析确认并发到达时互相覆盖。
func (s *Store) UpdateTelegramGroupMonitor(ctx context.Context, chatID int64, update func(*TelegramGroupMonitor) error) (*TelegramGroupMonitor, error) {
	if chatID == 0 {
		return nil, fmt.Errorf("telegram group monitor requires chat id")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) // no-op after commit
	key := TelegramGroupMonitorKey(chatID)
	if _, err := tx.Exec(ctx,
		`INSERT INTO kv_state (key, value) VALUES ($1, '') ON CONFLICT (key) DO NOTHING`, key); err != nil {
		return nil, err
	}
	var raw string
	if err := tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key = $1 FOR UPDATE`, key).Scan(&raw); err != nil {
		return nil, err
	}
	mon := TelegramGroupMonitor{ChatID: chatID}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &mon); err != nil {
			return nil, err
		}
		mon.ChatID = chatID
	}
	if update != nil {
		if err := update(&mon); err != nil {
			return nil, err
		}
	}
	normalizeTelegramGroupMonitor(&mon)
	encoded, err := json.Marshal(mon)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE kv_state SET value = $2 WHERE key = $1`, key, string(encoded)); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &mon, nil
}

func normalizeTelegramGroupMonitor(mon *TelegramGroupMonitor) {
	if mon == nil {
		return
	}
	now := time.Now()
	if mon.CreatedAt.IsZero() {
		mon.CreatedAt = now
	}
	if mon.UpdatedAt.IsZero() {
		mon.UpdatedAt = now
	}
	if len(mon.Buffer) > 30 {
		mon.Buffer = mon.Buffer[len(mon.Buffer)-30:]
	}
	for i, line := range mon.Buffer {
		runes := []rune(line)
		if len(runes) > 240 {
			mon.Buffer[i] = string(runes[:240])
		}
	}
}

func (s *Store) TelegramGroupMonitor(ctx context.Context, chatID int64) (*TelegramGroupMonitor, error) {
	raw, err := s.GetKV(ctx, TelegramGroupMonitorKey(chatID))
	if err != nil || raw == "" {
		if err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	var mon TelegramGroupMonitor
	if err := json.Unmarshal([]byte(raw), &mon); err != nil {
		return nil, err
	}
	if mon.ChatID == 0 {
		return nil, ErrNotFound
	}
	return &mon, nil
}

func (s *Store) ListTelegramGroupMonitors(ctx context.Context, limit int) ([]TelegramGroupMonitor, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	pairs, err := s.ListKVPrefix(ctx, KVTelegramGroupMonitorPrefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]TelegramGroupMonitor, 0, len(pairs))
	for _, pair := range pairs {
		var mon TelegramGroupMonitor
		if json.Unmarshal([]byte(pair.Value), &mon) == nil && mon.ChatID != 0 {
			out = append(out, mon)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

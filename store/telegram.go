package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	ChatID                 int64     `json:"chat_id"`
	Title                  string    `json:"title"`
	Type                   string    `json:"type"`
	Status                 string    `json:"status"`
	Listen                 bool      `json:"listen"`
	LastMembershipUpdateID int64     `json:"last_membership_update_id,omitempty"`
	UpdatedAt              time.Time `json:"updated_at"`
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, telegramGroupMembershipKey(st.ChatID)); err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	listenValue := ""
	if st.Listen {
		listenValue = "1"
	}
	for key, value := range map[string]string{
		telegramGroupKey(st.ChatID):       string(raw),
		TelegramGroupListenKey(st.ChatID): listenValue,
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO kv_state (key, value) VALUES ($1,$2)
			 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SetTelegramGroupListen changes the operational listen switch and its
// denormalized group-state view in one transaction. All membership and
// observation writers use the same advisory lock, so concurrent updates cannot
// leave the two KV records disagreeing.
func (s *Store) SetTelegramGroupListen(ctx context.Context, chatID int64, listen bool) error {
	return s.updateTelegramGroupView(ctx, chatID, func(st *TelegramGroupState) {
		st.Listen = listen
	}, &listen)
}

// SetTelegramGroupTitle updates only the mutable display fact while preserving
// membership ordering and listener state owned by their dedicated paths.
func (s *Store) SetTelegramGroupTitle(ctx context.Context, chatID int64, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrNotFound
	}
	return s.updateTelegramGroupView(ctx, chatID, func(st *TelegramGroupState) {
		st.Title = title
	}, nil)
}

func (s *Store) updateTelegramGroupView(ctx context.Context, chatID int64, update func(*TelegramGroupState), listen *bool) error {
	if chatID == 0 || update == nil {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, telegramGroupMembershipKey(chatID)); err != nil {
		return err
	}
	var raw string
	if err := tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key=$1`, telegramGroupKey(chatID)).Scan(&raw); err != nil {
		return wrapErr(err)
	}
	var st TelegramGroupState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return fmt.Errorf("解析 Telegram 群状态: %w", err)
	}
	update(&st)
	st.UpdatedAt = time.Now()
	rawBytes, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO kv_state (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, telegramGroupKey(chatID), string(rawBytes)); err != nil {
		return err
	}
	if listen != nil {
		value := ""
		if *listen {
			value = "1"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO kv_state (key, value) VALUES ($1,$2)
			 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, TelegramGroupListenKey(chatID), value); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// MergeTelegramGroupObservation updates non-authoritative facts observed on an
// ordinary group message. It shares the membership advisory lock and never
// overwrites a role/leave transition recorded by my_chat_member or a service
// update.
func (s *Store) MergeTelegramGroupObservation(ctx context.Context, st TelegramGroupState) error {
	if st.ChatID == 0 {
		return nil
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	membershipKey := telegramGroupMembershipKey(st.ChatID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, membershipKey); err != nil {
		return err
	}
	var raw string
	err = tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key=$1`, telegramGroupKey(st.ChatID)).Scan(&raw)
	if err == nil {
		var current TelegramGroupState
		if err := json.Unmarshal([]byte(raw), &current); err != nil {
			return fmt.Errorf("解析 Telegram 群状态: %w", err)
		}
		st.Status = current.Status
		st.LastMembershipUpdateID = current.LastMembershipUpdateID
		if strings.TrimSpace(st.Title) == "" {
			st.Title = current.Title
		}
		if strings.TrimSpace(st.Type) == "" {
			st.Type = current.Type
		}
	} else if wrapErr(err) != ErrNotFound {
		return err
	}
	var listenRaw string
	err = tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key=$1`, TelegramGroupListenKey(st.ChatID)).Scan(&listenRaw)
	if err == nil {
		st.Listen = listenRaw == "1"
	} else if wrapErr(err) != ErrNotFound {
		return err
	}
	rawBytes, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO kv_state (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, telegramGroupKey(st.ChatID), string(rawBytes)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ApplyTelegramBotMembershipUpdate atomically applies an ordered
// my_chat_member update. becameActive is true only when the persisted aggregate
// moves from inactive to active; retries and role refinements do not create
// another lifecycle transition.
func (s *Store) ApplyTelegramBotMembershipUpdate(ctx context.Context, st TelegramGroupState, active bool) (becameActive bool, err error) {
	if st.ChatID == 0 || st.LastMembershipUpdateID <= 0 || strings.TrimSpace(st.Status) == "" {
		return false, fmt.Errorf("应用 Telegram bot 成员更新: chat_id、update_id 和 status 必填")
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
	if previousFound && st.LastMembershipUpdateID > 0 && previous.LastMembershipUpdateID > st.LastMembershipUpdateID {
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
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
	becameActive = active && !previousActive

	listen := false
	if active {
		listen = becameActive
		if !becameActive {
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
	return becameActive, nil
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
	_, err := s.pool.Exec(ctx,
		`INSERT INTO kv_state (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = CASE
			WHEN kv_state.value ~ '^[0-9]{1,10}$'
			 AND kv_state.value::bigint >= EXCLUDED.value::bigint
			THEN kv_state.value
			ELSE EXCLUDED.value
		 END`, telegramGroupLastMessageKey(chatID), strconv.Itoa(messageID))
	return err
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
	key := telegramGroupSeenMemberKey(m.ChatID, m.UserID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return err
	}

	var currentRaw string
	err = tx.QueryRow(ctx, `SELECT value FROM kv_state WHERE key=$1`, key).Scan(&currentRaw)
	if err == nil {
		var current TelegramGroupSeenMember
		if err := json.Unmarshal([]byte(currentRaw), &current); err != nil {
			return fmt.Errorf("解析 Telegram 群成员状态: %w", err)
		}
		incomingMessageID := m.MessageID
		if current.LastSeen.After(m.LastSeen) {
			m.LastSeen = current.LastSeen
		}
		if incomingMessageID == 0 || current.MessageID > incomingMessageID {
			m.LastText = current.LastText
			m.MessageID = current.MessageID
		}
		if incomingMessageID > 0 && incomingMessageID < current.MessageID {
			m.Name = current.Name
			m.Username = current.Username
			m.IsBot = current.IsBot
		}
	} else if wrapErr(err) != ErrNotFound {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO kv_state (key, value) VALUES ($1,$2)
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, key, string(raw)); err != nil {
		return err
	}
	return tx.Commit(ctx)
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

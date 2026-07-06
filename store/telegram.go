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
const KVTelegramGroupLastMessagePrefix = "telegram.group.last_message:"
const KVTelegramGroupSeenMemberPrefix = "telegram.group.seen_member:"
const KVTelegramGroupAutoInvitePrefix = "telegram.group.auto_invite:"
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

func telegramGroupKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupPrefix, chatID)
}

func TelegramGroupListenKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupListenPrefix, chatID)
}

func TelegramGroupAutoInviteKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupAutoInvitePrefix, chatID)
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

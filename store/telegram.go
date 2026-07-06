package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const KVTelegramGroupPrefix = "telegram.group:"
const KVTelegramGroupListenPrefix = "tg_listen:"

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

func telegramGroupKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupPrefix, chatID)
}

func TelegramGroupListenKey(chatID int64) string {
	return fmt.Sprintf("%s%d", KVTelegramGroupListenPrefix, chatID)
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

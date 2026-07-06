package store

import (
	"context"
	"strings"
)

const (
	// KVAIStreamReasoning 保存运行时 AI 流式推理展示开关；配置文件只做默认值。
	KVAIStreamReasoning = "settings.ai.stream_reasoning"
	// KVAIModel 保存运行时主模型覆盖；空值表示使用配置文件默认模型。
	KVAIModel = "settings.ai.model"
	// KVTelegramBotUsername 缓存 Telegram bot username，用于生成员工邀请 deep link。
	KVTelegramBotUsername = "telegram.bot_username"
)

// BoolSetting 把 kv_state 里的布尔字符串解析成 bool；空值或未知值走 fallback。
func BoolSetting(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "t", "yes", "y", "on", "enabled":
		return true
	case "0", "false", "f", "no", "n", "off", "disabled":
		return false
	default:
		return fallback
	}
}

// GetKV 读键值；不存在返回空串。
func (s *Store) GetKV(ctx context.Context, key string) (string, error) {
	var v string
	err := s.pool.QueryRow(ctx, `SELECT value FROM kv_state WHERE key = $1`, key).Scan(&v)
	if err != nil {
		if wrapErr(err) == ErrNotFound {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// SetKV 写键值（upsert）。
func (s *Store) SetKV(ctx context.Context, key, value string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO kv_state (key, value) VALUES ($1, $2)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
	return err
}

// KVPair 是 kv_state 的一行。
type KVPair struct {
	Key   string
	Value string
}

// ListKVPrefix 按 key 前缀列出 kv_state。前缀必须是调用方控制的固定字符串。
func (s *Store) ListKVPrefix(ctx context.Context, prefix string, limit int) ([]KVPair, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM kv_state WHERE key LIKE $1 ORDER BY key LIMIT $2`, prefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KVPair{}
	for rows.Next() {
		var p KVPair
		if err := rows.Scan(&p.Key, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

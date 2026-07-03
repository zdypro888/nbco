package store

import "context"

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

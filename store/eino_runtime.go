package store

import (
	"context"
	"time"
)

// EinoRuntimeStats is aggregate operational metadata only. Event payloads are
// deliberately not exposed through the control-center endpoint.
type EinoRuntimeStats struct {
	Sessions     int64      `json:"sessions"`
	Events       int64      `json:"events"`
	Checkpoints  int64      `json:"checkpoints"`
	StorageBytes int64      `json:"storage_bytes"`
	LastEventAt  *time.Time `json:"last_event_at,omitempty"`
}

func (s *Store) EinoRuntimeStats(ctx context.Context) (EinoRuntimeStats, error) {
	var stats EinoRuntimeStats
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(DISTINCT session_id) FROM eino_session_events),
		  (SELECT count(*) FROM eino_session_events),
		  (SELECT count(*) FROM eino_checkpoints),
		  pg_total_relation_size('eino_session_events') + pg_total_relation_size('eino_checkpoints'),
		  (SELECT max(created_at) FROM eino_session_events)
	`).Scan(&stats.Sessions, &stats.Events, &stats.Checkpoints, &stats.StorageBytes, &stats.LastEventAt)
	return stats, err
}

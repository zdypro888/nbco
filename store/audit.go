package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// AuditActivity is one canonical tool invocation recorded by the shared tool
// wrapper. OK reports whether the handler returned a Go error; callers must
// still read Result because domain-level rejections are intentionally returned
// as model-readable text.
type AuditActivity struct {
	ID        int64
	UserID    int64
	UserName  string
	SessionID *int64
	Tool      string
	Args      json.RawMessage
	Result    string
	OK        bool
	CreatedAt time.Time
}

type AuditActivityFilter struct {
	UserID    int64
	SessionID int64
	Tool      string
	Query     string
	Since     *time.Time
	Limit     int
}

type ToolUsageStat struct {
	Tool       string    `json:"tool"`
	Calls      int64     `json:"calls"`
	Failures   int64     `json:"failures"`
	LastUsedAt time.Time `json:"last_used_at"`
}

func (s *Store) ToolUsageStatsSince(ctx context.Context, since time.Time) (map[string]ToolUsageStat, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT tool, count(*), count(*) FILTER (WHERE NOT ok), max(created_at)
		   FROM audit_log WHERE created_at >= $1 GROUP BY tool`, since)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	out := make(map[string]ToolUsageStat)
	for rows.Next() {
		var stat ToolUsageStat
		if err := rows.Scan(&stat.Tool, &stat.Calls, &stat.Failures, &stat.LastUsedAt); err != nil {
			return nil, wrapErr(err)
		}
		out[stat.Tool] = stat
	}
	return out, wrapErr(rows.Err())
}

// ListAuditActivity queries the low-level tool ledger. It is deliberately
// domain-neutral so new tools automatically become observable without adding
// another status-specific API.
func (s *Store) ListAuditActivity(ctx context.Context, f AuditActivityFilter) ([]*AuditActivity, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	toolName := strings.TrimSpace(f.Tool)
	query := strings.TrimSpace(f.Query)
	if query != "" {
		query = "%" + escapeLike(query) + "%"
	}
	var since any
	if f.Since != nil {
		since = *f.Since
	}
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.user_id, COALESCE(u.name, ''), a.session_id,
		        a.tool, a.args, a.result, a.ok, a.created_at
		   FROM audit_log a
		   LEFT JOIN users u ON u.id = a.user_id
		  WHERE ($1::bigint = 0 OR a.user_id = $1)
		    AND ($2::bigint = 0 OR a.session_id = $2)
		    AND ($3::text = '' OR lower(a.tool) = lower($3))
		    AND ($4::timestamptz IS NULL OR a.created_at >= $4)
		    AND ($5::text = '' OR a.tool ILIKE $5 ESCAPE '\' OR a.result ILIKE $5 ESCAPE '\'
		         OR a.args::text ILIKE $5 ESCAPE '\' OR COALESCE(u.name, '') ILIKE $5 ESCAPE '\')
		  ORDER BY a.id DESC
		  LIMIT $6`, f.UserID, f.SessionID, toolName, since, query, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()

	var out []*AuditActivity
	for rows.Next() {
		var item AuditActivity
		var sessionID sql.NullInt64
		if err := rows.Scan(&item.ID, &item.UserID, &item.UserName, &sessionID,
			&item.Tool, &item.Args, &item.Result, &item.OK, &item.CreatedAt); err != nil {
			return nil, wrapErr(err)
		}
		if sessionID.Valid {
			id := sessionID.Int64
			item.SessionID = &id
		}
		out = append(out, &item)
	}
	return out, wrapErr(rows.Err())
}

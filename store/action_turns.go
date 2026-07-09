package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type ActionTurnInput struct {
	UserID           int64
	SessionID        *int64
	Channel          string
	UserTextHash     string
	UserTextExcerpt  string
	ReplyExcerpt     string
	RequiresAction   bool
	Intent           string
	ExpectedTools    []string
	Evidence         map[string]any
	Outcome          string
	ToolCount        int
	SuccessToolCount int
}

type ActionTurn struct {
	ID               int64           `json:"id"`
	UserID           int64           `json:"user_id"`
	SessionID        *int64          `json:"session_id,omitempty"`
	Channel          string          `json:"channel"`
	UserTextHash     string          `json:"user_text_hash"`
	UserTextExcerpt  string          `json:"user_text_excerpt"`
	ReplyExcerpt     string          `json:"reply_excerpt"`
	RequiresAction   bool            `json:"requires_action"`
	Intent           string          `json:"intent"`
	ExpectedTools    []string        `json:"expected_tools"`
	Evidence         json.RawMessage `json:"evidence"`
	Outcome          string          `json:"outcome"`
	ToolCount        int             `json:"tool_count"`
	SuccessToolCount int             `json:"success_tool_count"`
	CreatedAt        time.Time       `json:"created_at"`
}

func (s *Store) RecordActionTurn(ctx context.Context, in ActionTurnInput) error {
	evidence := in.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	expectedTools := in.ExpectedTools
	if expectedTools == nil {
		expectedTools = []string{}
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO action_turns
		   (user_id, session_id, channel, user_text_hash, user_text_excerpt, reply_excerpt,
		    requires_action, intent, expected_tools, evidence, outcome, tool_count, success_tool_count)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		in.UserID, in.SessionID, in.Channel, in.UserTextHash,
		truncateRunes(in.UserTextExcerpt, 500), truncateRunes(in.ReplyExcerpt, 500),
		in.RequiresAction, truncateRunes(in.Intent, 500), expectedTools, raw, truncateRunes(in.Outcome, 80),
		in.ToolCount, in.SuccessToolCount)
	return wrapErr(err)
}

func (s *Store) ListActionTurns(ctx context.Context, userID int64, limit int) ([]*ActionTurn, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, session_id, channel, user_text_hash, user_text_excerpt, reply_excerpt,
		        requires_action, intent, expected_tools, evidence, outcome, tool_count, success_tool_count, created_at
		   FROM action_turns
		  WHERE ($1::bigint = 0 OR user_id = $1)
		  ORDER BY id DESC
		  LIMIT $2`, userID, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	var out []*ActionTurn
	for rows.Next() {
		var a ActionTurn
		var sid sql.NullInt64
		if err := rows.Scan(&a.ID, &a.UserID, &sid, &a.Channel, &a.UserTextHash, &a.UserTextExcerpt,
			&a.ReplyExcerpt, &a.RequiresAction, &a.Intent, &a.ExpectedTools, &a.Evidence, &a.Outcome,
			&a.ToolCount, &a.SuccessToolCount, &a.CreatedAt); err != nil {
			return nil, wrapErr(err)
		}
		if sid.Valid {
			v := sid.Int64
			a.SessionID = &v
		}
		out = append(out, &a)
	}
	return out, wrapErr(rows.Err())
}

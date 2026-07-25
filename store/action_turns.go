package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type ActionTurnInput struct {
	ConversationTurnID *int64
	UserID             int64
	SessionID          *int64
	Channel            string
	UserTextHash       string
	UserTextExcerpt    string
	ReplyExcerpt       string
	RequiresAction     bool
	Intent             string
	ExpectedTools      []string
	Evidence           map[string]any
	Outcome            string
	ToolCount          int
	SuccessToolCount   int
}

type ActionTurn struct {
	ID                 int64           `json:"id"`
	ConversationTurnID *int64          `json:"conversation_turn_id,omitempty"`
	UserID             int64           `json:"user_id"`
	SessionID          *int64          `json:"session_id,omitempty"`
	Channel            string          `json:"channel"`
	UserTextHash       string          `json:"user_text_hash"`
	UserTextExcerpt    string          `json:"user_text_excerpt"`
	ReplyExcerpt       string          `json:"reply_excerpt"`
	RequiresAction     bool            `json:"requires_action"`
	Intent             string          `json:"intent"`
	ExpectedTools      []string        `json:"expected_tools"`
	Evidence           json.RawMessage `json:"evidence"`
	Outcome            string          `json:"outcome"`
	ToolCount          int             `json:"tool_count"`
	SuccessToolCount   int             `json:"success_tool_count"`
	CreatedAt          time.Time       `json:"created_at"`
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
		    requires_action, intent, expected_tools, evidence, outcome, tool_count, success_tool_count,
		    conversation_turn_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		in.UserID, in.SessionID, in.Channel, in.UserTextHash,
		truncateRunes(in.UserTextExcerpt, 500), truncateRunes(in.ReplyExcerpt, 500),
		in.RequiresAction, truncateRunes(in.Intent, 500), expectedTools, raw, truncateRunes(in.Outcome, 80),
		in.ToolCount, in.SuccessToolCount, in.ConversationTurnID)
	return wrapErr(err)
}

func (s *Store) ListActionTurns(ctx context.Context, userID int64, limit int) ([]*ActionTurn, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, conversation_turn_id, user_id, session_id, channel, user_text_hash, user_text_excerpt, reply_excerpt,
		        requires_action, intent, expected_tools, evidence, outcome, tool_count, success_tool_count, created_at
		   FROM action_turns
		  WHERE ($1::bigint = 0 OR user_id = $1)
		  ORDER BY id DESC
		  LIMIT $2`, userID, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	return scanActionTurns(rows)
}

// ListActionTurnsBySession returns the current caller's recent execution facts
// for one exact conversation. It is used for capability continuity, not as a
// replacement for mutable domain state.
func (s *Store) ListActionTurnsBySession(ctx context.Context, userID, sessionID int64, limit int) ([]*ActionTurn, error) {
	if limit <= 0 || limit > 50 {
		limit = 8
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, conversation_turn_id, user_id, session_id, channel, user_text_hash, user_text_excerpt, reply_excerpt,
		        requires_action, intent, expected_tools, evidence, outcome, tool_count, success_tool_count, created_at
		   FROM action_turns
		  WHERE user_id = $1 AND session_id = $2
		  ORDER BY id DESC
		  LIMIT $3`, userID, sessionID, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	return scanActionTurns(rows)
}

func scanActionTurns(rows pgx.Rows) ([]*ActionTurn, error) {
	defer rows.Close()
	var out []*ActionTurn
	for rows.Next() {
		var a ActionTurn
		var conversationTurnID, sid sql.NullInt64
		if err := rows.Scan(&a.ID, &conversationTurnID, &a.UserID, &sid, &a.Channel, &a.UserTextHash, &a.UserTextExcerpt,
			&a.ReplyExcerpt, &a.RequiresAction, &a.Intent, &a.ExpectedTools, &a.Evidence, &a.Outcome,
			&a.ToolCount, &a.SuccessToolCount, &a.CreatedAt); err != nil {
			return nil, wrapErr(err)
		}
		if conversationTurnID.Valid {
			v := conversationTurnID.Int64
			a.ConversationTurnID = &v
		}
		if sid.Valid {
			v := sid.Int64
			a.SessionID = &v
		}
		out = append(out, &a)
	}
	return out, wrapErr(rows.Err())
}

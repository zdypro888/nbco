package store

import (
	"context"
	"encoding/json"
)

type ActionTurnInput struct {
	UserID         int64
	SessionID      *int64
	Channel        string
	UserTextHash   string
	RequiresAction bool
	Intent         string
	ExpectedTools  []string
	Evidence       map[string]any
	Outcome        string
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
		   (user_id, session_id, channel, user_text_hash, requires_action, intent, expected_tools, evidence, outcome)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		in.UserID, in.SessionID, in.Channel, in.UserTextHash, in.RequiresAction,
		truncateRunes(in.Intent, 500), expectedTools, raw, truncateRunes(in.Outcome, 80))
	return wrapErr(err)
}

package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var ErrApprovalExpired = errors.New("approval expired")

// ConversationApprovalTool keeps exact private arguments out of client authority.
type ConversationApprovalTool struct {
	InterruptID string          `json:"interrupt_id"`
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	ToolName    string          `json:"tool_name"`
	Args        json.RawMessage `json:"args"`
	ArgsHash    string          `json:"args_hash"`
}

type ConversationApproval struct {
	ID               int64
	TurnID           int64
	CheckPointID     string
	EngineSession    string
	Tools            []ConversationApprovalTool
	Decisions        map[string]bool
	Progress         json.RawMessage
	Status           string
	ConsentMessageID *int64
	ExpiresAt        time.Time
	ConsentKey       []byte `json:"-"`
}

const conversationApprovalCols = `id, turn_id, checkpoint_id, engine_session, tools, decisions,
 progress, status, consent_message_id, expires_at, consent_key`

func scanConversationApproval(row interface{ Scan(...any) error }) (*ConversationApproval, error) {
	var a ConversationApproval
	var tools, decisions []byte
	if err := row.Scan(&a.ID, &a.TurnID, &a.CheckPointID, &a.EngineSession, &tools, &decisions,
		&a.Progress, &a.Status, &a.ConsentMessageID, &a.ExpiresAt, &a.ConsentKey); err != nil {
		return nil, wrapErr(err)
	}
	if err := json.Unmarshal(tools, &a.Tools); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(decisions, &a.Decisions); err != nil {
		return nil, err
	}
	return &a, nil
}

func validateApprovalTools(tools []ConversationApprovalTool) error {
	if len(tools) == 0 || len(tools) > 32 {
		return ErrConflict
	}
	seen := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.InterruptID) == "" || seen[tool.InterruptID] ||
			strings.TrimSpace(tool.ToolName) == "" || tool.ArgsHash == "" || !json.Valid(tool.Args) {
			return ErrConflict
		}
		seen[tool.InterruptID] = true
	}
	return nil
}

// PauseConversationTurn publishes a native checkpoint only after its private
// state is durable. A stale executor cannot pause a terminal or newer attempt.
func (s *Store) PauseConversationTurn(ctx context.Context, turnID int64, attempt int,
	checkpointID, engineSession string, tools []ConversationApprovalTool, progress json.RawMessage) (*ConversationApproval, error) {
	if turnID <= 0 || attempt <= 0 || strings.TrimSpace(checkpointID) == "" || strings.TrimSpace(engineSession) == "" {
		return nil, ErrConflict
	}
	if err := validateApprovalTools(tools); err != nil {
		return nil, err
	}
	if !json.Valid(progress) {
		return nil, ErrConflict
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return nil, err
	}
	consentKey := make([]byte, 32)
	if _, err := rand.Read(consentKey); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE conversation_turns SET status='awaiting_approval', engine_session=$3, updated_at=now()
	 WHERE id=$1 AND status='running' AND attempts=$2`, turnID, attempt, engineSession)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrConflict
	}
	approval, err := scanConversationApproval(tx.QueryRow(ctx, `INSERT INTO conversation_approvals
	 (turn_id,checkpoint_id,engine_session,tools,progress,consent_key) VALUES ($1,$2,$3,$4,$5,$6)
	 RETURNING `+conversationApprovalCols, turnID, checkpointID, engineSession, raw, progress, consentKey))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return approval, nil
}

func (s *Store) PendingConversationApprovals(ctx context.Context, userID int64, channel string) ([]ConversationApproval, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE conversation_turns t SET status='failed',last_error='approval expired',completed_at=now(),updated_at=now()
	 WHERE t.user_id=$1 AND t.channel=$2 AND t.status='awaiting_approval'
	 AND EXISTS(SELECT 1 FROM conversation_approvals a WHERE a.turn_id=t.id AND a.status='pending' AND a.expires_at<=now())`, userID, channel); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+prefixedApprovalCols()+` FROM conversation_approvals a
	 JOIN conversation_turns t ON t.id=a.turn_id
	 WHERE t.user_id=$1 AND t.channel=$2 AND t.status='awaiting_approval' AND a.status='pending'
	 AND a.expires_at>now() ORDER BY a.id`, userID, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversationApproval
	for rows.Next() {
		a, err := scanConversationApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func prefixedApprovalCols() string {
	cols := strings.Split(conversationApprovalCols, ",")
	for i := range cols {
		cols[i] = "a." + strings.TrimSpace(cols[i])
	}
	return strings.Join(cols, ",")
}

// DecideConversationApproval serializes card decisions and claims the original
// execution exactly once when the whole batch has been decided. It never
// reclaims a running attempt: uncertain side effects remain fail-closed.
func (s *Store) DecideConversationApproval(ctx context.Context, userID int64, channel string,
	approvalID int64, decisions map[string]bool) (*ConversationApproval, *ConversationTurn, bool, error) {
	if len(decisions) == 0 {
		return nil, nil, false, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Lock order matches PauseConversationTurn: turn first, approval second.
	turn, err := scanConversationTurn(tx.QueryRow(ctx, `SELECT `+conversationTurnCols+`
	 FROM conversation_turns WHERE id=(SELECT turn_id FROM conversation_approvals WHERE id=$1)
	 AND user_id=$2 AND channel=$3 FOR UPDATE`, approvalID, userID, channel))
	if err != nil {
		return nil, nil, false, err
	}
	a, err := scanConversationApproval(tx.QueryRow(ctx, `SELECT `+conversationApprovalCols+`
	 FROM conversation_approvals WHERE id=$1 FOR UPDATE`, approvalID))
	if err != nil {
		return nil, nil, false, err
	}
	if turn.Status != "awaiting_approval" || a.Status != "pending" || a.EngineSession != turn.EngineSession || turn.UserMessageID == nil {
		return nil, nil, false, ErrConflict
	}
	var unexpired, active bool
	if err := tx.QueryRow(ctx, `SELECT $1::timestamptz>now(), EXISTS(SELECT 1 FROM users WHERE id=$2 AND status='active')`,
		a.ExpiresAt, userID).Scan(&unexpired, &active); err != nil {
		return nil, nil, false, err
	}
	if !unexpired {
		if _, err := tx.Exec(ctx, `UPDATE conversation_turns SET status='failed',last_error='approval expired',completed_at=now(),updated_at=now() WHERE id=$1`, turn.ID); err != nil {
			return nil, nil, false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, err
		}
		return nil, nil, false, ErrApprovalExpired
	}
	if !active {
		return nil, nil, false, ErrConflict
	}
	if err := mergeApprovalDecisions(a, decisions); err != nil {
		return nil, nil, false, err
	}
	rawDecisions, err := json.Marshal(a.Decisions)
	if err != nil {
		return nil, nil, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_approvals SET decisions=$2 WHERE id=$1`, a.ID, rawDecisions); err != nil {
		return nil, nil, false, err
	}
	if len(a.Decisions) != len(a.Tools) {
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, false, err
		}
		return a, turn, false, nil
	}
	var consentMessageID int64
	consent, _ := json.Marshal(map[string]any{"approval_id": a.ID, "decisions": a.Decisions})
	if err := tx.QueryRow(ctx, `INSERT INTO chat_messages(session_id,role,content)
	 VALUES($1,'user',$2) RETURNING id`, turn.SessionID, "[Authenticated tool consent] "+string(consent)).Scan(&consentMessageID); err != nil {
		return nil, nil, false, err
	}
	// The normal domain gate still consumes exact, expiring, cross-message rows.
	for _, tool := range a.Tools {
		if !a.Decisions[tool.InterruptID] {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM pending_approvals WHERE user_id=$1 AND tool=$2 AND args_hash=$3 AND session_id=$4`,
			userID, tool.ToolName, tool.ArgsHash, turn.SessionID); err != nil {
			return nil, nil, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pending_approvals(user_id,tool,args_hash,session_id,requested_message_id,expires_at,consent_message_id)
		 VALUES($1,$2,$3,$4,$5,$6,$7)`, userID, tool.ToolName, tool.ArgsHash, turn.SessionID, *turn.UserMessageID, a.ExpiresAt, consentMessageID); err != nil {
			return nil, nil, false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_approvals SET status='claimed',consent_message_id=$2,claimed_at=now() WHERE id=$1`, a.ID, consentMessageID); err != nil {
		return nil, nil, false, err
	}
	turn, err = scanConversationTurn(tx.QueryRow(ctx, `UPDATE conversation_turns SET status='running',attempts=attempts+1,started_at=now(),updated_at=now()
	 WHERE id=$1 RETURNING `+conversationTurnCols, turn.ID))
	if err != nil {
		return nil, nil, false, err
	}
	a.Status = "claimed"
	a.ConsentMessageID = &consentMessageID
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, false, err
	}
	return a, turn, true, nil
}

func mergeApprovalDecisions(a *ConversationApproval, incoming map[string]bool) error {
	known := make(map[string]bool, len(a.Tools))
	for _, tool := range a.Tools {
		known[tool.InterruptID] = true
	}
	for id, decision := range incoming {
		if !known[id] {
			return ErrConflict
		}
		if previous, exists := a.Decisions[id]; exists && previous != decision {
			return ErrConflict
		}
	}
	if a.Decisions == nil {
		a.Decisions = make(map[string]bool)
	}
	for id, decision := range incoming {
		a.Decisions[id] = decision
	}
	return nil
}

// FailConversationTurnAttempt prevents late completion cleanup from mutating a
// newer execution. Running attempts are not automatically resumed or reclaimed.
func (s *Store) FailConversationTurnAttempt(ctx context.Context, turnID int64, attempt int, cause string) error {
	_, err := s.pool.Exec(ctx, `UPDATE conversation_turns SET status='failed',last_error=$3,completed_at=now(),updated_at=now()
	 WHERE id=$1 AND status='running' AND attempts=$2`, turnID, attempt, truncateRunes(strings.TrimSpace(cause), 2000))
	return wrapErr(err)
}

// RevokeConversationApprovalGrants closes unconsumed grants when a resumed
// attempt exits, so a failed execution cannot authorize a later fresh turn.
func (s *Store) RevokeConversationApprovalGrants(ctx context.Context, approvalID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM pending_approvals p USING conversation_approvals a, conversation_turns t
	 WHERE a.id=$1 AND a.turn_id=t.id AND p.user_id=t.user_id AND p.session_id=t.session_id
	 AND p.consent_message_id=a.consent_message_id
	 AND p.requested_message_id=t.user_message_id
	 AND EXISTS(SELECT 1 FROM jsonb_array_elements(a.tools) tool WHERE tool->>'tool_name'=p.tool AND tool->>'args_hash'=p.args_hash)`, approvalID)
	return wrapErr(err)
}

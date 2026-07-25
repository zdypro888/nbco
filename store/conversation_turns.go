package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const conversationTurnCols = `id, user_id, session_id, channel, user_message_id,
	assistant_message_id, status, delivery_status, result_text, engine_session,
	attempts, delivery_attempts, last_error, started_at, completed_at, delivered_at, updated_at`

var (
	ErrTurnInProgress = errors.New("对话轮次仍在执行")
	ErrTurnFailed     = errors.New("对话轮次已失败")
)

// ConversationTurn is the durable lifecycle shared by Agent execution,
// canonical chat history, audit/learning projections and channel delivery.
type ConversationTurn struct {
	ID                 int64
	UserID             int64
	SessionID          int64
	Channel            string
	UserMessageID      *int64
	AssistantMessageID *int64
	Status             string
	DeliveryStatus     string
	ResultText         string
	EngineSession      string
	Attempts           int
	DeliveryAttempts   int
	LastError          string
	StartedAt          time.Time
	CompletedAt        *time.Time
	DeliveredAt        *time.Time
	UpdatedAt          time.Time
}

type ConversationTurnCompletion struct {
	TurnID         int64
	AssistantText  string
	ResultText     string
	EngineSession  string
	Action         *ActionTurnInput
	Usage          *AIUsage
	EnqueueMemory  bool
	MemoryEvidence string
	ExplicitMemory bool
}

func scanConversationTurn(row interface{ Scan(...any) error }) (*ConversationTurn, error) {
	var turn ConversationTurn
	if err := row.Scan(
		&turn.ID, &turn.UserID, &turn.SessionID, &turn.Channel, &turn.UserMessageID,
		&turn.AssistantMessageID, &turn.Status, &turn.DeliveryStatus, &turn.ResultText,
		&turn.EngineSession, &turn.Attempts, &turn.DeliveryAttempts, &turn.LastError,
		&turn.StartedAt, &turn.CompletedAt, &turn.DeliveredAt, &turn.UpdatedAt,
	); err != nil {
		return nil, wrapErr(err)
	}
	return &turn, nil
}

func conversationSourceHash(sourceKey string) (string, error) {
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return "", ErrNotFound
	}
	sum := sha256.Sum256([]byte(sourceKey))
	return hex.EncodeToString(sum[:]), nil
}

// BeginConversationTurn atomically claims an idempotency key and appends the
// canonical user message. created=false means the caller must inspect the
// returned terminal/running state instead of executing the Agent again.
func (s *Store) BeginConversationTurn(
	ctx context.Context,
	sourceKey string,
	userID, sessionID int64,
	channel, userText string,
) (turn *ConversationTurn, created bool, err error) {
	if userID <= 0 || sessionID <= 0 {
		return nil, false, ErrNotFound
	}
	sourceHash, err := conversationSourceHash(sourceKey)
	if err != nil {
		return nil, false, err
	}
	channel = strings.TrimSpace(channel)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	turn, err = scanConversationTurn(tx.QueryRow(ctx,
		`INSERT INTO conversation_turns (source_key_hash, user_id, session_id, channel)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_id, channel, source_key_hash) DO NOTHING
		 RETURNING `+conversationTurnCols,
		sourceHash, userID, sessionID, channel))
	if errors.Is(err, ErrNotFound) {
		turn, err = scanConversationTurn(tx.QueryRow(ctx,
			`SELECT `+conversationTurnCols+`
			   FROM conversation_turns
			  WHERE user_id = $1 AND channel = $2 AND source_key_hash = $3`,
			userID, channel, sourceHash))
		if err != nil {
			return nil, false, err
		}
		if turn.UserMessageID == nil {
			return nil, false, ErrConflict
		}
		var existingText string
		if err := tx.QueryRow(ctx,
			`SELECT content FROM chat_messages WHERE id = $1`, *turn.UserMessageID).
			Scan(&existingText); err != nil {
			return nil, false, wrapErr(err)
		}
		if existingText != userText {
			return nil, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return turn, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	var messageID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO chat_messages (session_id, role, content)
		 VALUES ($1, 'user', $2) RETURNING id`,
		sessionID, userText).Scan(&messageID); err != nil {
		return nil, false, wrapErr(err)
	}
	turn.UserMessageID = &messageID
	if _, err := tx.Exec(ctx,
		`UPDATE conversation_turns SET user_message_id = $2, updated_at = now() WHERE id = $1`,
		turn.ID, messageID); err != nil {
		return nil, false, wrapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return turn, true, nil
}

// CompleteConversationTurn atomically publishes every durable projection of a
// successful Agent result. A retry after commit returns the existing assistant
// message and never duplicates audit, usage or learning rows.
func (s *Store) CompleteConversationTurn(ctx context.Context, in ConversationTurnCompletion) (int64, error) {
	if in.TurnID <= 0 {
		return 0, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	turn, err := scanConversationTurn(tx.QueryRow(ctx,
		`SELECT `+conversationTurnCols+` FROM conversation_turns WHERE id = $1 FOR UPDATE`,
		in.TurnID))
	if err != nil {
		return 0, err
	}
	if turn.Status == "completed" && turn.AssistantMessageID != nil {
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return *turn.AssistantMessageID, nil
	}
	if turn.Status == "failed" {
		return 0, ErrTurnFailed
	}
	if turn.UserMessageID == nil {
		return 0, ErrNotFound
	}

	var assistantMessageID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO chat_messages (session_id, role, content)
		 VALUES ($1, 'assistant', $2) RETURNING id`,
		turn.SessionID, in.AssistantText).Scan(&assistantMessageID); err != nil {
		return 0, wrapErr(err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE conversation_turns
		    SET assistant_message_id = $2, status = 'completed', result_text = $3,
		        engine_session = $4, completed_at = now(), last_error = '', updated_at = now()
		  WHERE id = $1`,
		turn.ID, assistantMessageID, in.ResultText, strings.TrimSpace(in.EngineSession)); err != nil {
		return 0, wrapErr(err)
	}
	if in.Action != nil {
		action := *in.Action
		sessionID := turn.SessionID
		action.UserID = turn.UserID
		action.SessionID = &sessionID
		action.Channel = turn.Channel
		if err := insertActionTurn(ctx, tx, turn.ID, action); err != nil {
			return 0, err
		}
	}
	if in.Usage != nil && (in.Usage.InputTokens != 0 || in.Usage.OutputTokens != 0) {
		usage := *in.Usage
		sessionID := turn.SessionID
		usage.UserID = turn.UserID
		usage.SessionID = &sessionID
		if _, err := tx.Exec(ctx,
			`INSERT INTO ai_usage
			   (user_id, session_id, kind, model, input_tokens, output_tokens, goal_id, conversation_turn_id)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			usage.UserID, usage.SessionID, usage.Kind, usage.Model,
			usage.InputTokens, usage.OutputTokens, usage.GoalID, turn.ID); err != nil {
			return 0, wrapErr(err)
		}
	}
	if in.EnqueueMemory {
		if _, err := tx.Exec(ctx,
			`INSERT INTO memory_mining_jobs
			   (user_id, channel, session_id, user_message_id, assistant_message_id, tool_evidence, explicit_commit)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 ON CONFLICT (session_id, user_message_id) DO NOTHING`,
			turn.UserID, turn.Channel, turn.SessionID, *turn.UserMessageID, assistantMessageID,
			in.MemoryEvidence, in.ExplicitMemory); err != nil {
			return 0, wrapErr(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return assistantMessageID, nil
}

func insertActionTurn(ctx context.Context, tx pgx.Tx, conversationTurnID int64, in ActionTurnInput) error {
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
	_, err = tx.Exec(ctx,
		`INSERT INTO action_turns
		   (user_id, session_id, channel, user_text_hash, user_text_excerpt, reply_excerpt,
		    requires_action, intent, expected_tools, evidence, outcome, tool_count, success_tool_count,
		    conversation_turn_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		in.UserID, in.SessionID, in.Channel, in.UserTextHash,
		truncateRunes(in.UserTextExcerpt, 500), truncateRunes(in.ReplyExcerpt, 500),
		in.RequiresAction, truncateRunes(in.Intent, 500), expectedTools, raw,
		truncateRunes(in.Outcome, 80), in.ToolCount, in.SuccessToolCount, conversationTurnID)
	return wrapErr(err)
}

func (s *Store) FailConversationTurn(ctx context.Context, turnID int64, cause string) error {
	if turnID <= 0 {
		return ErrNotFound
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE conversation_turns
		    SET status = 'failed', last_error = $2, completed_at = now(), updated_at = now()
		  WHERE id = $1 AND status = 'running'`,
		turnID, truncateRunes(strings.TrimSpace(cause), 2000))
	return wrapErr(err)
}

// FailStaleConversationTurns closes claims that outlived the configured Agent
// deadline. They are never auto-replayed because an external side effect may
// have succeeded immediately before the process stopped.
func (s *Store) FailStaleConversationTurns(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversation_turns
		    SET status = 'failed',
		        last_error = 'agent process ended before durable completion',
		        completed_at = now(), updated_at = now()
		  WHERE status = 'running' AND started_at < now() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, wrapErr(err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) MarkConversationTurnDelivered(ctx context.Context, turnID int64) error {
	return s.execOne(ctx,
		`UPDATE conversation_turns
		    SET delivery_status = 'delivered', delivery_attempts = delivery_attempts + 1,
		        delivered_at = now(), last_error = '', updated_at = now()
		  WHERE id = $1 AND status = 'completed'`, turnID)
}

func (s *Store) MarkConversationTurnDeliveryFailed(ctx context.Context, turnID int64, cause string) error {
	return s.execOne(ctx,
		`UPDATE conversation_turns
		    SET delivery_status = 'failed', delivery_attempts = delivery_attempts + 1,
		        last_error = $2, updated_at = now()
		  WHERE id = $1 AND status = 'completed'`,
		turnID, truncateRunes(strings.TrimSpace(cause), 2000))
}

func (s *Store) ConversationTurnByID(ctx context.Context, id int64) (*ConversationTurn, error) {
	return scanConversationTurn(s.pool.QueryRow(ctx,
		`SELECT `+conversationTurnCols+` FROM conversation_turns WHERE id = $1`, id))
}

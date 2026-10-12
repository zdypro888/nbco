package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

type approvalResumeKey struct{}
type approvalResume struct {
	turn     *store.ConversationTurn
	approval *store.ConversationApproval
	progress *ai.TurnResult
}

func (o *Orchestrator) PendingApprovals(ctx context.Context, user *store.User, channel string) ([]store.ConversationApproval, error) {
	if user == nil || user.Status != store.UserActive {
		return nil, store.ErrConflict
	}
	return o.store.PendingConversationApprovals(ctx, user.ID, channel)
}

// ResumeApprovalWithExtension records exact authenticated decisions before
// entering the same orchestrator with the original turn and fresh scoped tools.
// A partial batch returns PendingApproval without invoking the engine.
func (o *Orchestrator) ResumeApprovalWithExtension(ctx context.Context, user *store.User, channel string,
	approvalID int64, decisions map[string]bool, onDelta func(string), extension TurnExtension) (reply TurnReply, err error) {
	if user == nil || user.Status != store.UserActive || isGroupChannel(channel) {
		return TurnReply{}, store.ErrConflict
	}
	ctx, cancel := context.WithTimeout(ctx, o.turnTimeout)
	defer cancel()
	release, err := o.locks.AcquireContext(ctx, user.ID)
	if err != nil {
		return TurnReply{}, err
	}
	defer release()
	fresh, err := o.store.UserByID(ctx, user.ID)
	if err != nil || fresh.Status != store.UserActive {
		return TurnReply{}, store.ErrConflict
	}
	approval, turn, claimed, err := o.store.DecideConversationApproval(ctx, user.ID, channel, approvalID, decisions)
	if err != nil {
		return TurnReply{}, err
	}
	if !claimed {
		return TurnReply{TurnID: turn.ID, PendingApproval: approval}, nil
	}
	defer func() {
		finalCtx, finalCancel := turnFinalizationContext(ctx)
		defer finalCancel()
		_ = o.store.RevokeConversationApprovalGrants(finalCtx, approval.ID)
		if err != nil {
			_ = o.store.FailConversationTurnAttempt(finalCtx, turn.ID, turn.Attempts, sanitizeEngineError(err))
		}
	}()
	session, err := o.store.SessionByID(ctx, turn.SessionID)
	if err != nil {
		return TurnReply{TurnID: turn.ID}, err
	}
	if session.UserID != fresh.ID || session.Channel != channel || turn.UserMessageID == nil || approval.ConsentMessageID == nil {
		return TurnReply{TurnID: turn.ID}, store.ErrConflict
	}
	message, err := o.store.ChatMessageByID(ctx, *turn.UserMessageID)
	if err != nil {
		return TurnReply{TurnID: turn.ID}, err
	}
	var progress ai.TurnResult
	if err := json.Unmarshal(approval.Progress, &progress); err != nil {
		return TurnReply{TurnID: turn.ID}, err
	}
	ctx = context.WithValue(ctx, approvalResumeKey{}, &approvalResume{turn: turn, approval: approval, progress: &progress})
	extension.RequireApproval = true
	return o.runTurn(ctx, fresh, session, channel, message.Content, onDelta, &extension)
}

func durableApprovalTools(toolset []ai.Tool, pending []ai.ApprovalTool) ([]store.ConversationApprovalTool, error) {
	if len(pending) == 0 || len(pending) > 32 {
		return nil, errors.New("invalid approval interrupt batch")
	}
	available := make(map[string]ai.Tool, len(toolset))
	for _, tool := range toolset {
		available[tool.Name] = tool
	}
	seen := make(map[string]bool, len(pending))
	var out []store.ConversationApprovalTool
	for _, call := range pending {
		tool, ok := available[call.ToolName]
		if !ok || !tool.ApprovalRequired || call.InterruptID == "" || seen[call.InterruptID] || !json.Valid(call.Args) {
			return nil, fmt.Errorf("unknown or invalid approval interrupt for %q", call.ToolName)
		}
		seen[call.InterruptID] = true
		args := append(json.RawMessage(nil), call.Args...)
		if tool.NormalizeInput != nil {
			args = tool.NormalizeInput(args)
		}
		out = append(out, store.ConversationApprovalTool{InterruptID: call.InterruptID, ToolName: call.ToolName,
			ToolCallID: call.ToolCallID, Args: args, ArgsHash: tools.ApprovalArgsHash(args)})
	}
	return out, nil
}

func validateResumedTools(toolset []ai.Tool, pending []store.ConversationApprovalTool) error {
	available := make(map[string]ai.Tool, len(toolset))
	for _, tool := range toolset {
		available[tool.Name] = tool
	}
	for _, call := range pending {
		tool, ok := available[call.ToolName]
		if !ok || !tool.ApprovalRequired {
			return fmt.Errorf("approval capability no longer available: %s", call.ToolName)
		}
		args := call.Args
		if tool.NormalizeInput != nil {
			args = tool.NormalizeInput(args)
		}
		if tools.ApprovalArgsHash(args) != call.ArgsHash {
			return errors.New("approval arguments changed")
		}
	}
	return nil
}

func mergeApprovalProgress(result, previous *ai.TurnResult) {
	if previous == nil {
		return
	}
	merged := append([]ai.Step(nil), previous.Steps...)
	byID := make(map[string]int, len(merged))
	for i, step := range merged {
		if step.ToolCallID != "" {
			byID[step.ToolCallID] = i
		}
	}
	for _, step := range result.Steps {
		if i, exists := byID[step.ToolCallID]; step.ToolCallID != "" && exists {
			merged[i] = step
		} else {
			if step.ToolCallID != "" {
				byID[step.ToolCallID] = len(merged)
			}
			merged = append(merged, step)
		}
	}
	result.Steps = merged
	result.Usage.InputTokens += previous.Usage.InputTokens
	result.Usage.OutputTokens += previous.Usage.OutputTokens
}

func markPendingApprovalSteps(result *ai.TurnResult) {
	if result.PendingApproval == nil {
		return
	}
	for i := range result.Steps {
		step := &result.Steps[i]
		if step.Kind != ai.StepToolCall || step.Err != "" || step.Result != "" || step.ApprovalRejected {
			continue
		}
		for _, pending := range result.PendingApproval.Tools {
			if pending.ToolCallID != "" && pending.ToolCallID == step.ToolCallID {
				step.ApprovalPending = true
				break
			}
		}
	}
}

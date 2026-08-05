package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	nbtools "github.com/zdypro888/nbco/tools"
)

type actionAudit struct {
	RequiresAction bool
	Intent         string
	ActionTools    []string
}

type AutomationToolEvidence struct {
	Tool       string            `json:"tool"`
	Effect     string            `json:"effect"`
	Completion ai.ToolCompletion `json:"completion,omitempty"`
	Result     string            `json:"result,omitempty"`
	Error      string            `json:"error,omitempty"`
	Replayed   bool              `json:"replayed,omitempty"`
}

// AutomationExecution is a redacted projection of one completed Agent turn.
// Scheduled write jobs use it to decide lifecycle state from actual handler
// boundaries instead of trusting the prose in the final assistant message.
type AutomationExecution struct {
	ToolCalls             int                      `json:"tool_calls"`
	SuccessfulToolCalls   int                      `json:"successful_tool_calls"`
	ActionCalls           int                      `json:"action_calls"`
	SuccessfulActionCalls int                      `json:"successful_action_calls"`
	SuccessfulReadCalls   int                      `json:"successful_read_calls"`
	TrustedInputEvidence  bool                     `json:"trusted_input_evidence,omitempty"`
	Evidence              []AutomationToolEvidence `json:"evidence"`
}

func buildAutomationExecution(toolset []ai.Tool, res *ai.TurnResult) AutomationExecution {
	byName := toolDefinitionsByName(toolset)
	execution := AutomationExecution{Evidence: []AutomationToolEvidence{}}
	if res == nil {
		return execution
	}
	for _, step := range res.Steps {
		if step.Kind != ai.StepToolCall {
			continue
		}
		execution.ToolCalls++
		definition := byName[step.ToolName]
		effect := definition.Effect
		if effect == "" {
			effect = ai.ToolEffectUnknown
		}
		action := effect == ai.ToolEffectWrite || effect == ai.ToolEffectExecute
		if action {
			execution.ActionCalls++
		}
		successful := step.Err == "" && !step.Replayed && !nbtools.ToolResultRejected(step.Result) &&
			!toolResultLooksPendingApproval(step.Result)
		if successful {
			execution.SuccessfulToolCalls++
			if action {
				execution.SuccessfulActionCalls++
			} else if effect == ai.ToolEffectRead {
				execution.SuccessfulReadCalls++
			}
		}
		if len(execution.Evidence) >= 40 {
			continue
		}
		execution.Evidence = append(execution.Evidence, AutomationToolEvidence{
			Tool:       step.ToolName,
			Effect:     effect,
			Completion: step.Completion,
			Result:     textfmt.RedactSecrets(textfmt.TruncateRunes(strings.TrimSpace(step.Result), 500)),
			Error:      textfmt.RedactSecrets(textfmt.TruncateRunes(strings.TrimSpace(step.Err), 240)),
			Replayed:   step.Replayed,
		})
	}
	return execution
}

func buildActionAudit(text string, toolset []ai.Tool, res *ai.TurnResult) *actionAudit {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	byName := toolDefinitionsByName(toolset)
	actualActionSet := map[string]bool{}
	var actualActions []string
	toolCalls := 0
	pending := false
	if res != nil {
		for _, st := range res.Steps {
			if st.Kind != ai.StepToolCall {
				continue
			}
			toolCalls++
			pending = pending || toolResultLooksPendingApproval(st.Result)
			if !st.Replayed && toolCanProveAction(st.ToolName, byName) && !actualActionSet[st.ToolName] {
				actualActionSet[st.ToolName] = true
				actualActions = append(actualActions, st.ToolName)
			}
		}
	}
	intent := "本轮未调用工具"
	if toolCalls > 0 {
		intent = "本轮调用了只读或状态工具"
	}
	if len(actualActions) > 0 || pending {
		intent = "本轮调用了写入或执行工具"
	}
	return &actionAudit{
		RequiresAction: len(actualActions) > 0 || pending,
		Intent:         intent,
		ActionTools:    actualActions,
	}
}

func toolNames(toolset []ai.Tool) map[string]bool {
	m := make(map[string]bool, len(toolset))
	for _, t := range toolset {
		m[t.Name] = true
	}
	return m
}

type toolEvidence struct {
	Tool            string `json:"tool"`
	HandlerReturned bool   `json:"handler_returned"`
	Replayed        bool   `json:"replayed,omitempty"`
	Rejected        bool   `json:"rejected,omitempty"`
	Completion      string `json:"completion,omitempty"`
	Summary         string `json:"summary,omitempty"`
}

type turnDiagnostics struct {
	Route                string   `json:"route,omitempty"`
	SystemChars          int      `json:"system_chars,omitempty"`
	HistoryChars         int      `json:"history_chars,omitempty"`
	ToolCount            int      `json:"tool_count,omitempty"`
	FullToolCount        int      `json:"full_tool_count,omitempty"`
	ToolSchemaChars      int      `json:"tool_schema_chars,omitempty"`
	Tools                []string `json:"tools,omitempty"`
	AgentIterations      int      `json:"agent_iterations,omitempty"`
	ModelCalls           int      `json:"model_calls,omitempty"`
	ModelPeakToolCount   int      `json:"model_peak_tool_count,omitempty"`
	ModelPeakSchemaChars int      `json:"model_peak_schema_chars,omitempty"`
	ModelPeakTools       []string `json:"model_peak_tools,omitempty"`
}

func summarizeToolEvidence(steps []ai.Step) []toolEvidence {
	var out []toolEvidence
	for _, st := range steps {
		if st.Kind != ai.StepToolCall {
			continue
		}
		rejected := nbtools.ToolResultRejected(st.Result)
		ok := st.Err == "" && !rejected
		summary := st.Err
		if summary == "" {
			summary = st.Result
		}
		out = append(out, toolEvidence{
			Tool:            st.ToolName,
			HandlerReturned: ok && !st.Replayed,
			Replayed:        st.Replayed,
			Rejected:        rejected,
			Completion:      string(st.Completion),
			Summary:         textfmt.TruncateRunes(textfmt.RedactSecrets(strings.TrimSpace(summary)), 220),
		})
	}
	return out
}

func countToolEvidence(steps []ai.Step) (total, success int) {
	for _, st := range steps {
		if st.Kind != ai.StepToolCall {
			continue
		}
		total++
		if !st.Replayed && st.Err == "" && !nbtools.ToolResultRejected(st.Result) {
			success++
		}
	}
	return total, success
}

func toolDefinitionsByName(toolset []ai.Tool) map[string]ai.Tool {
	out := make(map[string]ai.Tool, len(toolset))
	for _, t := range toolset {
		out[t.Name] = t
	}
	return out
}

func toolCanProveAction(name string, byName map[string]ai.Tool) bool {
	if t, ok := byName[name]; ok {
		return nbtools.ToolCanProveActionTool(t)
	}
	return nbtools.ToolCanProveAction(name)
}

func toolResultLooksPendingApproval(result string) bool {
	return strings.Contains(result, nbtools.PendingApprovalMarker)
}

func firstPendingApprovalStep(steps []ai.Step) (ai.Step, bool) {
	for _, st := range steps {
		if st.Kind == ai.StepToolCall && st.Err == "" && toolResultLooksPendingApproval(st.Result) {
			return st, true
		}
	}
	return ai.Step{}, false
}

func (o *Orchestrator) recordActionTurn(ctx context.Context, u *store.User, sess *store.ChatSession, channel, text string, audit *actionAudit, res *ai.TurnResult, diag turnDiagnostics) {
	if o == nil || o.store == nil {
		return
	}
	in := buildActionTurnInput(u, sess, channel, text, audit, res, diag)
	if in == nil {
		return
	}
	if err := o.store.RecordActionTurn(ctx, *in); err != nil {
		slog.Warn("动作轮次记录失败", "session", sess.ID, "user", u.ID, "err", err)
	}
}

func buildActionTurnInput(u *store.User, sess *store.ChatSession, channel, text string, audit *actionAudit, res *ai.TurnResult, diag turnDiagnostics) *store.ActionTurnInput {
	if u == nil || sess == nil || audit == nil {
		return nil
	}
	outcome := actionTurnOutcome(audit, res)
	evidence := map[string]any{
		"tool_evidence": summarizeToolEvidence(nil),
	}
	if diag.Route != "" || diag.ToolCount > 0 || diag.SystemChars > 0 {
		evidence["turn_context"] = diag
	}
	if res != nil {
		evidence["tool_evidence"] = summarizeToolEvidence(res.Steps)
		evidence["finish_reason"] = res.FinishReason
	}
	toolCount, successToolCount := 0, 0
	replyExcerpt := ""
	if res != nil {
		toolCount, successToolCount = countToolEvidence(res.Steps)
		replyExcerpt = textfmt.RedactSecrets(textfmt.SanitizeVisibleReply(res.Text))
	}
	sid := sess.ID
	return &store.ActionTurnInput{
		UserID:           u.ID,
		SessionID:        &sid,
		Channel:          channel,
		UserTextHash:     contentHash(text),
		UserTextExcerpt:  textfmt.RedactSecrets(text),
		ReplyExcerpt:     replyExcerpt,
		RequiresAction:   audit.RequiresAction,
		Intent:           audit.Intent,
		ExpectedTools:    audit.ActionTools,
		Evidence:         evidence,
		Outcome:          outcome,
		ToolCount:        toolCount,
		SuccessToolCount: successToolCount,
	}
}

// executionContinuityContext replays a small, structured projection of recent
// tool-backed turns. The visible transcript remains the conversational source
// of truth; this ledger supplies facts that a concise final reply may omit.
func (o *Orchestrator) executionContinuityContext(ctx context.Context, u *store.User, sess *store.ChatSession) string {
	if o == nil || o.store == nil || u == nil || sess == nil {
		return ""
	}
	turns, err := o.store.ListActionTurnsBySession(ctx, u.ID, sess.ID, 6)
	if err != nil {
		slog.Warn("读取近期执行事实失败", "session", sess.ID, "user", u.ID, "err", err)
		return ""
	}
	type executionFact struct {
		UserText     string         `json:"user_text"`
		Reply        string         `json:"reply"`
		Outcome      string         `json:"outcome"`
		ToolCount    int            `json:"tool_count"`
		ToolEvidence []toolEvidence `json:"tool_evidence,omitempty"`
		OccurredAt   time.Time      `json:"occurred_at"`
	}
	facts := make([]executionFact, 0, 3)
	for _, turn := range turns {
		if turn == nil || (turn.ToolCount == 0 && !turn.RequiresAction) {
			continue
		}
		var evidence struct {
			ToolEvidence []toolEvidence `json:"tool_evidence"`
		}
		_ = json.Unmarshal(turn.Evidence, &evidence)
		if len(evidence.ToolEvidence) > 6 {
			evidence.ToolEvidence = append(
				append([]toolEvidence(nil), evidence.ToolEvidence[:3]...),
				evidence.ToolEvidence[len(evidence.ToolEvidence)-3:]...,
			)
		}
		facts = append(facts, executionFact{
			UserText: textfmt.TruncateRunes(turn.UserTextExcerpt, 300),
			Reply:    textfmt.TruncateRunes(turn.ReplyExcerpt, 300),
			Outcome:  turn.Outcome, ToolCount: turn.ToolCount,
			ToolEvidence: evidence.ToolEvidence, OccurredAt: turn.CreatedAt,
		})
		if len(facts) == 3 {
			break
		}
	}
	if len(facts) == 0 {
		return ""
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		return ""
	}
	return "\n\n[近期已提交的执行事实]\n" + string(raw) +
		"\n这些记录只用于理解连续对话；可变业务状态仍应按需调用读取工具核实。"
}

func actionTurnOutcome(audit *actionAudit, res *ai.TurnResult) string {
	if audit == nil {
		return "not_recorded"
	}
	if res == nil {
		return "no_result"
	}
	if _, ok := firstPendingApprovalStep(res.Steps); ok {
		return "pending_approval"
	}
	actionTools := make(map[string]struct{}, len(audit.ActionTools))
	for _, name := range audit.ActionTools {
		actionTools[name] = struct{}{}
	}
	hadTool, hadSuccess, hadError, actionSuccess, actionAccepted, actionError := false, false, false, false, false, false
	for _, step := range res.Steps {
		if step.Kind != ai.StepToolCall {
			continue
		}
		hadTool = true
		_, action := actionTools[step.ToolName]
		if step.Replayed {
			continue
		}
		if step.Err != "" || nbtools.ToolResultRejected(step.Result) {
			hadError = true
			actionError = actionError || action
			continue
		}
		hadSuccess = true
		actionSuccess = actionSuccess || action
		actionAccepted = actionAccepted || (action && step.Completion == ai.ToolCompletionAsynchronous && nbtools.ToolResultAccepted(step.Result))
	}
	if audit.RequiresAction && actionSuccess {
		if actionAccepted {
			return "action_accepted"
		}
		return "action_tool_returned"
	}
	if actionError || (!hadSuccess && hadError) {
		return "tool_handler_error"
	}
	if hadSuccess {
		return "read_tool_returned"
	}
	if hadTool {
		return "tool_handler_error"
	}
	return "answered_without_tool"
}

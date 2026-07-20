package chat

import (
	"context"
	"log/slog"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	nbtools "github.com/zdypro888/nbco/tools"
)

type actionPlan struct {
	RequiresAction  bool     `json:"requires_action"`
	Intent          string   `json:"intent"`
	ExpectedTools   []string `json:"expected_tools"`
	SuccessEvidence []string `json:"success_evidence"`
	MissingInfo     []string `json:"missing_info"`
	Confidence      float64  `json:"confidence"`

	Source string `json:"-"`
	Raw    string `json:"-"`
}

func buildActionAuditPlan(text string, toolset []ai.Tool, res *ai.TurnResult) *actionPlan {
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
	return &actionPlan{
		RequiresAction:  len(actualActions) > 0 || pending,
		Intent:          intent,
		ExpectedTools:   actualActions,
		SuccessEvidence: []string{"记录实际工具调用及 handler 返回；业务状态仍以领域数据为准"},
		Confidence:      1,
		Source:          "tool_trace",
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
	PreferredTools       []string `json:"preferred_tools,omitempty"`
	AgentIterations      int      `json:"agent_iterations,omitempty"`
	ModelCalls           int      `json:"model_calls,omitempty"`
	ModelPeakToolCount   int      `json:"model_peak_tool_count,omitempty"`
	ModelPeakSchemaChars int      `json:"model_peak_schema_chars,omitempty"`
	ModelPeakTools       []string `json:"model_peak_tools,omitempty"`
	ReplyGrounded        bool     `json:"reply_grounded,omitempty"`
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

func (o *Orchestrator) recordActionTurn(ctx context.Context, u *store.User, sess *store.ChatSession, channel, text string, plan *actionPlan, res *ai.TurnResult, diag turnDiagnostics) {
	if o == nil || o.store == nil || u == nil || sess == nil || plan == nil {
		return
	}
	outcome := actionTurnOutcome(plan, res)
	evidence := map[string]any{
		"planner_source":   plan.Source,
		"confidence":       plan.Confidence,
		"success_evidence": plan.SuccessEvidence,
		"missing_info":     plan.MissingInfo,
		"tool_evidence":    summarizeToolEvidence(nil),
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
	if err := o.store.RecordActionTurn(ctx, store.ActionTurnInput{
		UserID:           u.ID,
		SessionID:        &sid,
		Channel:          channel,
		UserTextHash:     contentHash(text),
		UserTextExcerpt:  textfmt.RedactSecrets(text),
		ReplyExcerpt:     replyExcerpt,
		RequiresAction:   plan.RequiresAction,
		Intent:           plan.Intent,
		ExpectedTools:    plan.ExpectedTools,
		Evidence:         evidence,
		Outcome:          outcome,
		ToolCount:        toolCount,
		SuccessToolCount: successToolCount,
	}); err != nil {
		slog.Warn("动作轮次记录失败", "session", sess.ID, "user", u.ID, "err", err)
	}
}

func actionTurnOutcome(plan *actionPlan, res *ai.TurnResult) string {
	if plan == nil {
		return "not_recorded"
	}
	if res == nil {
		return "no_result"
	}
	if _, ok := firstPendingApprovalStep(res.Steps); ok {
		return "pending_approval"
	}
	actionTools := make(map[string]struct{}, len(plan.ExpectedTools))
	for _, name := range plan.ExpectedTools {
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
	if plan.RequiresAction && actionSuccess {
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

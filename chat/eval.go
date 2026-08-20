package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	"github.com/zdypro888/nbco/tools"
)

// EvalAssertions is intentionally behavioral: cases specify observable output
// and tool choices, while Eino remains responsible for planning the turn.
type EvalAssertions struct {
	RequiredSubstrings    []string          `json:"required_substrings"`
	ForbiddenSubstrings   []string          `json:"forbidden_substrings"`
	RequiredTools         []string          `json:"required_tools"`
	RequiredAnyToolGroups [][]string        `json:"required_any_tool_groups"`
	ForbiddenTools        []string          `json:"forbidden_tools"`
	MinSuccessfulTools    int               `json:"min_successful_tools"`
	MaxToolCalls          int               `json:"max_tool_calls"`
	NoMarkdownTable       bool              `json:"no_markdown_table"`
	ToolResults           map[string]string `json:"tool_results"`
}

type evalRunDetails struct {
	Passed          bool     `json:"passed"`
	Failures        []string `json:"failures"`
	ToolCalls       []string `json:"tool_calls"`
	SuccessfulTools int      `json:"successful_tools"`
	Model           string   `json:"model,omitempty"`
	InputTokens     int64    `json:"input_tokens,omitempty"`
	OutputTokens    int64    `json:"output_tokens,omitempty"`
	DurationMS      int64    `json:"duration_ms"`
	Error           string   `json:"error,omitempty"`
}

// RunEvalCase executes one production-shaped Eino turn against simulated tool
// handlers. The permission-filtered tool catalog, schemas and retrieval are
// real; handlers never mutate business state. The resulting run is always persisted,
// including model/configuration failures.
func (o *Orchestrator) RunEvalCase(ctx context.Context, ranBy *store.User, evalCase *store.ConversationEvalCase) (*store.ConversationEvalRun, error) {
	if o == nil || o.store == nil || o.engine == nil || ranBy == nil || evalCase == nil || evalCase.ID <= 0 {
		return nil, store.ErrNotFound
	}
	started := time.Now()
	details := evalRunDetails{Failures: []string{}, ToolCalls: []string{}}
	status := "error"
	output := ""
	caseID, userID := evalCase.ID, ranBy.ID

	var assertions EvalAssertions
	if len(evalCase.Assertions) > 0 && json.Unmarshal(evalCase.Assertions, &assertions) != nil {
		details.Error = "评测断言不是有效 JSON 对象"
		return o.persistEvalRun(ctx, &caseID, &userID, status, output, started, details)
	}

	channel := strings.TrimSpace(evalCase.Channel)
	if channel == "" {
		channel = "telegram"
	}
	toolset := tools.ForUserContextWithTools(ctx, o.deps, ranBy, nil, nil)
	if isGroupChannel(channel) {
		toolset = tools.StripGroupSensitive(toolset)
	}
	availableTools := toolNames(toolset)
	system, err := o.systemPrompt(ctx, ranBy, channel, availableTools)
	if err != nil {
		details.Error = textfmt.RedactSecrets(err.Error())
		return o.persistEvalRun(ctx, &caseID, &userID, status, output, started, details)
	}
	rules, skills, _ := o.turnRetrievalContext(ctx, ranBy, channel, evalCase.UserInput, true)
	system += rules + o.peopleContext(ctx, ranBy, channel)
	toolset = simulatedEvalTools(toolset, assertions.ToolResults)
	model := o.runtimeModel(ctx)
	details.Model = model
	result, runErr := o.engine.RunTurn(ctx, &ai.TurnRequest{
		Mode:           ai.TurnModeDeep,
		SessionID:      fmt.Sprintf("eval:%d:%d", evalCase.ID, started.UnixNano()),
		DisableSession: true,
		System:         system,
		UserText:       modelUserContent(evalCase.UserInput, started, o.tz),
		Model:          model,
		Tools:          toolset,
		Skills:         skills,
	})
	if runErr != nil {
		details.Error = textfmt.RedactSecrets(runErr.Error())
		return o.persistEvalRun(ctx, &caseID, &userID, status, output, started, details)
	}
	rawOutput := result.Text
	output = normalizeAssistantReply(channel, rawOutput)
	details.InputTokens = result.Usage.InputTokens
	details.OutputTokens = result.Usage.OutputTokens
	for _, step := range result.Steps {
		if step.Kind != ai.StepToolCall {
			continue
		}
		details.ToolCalls = append(details.ToolCalls, step.ToolName)
		if step.Err == "" && !step.Replayed && !tools.ToolResultRejected(step.Result) {
			details.SuccessfulTools++
		}
	}
	details.Failures = evaluateAssertions(assertions, output, details.ToolCalls, details.SuccessfulTools)
	if assertions.NoMarkdownTable && rawOutput != output && hasMarkdownTable(rawOutput) &&
		!slices.Contains(details.Failures, "输出包含 Markdown 表格") {
		details.Failures = append(details.Failures, "输出包含 Markdown 表格")
	}
	details.Passed = len(details.Failures) == 0
	if details.Passed {
		status = "passed"
	} else {
		status = "failed"
	}
	return o.persistEvalRun(ctx, &caseID, &userID, status, output, started, details)
}

func (o *Orchestrator) persistEvalRun(
	ctx context.Context,
	caseID, ranBy *int64,
	status, output string,
	started time.Time,
	details evalRunDetails,
) (*store.ConversationEvalRun, error) {
	details.DurationMS = time.Since(started).Milliseconds()
	raw, _ := json.Marshal(details)
	persistCtx, cancel := evalPersistenceContext(ctx)
	defer cancel()
	return o.store.CreateConversationEvalRun(persistCtx, store.ConversationEvalRun{
		CaseID: caseID, Status: status, Output: output, Details: raw, RanBy: ranBy,
	})
}

func evalPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func simulatedEvalTools(toolset []ai.Tool, results map[string]string) []ai.Tool {
	out := make([]ai.Tool, len(toolset))
	for i, original := range toolset {
		tool := original
		name := tool.Name
		completion := tool.Completion
		tool.Handler = func(context.Context, json.RawMessage) (string, error) {
			if value := strings.TrimSpace(results[name]); value != "" {
				return value, nil
			}
			payload, _ := json.Marshal(map[string]any{
				"status":     "ok",
				"evaluation": true,
				"tool":       name,
				"completion": completion,
				"message":    "评测模拟调用成功；未修改生产数据。",
			})
			return string(payload), nil
		}
		out[i] = tool
	}
	return out
}

func evaluateAssertions(assertions EvalAssertions, output string, toolCalls []string, successfulTools int) []string {
	var failures []string
	lowerOutput := strings.ToLower(output)
	for _, required := range assertions.RequiredSubstrings {
		if required = strings.TrimSpace(required); required != "" && !strings.Contains(lowerOutput, strings.ToLower(required)) {
			failures = append(failures, "输出缺少必需文本: "+required)
		}
	}
	for _, forbidden := range assertions.ForbiddenSubstrings {
		if forbidden = strings.TrimSpace(forbidden); forbidden != "" && strings.Contains(lowerOutput, strings.ToLower(forbidden)) {
			failures = append(failures, "输出包含禁用文本: "+forbidden)
		}
	}
	for _, required := range assertions.RequiredTools {
		if required = strings.TrimSpace(required); required != "" && !slices.Contains(toolCalls, required) {
			failures = append(failures, "未调用必需工具: "+required)
		}
	}
	for _, group := range assertions.RequiredAnyToolGroups {
		matched := false
		for _, name := range group {
			if slices.Contains(toolCalls, strings.TrimSpace(name)) {
				matched = true
				break
			}
		}
		if !matched && len(group) > 0 {
			failures = append(failures, "未调用任一候选工具: "+strings.Join(group, " / "))
		}
	}
	for _, forbidden := range assertions.ForbiddenTools {
		if forbidden = strings.TrimSpace(forbidden); forbidden != "" && slices.Contains(toolCalls, forbidden) {
			failures = append(failures, "调用了禁用工具: "+forbidden)
		}
	}
	if assertions.MinSuccessfulTools > 0 && successfulTools < assertions.MinSuccessfulTools {
		failures = append(failures, fmt.Sprintf("成功工具数不足: %d < %d", successfulTools, assertions.MinSuccessfulTools))
	}
	if assertions.MaxToolCalls > 0 && len(toolCalls) > assertions.MaxToolCalls {
		failures = append(failures, fmt.Sprintf("工具调用过多: %d > %d", len(toolCalls), assertions.MaxToolCalls))
	}
	if assertions.NoMarkdownTable && hasMarkdownTable(output) {
		failures = append(failures, "输出包含 Markdown 表格")
	}
	return failures
}

func hasMarkdownTable(output string) bool {
	lines := strings.Split(output, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.Contains(lines[i-1], "|") && markdownTableSeparator(lines[i]) {
			return true
		}
	}
	return false
}

func markdownTableSeparator(line string) bool {
	line = strings.Trim(strings.TrimSpace(line), "|")
	if !strings.Contains(line, "-") {
		return false
	}
	for _, cell := range strings.Split(line, "|") {
		cell = strings.TrimSpace(cell)
		if strings.Count(cell, "-") < 3 || strings.Trim(cell, "-:") != "" {
			return false
		}
	}
	return true
}

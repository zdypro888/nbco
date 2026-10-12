// Package einoengine adapts nbco's domain contract to ihtml's shared Eino runtime.
package einoengine

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	sharedai "github.com/zdypro888/ihtml/agent"
	shared "github.com/zdypro888/ihtml/agent/eino"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
)

// Engine owns no model loop; the implementation and regression suite live in ihtml.
type Engine struct{ runtime *shared.Engine }
type Option = shared.Option

const einoToolSearchReminderKey = shared.ToolSearchReminderKey

func WithRuntimeStore(store RuntimeStore) Option { return shared.WithRuntimeStore(store) }

func New(ctx context.Context, cfg config.AIConfig, opts ...Option) (*Engine, error) {
	runtime, err := shared.New(ctx, shared.Config{
		AgentName: "nbco", AgentDescription: "nbco 公司运营中枢",
		Provider: cfg.Provider, OpenAIAPI: cfg.OpenAIAPI, APIKey: cfg.APIKey,
		BaseURL: cfg.BaseURL, Model: cfg.Model, MaxTokens: cfg.MaxTokens,
		MaxCompletionTokens: cfg.MaxCompletionTokens, ReasoningEffort: cfg.ReasoningEffort,
		Temperature: cfg.Temperature, TimeoutMS: cfg.TimeoutMS, MaxTurns: cfg.MaxTurns,
		SummarizeAfterTokens: cfg.SummarizeAfterTokens, SummarizeAfterMessages: cfg.SummarizeAfterMessages,
		SessionNamespace: "eino:chat", ValidateSessionID: validNBCOSessionID,
	}, opts...)
	if err != nil {
		return nil, err
	}
	return &Engine{runtime: runtime}, nil
}

func validNBCOSessionID(id string) bool {
	id = strings.TrimPrefix(strings.TrimSpace(id), "turn:")
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0
}

func (e *Engine) Name() string { return e.runtime.Name() }

func (e *Engine) RunTurn(ctx context.Context, req *ai.TurnRequest) (*ai.TurnResult, error) {
	if req == nil {
		_, err := e.runtime.RunTurn(ctx, nil)
		return nil, err
	}
	input := &sharedai.TurnRequest{
		Mode: sharedai.TurnMode(req.Mode), SessionID: req.SessionID, InvocationScope: req.InvocationScope,
		EngineSession: req.EngineSession, DisableSession: req.DisableSession, SessionCapability: req.SessionCapability,
		System: req.System, UserText: req.UserText, Model: req.Model, OnDelta: req.OnDelta,
		StreamReasoning: req.StreamReasoning, MaxOutputTokens: req.MaxOutputTokens, MaxIterations: req.MaxIterations,
		Reasoning: sharedai.ReasoningMode(req.Reasoning), JSONOutput: req.JSONOutput,
	}
	for _, msg := range req.History {
		input.History = append(input.History, sharedai.Message{Role: sharedai.Role(msg.Role), Content: msg.Content})
	}
	for _, skill := range req.Skills {
		input.Skills = append(input.Skills, sharedai.Skill(skill))
	}
	for _, tool := range req.Tools {
		input.Tools = append(input.Tools, sharedTool(tool))
	}
	if req.OnEvent != nil {
		input.OnEvent = func(step sharedai.Step) { req.OnEvent(domainStep(step)) }
	}
	result, err := e.runtime.RunTurn(ctx, input)
	if result == nil {
		return nil, err
	}
	out := &ai.TurnResult{
		Text: result.Text, EngineSession: result.EngineSession, FinishReason: result.FinishReason,
		OutputLikelyTruncated: result.OutputLikelyTruncated,
		Usage:                 ai.Usage(result.Usage), ToolExposure: ai.ToolExposure(result.ToolExposure),
	}
	for _, step := range result.Steps {
		out.Steps = append(out.Steps, domainStep(step))
	}
	return out, err
}

func sharedTool(item ai.Tool) sharedai.Tool {
	result := sharedai.Tool{
		Name: item.Name, Description: item.Description, Maturity: sharedai.ToolMaturity(item.Maturity),
		Domain: item.Domain, Effect: item.Effect, LoadMode: sharedai.ToolLoadMode(item.LoadMode),
		Completion: sharedai.ToolCompletion(item.Completion), RequiredAction: item.RequiredAction,
		ResolvePermissionTarget: item.ResolvePermissionTarget, GroupSensitive: item.GroupSensitive,
		ApprovalRequired: item.ApprovalRequired, InputSchema: item.InputSchema, NormalizeInput: item.NormalizeInput,
		RecoverResult: item.RecoverResult, ResultPersisted: item.ResultPersisted,
	}
	if item.Handler != nil {
		result.Handler = func(ctx context.Context, args json.RawMessage) (string, error) {
			// Keep domain receipts on nbco's context key while the runtime owns identity.
			return item.Handler(ai.WithToolInvocationKey(ctx, sharedai.ToolInvocationKey(ctx)), args)
		}
	}
	return result
}

func domainStep(step sharedai.Step) ai.Step {
	return ai.Step{Kind: ai.StepKind(step.Kind), ToolName: step.ToolName, Args: step.Args,
		Result: step.Result, Err: step.Err, Completion: ai.ToolCompletion(step.Completion), Replayed: step.Replayed}
}

var _ ai.Engine = (*Engine)(nil)

// Package einoengine 用 eino ADK 实现 ai.Engine：直接调模型 API（Claude / OpenAI 兼容），
// tool 循环在本进程内执行。这是客户自带 API key 的产品路径。
package einoengine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
	"github.com/cloudwego/eino/adk/middlewares/patchtoolcalls"
	skillmw "github.com/cloudwego/eino/adk/middlewares/skill"
	"github.com/cloudwego/eino/adk/middlewares/summarization"
	"github.com/cloudwego/eino/adk/prebuilt/deep"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
	"github.com/zdypro888/nbco/textfmt"
)

// Engine 持有一个已初始化的 ChatModel；Agent 按轮次构建（工具集因用户而异）。
type Engine struct {
	cfg          config.AIConfig
	maxTurns     int
	defaultModel string
	mu           sync.Mutex
	models       map[string]einomodel.ToolCallingChatModel
	runtime      RuntimeStore
}

// Option customizes the Eino runtime without coupling the ai.Engine interface to
// a concrete persistence implementation.
type Option func(*Engine)

// WithRuntimeStore enables Eino-managed durable sessions and checkpoints.
func WithRuntimeStore(store RuntimeStore) Option {
	return func(engine *Engine) { engine.runtime = store }
}

// New 按配置创建模型。
func New(ctx context.Context, cfg config.AIConfig, opts ...Option) (*Engine, error) {
	if err := adk.SetLanguage(adk.LanguageChinese); err != nil {
		return nil, fmt.Errorf("设置 Eino 内置提示语言: %w", err)
	}
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 64
	}
	if cfg.SummarizeAfterTokens <= 0 {
		cfg.SummarizeAfterTokens = 24000
	}
	if cfg.SummarizeAfterMessages <= 0 {
		cfg.SummarizeAfterMessages = 80
	}
	m, err := newChatModel(ctx, cfg)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(cfg.Model)
	engine := &Engine{
		cfg:          cfg,
		maxTurns:     cfg.MaxTurns,
		defaultModel: model,
		models:       map[string]einomodel.ToolCallingChatModel{model: m},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(engine)
		}
	}
	return engine, nil
}

func newChatModel(ctx context.Context, cfg config.AIConfig) (einomodel.ToolCallingChatModel, error) {
	timeout := chatHTTPTimeout(cfg.TimeoutMS)
	switch cfg.Provider {
	case config.ProviderClaude:
		c := &claude.Config{
			APIKey:     cfg.APIKey,
			Model:      cfg.Model,
			MaxTokens:  cfg.MaxTokens,
			HTTPClient: &http.Client{Timeout: timeout},
		}
		if base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"); base != "" {
			c.BaseURL = &base
		}
		if cfg.Temperature > 0 {
			t := cfg.Temperature
			c.Temperature = &t
		}
		return claude.NewChatModel(ctx, c)
	case config.ProviderOpenAI:
		c := &openai.ChatModelConfig{
			APIKey:  cfg.APIKey,
			BaseURL: cfg.BaseURL,
			Model:   cfg.Model,
			Timeout: timeout,
		}
		if cfg.MaxCompletionTokens > 0 {
			mt := cfg.MaxCompletionTokens
			c.MaxCompletionTokens = &mt
		} else if cfg.MaxTokens > 0 {
			mt := cfg.MaxTokens
			c.MaxTokens = &mt
		}
		if effort := strings.TrimSpace(cfg.ReasoningEffort); effort != "" {
			c.ReasoningEffort = openai.ReasoningEffortLevel(effort)
		}
		if cfg.Temperature > 0 {
			t := cfg.Temperature
			c.Temperature = &t
		}
		return openai.NewChatModel(ctx, c)
	default:
		return nil, fmt.Errorf("不支持的 provider: %q", cfg.Provider)
	}
}

func chatHTTPTimeout(ms int) time.Duration {
	if ms <= 0 {
		ms = 300000
	}
	return time.Duration(ms) * time.Millisecond
}

// Name 实现 ai.Engine。
func (e *Engine) Name() string { return config.EngineEino }

// RunTurn selects an explicit Eino execution profile. Product conversations use
// DeepAgent; bounded internal transformations use a plain ChatModelAgent.
func (e *Engine) RunTurn(ctx context.Context, req *ai.TurnRequest) (*ai.TurnResult, error) {
	if req == nil {
		return nil, errors.New("turn request is nil")
	}
	mode, err := normalizeTurnMode(req.Mode)
	if err != nil {
		return nil, err
	}
	if mode == ai.TurnModeOneShot {
		if err := validateOneShotRequest(req); err != nil {
			return nil, err
		}
	}
	model, err := e.modelFor(ctx, req.Model)
	if err != nil {
		return nil, err
	}
	var summaryUsage ai.Usage
	var toolExposure ai.ToolExposure
	var agent adk.Agent
	var toolScope *turnToolMiddleware
	switch mode {
	case ai.TurnModeDeep:
		agent, toolScope, err = e.newDeepAgent(ctx, model, req, &summaryUsage, &toolExposure)
	case ai.TurnModeOneShot:
		agent, err = newOneShotAgent(ctx, model, req, &toolExposure)
	}
	if err != nil {
		return nil, err
	}

	// Deep creates durable product sessions. OneShot normally stays stateless,
	// but may append a rendering closure to an already-existing Deep session.
	engineSession := ""
	if mode == ai.TurnModeDeep {
		engineSession = e.engineSessionID(req)
	} else if strings.TrimSpace(req.EngineSession) != "" {
		engineSession = e.engineSessionID(req)
		if engineSession != req.EngineSession {
			return nil, errors.New("one-shot Eino turn cannot create or rotate a managed session")
		}
	}
	managedSession := engineSession != "" && e.runtime != nil
	slog.Debug("Eino 执行模式", "mode", mode, "session", req.SessionID,
		"managed_session", managedSession, "tools", len(req.Tools), "skills", len(req.Skills))
	includeHistory := true
	if managedSession {
		hasEvents, err := e.sessionHasEvents(ctx, engineSession)
		if err != nil {
			return nil, fmt.Errorf("读取 Eino 会话状态: %w", err)
		}
		includeHistory = !hasEvents
	}
	msgs := make([]*schema.Message, 0, len(req.History)+1)
	// 引擎失败的轮次历史里只有 user 消息，重放会出现同角色相邻，
	// 部分 API 要求角色严格交替：相邻同角色合并为一条。
	push := func(m *schema.Message) {
		if n := len(msgs); n > 0 && msgs[n-1].Role == m.Role {
			msgs[n-1].Content += "\n\n" + m.Content
			return
		}
		msgs = append(msgs, m)
	}
	if includeHistory {
		for _, m := range req.History {
			switch m.Role {
			case ai.RoleUser:
				push(schema.UserMessage(m.Content))
			case ai.RoleAssistant:
				push(schema.AssistantMessage(m.Content, nil))
			}
		}
		msgs = dropLeadingNonUser(msgs)
	}
	push(schema.UserMessage(req.UserText))

	runnerConfig := adk.RunnerConfig{
		Agent:           agent,
		EnableStreaming: mode == ai.TurnModeDeep || req.OnDelta != nil,
	}
	if managedSession {
		runnerConfig.SessionID = engineSession
		runnerConfig.SessionStore = e.runtime
		runnerConfig.CheckPointStore = e.runtime
	}
	runner := adk.NewRunner(ctx, runnerConfig)
	var blockedToolCall func(string, string) bool
	var replayedToolCall func(string, string) bool
	if toolScope != nil {
		blockedToolCall = toolScope.blocked
		replayedToolCall = toolScope.replayed
	}
	runOptions := turnRunOptions(e.cfg.Provider, req)
	outputLimit := e.outputTokenLimit()
	if req.MaxOutputTokens > 0 {
		outputLimit = req.MaxOutputTokens
	}
	result, err := collect(runner.Run(ctx, msgs, runOptions...), req.OnEvent, req.OnDelta, req.StreamReasoning,
		outputLimit, blockedToolCall, replayedToolCall, toolCompletions(req.Tools))
	terminalFailure := err != nil || result != nil && result.FinishReason == "agent_error"
	if terminalFailure && managedSession {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		rollbackErr := e.rollbackFailedSession(cleanupCtx, engineSession)
		cancel()
		if rollbackErr != nil {
			slog.Error("Eino 失败会话回滚失败", "session", engineSession, "err", rollbackErr)
		}
	}
	if result != nil {
		result.Usage.InputTokens += summaryUsage.InputTokens
		result.Usage.OutputTokens += summaryUsage.OutputTokens
		result.ToolExposure = toolExposure
		if managedSession && !terminalFailure {
			result.EngineSession = engineSession
		}
	}
	return result, err
}

func turnRunOptions(provider string, req *ai.TurnRequest) []adk.AgentRunOption {
	if req == nil {
		return nil
	}
	var options []einomodel.Option
	if req.MaxOutputTokens > 0 {
		if provider == config.ProviderOpenAI {
			options = append(options, openai.WithMaxCompletionTokens(req.MaxOutputTokens))
		} else {
			options = append(options, einomodel.WithMaxTokens(req.MaxOutputTokens))
		}
	}
	if provider == config.ProviderOpenAI {
		extra := map[string]any{}
		if req.Reasoning == ai.ReasoningDisabled {
			options = append(options, openai.WithReasoningEffort(openai.ReasoningEffortLevel("none")))
		}
		if req.JSONOutput {
			extra["response_format"] = map[string]any{"type": "json_object"}
		}
		if len(extra) > 0 {
			options = append(options, openai.WithExtraFields(extra))
		}
	}
	if len(options) == 0 {
		return nil
	}
	return []adk.AgentRunOption{adk.WithChatModelOptions(options)}
}

func toolCompletions(toolset []ai.Tool) map[string]ai.ToolCompletion {
	out := make(map[string]ai.ToolCompletion)
	for _, item := range toolset {
		if item.Completion != ai.ToolCompletionImmediate {
			out[item.Name] = item.Completion
		}
	}
	return out
}

func normalizeTurnMode(mode ai.TurnMode) (ai.TurnMode, error) {
	switch mode {
	case "", ai.TurnModeDeep:
		return ai.TurnModeDeep, nil
	case ai.TurnModeOneShot:
		return ai.TurnModeOneShot, nil
	default:
		return "", fmt.Errorf("unsupported Eino turn mode %q", mode)
	}
}

func validateOneShotRequest(req *ai.TurnRequest) error {
	if len(req.Tools) > 0 {
		return errors.New("one-shot Eino turn cannot expose tools")
	}
	if len(req.Skills) > 0 {
		return errors.New("one-shot Eino turn cannot expose skills")
	}
	if strings.TrimSpace(req.EngineSession) != "" && strings.TrimSpace(req.SessionCapability) == "" {
		return errors.New("one-shot Eino session continuation requires a capability scope")
	}
	return nil
}

func newOneShotAgent(
	ctx context.Context,
	model einomodel.ToolCallingChatModel,
	req *ai.TurnRequest,
	exposure *ai.ToolExposure,
) (adk.Agent, error) {
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:             "nbco",
		Description:      "nbco 受控单次模型任务",
		Instruction:      req.System,
		Model:            model,
		MaxIterations:    1,
		Handlers:         []adk.TypedChatModelAgentMiddleware[*schema.Message]{&oneShotMiddleware{exposure: exposure}},
		ModelRetryConfig: modelRetryConfig(),
	})
	if err != nil {
		return nil, fmt.Errorf("构建 Eino OneShot Agent: %w", err)
	}
	return agent, nil
}

const einoToolSearchReminderKey = "__toolsearch_reminder__"

// oneShotMiddleware keeps a plain generation tool-free even when it closes an
// existing Deep session. Eino's tool-search middleware persists its deferred
// tool catalog as an internally marked user message; replaying that hard
// requirement without tool_search would give the model contradictory input.
type oneShotMiddleware struct {
	adk.TypedBaseChatModelAgentMiddleware[*schema.Message]

	mu       sync.Mutex
	exposure *ai.ToolExposure
}

func (*oneShotMiddleware) BeforeModelRewriteState(
	ctx context.Context,
	state *adk.TypedChatModelAgentState[*schema.Message],
	_ *adk.TypedModelContext[*schema.Message],
) (context.Context, *adk.TypedChatModelAgentState[*schema.Message], error) {
	if state == nil {
		return ctx, state, nil
	}
	state.ToolInfos = nil
	state.DeferredToolInfos = nil
	messages := make([]*schema.Message, 0, len(state.Messages))
	for _, message := range state.Messages {
		if message != nil && message.Extra != nil {
			if marked, _ := message.Extra[einoToolSearchReminderKey].(bool); marked {
				continue
			}
		}
		messages = append(messages, message)
	}
	state.Messages = messages
	return ctx, state, nil
}

func (m *oneShotMiddleware) WrapModel(
	_ context.Context,
	inner einomodel.BaseModel[*schema.Message],
	_ *adk.TypedModelContext[*schema.Message],
) (einomodel.BaseModel[*schema.Message], error) {
	return &observedModel{inner: inner, onCall: m.recordModelCall}, nil
}

func (m *oneShotMiddleware) recordModelCall() {
	if m == nil || m.exposure == nil {
		return
	}
	m.mu.Lock()
	m.exposure.AgentIterations++
	m.exposure.ModelCalls++
	m.mu.Unlock()
}

func (e *Engine) newDeepAgent(
	ctx context.Context,
	model einomodel.ToolCallingChatModel,
	req *ai.TurnRequest,
	summaryUsage *ai.Usage,
	toolExposure *ai.ToolExposure,
) (adk.Agent, *turnToolMiddleware, error) {
	toolScope, err := newTurnToolMiddleware(ctx, req.Tools, toolExposure)
	if err != nil {
		return nil, nil, err
	}
	dynamicTools := make([]tool.BaseTool, 0, len(req.Tools))
	immediateTools := make([]tool.BaseTool, 0, 2)
	for _, t := range req.Tools {
		adapted := &einoTool{t: t}
		if t.LoadMode == ai.ToolLoadImmediate {
			immediateTools = append(immediateTools, adapted)
			continue
		}
		dynamicTools = append(dynamicTools, adapted)
	}
	handlers := make([]adk.TypedChatModelAgentMiddleware[*schema.Message], 0, 5)
	// Let Eino normalize interrupted durable histories before any middleware
	// inspects them. The synthetic result deliberately reports an unknown
	// outcome: a process may have stopped after a side effect but before its
	// result was persisted, so the Agent must verify state instead of replaying
	// the call blindly.
	patchMiddleware, err := patchtoolcalls.New(ctx, &patchtoolcalls.Config{
		PatchedContentGenerator: func(_ context.Context, toolName, toolCallID string) (string, error) {
			return fmt.Sprintf("工具调用 %s（ID 为 %s）在会话中断前没有留下结果，执行状态未知。不要假定成功或失败；如仍与当前请求相关，先用只读能力核实当前状态，再决定是否继续。", toolName, toolCallID), nil
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("构建 Eino 工具调用修复中间件: %w", err)
	}
	handlers = append(handlers, patchMiddleware)
	if len(dynamicTools) > 0 {
		middleware, err := toolsearch.New(ctx, &toolsearch.Config{DynamicTools: dynamicTools})
		if err != nil {
			return nil, nil, fmt.Errorf("构建 Eino 动态工具检索: %w", err)
		}
		handlers = append(handlers, middleware)
	}
	if len(req.Skills) > 0 {
		middleware, err := skillmw.NewMiddleware(ctx, &skillmw.Config{
			Backend: newTurnSkillBackend(req.Skills),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("构建 Eino skill 中间件: %w", err)
		}
		handlers = append(handlers, middleware)
	}
	summaryTokens := e.cfg.SummarizeAfterTokens
	if summaryTokens <= 0 {
		summaryTokens = 24000
	}
	summaryMessages := e.cfg.SummarizeAfterMessages
	if summaryMessages <= 0 {
		summaryMessages = 80
	}
	summaryMiddleware, err := summarization.New(ctx, &summarization.Config{
		Model: model,
		Trigger: &summarization.TriggerCondition{
			ContextTokens:   summaryTokens,
			ContextMessages: summaryMessages,
		},
		Finalize: func(ctx context.Context, original []*schema.Message, summary *schema.Message) ([]*schema.Message, error) {
			if summaryUsage != nil && summary != nil && summary.ResponseMeta != nil && summary.ResponseMeta.Usage != nil {
				summaryUsage.InputTokens += int64(summary.ResponseMeta.Usage.PromptTokens)
				summaryUsage.OutputTokens += int64(summary.ResponseMeta.Usage.CompletionTokens)
			}
			return summarization.DefaultFinalize(ctx, original, summary)
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("构建 Eino 上下文摘要: %w", err)
	}
	handlers = append(handlers, summaryMiddleware)
	handlers = append(handlers, toolScope)

	maxIterations := deepMaxIterations(e.maxTurns, req.MaxIterations)
	agent, err := deep.New(ctx, &deep.Config{
		Name:        "nbco",
		Description: "nbco 公司运营中枢",
		// Deep Agent's loop, middleware and lifecycle remain native Eino. Its
		// bundled prompt targets a coding CLI, so nbco supplies the product
		// instruction through the framework's supported configuration point.
		Instruction:  req.System,
		ChatModel:    model,
		MaxIteration: maxIterations,
		// nbco has durable tasks, goals and workflows. Deep's ephemeral coding
		// checklist adds two model turns to simple operational actions without
		// improving recoverability.
		WithoutWriteTodos:      true,
		WithoutGeneralSubAgent: true,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: immediateTools,
			ToolCallMiddlewares: []compose.ToolMiddleware{{
				Name:      "nbco_deferred_tool_scope",
				Invokable: toolScope.guardInvokable,
			}},
		}},
		Handlers:         handlers,
		ModelRetryConfig: modelRetryConfig(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("构建 Eino DeepAgent: %w", err)
	}
	return agent, toolScope, nil
}

func deepMaxIterations(configured, requested int) int {
	if requested > 0 && requested < configured {
		return requested
	}
	return configured
}

type sessionResetter interface {
	DeleteSession(context.Context, string) error
}

func (e *Engine) rollbackFailedSession(ctx context.Context, sessionID string) error {
	result, err := e.runtime.LoadEvents(ctx, sessionID, &adk.LoadSessionEventsRequest{
		Reverse: true,
		Kinds:   []adk.SessionEventKind{adk.SessionEventSessionStatusIdle},
	})
	if err != nil {
		return err
	}
	sawCommitted := false
	if result != nil {
		for _, event := range result.Events {
			if event == nil || event.Lifecycle == nil || event.Lifecycle.StopReason == nil ||
				event.Lifecycle.StopReason.Type != "end_turn" {
				continue
			}
			sawCommitted = true
			err := adk.RollbackSession(ctx, e.runtime, sessionID, event.EventID,
				adk.WithRollbackSessionCheckPointStore[*schema.Message](e.runtime))
			if errors.Is(err, adk.ErrRollbackTargetInactive) || errors.Is(err, adk.ErrInvalidRollbackTarget) ||
				errors.Is(err, adk.ErrRollbackTargetNotFound) {
				continue
			}
			return err
		}
	}
	if sawCommitted {
		return errors.New("eino session has committed history but no active rollback target")
	}
	if resetter, ok := e.runtime.(sessionResetter); ok {
		return resetter.DeleteSession(ctx, sessionID)
	}
	return errors.New("eino session has no committed boundary and runtime store cannot reset it")
}

func (e *Engine) sessionHasEvents(ctx context.Context, sessionID string) (bool, error) {
	result, err := e.runtime.LoadEvents(ctx, sessionID, &adk.LoadSessionEventsRequest{Limit: 1})
	if err != nil {
		return false, err
	}
	return result != nil && len(result.Events) > 0, nil
}

func (e *Engine) engineSessionID(req *ai.TurnRequest) string {
	if e.runtime == nil || req.DisableSession || !persistentChatSessionID(req.SessionID) {
		return ""
	}
	if req.Mode == ai.TurnModeOneShot && req.EngineSession != "" {
		prefix := "eino:chat:" + req.SessionID + ":" + scopeFingerprint(req.SessionCapability) + "-"
		if strings.HasPrefix(req.EngineSession, prefix) {
			return req.EngineSession
		}
	}
	fingerprint := capabilityFingerprint(req.Tools, req.SessionCapability)
	prefix := "eino:chat:" + req.SessionID + ":" + fingerprint + ":"
	if strings.HasPrefix(req.EngineSession, prefix) {
		return req.EngineSession
	}
	generation := "initial"
	if req.EngineSession != "" {
		sum := sha256.Sum256([]byte(req.EngineSession))
		generation = fmt.Sprintf("%x", sum[:6])
	}
	return prefix + generation
}

func persistentChatSessionID(id string) bool {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	return err == nil && n > 0
}

func capabilityFingerprint(tools []ai.Tool, stableScope string) string {
	contracts := make([]string, 0, len(tools))
	for _, item := range tools {
		schema, _ := json.Marshal(item.InputSchema)
		contracts = append(contracts, strings.Join([]string{
			item.Name,
			item.Domain,
			item.Effect,
			string(item.LoadMode),
			string(item.Completion),
			item.RequiredAction,
			strconv.FormatBool(item.ResolvePermissionTarget != nil),
			strconv.FormatBool(item.GroupSensitive),
			item.Description,
			string(schema),
		}, "\x1f"))
	}
	sort.Strings(contracts)
	// Authorization and tool contracts are both session boundaries. Keeping a
	// durable agent trace after a schema/description change teaches the model
	// obsolete field names and behavior even though its permissions are valid.
	contractSum := sha256.Sum256([]byte(deepAgentRuntimeVersion + "\x00" + strings.Join(contracts, "\x00")))
	return scopeFingerprint(stableScope) + "-" + fmt.Sprintf("%x", contractSum[:8])
}

func scopeFingerprint(stableScope string) string {
	sum := sha256.Sum256([]byte(stableScope))
	return fmt.Sprintf("%x", sum[:8])
}

func (e *Engine) outputTokenLimit() int {
	if e.cfg.Provider == config.ProviderOpenAI && e.cfg.MaxCompletionTokens > 0 {
		return e.cfg.MaxCompletionTokens
	}
	return e.cfg.MaxTokens
}

func (e *Engine) modelFor(ctx context.Context, name string) (einomodel.ToolCallingChatModel, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = e.defaultModel
	}
	e.mu.Lock()
	if m, ok := e.models[name]; ok {
		e.mu.Unlock()
		return m, nil
	}
	cfg := e.cfg
	cfg.Model = name
	e.mu.Unlock()

	m, err := newChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("构建模型 %q: %w", name, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if cached, ok := e.models[name]; ok {
		return cached, nil
	}
	e.models[name] = m
	slog.Info("运行时模型已初始化", "model", name)
	return m, nil
}

func modelRetryConfig() *adk.ModelRetryConfig {
	return &adk.ModelRetryConfig{
		MaxRetries: 2,
		ShouldRetry: func(ctx context.Context, retryCtx *adk.RetryContext) *adk.RetryDecision {
			if retryCtx == nil || retryCtx.Err == nil || !isRetryableModelErr(ctx, retryCtx.Err) {
				return &adk.RetryDecision{Retry: false}
			}
			backoff := modelRetryBackoff(retryCtx.RetryAttempt)
			slog.Warn("模型调用失败，准备重试",
				"attempt", retryCtx.RetryAttempt,
				"backoff", backoff,
				"err", retryCtx.Err)
			return &adk.RetryDecision{Retry: true, Backoff: backoff}
		},
	}
}

func modelRetryBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 2 * time.Second
	case 2:
		return 6 * time.Second
	default:
		return 10 * time.Second
	}
}

func isRetryableModelErr(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	nonRetryable := []string{"401", "403", "unauthorized", "forbidden", "invalid api key"}
	for _, marker := range nonRetryable {
		if strings.Contains(s, marker) {
			return false
		}
	}
	retryable := []string{
		"502", "503", "504", "bad gateway", "service unavailable", "gateway timeout",
		"unexpected end of json input", "unexpected eof", "eof",
		"connection reset", "connection refused", "connection closed",
		"timeout", "deadline exceeded",
		"temporarily unavailable", "too many requests", "rate limit",
	}
	for _, marker := range retryable {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func dropLeadingNonUser(msgs []*schema.Message) []*schema.Message {
	for len(msgs) > 0 && msgs[0].Role != schema.User {
		msgs = msgs[1:]
	}
	return msgs
}

// readStream 逐块读一条流式消息，末尾用 ConcatMessages 重组成完整消息（含拼好的
// tool_calls）。onDelta 收到的是【本条助手消息累积到目前的可显示文本】快照——网关
// 据此「替换」显示。默认只展示正文 Content；showReasoning=true 时才包含推理内容。
// 这样：
//   - 默认不暴露模型内部推理；
//   - 一条消息若以 tool_calls 收尾，其前导文字只在本消息窗口内显示，下一条消息
//     （最终答复）开始时快照重置，网关随之刷新，不会「前导+答复」拼接。
//
// 收尾时网关用权威 res.Text（仅 Content）覆盖。
func readStream(sr *schema.StreamReader[*schema.Message], role schema.RoleType, onDelta func(string), showReasoning bool) (*schema.Message, error) {
	defer sr.Close()
	var chunks []*schema.Message
	var acc strings.Builder // 本条消息累积的可显示文本
	for {
		m, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if m == nil {
			continue
		}
		chunks = append(chunks, m)
		if onDelta != nil && role == schema.Assistant {
			wrote := false
			if showReasoning && m.ReasoningContent != "" {
				acc.WriteString(m.ReasoningContent)
				wrote = true
			}
			if m.Content != "" {
				acc.WriteString(m.Content)
				wrote = true
			}
			if wrote {
				snapshot := acc.String()
				if !showReasoning {
					snapshot = textfmt.StripReasoning(snapshot)
				}
				if strings.TrimSpace(snapshot) != "" {
					onDelta(snapshot)
				}
			}
		}
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	msg, err := schema.ConcatMessages(chunks)
	if err != nil || msg == nil {
		return msg, err
	}
	if role == schema.Assistant && !showReasoning {
		msg.Content = textfmt.StripReasoning(msg.Content)
	}
	return msg, nil
}

// collect 消费 ADK 事件流：配对 tool 调用与结果、累计用量、取最终文本。
func collect(iter *adk.AsyncIterator[*adk.AgentEvent], onEvent func(ai.Step), onDelta func(string), showReasoning bool, maxTokens int, blockedToolCall, replayedToolCall func(string, string) bool, completions map[string]ai.ToolCompletion) (*ai.TurnResult, error) {
	res := &ai.TurnResult{}
	// tool_call 步骤按 ToolCallID 待配对；结果事件到达时回填。
	pending := map[string]int{} // tool call id -> res.Steps 下标
	var finalText string
	var terminalErr error

	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			var willRetry *adk.WillRetryError
			if errors.As(event.Err, &willRetry) {
				continue
			}
			if terminalErr == nil {
				terminalErr = event.Err
			}
			// Drain the iterator so Runner can flush the durable idle/error boundary
			// and release its managed-session handle before rollback or retry.
			continue
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mo := event.Output.MessageOutput
		msg := mo.Message
		if mo.IsStreaming && mo.MessageStream != nil {
			// 流式：逐块读，助手最终答复的文本增量经 onDelta 实时推出，重组成完整消息。
			m, err := readStream(mo.MessageStream, mo.Role, onDelta, showReasoning)
			if err != nil {
				var willRetry *adk.WillRetryError
				if errors.As(err, &willRetry) {
					continue
				}
				return preservePartialAgentError(res, pending, err)
			}
			msg = m
		}
		if msg == nil {
			continue
		}
		switch mo.Role {
		case schema.Assistant:
			if u := msg.ResponseMeta; u != nil && u.Usage != nil {
				res.Usage.InputTokens += int64(u.Usage.PromptTokens)
				res.Usage.OutputTokens += int64(u.Usage.CompletionTokens)
				if u.FinishReason != "" {
					res.FinishReason = u.FinishReason
				}
			}
			for _, tc := range msg.ToolCalls {
				step := ai.Step{
					Kind:       ai.StepToolCall,
					ToolName:   tc.Function.Name,
					Args:       json.RawMessage(tc.Function.Arguments),
					Completion: completions[tc.Function.Name],
				}
				if replayedToolCall != nil && replayedToolCall(tc.Function.Name, tc.ID) {
					step.Replayed = true
				}
				pending[tc.ID] = len(res.Steps)
				res.Steps = append(res.Steps, step)
			}
			if txt := strings.TrimSpace(msg.Content); txt != "" && len(msg.ToolCalls) == 0 {
				finalText = txt
				res.Steps = append(res.Steps, ai.Step{Kind: ai.StepText, Result: txt})
			}
		case schema.Tool:
			idx, found := pending[msg.ToolCallID]
			if !found {
				// 理论上不会发生；单独记一条避免丢审计。
				res.Steps = append(res.Steps, ai.Step{Kind: ai.StepToolCall, ToolName: mo.ToolName, Result: msg.Content, Completion: completions[mo.ToolName]})
				idx = len(res.Steps) - 1
			} else {
				delete(pending, msg.ToolCallID)
				res.Steps[idx].Result = msg.Content
			}
			if blockedToolCall != nil && blockedToolCall(msg.ToolName, msg.ToolCallID) {
				res.Steps[idx].Err = "deferred tool was not loaded for the current turn"
			}
			if replayedToolCall != nil && replayedToolCall(msg.ToolName, msg.ToolCallID) {
				res.Steps[idx].Replayed = true
			}
			if onEvent != nil {
				onEvent(res.Steps[idx])
			}
		}
	}
	if terminalErr != nil {
		return preservePartialAgentError(res, pending, terminalErr)
	}
	if finalText == "" {
		for _, idx := range pending {
			if idx < 0 || idx >= len(res.Steps) || res.Steps[idx].Err != "" {
				continue
			}
			if res.Steps[idx].Replayed {
				res.Steps[idx].Err = "exact side-effect replay suppressed before execution"
			} else {
				res.Steps[idx].Err = "tool call ended without a handler result"
			}
		}
		if emptyTurnNeedsRepair(res, maxTokens) {
			// A tool loop that exhausts its lifecycle, or a reasoning model that
			// spends its output budget before visible text, may use the bounded
			// tool-free rendering fallback in the orchestrator.
			res.OutputLikelyTruncated = true
			return res, nil
		}
		return nil, errors.New("模型未给出最终答复（可能超出 tool 循环上限）")
	}
	res.Text = finalText
	res.OutputLikelyTruncated = outputLikelyTruncated(res.Usage, res.FinishReason, maxTokens)
	if onEvent != nil {
		onEvent(ai.Step{Kind: ai.StepText, Result: finalText})
	}
	return res, nil
}

func emptyTurnNeedsRepair(res *ai.TurnResult, maxTokens int) bool {
	if res == nil {
		return false
	}
	return len(res.Steps) > 0 || outputLikelyTruncated(res.Usage, res.FinishReason, maxTokens)
}

// preservePartialAgentError keeps tool evidence that predates a terminal agent
// error. Discarding it would make the orchestrator believe nothing happened and
// a repair/retry could execute an already-completed side effect again.
func preservePartialAgentError(res *ai.TurnResult, pending map[string]int, err error) (*ai.TurnResult, error) {
	if res == nil || (len(res.Steps) == 0 && res.Usage.InputTokens == 0 && res.Usage.OutputTokens == 0) {
		return nil, err
	}
	message := textfmt.RedactSecrets(err.Error())
	for _, idx := range pending {
		if idx >= 0 && idx < len(res.Steps) && res.Steps[idx].Err == "" {
			res.Steps[idx].Err = message
		}
	}
	res.FinishReason = "agent_error"
	res.OutputLikelyTruncated = true
	return res, nil
}

func outputLikelyTruncated(usage ai.Usage, finishReason string, maxTokens int) bool {
	reason := strings.ToLower(strings.TrimSpace(finishReason))
	for _, marker := range []string{"length", "max_token", "max token", "max_tokens", "max_output"} {
		if strings.Contains(reason, marker) {
			return true
		}
	}
	if maxTokens <= 0 || usage.OutputTokens <= 0 {
		return false
	}
	// 留一点余量：不同兼容网关可能在等于 max 或 max-1/max-2 时截断。
	return usage.OutputTokens >= int64(maxTokens-8)
}

// einoTool 把 ai.Tool 适配成 eino 的 InvokableTool。
type einoTool struct {
	t ai.Tool
}

func (e *einoTool) Info(context.Context) (*schema.ToolInfo, error) {
	js, err := toJSONSchema(e.t.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("工具 %s schema: %w", e.t.Name, err)
	}
	description := e.t.Description
	if e.t.Completion == ai.ToolCompletionAsynchronous {
		description += " 成功返回表示任务已持久化并被异步受理，不表示任务已经执行完成；后续进度和最终结果以任务状态为准。"
	}
	return &schema.ToolInfo{
		Name:        e.t.Name,
		Desc:        description,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js),
	}, nil
}

func (e *einoTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	args := strings.TrimSpace(argumentsInJSON)
	if args == "" {
		args = "{}"
	}
	return e.t.Handler(ctx, json.RawMessage(args))
}

func toJSONSchema(m map[string]any) (*jsonschema.Schema, error) {
	if m == nil {
		m = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var js jsonschema.Schema
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, err
	}
	return &js, nil
}

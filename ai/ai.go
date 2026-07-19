// Package ai 定义引擎无关的 AI 对话抽象。
//
// 设计原则（接口皆可换，中枢不可换）：
//   - 核心自持有 Tool 定义（名称 + JSON Schema + handler），不依赖任何框架类型；
//     eino、HTTP MCP 各自做薄适配。
//   - Engine 的抽象层级是「跑完一次明确模式的模型任务」：Deep 可含完整
//     tool 循环，OneShot 是受控单次生成；这样可保持替换模型框架时的边界稳定。
//   - 产品聊天记录由调用方落库；支持持久会话的引擎自行管理完整 agent 轨迹，
//     History 只用于首次建立引擎会话时的种子上下文。
package ai

import (
	"context"
	"encoding/json"
	"math"
)

// Tool 是领域能力暴露给 AI 的最小单元。
type Tool struct {
	Name        string
	Description string
	// Domain 是稳定的能力领域标识（people/work/workers/...）。编排器用它做
	// 语义路由；外部工具未声明时归入 extension。
	Domain string
	// Effect 描述工具的外部影响，供路由、审计和动作证据判断使用。
	// 未声明的第三方工具按 unknown 处理。
	Effect string
	// RequiredAction 声明调用工具所需的主动权限。空值表示沿用内建工具权限
	// 注册表；外部工具应显式声明，"superadmin" 表示仅超级管理员可用。
	RequiredAction string
	GroupSensitive bool // true 表示不得暴露给群共享会话
	// ApprovalRequired marks a host-provided destructive capability for nbco's
	// normal cross-turn confirmation wrapper. Built-ins may still use the
	// central name registry; extensions must declare the property on the tool
	// itself so governance does not depend on hard-coded external tool names.
	ApprovalRequired bool
	// InputSchema 是 JSON Schema（object 类型）。
	InputSchema map[string]any
	// Handler 执行工具并返回给模型的文本结果。
	// 业务错误（权限不足、目标不存在）应返回 (提示文本, nil) 让模型自行转述；
	// 只有系统性故障才返回 error。
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}

const (
	ToolEffectRead    = "read"
	ToolEffectWrite   = "write"
	ToolEffectExecute = "execute"
	ToolEffectUnknown = "unknown"
)

// Role 消息角色。
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message 一条对话消息（仅文本轮次；tool 轨迹见 Step，单独审计）。
type Message struct {
	Role    Role
	Content string
}

// Skill 是本轮与目标相关、可由 agent 按需加载的执行方法。候选检索和作用域
// 校验属于业务层，实际选择与加载交给 agent 框架。
type Skill struct {
	Name        string
	Description string
	Content     string
}

// StepKind 轨迹条目类型。
type StepKind string

const (
	StepToolCall StepKind = "tool_call"
	StepText     StepKind = "text"
)

// Step 一次引擎轮次内的执行轨迹条目，用于审计与回放。
type Step struct {
	Kind     StepKind
	ToolName string
	Args     json.RawMessage
	Result   string
	Err      string
}

// Usage 一轮的用量统计（能取到多少记多少，取不到为零值）。
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// ToolExposure records what the model actually saw after framework middleware
// applied deferred-tool selection. It intentionally differs from the full
// authorized catalog supplied in TurnRequest.Tools.
type ToolExposure struct {
	AgentIterations int
	ModelCalls      int
	ProtocolRetries int
	PeakToolCount   int
	PeakSchemaChars int
	PeakTools       []string
}

// CompletionOutcome is the model-declared reason a Deep turn is allowed to
// stop. The engine validates outcomes against the current turn's tool trace.
type CompletionOutcome string

const (
	CompletionOutcomeAnswer       CompletionOutcome = "answer"
	CompletionOutcomeActionResult CompletionOutcome = "action_result"
	CompletionOutcomeBlocked      CompletionOutcome = "blocked"
	CompletionOutcomeClarify      CompletionOutcome = "clarify"
)

// TurnMode defines the orchestration contract for a model call. It is selected
// by the call site, never inferred from user wording.
type TurnMode string

const (
	// TurnModeDeep enables the full autonomous agent loop. It is also the default
	// for the zero value so product conversations cannot silently lose capability.
	TurnModeDeep TurnMode = "deep"
	// TurnModeOneShot performs exactly one tool-free model generation for bounded
	// internal transformations such as extraction, summarization, and rendering.
	TurnModeOneShot TurnMode = "one_shot"
)

// TurnRequest 一轮对话请求。
type TurnRequest struct {
	// Mode selects the orchestration profile. Empty defaults to TurnModeDeep.
	Mode TurnMode
	// SessionID 是 nbco 侧会话 ID（落库主键），引擎可用它做日志关联。
	SessionID string
	// EngineSession 是引擎侧持久会话标识。空表示由引擎创建；调用方应保存
	// TurnResult 返回值并在下一轮传回。
	EngineSession string
	// DisableSession disables engine-managed history for channels whose context
	// can receive out-of-band messages (for example passive group listening).
	DisableSession bool
	// SessionCapability identifies the stable permission-scoped capability set.
	// It may be broader than Tools for a transient read-only turn, preventing
	// unnecessary session rotation while handlers still enforce the narrower set.
	SessionCapability string
	// System 系统提示，由 chat 编排器组装（身份、角色、当前时间等）。
	System string
	// History 本会话既往文本消息（不含本轮），按时间升序。
	History []Message
	// UserText 本轮用户输入。
	UserText string
	// Model 是本轮使用的模型名覆盖；空值表示使用引擎默认配置。
	Model string
	// Tools 本轮可用工具集（已按用户权限裁剪）。
	Tools []Tool
	// Skills 是已按语义相关性和用户/渠道作用域裁剪的候选执行方法。
	// 引擎只暴露元数据，由模型按需加载完整 Content。
	Skills []Skill
	// OnEvent 可选回调：工具调用与文本产出实时上报（审计/流式）。
	// 引擎在任意 goroutine 调用它，实现方需自行保证并发安全。
	OnEvent func(Step)
	// OnDelta 可选：流式渐进显示回调（eino 流式）。nil = 不流式。传入的是【当前
	// 助手消息累积到目前的可显示文本快照】，网关应「替换」显示而非
	// 追加；新消息开始时快照重置（短），网关随之刷新。收尾以 TurnResult.Text 为准。
	// 与 OnEvent 一样可能在任意 goroutine 调用，实现方自行保证并发安全。
	OnDelta func(text string)
	// StreamReasoning 为 true 时，OnDelta 会包含模型 ReasoningContent；默认 false，
	// 只流式展示最终正文，避免把内部推理暴露给用户。
	StreamReasoning bool
}

// TurnResult 一轮对话结果。
type TurnResult struct {
	// Text 模型最终答复（给用户看的）。
	Text string
	// EngineSession 引擎侧会话标识；调用方应持久化以便下一轮传回。
	EngineSession string
	// FinishReason 是底层模型返回的结束原因（如 stop/length/max_tokens）。
	FinishReason string
	// OutputLikelyTruncated 表示底层模型疑似耗尽输出预算。对思考型模型来说，
	// 这常表现为 output tokens 打满，但可见正文只有一两个词。
	OutputLikelyTruncated bool
	// Steps 本轮完整执行轨迹（含 tool 调用），用于审计。
	Steps             []Step
	Usage             Usage
	ToolExposure      ToolExposure
	CompletionOutcome CompletionOutcome
}

// Engine 跑一轮带工具的对话。实现：einoengine。
type Engine interface {
	// RunTurn 阻塞直到本轮完成（模型给出最终文本或出错）。
	RunTurn(ctx context.Context, req *TurnRequest) (*TurnResult, error)
	// Name 引擎标识（配置值），用于日志与会话落库。
	Name() string
}

// Embedder 把文本编码成向量，供语义检索（知识库、worker 经验召回）。
// 可选组件：未配置时为 nil，检索回退到词法匹配。实现：ai/embed。
type Embedder interface {
	// Embed 批量向量化，返回与 texts 一一对应、等维的向量。
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model 向量模型标识，随向量落库，模型变更时据此识别需重嵌入的旧数据。
	Model() string
}

// Cosine 余弦相似度（两向量维度需一致；任一为零向量返回 0）。
// 语义排序用；nbco 规模下暴力两两点积足够，无需向量索引扩展。
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt32(na) * sqrt32(nb))
}

func sqrt32(x float32) float32 {
	return float32(math.Sqrt(float64(x)))
}

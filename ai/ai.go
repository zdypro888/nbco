// Package ai 定义引擎无关的 AI 对话抽象。
//
// 设计原则（接口皆可换，中枢不可换）：
//   - 核心自持有 Tool 定义（名称 + JSON Schema + handler），不依赖任何框架类型；
//     eino、HTTP MCP 各自做薄适配。
//   - Engine 的抽象层级是「跑完一轮对话」（含 tool 循环），而非单次补全——
//     这样可保持后续替换模型框架时的边界稳定。
//   - 会话历史由调用方（chat 编排器）落库管理；eino 引擎重放 History。
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
	// InputSchema 是 JSON Schema（object 类型）。
	InputSchema map[string]any
	// Handler 执行工具并返回给模型的文本结果。
	// 业务错误（权限不足、目标不存在）应返回 (提示文本, nil) 让模型自行转述；
	// 只有系统性故障才返回 error。
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}

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

// TurnRequest 一轮对话请求。
type TurnRequest struct {
	// SessionID 是 nbco 侧会话 ID（落库主键），引擎可用它做日志关联。
	SessionID string
	// EngineSession 是引擎侧会话标识；当前 eino 引擎忽略。空表示新会话。
	EngineSession string
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
	Steps []Step
	Usage Usage
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

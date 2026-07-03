// Package ai 定义引擎无关的 AI 对话抽象。
//
// 设计原则（接口皆可换，中枢不可换）：
//   - 核心自持有 Tool 定义（名称 + JSON Schema + handler），不依赖任何框架类型；
//     eino、claude CLI、HTTP MCP 各自做薄适配。
//   - Engine 的抽象层级是「跑完一轮对话」（含 tool 循环），而非单次补全——
//     因为 claude CLI 引擎的 tool 循环发生在子进程内部，无法拆到补全粒度。
//   - 会话历史由调用方（chat 编排器）落库管理；eino 引擎重放 History，
//     claudecli 引擎用 EngineSession 恢复子进程会话，History 仅作审计。
package ai

import (
	"context"
	"encoding/json"
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
	// EngineSession 是引擎侧会话标识：claudecli 引擎用它 --resume 子进程会话；
	// eino 引擎忽略。空表示新会话。
	EngineSession string
	// System 系统提示，由 chat 编排器组装（身份、角色、当前时间等）。
	System string
	// History 本会话既往文本消息（不含本轮），按时间升序。
	History []Message
	// UserText 本轮用户输入。
	UserText string
	// Tools 本轮可用工具集（已按用户权限裁剪）。
	Tools []Tool
	// OnEvent 可选回调：工具调用与文本产出实时上报（审计/流式）。
	// 引擎在任意 goroutine 调用它，实现方需自行保证并发安全。
	OnEvent func(Step)
}

// TurnResult 一轮对话结果。
type TurnResult struct {
	// Text 模型最终答复（给用户看的）。
	Text string
	// EngineSession 引擎侧会话标识；调用方应持久化以便下一轮传回。
	EngineSession string
	// Steps 本轮完整执行轨迹（含 tool 调用），用于审计。
	Steps []Step
	Usage Usage
}

// Engine 跑一轮带工具的对话。实现：einoengine、claudecli。
type Engine interface {
	// RunTurn 阻塞直到本轮完成（模型给出最终文本或出错）。
	RunTurn(ctx context.Context, req *TurnRequest) (*TurnResult, error)
	// Name 引擎标识（配置值），用于日志与会话落库。
	Name() string
}

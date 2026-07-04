// Package einoengine 用 eino ADK 实现 ai.Engine：直接调模型 API（Claude / OpenAI 兼容），
// tool 循环在本进程内执行。这是客户自带 API key 的产品路径。
package einoengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
)

// Engine 持有一个已初始化的 ChatModel；Agent 按轮次构建（工具集因用户而异）。
type Engine struct {
	model    einomodel.ToolCallingChatModel
	maxTurns int
}

// New 按配置创建模型。
func New(ctx context.Context, cfg config.AIConfig) (*Engine, error) {
	m, err := newChatModel(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Engine{model: m, maxTurns: cfg.MaxTurns}, nil
}

func newChatModel(ctx context.Context, cfg config.AIConfig) (einomodel.ToolCallingChatModel, error) {
	switch cfg.Provider {
	case config.ProviderClaude:
		c := &claude.Config{
			APIKey:    cfg.APIKey,
			Model:     cfg.Model,
			MaxTokens: cfg.MaxTokens,
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
		}
		if cfg.MaxTokens > 0 {
			mt := cfg.MaxTokens
			c.MaxTokens = &mt
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

// Name 实现 ai.Engine。
func (e *Engine) Name() string { return config.EngineEino }

// RunTurn 实现 ai.Engine：构建带用户工具集的 agent，重放历史 + 本轮输入，收集事件直到最终答复。
func (e *Engine) RunTurn(ctx context.Context, req *ai.TurnRequest) (*ai.TurnResult, error) {
	tools := make([]tool.BaseTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, &einoTool{t: t})
	}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "nbco",
		Description: "nbco 公司运营中枢",
		Instruction: req.System,
		Model:       e.model,
		ToolsConfig: adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools},
		},
		MaxIterations: e.maxTurns,
	})
	if err != nil {
		return nil, fmt.Errorf("构建 agent: %w", err)
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
	for _, m := range req.History {
		switch m.Role {
		case ai.RoleUser:
			push(schema.UserMessage(m.Content))
		case ai.RoleAssistant:
			push(schema.AssistantMessage(m.Content, nil))
		}
	}
	push(schema.UserMessage(req.UserText))

	// 开启流式：ADK 把助手消息以 StreamReader 逐块吐出，collect 逐块读、把最终
	// 答复的文本增量经 OnDelta 实时推给网关（本地模型慢，用户能看到边冒字）。
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent, EnableStreaming: true})
	return collect(runner.Run(ctx, msgs), req.OnEvent, req.OnDelta)
}

// readStream 逐块读一条流式消息，末尾用 ConcatMessages 重组成完整消息（含拼好的
// tool_calls）。onDelta 收到的是【本条助手消息累积到目前的可显示文本】快照（含推理
// ReasoningContent + 正文 Content）——网关据此「替换」显示。这样：
//   - 推理模型（DeepSeek-R1/o1 等）的思考过程边冒字，不再干等；
//   - 一条消息若以 tool_calls 收尾，其前导文字只在本消息窗口内显示，下一条消息
//     （最终答复）开始时快照重置，网关随之刷新，不会「前导+答复」拼接。
// 收尾时网关用权威 res.Text（仅 Content）覆盖，抹掉推理态。
func readStream(sr *schema.StreamReader[*schema.Message], role schema.RoleType, onDelta func(string)) (*schema.Message, error) {
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
			if m.ReasoningContent != "" {
				acc.WriteString(m.ReasoningContent)
				wrote = true
			}
			if m.Content != "" {
				acc.WriteString(m.Content)
				wrote = true
			}
			if wrote {
				onDelta(acc.String())
			}
		}
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	return schema.ConcatMessages(chunks)
}

// collect 消费 ADK 事件流：配对 tool 调用与结果、累计用量、取最终文本。
func collect(iter *adk.AsyncIterator[*adk.AgentEvent], onEvent func(ai.Step), onDelta func(string)) (*ai.TurnResult, error) {
	res := &ai.TurnResult{}
	// tool_call 步骤按 ToolCallID 待配对；结果事件到达时回填。
	pending := map[string]int{} // tool call id -> res.Steps 下标
	var finalText string

	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			return nil, event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		mo := event.Output.MessageOutput
		msg := mo.Message
		if mo.IsStreaming && mo.MessageStream != nil {
			// 流式：逐块读，助手最终答复的文本增量经 onDelta 实时推出，重组成完整消息。
			m, err := readStream(mo.MessageStream, mo.Role, onDelta)
			if err != nil {
				return nil, err
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
			}
			for _, tc := range msg.ToolCalls {
				step := ai.Step{
					Kind:     ai.StepToolCall,
					ToolName: tc.Function.Name,
					Args:     json.RawMessage(tc.Function.Arguments),
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
				res.Steps = append(res.Steps, ai.Step{Kind: ai.StepToolCall, ToolName: mo.ToolName, Result: msg.Content})
				idx = len(res.Steps) - 1
			} else {
				delete(pending, msg.ToolCallID)
				res.Steps[idx].Result = msg.Content
			}
			if onEvent != nil {
				onEvent(res.Steps[idx])
			}
		}
	}
	if finalText == "" {
		return nil, errors.New("模型未给出最终答复（可能超出 tool 循环上限）")
	}
	res.Text = finalText
	if onEvent != nil {
		onEvent(ai.Step{Kind: ai.StepText, Result: finalText})
	}
	return res, nil
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
	return &schema.ToolInfo{
		Name:        e.t.Name,
		Desc:        e.t.Description,
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

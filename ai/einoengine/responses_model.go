package einoengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/zdypro888/nbco/config"
)

const responsesItemsKey = "nbco_responses_output_items"

type responsesCallOptions struct {
	DisableReasoning bool
	JSONOutput       bool
}

type responsesChatModel struct {
	client sdk.Client
	cfg    config.AIConfig
	tools  []*schema.ToolInfo
}

var _ einomodel.ToolCallingChatModel = (*responsesChatModel)(nil)

func newResponsesChatModel(cfg config.AIConfig) *responsesChatModel {
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(&http.Client{Timeout: chatHTTPTimeout(cfg.TimeoutMS)}),
		// Agent retries already own the tool replay and rollback boundary.
		option.WithMaxRetries(0),
	}
	if base := strings.TrimSpace(cfg.BaseURL); base != "" {
		opts = append(opts, option.WithBaseURL(strings.TrimRight(base, "/")+"/"))
	}
	return &responsesChatModel{client: sdk.NewClient(opts...), cfg: cfg}
}

func (m *responsesChatModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	clone := *m
	clone.tools = append([]*schema.ToolInfo(nil), tools...)
	return &clone, nil
}

func responsesMinimumEffort(model string) string {
	if strings.HasPrefix(model, "gpt-6.1-sol") || strings.HasPrefix(model, "gpt-6-astra") {
		return "low"
	}
	return "none"
}

func (m *responsesChatModel) request(input []*schema.Message, opts ...einomodel.Option) (responses.ResponseNewParams, error) {
	common := einomodel.GetCommonOptions(&einomodel.Options{Tools: m.tools}, opts...)
	call := einomodel.GetImplSpecificOptions[responsesCallOptions](nil, opts...)
	model := m.cfg.Model
	if common.Model != nil {
		model = *common.Model
	}
	items, err := responsesInput(input)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	body := map[string]any{
		"model": model, "input": items, "store": false,
		"include": []string{"reasoning.encrypted_content"},
	}
	budget := m.cfg.MaxCompletionTokens
	if budget <= 0 {
		budget = m.cfg.MaxTokens
	}
	if common.MaxTokens != nil {
		budget = *common.MaxTokens
	}
	if budget > 0 {
		body["max_output_tokens"] = budget
	}
	effort := strings.TrimSpace(m.cfg.ReasoningEffort)
	if call.DisableReasoning {
		effort = responsesMinimumEffort(model)
	} else if responsesMinimumEffort(model) == "low" && (effort == "none" || effort == "minimal") {
		effort = "low"
	}
	if effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	if call.JSONOutput {
		body["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
	}
	temperature := m.cfg.Temperature
	if common.Temperature != nil {
		temperature = *common.Temperature
	}
	if temperature > 0 && responsesMinimumEffort(model) != "low" {
		body["temperature"] = temperature
	}
	tools := make([]any, 0, len(common.Tools))
	for _, tool := range common.Tools {
		if tool == nil {
			continue
		}
		parameters, err := tool.ParamsOneOf.ToJSONSchema()
		if err != nil {
			return responses.ResponseNewParams{}, fmt.Errorf("Responses tool %s schema: %w", tool.Name, err)
		}
		// Existing tool schemas contain optional fields, unlike strict schemas.
		var params any = parameters
		if parameters == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{"type": "function", "name": tool.Name,
			"description": tool.Desc, "parameters": params, "strict": false})
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if common.ToolChoice != nil {
		switch *common.ToolChoice {
		case schema.ToolChoiceForbidden:
			body["tool_choice"] = "none"
		case schema.ToolChoiceForced:
			body["tool_choice"] = "required"
		case schema.ToolChoiceAllowed:
			body["tool_choice"] = "auto"
		}
		if *common.ToolChoice != schema.ToolChoiceForbidden && len(common.AllowedToolNames) > 0 {
			allowed := make([]any, 0, len(common.AllowedToolNames))
			for _, name := range common.AllowedToolNames {
				allowed = append(allowed, map[string]any{"type": "function", "name": name})
			}
			body["tool_choice"] = map[string]any{"type": "allowed_tools", "mode": body["tool_choice"], "tools": allowed}
		}
	}
	return param.Override[responses.ResponseNewParams](body), nil
}

func responsesInput(messages []*schema.Message) ([]any, error) {
	items := make([]any, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		replayed := map[string]bool{}
		hasMessage := false
		if msg.Role == schema.Assistant {
			// JSON strings survive both checkpoint and event-store round trips.
			if raw, ok := msg.Extra[responsesItemsKey]; ok {
				data, err := json.Marshal(raw)
				if err != nil {
					return nil, err
				}
				var reasoning []string
				if err := json.Unmarshal(data, &reasoning); err != nil {
					return nil, fmt.Errorf("Responses reasoning metadata: %w", err)
				}
				for _, item := range reasoning {
					var decoded map[string]any
					if json.Unmarshal([]byte(item), &decoded) != nil {
						return nil, errors.New("invalid Responses reasoning item")
					}
					switch decoded["type"] {
					case "message":
						hasMessage = true
					case "reasoning":
					case "function_call":
						id, _ := decoded["call_id"].(string)
						var found bool
						for _, call := range msg.ToolCalls {
							if call.ID == id {
								decoded["name"], decoded["arguments"] = call.Function.Name, call.Function.Arguments
								replayed[id], found = true, true
								break
							}
						}
						if !found {
							continue
						}
					default:
						return nil, errors.New("unsupported Responses replay item")
					}
					items = append(items, decoded)
				}
			}
		}
		if msg.Role == schema.Tool {
			items = append(items, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": msg.Content})
			continue
		}
		content := []any{}
		textType := "input_text"
		if msg.Role == schema.Assistant {
			textType = "output_text"
		}
		if msg.Content != "" && !hasMessage {
			content = append(content, map[string]any{"type": textType, "text": msg.Content})
		}
		for _, part := range msg.UserInputMultiContent {
			switch part.Type {
			case schema.ChatMessagePartTypeText:
				content = append(content, map[string]any{"type": "input_text", "text": part.Text})
			case schema.ChatMessagePartTypeImageURL:
				if part.Image == nil {
					return nil, errors.New("Responses image has no image payload")
				}
				content = append(content, map[string]any{"type": "input_image", "image_url": part.Image.URL})
			default:
				return nil, fmt.Errorf("unsupported Responses input part %q", part.Type)
			}
		}
		for _, part := range msg.MultiContent {
			switch part.Type {
			case schema.ChatMessagePartTypeText:
				content = append(content, map[string]any{"type": textType, "text": part.Text})
			case schema.ChatMessagePartTypeImageURL:
				if part.ImageURL == nil {
					return nil, errors.New("Responses image has no image payload")
				}
				content = append(content, map[string]any{"type": "input_image", "image_url": part.ImageURL.URL})
			default:
				return nil, fmt.Errorf("unsupported legacy Responses input part %q", part.Type)
			}
		}
		if len(content) > 0 {
			items = append(items, map[string]any{"type": "message", "role": msg.Role, "content": content})
		}
		for _, call := range msg.ToolCalls {
			if replayed[call.ID] {
				continue
			}
			items = append(items, map[string]any{"type": "function_call", "call_id": call.ID,
				"name": call.Function.Name, "arguments": call.Function.Arguments})
		}
	}
	return items, nil
}

type responsesOutput struct {
	Status            string `json:"status"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
		InputDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func responsesMessage(raw string) (*schema.Message, error) {
	var output responsesOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return nil, fmt.Errorf("decode Responses output: %w", err)
	}
	if output.Status != "completed" && output.Status != "incomplete" {
		return nil, fmt.Errorf("Responses status %s: %s %s", output.Status, output.Error.Code, output.Error.Message)
	}
	var envelope struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, err
	}
	msg := schema.AssistantMessage("", nil)
	var replay []string
	for i, item := range output.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					msg.Content += part.Text
				}
				if part.Type == "refusal" {
					msg.Content += part.Refusal
				}
			}
		case "function_call":
			if output.Status == "incomplete" {
				continue
			}
			if item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) {
				return nil, errors.New("Responses returned an incomplete function call")
			}
			index := len(msg.ToolCalls)
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{Index: &index, ID: item.CallID, Type: "function",
				Function: schema.FunctionCall{Name: item.Name, Arguments: item.Arguments}})
		case "reasoning":
			for _, summary := range item.Summary {
				msg.ReasoningContent += summary.Text
			}
		default:
			return nil, fmt.Errorf("unsupported Responses output item %q", item.Type)
		}
		replay = append(replay, string(envelope.Output[i]))
	}
	msg.Extra = map[string]any{responsesItemsKey: replay}
	finish := "stop"
	if len(msg.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	if output.Status == "incomplete" {
		// Never execute calls from a truncated response.
		msg.ToolCalls = nil
		switch output.IncompleteDetails.Reason {
		case "max_output_tokens":
			finish = "length"
		case "content_filter":
			finish = "content_filter"
		default:
			return nil, fmt.Errorf("Responses incomplete: %s", output.IncompleteDetails.Reason)
		}
	}
	msg.ResponseMeta = &schema.ResponseMeta{FinishReason: finish, Usage: &schema.TokenUsage{
		PromptTokens: output.Usage.InputTokens, CompletionTokens: output.Usage.OutputTokens, TotalTokens: output.Usage.TotalTokens,
	}}
	msg.ResponseMeta.Usage.PromptTokenDetails.CachedTokens = output.Usage.InputDetails.CachedTokens
	msg.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens = output.Usage.OutputDetails.ReasoningTokens
	return msg, nil
}

func (m *responsesChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	request, err := m.request(input, opts...)
	if err != nil {
		return nil, err
	}
	response, err := m.client.Responses.New(ctx, request)
	if err != nil {
		return nil, err
	}
	return responsesMessage(response.RawJSON())
}

func (m *responsesChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	request, err := m.request(input, opts...)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stream := m.client.Responses.NewStreaming(streamCtx, request)
	if err := stream.Err(); err != nil {
		cancel()
		_ = stream.Close()
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	type streamEvent struct {
		raw string
		err error
	}
	events := make(chan streamEvent, 1)
	go func() {
		defer close(events)
		defer stream.Close()
		for stream.Next() {
			select {
			case events <- streamEvent{raw: stream.Current().RawJSON()}:
			case <-streamCtx.Done():
				return
			}
		}
		if err := stream.Err(); err != nil {
			select {
			case events <- streamEvent{err: err}:
			case <-streamCtx.Done():
			}
		}
	}()
	go func() {
		defer cancel()
		defer writer.Close()
		// Eino has no reader-close callback. Empty transport chunks detect closure
		// while the upstream is reasoning without producing visible deltas.
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var sentText, sentReasoning string
		terminal := false
		for {
			var next streamEvent
			select {
			case <-ticker.C:
				if writer.Send(schema.AssistantMessage("", nil), nil) {
					return
				}
				continue
			case <-streamCtx.Done():
				writer.Send(nil, streamCtx.Err())
				return
			case value, ok := <-events:
				if !ok {
					writer.Send(nil, errors.New("Responses stream ended without a terminal response"))
					return
				}
				next = value
			}
			if next.err != nil {
				writer.Send(nil, next.err)
				return
			}
			var event struct {
				Type     string          `json:"type"`
				Delta    string          `json:"delta"`
				Response json.RawMessage `json:"response"`
				Message  string          `json:"message"`
			}
			if err := json.Unmarshal([]byte(next.raw), &event); err != nil {
				writer.Send(nil, err)
				return
			}
			msg := schema.AssistantMessage("", nil)
			switch event.Type {
			case "response.output_text.delta":
				msg.Content = event.Delta
				sentText += event.Delta
			case "response.reasoning_summary_text.delta":
				msg.ReasoningContent = event.Delta
				sentReasoning += event.Delta
			case "response.completed", "response.incomplete", "response.failed":
				terminal = true
				msg, err = responsesMessage(string(event.Response))
				if err != nil {
					writer.Send(nil, err)
					return
				}
				if !strings.HasPrefix(msg.Content, sentText) || !strings.HasPrefix(msg.ReasoningContent, sentReasoning) {
					writer.Send(nil, errors.New("Responses terminal output does not match streamed deltas"))
					return
				}
				msg.Content = strings.TrimPrefix(msg.Content, sentText)
				msg.ReasoningContent = strings.TrimPrefix(msg.ReasoningContent, sentReasoning)
			case "error":
				writer.Send(nil, fmt.Errorf("Responses stream error: %s", event.Message))
				return
			default:
				continue
			}
			if closed := writer.Send(msg, nil); closed {
				return
			}
			if terminal {
				return
			}
		}
	}()
	return reader, nil
}

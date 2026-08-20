// Package mcpbridge 把 ai.Tool 集合适配成 MCP server（官方 go-sdk）。
// 复用点：HTTP API 的对外 MCP 端点。
package mcpbridge

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/ai"
)

// NewServer 用给定工具集构建一个 MCP server。工具集应已按调用者权限裁剪。
func NewServer(name, version string, tools []ai.Tool) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: version}, nil)
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		handler := t.Handler
		s.AddTool(&mcp.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			raw, err := rawArguments(req.Params.Arguments)
			if err != nil {
				return errResult("参数解析失败: " + err.Error()), nil
			}
			ctx = withMCPInvocationKey(ctx, req, t.Effect)
			out, err := handler(ctx, raw)
			if err != nil {
				// MCP 规范：工具错误放 content + IsError，让模型可见并自我修正。
				return errResult(err.Error()), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil
		})
	}
	return s
}

// withMCPInvocationKey carries the standard HTTP idempotency header into the
// same write/execute middleware used by Eino and REST. The MCP SDK does not
// currently expose the JSON-RPC request ID to tool handlers, so clients that
// need response-loss recovery supply an opaque Idempotency-Key themselves.
func withMCPInvocationKey(ctx context.Context, req *mcp.CallToolRequest, effect string) context.Context {
	if effect != ai.ToolEffectWrite && effect != ai.ToolEffectExecute || req == nil || req.Extra == nil {
		return ctx
	}
	key := strings.TrimSpace(req.Extra.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 200 || strings.ContainsAny(key, "\r\n") {
		return ctx
	}
	return ai.WithToolInvocationKey(ctx, "mcp:"+key)
}

func rawArguments(args any) (json.RawMessage, error) {
	switch a := args.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case json.RawMessage:
		if len(a) == 0 {
			return json.RawMessage("{}"), nil
		}
		return a, nil
	default:
		return json.Marshal(a)
	}
}

func errResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

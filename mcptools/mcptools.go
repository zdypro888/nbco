// Package mcptools 把外部 MCP server（Streamable HTTP）的工具适配成 ai.Tool。
//
// 库的统一：MCP 的 server 与 client 都用官方 modelcontextprotocol/go-sdk。
// 刻意不用 eino-ext 的 mcp 组件 —— 它只有 client 能力且底层是另一个
// MCP 库（mark3labs/mcp-go），会造成同一协议两套依赖。适配成 ai.Tool 后，
// eino / claude CLI / codex CLI 三个引擎与对外 MCP 端点全部自然复用。
package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
)

// Connect 连接一个外部 MCP server 并取回其工具集（已包装为 ai.Tool）。
// 返回的 close 用于进程退出时断开会话。
func Connect(ctx context.Context, srv config.MCPServer) (tools []ai.Tool, close func(), err error) {
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL,
		HTTPClient: &http.Client{Transport: &headerTransport{headers: srv.Headers}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "nbco", Version: "1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("连接 MCP %s: %w", srv.Name, err)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("列举 MCP %s 工具: %w", srv.Name, err)
	}
	for _, t := range listed.Tools {
		schema := toSchemaMap(t.InputSchema)
		name := t.Name
		tools = append(tools, ai.Tool{
			// 前缀隔离命名空间，避免与内建工具或其他 server 冲突。
			Name:        srv.Name + "__" + name,
			Description: fmt.Sprintf("[外部工具·%s] %s", srv.Name, t.Description),
			InputSchema: schema,
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
				if err != nil {
					return "", fmt.Errorf("外部工具 %s/%s: %w", srv.Name, name, err)
				}
				text := contentText(res.Content)
				if res.IsError {
					return "[外部工具错误] " + text, nil
				}
				return text, nil
			},
		})
	}
	return tools, func() { _ = session.Close() }, nil
}

// toSchemaMap 把 client 侧收到的 InputSchema（any）规整为 map。
func toSchemaMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(v)
	if err == nil {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && m != nil {
			return m
		}
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func contentText(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if b.Len() == 0 {
		return "（无文本结果）"
	}
	return b.String()
}

// headerTransport 给每个请求追加固定 header。
type headerTransport struct {
	headers map[string]string
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

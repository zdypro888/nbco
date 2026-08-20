package mcpbridge

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/ai"
)

func TestWithMCPInvocationKey(t *testing.T) {
	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: http.Header{
		"Idempotency-Key": []string{"request-42"},
	}}}
	ctx := withMCPInvocationKey(context.Background(), req, ai.ToolEffectWrite)
	if got := ai.ToolInvocationKey(ctx); got != "mcp:request-42" {
		t.Fatalf("invocation key=%q", got)
	}
	if got := ai.ToolInvocationKey(withMCPInvocationKey(context.Background(), req, ai.ToolEffectRead)); got != "" {
		t.Fatalf("read invocation key=%q, want empty", got)
	}
	req.Extra.Header.Set("Idempotency-Key", "bad\nkey")
	if got := ai.ToolInvocationKey(withMCPInvocationKey(context.Background(), req, ai.ToolEffectExecute)); got != "" {
		t.Fatalf("invalid invocation key=%q, want empty", got)
	}
}

package mcptools

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestExternalToolName(t *testing.T) {
	tests := []struct {
		name, server, upstream string
		want                   string
	}{
		{"portable unchanged", "ops", "lookup-user", "ops__lookup-user"},
		{"unicode normalized", "ops", "查询 用户", ""},
		{"long truncated", "server", strings.Repeat("x", 100), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := externalToolName(tc.server, tc.upstream)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("name = %q, want %q", got, tc.want)
			}
			if len(got) > 64 {
				t.Fatalf("name 超过 64 bytes: %d %q", len(got), got)
			}
			for _, r := range got {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
					t.Fatalf("name 含非法字符 %q: %q", r, got)
				}
			}
		})
	}
	one, _ := externalToolName("ops", "查询 用户")
	two, _ := externalToolName("ops", "查询-用户")
	if one == two {
		t.Fatalf("不同上游名称归一化后冲突: %q", one)
	}
	if _, err := externalToolName("ops", " "); err == nil {
		t.Fatal("空上游名称应报错")
	}
}

func TestContentTextFallsBackToStructuredResult(t *testing.T) {
	if got := contentText(nil, map[string]any{"ok": true}); got != `{"ok":true}` {
		t.Fatalf("structured fallback = %q", got)
	}
	got := contentText([]mcp.Content{&mcp.TextContent{Text: "plain"}}, map[string]any{"ignored": true})
	if got != "plain" {
		t.Fatalf("text content 应优先: %q", got)
	}
}

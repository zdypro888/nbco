package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/ai"
)

func TestArgumentBoundaryValidatesBeforeBusinessHandler(t *testing.T) {
	called := 0
	wrapped := withArgumentNormalization(ai.Tool{
		Name: "validated_tool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"worker_id": map[string]any{"type": "integer"},
				"mode":      map[string]any{"type": "string", "enum": []any{"read", "write"}},
			},
			"required": []string{"worker_id", "mode"},
		},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			called++
			return `{"ok":true}`, nil
		},
	})

	for _, raw := range []string{`{}`, `{"worker_id":2,"mode":"other"}`, `{"worker_id":"bad","mode":"read"}`, `{broken`, `{"worker_id":2,"mode":"read"} {"worker_id":3,"mode":"write"}`} {
		result, err := wrapped.Handler(context.Background(), json.RawMessage(raw))
		if err != nil {
			t.Fatalf("invalid model input should be repairable tool output, got %v", err)
		}
		if !ToolResultRejected(result) || !strings.Contains(result, "invalid_arguments") {
			t.Fatalf("invalid args result = %q", result)
		}
	}
	if called != 0 {
		t.Fatalf("business handler called %d times for rejected inputs", called)
	}

	result, err := wrapped.Handler(context.Background(), json.RawMessage(`{"worker_ref":"2.0","mode":"read"}`))
	if err != nil || result != `{"ok":true}` || called != 1 {
		t.Fatalf("normalized valid call: result=%q err=%v called=%d", result, err, called)
	}
	if wrapped.NormalizeInput == nil || string(wrapped.NormalizeInput(json.RawMessage(`{ "mode":"read", "worker_ref":"2.0" }`))) != `{"mode":"read","worker_id":2}` {
		t.Fatalf("runtime invocation identity does not share handler normalization")
	}
}

func TestToolResultLifecycleIsStructured(t *testing.T) {
	accepted := asynchronousAcceptedResult("任务已持久化")
	if !ToolResultAccepted(accepted) || ToolResultRejected(accepted) {
		t.Fatalf("accepted result was not recognized: %s", accepted)
	}
	result, ok := ParseToolResult(accepted)
	if !ok || result.Message != "任务已持久化" || result.Completion != ai.ToolCompletionAsynchronous {
		t.Fatalf("accepted envelope = %+v, ok=%v", result, ok)
	}
	if ToolResultAccepted("任务已持久化") {
		t.Fatal("plain prose must not be treated as durable acceptance evidence")
	}
	pending := pendingApprovalResult("请确认")
	if !ToolResultPendingApproval(pending) || ToolResultRejected(pending) || ToolResultAccepted(pending) {
		t.Fatalf("pending approval result was not classified structurally: %s", pending)
	}
	if ToolResultPendingApproval("[nbco:pending_approval] 请确认") {
		t.Fatal("plain-text sentinels must not be interpreted as lifecycle state")
	}
}

func TestBoundedToolSchemaCacheEvictsOldestEntry(t *testing.T) {
	cache := newBoundedToolSchemaCache(2)
	key := func(name string) toolSchemaCacheKey {
		return toolSchemaCacheKey{Name: name, Hash: sha256.Sum256([]byte(name))}
	}
	first, second, third := key("first"), key("second"), key("third")
	cache.Store(first, toolSchemaCacheEntry{})
	cache.Store(second, toolSchemaCacheEntry{})
	cache.Store(second, toolSchemaCacheEntry{})
	cache.Store(third, toolSchemaCacheEntry{})
	if _, ok := cache.Load(first); ok {
		t.Fatal("oldest schema cache entry was not evicted")
	}
	if _, ok := cache.Load(second); !ok {
		t.Fatal("duplicate store changed FIFO order")
	}
	if _, ok := cache.Load(third); !ok {
		t.Fatal("new schema cache entry is missing")
	}
}

func TestNormalizeToolArgsUsesSchemaTypesAndUnambiguousAliases(t *testing.T) {
	schema := obj(map[string]any{
		"project_id": p("integer", "project"),
		"enabled":    p("boolean", "enabled"),
		"daily_at":   p("string", "time"),
		"limit":      p("integer", "limit"),
	})
	got := normalizeToolArgs(json.RawMessage(`{
		"project_ref":"6",
		"enabled":"True",
		"schedule_time":"18:30",
		"limit":"50"
	}`), schema)
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["project_id"] != float64(6) || decoded["enabled"] != true || decoded["daily_at"] != "18:30" || decoded["limit"] != float64(50) {
		t.Fatalf("normalized args = %#v", decoded)
	}
	for _, old := range []string{"project_ref", "schedule_time"} {
		if _, exists := decoded[old]; exists {
			t.Fatalf("legacy alias %q was not removed: %#v", old, decoded)
		}
	}
}

func TestNormalizeToolArgsInternalIDOnlyWhenUnambiguous(t *testing.T) {
	oneID := obj(map[string]any{"id": p("integer", "id")})
	if got := string(normalizeToolArgs(json.RawMessage(`{"internal_id":"19"}`), oneID)); got != `{"id":19}` {
		t.Fatalf("single id normalization = %s", got)
	}
	ambiguous := obj(map[string]any{
		"user_id":    p("integer", "user"),
		"project_id": p("integer", "project"),
	})
	var decoded map[string]any
	if err := json.Unmarshal(normalizeToolArgs(json.RawMessage(`{"internal_id":"2"}`), ambiguous), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["internal_id"]; !ok {
		t.Fatalf("ambiguous alias must be preserved: %#v", decoded)
	}
}

func TestNormalizeToolArgsNestedArray(t *testing.T) {
	schema := obj(map[string]any{
		"updates": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"user_id": p("integer", "user"),
				},
			},
		},
	})
	got := normalizeToolArgs(json.RawMessage(`{"updates":[{"user_ref":"3.0"}]}`), schema)
	if string(got) != `{"updates":[{"user_id":3}]}` {
		t.Fatalf("nested normalization = %s", got)
	}
}

func TestNormalizeToolArgsMapsGenericTimeToAt(t *testing.T) {
	schema := obj(map[string]any{
		"at":      p("string", "time"),
		"message": p("string", "message"),
	})
	got := normalizeToolArgs(json.RawMessage(`{"time":"2026-07-20T09:00:00+08:00","message":"test"}`), schema)
	if string(got) != `{"at":"2026-07-20T09:00:00+08:00","message":"test"}` {
		t.Fatalf("time normalization = %s", got)
	}
}

func TestNormalizeToolArgsRejectsFractionalInteger(t *testing.T) {
	schema := obj(map[string]any{"limit": p("integer", "limit")})
	got := normalizeToolArgs(json.RawMessage(`{"limit":"2.5"}`), schema)
	if string(got) != `{"limit":"2.5"}` {
		t.Fatalf("fractional integer must not be rounded: %s", got)
	}
}

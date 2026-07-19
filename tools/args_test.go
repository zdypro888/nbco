package tools

import (
	"encoding/json"
	"testing"
)

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

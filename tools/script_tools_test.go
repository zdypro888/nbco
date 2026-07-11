package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/store"
)

func TestRunStoredScriptToolValidatesComposedCapabilitiesWithoutSideEffects(t *testing.T) {
	u := &store.User{ID: 1, IsSuperadmin: true}
	d := Deps{SubcallAI: func(context.Context, *store.User, string, string) (string, error) {
		t.Fatal("validation stub must not execute AI")
		return "", nil
	}}
	st := &store.ScriptTool{
		Name:    "test_composed_tool",
		Runtime: "starlark",
		Source: `def run(args):
    tool_result = nbco_tool("list_roles", {})
    ai_result = nbco_ai("classify this")
    return tool_result + ai_result
`,
	}
	out, err := runStoredScriptTool(context.Background(), d, u, nil, st, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "{}" {
		t.Fatalf("output = %q", out)
	}

	st.Source = `def run(args): return nbco_tool("does_not_exist", {})`
	if _, err := runStoredScriptTool(context.Background(), d, u, nil, st, map[string]any{}); err == nil {
		t.Fatal("unknown composed tool must fail validation test")
	}
}

func TestRunStoredScriptToolEnforcesNestedCallBudgets(t *testing.T) {
	u := &store.User{ID: 1, IsSuperadmin: true}
	d := Deps{SubcallAI: func(context.Context, *store.User, string, string) (string, error) {
		return "", nil
	}}
	toolLines := make([]string, scriptNestedToolLimit+1)
	for i := range toolLines {
		toolLines[i] = `    nbco_tool("list_roles", {})`
	}
	st := &store.ScriptTool{
		Name: "budgeted_tool", Runtime: "starlark",
		Source: "def run(args):\n" + strings.Join(toolLines, "\n") + "\n    return \"ok\"\n",
	}
	if _, err := runStoredScriptTool(context.Background(), d, u, nil, st, nil); err == nil || !strings.Contains(err.Error(), fmt.Sprint(scriptNestedToolLimit)) {
		t.Fatalf("nested tool budget error = %v", err)
	}

	aiLines := make([]string, scriptNestedAILimit+1)
	for i := range aiLines {
		aiLines[i] = `    nbco_ai("classify")`
	}
	st.Source = "def run(args):\n" + strings.Join(aiLines, "\n") + "\n    return \"ok\"\n"
	if _, err := runStoredScriptTool(context.Background(), d, u, nil, st, nil); err == nil || !strings.Contains(err.Error(), fmt.Sprint(scriptNestedAILimit)) {
		t.Fatalf("nested AI budget error = %v", err)
	}
}

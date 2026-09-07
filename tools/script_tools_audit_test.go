package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/scripttool"
	"github.com/zdypro888/nbco/store"
)

func TestScriptAuditAssembledToolRejectsStaleDefinition(t *testing.T) {
	for _, change := range []string{"disabled", "source_changed"} {
		t.Run(change, func(t *testing.T) {
			s := openToolsTestStore(t)
			ctx := context.Background()
			admin := mkToolsUser(t, s, "script-audit-admin", true)
			actor := mkToolsUser(t, s, "script-audit-actor", false)
			calls := 0
			d := Deps{
				Store: s,
				SubcallAI: func(_ context.Context, _ *store.User, req SubcallRequest) (string, error) {
					calls++
					return req.Prompt, nil
				},
			}
			script, err := s.CreateScriptTool(ctx, store.ScriptTool{
				Name: "script_audit_freshness", Description: "Script freshness probe",
				Runtime: scripttool.RuntimeStarlark, InputSchema: []byte(`{"type":"object"}`),
				Source: `def run(args): return nbco_ai("old")`, CreatedBy: admin.ID,
			})
			if err != nil {
				t.Fatal(err)
			}
			publish := func(script *store.ScriptTool) {
				t.Helper()
				out, err := runStoredScriptTool(ctx, d, admin, nil, script, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.RecordScriptToolTest(ctx, script.ID, script.Source, out, true); err != nil {
					t.Fatal(err)
				}
				if err := s.SetScriptToolEnabled(ctx, script.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			publish(script)
			assembled := scriptAuditFindTool(t, ForUserContext(ctx, d, actor, nil), script.Name)
			if out, err := assembled.Handler(ctx, json.RawMessage(`{}`)); err != nil || out != "old" || calls != 1 {
				t.Fatalf("baseline out=%q err=%v calls=%d", out, err, calls)
			}

			if change == "disabled" {
				if err := s.SetScriptToolEnabled(ctx, script.ID, false); err != nil {
					t.Fatal(err)
				}
			} else {
				next := *script
				next.Source = `def run(args): return nbco_ai("new")`
				updated, err := s.UpdateScriptTool(ctx, script.ID, next)
				if err != nil {
					t.Fatal(err)
				}
				// Re-enable the new definition so rejection cannot pass on disabled alone.
				publish(updated)
			}
			if out, err := assembled.Handler(ctx, json.RawMessage(`{}`)); err == nil || out != "" || calls != 1 {
				t.Fatalf("stale tool executed: out=%q err=%v calls=%d", out, err, calls)
			}
			fresh := ForUserContext(ctx, d, actor, nil)
			if change == "disabled" {
				for _, candidate := range fresh {
					if candidate.Name == script.Name {
						t.Fatal("disabled script was assembled again")
					}
				}
				return
			}
			updated := scriptAuditFindTool(t, fresh, script.Name)
			if out, err := updated.Handler(ctx, json.RawMessage(`{}`)); err != nil || out != "new" || calls != 2 {
				t.Fatalf("fresh tool out=%q err=%v calls=%d", out, err, calls)
			}
		})
	}
}

func TestScriptAuditNestedToolInvocationIdentity(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	actor := mkToolsUser(t, s, "script-audit-nested-actor", false)
	calls := map[int]int{}
	probe := ai.Tool{
		Name: "script_audit_write_probe", Description: "Nested write probe",
		Effect: ai.ToolEffectWrite, InputSchema: obj(map[string]any{"value": p("integer", "Value")}, "value"),
		Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				Value int `json:"value"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			calls[args.Value]++
			return fmt.Sprintf("value=%d execution=%d", args.Value, calls[args.Value]), nil
		},
	}
	d := Deps{Store: s, Extra: []ai.Tool{probe}}
	parentCtx := ai.WithToolInvocationKey(ctx, "script-audit-parent-1")
	source := `def run(args): return nbco_tool("script_audit_write_probe", args)`
	builtins := scriptBuiltins(parentCtx, d, actor, nil, "script_audit_parent")
	run := func(callCtx context.Context, raw, want string, wantCalls7, wantCalls8 int, rebuild bool) {
		t.Helper()
		if rebuild {
			builtins = scriptBuiltins(callCtx, d, actor, nil, "script_audit_parent")
		}
		out, err := scripttool.Run(callCtx, "script_audit_parent", source, json.RawMessage(raw), scripttool.RunOptions{
			Predeclared: builtins,
		})
		if err != nil || out != want || calls[7] != wantCalls7 || calls[8] != wantCalls8 {
			t.Fatalf("nested call args=%s out=%q want=%q err=%v calls=%v", raw, out, want, err, calls)
		}
	}

	run(parentCtx, `{"value":7}`, "value=7 execution=1", 1, 0, false)
	run(parentCtx, `{"value":7}`, "value=7 execution=1", 1, 0, false)
	// Invocation identity must use normalized arguments and survive new builtins.
	run(parentCtx, `{"value":"7"}`, "value=7 execution=1", 1, 0, true)
	run(parentCtx, `{"value":8}`, "value=8 execution=1", 1, 1, false)
	run(parentCtx, `{"value":8}`, "value=8 execution=1", 1, 1, true)
	run(parentCtx, `{"value":7}`, "value=7 execution=1", 1, 1, false)
	run(ai.WithToolInvocationKey(ctx, "script-audit-parent-2"), `{"value":7}`, "value=7 execution=2", 2, 1, true)
}

func scriptAuditFindTool(t *testing.T, tools []ai.Tool, name string) ai.Tool {
	t.Helper()
	for _, candidate := range tools {
		if candidate.Name == name {
			return candidate
		}
	}
	t.Fatalf("script tool %q was not assembled", name)
	return ai.Tool{}
}

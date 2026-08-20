package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

func TestHostToolsFlowThroughCoreGovernance(t *testing.T) {
	runs := 0
	host := ai.Tool{
		Name: "host_destructive_write", Description: "host write", Domain: "ui",
		Effect: ai.ToolEffectWrite, GroupSensitive: true, ApprovalRequired: true,
		InputSchema: obj(nil), Handler: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "executed", nil
		},
	}
	user := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	assembled := ForUserContextWithTools(context.Background(), Deps{}, user, nil, []ai.Tool{host})
	var got ai.Tool
	for _, tool := range assembled {
		if tool.Name == host.Name {
			got = tool
			break
		}
	}
	if got.Handler == nil {
		t.Fatal("host tool was not assembled")
	}
	capability := capabilityForTool(got)
	if capability.Domain != "ui" || !capability.ApprovalRequired || capability.GroupAllowed {
		t.Fatalf("host capability = %+v", capability)
	}
	out, err := got.Handler(context.Background(), nil)
	if err != nil || runs != 0 || !strings.Contains(out, "审批") {
		t.Fatalf("approval wrapper = %q, err=%v, runs=%d", out, err, runs)
	}
	if namesOf(StripApprovalRequired([]ai.Tool{got}))[host.Name] {
		t.Fatal("approval-required host tool leaked into an entry without confirmation turns")
	}
}

func TestAssembledHostToolRechecksPermissionAtExecution(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	root := mkToolsUser(t, s, "host-permission-root", true)
	actor := mkToolsUser(t, s, "host-permission-actor", false)
	if err := s.GrantPerm(ctx, store.Grant{
		Kind: store.KindActive, UserID: actor.ID, Action: perm.ActManageWorker,
		Target: store.TargetAll, GrantedBy: root.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runs := 0
	host := ai.Tool{
		Name: "host_permission_probe", Description: "permission probe",
		Effect: ai.ToolEffectWrite, RequiredAction: perm.ActManageWorker,
		InputSchema: obj(nil), Handler: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "executed", nil
		},
	}
	var assembled ai.Tool
	for _, candidate := range ForUserContextWithTools(ctx, Deps{Store: s}, actor, nil, []ai.Tool{host}) {
		if candidate.Name == host.Name {
			assembled = candidate
			break
		}
	}
	if assembled.Handler == nil {
		t.Fatal("authorized host tool was not assembled")
	}
	if err := s.RevokePerm(ctx, root.ID, store.KindActive, actor.ID, perm.ActManageWorker, store.TargetAll); err != nil {
		t.Fatal(err)
	}
	if _, err := assembled.Handler(ctx, json.RawMessage(`{}`)); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("stale assembled tool error = %v, want ErrForbidden", err)
	}
	if runs != 0 {
		t.Fatalf("handler ran %d times after authority revocation", runs)
	}
}

func TestAssembledWriteToolUsesDurableInvocationIdentity(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	actor := mkToolsUser(t, s, "host-idempotency-root", true)
	runs := 0
	host := ai.Tool{
		Name: "host_idempotency_probe", Description: "idempotency probe",
		Effect:      ai.ToolEffectWrite,
		InputSchema: obj(map[string]any{"value": p("string", "value")}, "value"),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "executed", nil
		},
	}
	var assembled ai.Tool
	for _, candidate := range ForUserContextWithTools(ctx, Deps{Store: s}, actor, nil, []ai.Tool{host}) {
		if candidate.Name == host.Name {
			assembled = candidate
			break
		}
	}
	if assembled.Handler == nil {
		t.Fatal("write host tool was not assembled")
	}

	firstCtx := ai.WithToolInvocationKey(ctx, "turn-1-call-1")
	if out, err := assembled.Handler(firstCtx, json.RawMessage(`{"value":"same"}`)); err != nil || out != "executed" {
		t.Fatalf("first call out=%q err=%v", out, err)
	}
	out, err := assembled.Handler(firstCtx, json.RawMessage(`{ "value": "same" }`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "executed" || runs != 1 {
		t.Fatalf("durable replay raw=%q runs=%d", out, runs)
	}

	conflict, err := assembled.Handler(firstCtx, json.RawMessage(`{"value":"changed"}`))
	if err != nil {
		t.Fatal(err)
	}
	rejected, ok := ParseToolResult(conflict)
	if !ok || rejected.Status != "rejected" || rejected.ErrorType != "execution_state_unknown" || runs != 1 {
		t.Fatalf("mutated replay result=%+v raw=%q runs=%d", rejected, conflict, runs)
	}

	secondCtx := ai.WithToolInvocationKey(ctx, "turn-2-call-1")
	if out, err := assembled.Handler(secondCtx, json.RawMessage(`{"value":"same"}`)); err != nil || out != "executed" || runs != 2 {
		t.Fatalf("distinct invocation out=%q err=%v runs=%d", out, err, runs)
	}
}

func TestRecoverableToolReconstructsInterruptedResult(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	actor := mkToolsUser(t, s, "host-recovery-root", true)
	handlerRuns := 0
	recoveryRuns := 0
	finalizerRuns := 0
	host := ai.Tool{
		Name: "host_recovery_probe", Description: "recovery probe",
		Effect:      ai.ToolEffectWrite,
		InputSchema: obj(map[string]any{"value": p("string", "value")}, "value"),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			handlerRuns++
			return "new-result", nil
		},
		RecoverResult: func(context.Context, json.RawMessage) (string, bool, error) {
			recoveryRuns++
			return "recovered-result", true, nil
		},
		ResultPersisted: func(context.Context, json.RawMessage, string) error {
			finalizerRuns++
			return nil
		},
	}
	var assembled ai.Tool
	for _, candidate := range ForUserContextWithTools(ctx, Deps{Store: s}, actor, nil, []ai.Tool{host}) {
		if candidate.Name == host.Name {
			assembled = candidate
			break
		}
	}
	if assembled.Handler == nil {
		t.Fatal("recoverable host tool was not assembled")
	}

	args := json.RawMessage(`{"value":"same"}`)
	invocation := "turn-interrupted-call-1"
	key := toolInvocationReceiptKey(actor.ID, host.Name, invocation)
	if _, created, err := s.BeginExternalAction(ctx, key, "tool:"+host.Name, canonicalArgsHash(args)); err != nil || !created {
		t.Fatalf("seed interrupted receipt created=%v err=%v", created, err)
	}
	callCtx := ai.WithToolInvocationKey(ctx, invocation)
	out, err := assembled.Handler(callCtx, args)
	if err != nil || out != "recovered-result" || handlerRuns != 0 || recoveryRuns != 1 || finalizerRuns != 1 {
		t.Fatalf("recovery out=%q err=%v handler=%d recover=%d finalize=%d", out, err, handlerRuns, recoveryRuns, finalizerRuns)
	}
	out, err = assembled.Handler(callCtx, args)
	if err != nil || out != "recovered-result" || handlerRuns != 0 || recoveryRuns != 1 || finalizerRuns != 1 {
		t.Fatalf("durable recovered replay out=%q err=%v handler=%d recover=%d finalize=%d", out, err, handlerRuns, recoveryRuns, finalizerRuns)
	}
}

func TestHostToolCanDeclareResourceTargetAuthorization(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	root := mkToolsUser(t, s, "host-target-root", true)
	actor := mkToolsUser(t, s, "host-target-actor", false)
	target := mkToolsUser(t, s, "host-target-allowed", false)
	other := mkToolsUser(t, s, "host-target-denied", false)
	if err := s.GrantPerm(ctx, store.Grant{
		Kind: store.KindActive, UserID: actor.ID, Action: perm.ActSendMsg,
		Target: strconv.FormatInt(target.ID, 10), GrantedBy: root.ID,
	}); err != nil {
		t.Fatal(err)
	}
	runs := 0
	host := ai.Tool{
		Name: "host_target_probe", Description: "target probe",
		Effect: ai.ToolEffectWrite, RequiredAction: perm.ActSendMsg,
		InputSchema: obj(map[string]any{"user_id": p("integer", "target")}, "user_id"),
		ResolvePermissionTarget: func(_ context.Context, raw json.RawMessage) (string, error) {
			var args struct {
				UserID int64 `json:"user_id"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", err
			}
			return strconv.FormatInt(args.UserID, 10), nil
		},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "executed", nil
		},
	}
	var assembled ai.Tool
	for _, candidate := range ForUserContextWithTools(ctx, Deps{Store: s}, actor, nil, []ai.Tool{host}) {
		if candidate.Name == host.Name {
			assembled = candidate
			break
		}
	}
	if assembled.Handler == nil {
		t.Fatal("target-aware host tool was not assembled")
	}
	denied, _ := json.Marshal(map[string]any{"user_id": other.ID})
	if _, err := assembled.Handler(ctx, denied); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("wrong target call = %v, want ErrForbidden", err)
	}
	allowed, _ := json.Marshal(map[string]any{"user_id": target.ID})
	if out, err := assembled.Handler(ctx, allowed); err != nil || out != "executed" {
		t.Fatalf("allowed target call = %q, %v", out, err)
	}
	if runs != 1 {
		t.Fatalf("host handler runs = %d, want 1", runs)
	}
}

func TestAssembledToolRejectsStaleAdminWorkerIdentity(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	root := mkToolsUser(t, s, "host-admin-root", true)
	worker, _, err := s.CreateWorker(ctx, "host-admin-worker", root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkerAdmin(ctx, root.ID, worker.ID, true); err != nil {
		t.Fatal(err)
	}
	worker, err = s.UserByID(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	host := ai.Tool{
		Name: "host_super_probe", Description: "super probe",
		Effect: ai.ToolEffectWrite, RequiredAction: reqSuper,
		InputSchema: obj(nil), Handler: func(context.Context, json.RawMessage) (string, error) {
			runs++
			return "executed", nil
		},
	}
	var assembled ai.Tool
	for _, candidate := range ForUserContextWithTools(ctx, Deps{Store: s}, worker, nil, []ai.Tool{host}) {
		if candidate.Name == host.Name {
			assembled = candidate
			break
		}
	}
	if assembled.Handler == nil {
		t.Fatal("admin worker host tool was not assembled")
	}
	if err := s.SetWorkerAdmin(ctx, root.ID, worker.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := assembled.Handler(ctx, json.RawMessage(`{}`)); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("stale admin worker call error = %v, want ErrForbidden", err)
	}
	if runs != 0 {
		t.Fatalf("stale admin worker handler ran %d times", runs)
	}
}

func TestHostToolsCannotShadowCoreTools(t *testing.T) {
	user := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	host := ai.Tool{Name: "company_overview", Description: "untrusted replacement", Handler: func(context.Context, json.RawMessage) (string, error) {
		return "shadowed", nil
	}}
	assembled := ForUserContextWithTools(context.Background(), Deps{}, user, nil, []ai.Tool{host})
	for _, tool := range assembled {
		if tool.Name == host.Name {
			if tool.Description == host.Description {
				t.Fatal("host tool shadowed the built-in capability")
			}
			return
		}
	}
	t.Fatal("built-in company_overview is missing")
}

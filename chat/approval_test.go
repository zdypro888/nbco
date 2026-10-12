package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

func TestDurableApprovalToolsRejectUnknownAndChangedArguments(t *testing.T) {
	tool := ai.Tool{Name: "danger", ApprovalRequired: true}
	for _, pending := range [][]ai.ApprovalTool{
		{{InterruptID: "i", ToolName: "missing", Args: json.RawMessage(`{}`)}},
		{{InterruptID: "i", ToolName: "danger", Args: json.RawMessage(`{}`)}, {InterruptID: "i", ToolName: "danger", Args: json.RawMessage(`{}`)}},
		{{InterruptID: "i", ToolName: "danger", Args: json.RawMessage(`invalid`)}},
	} {
		if _, err := durableApprovalTools([]ai.Tool{tool}, pending); err == nil {
			t.Fatal("invalid interrupt accepted")
		}
	}
	pending, err := durableApprovalTools([]ai.Tool{tool}, []ai.ApprovalTool{{InterruptID: "i", ToolName: "danger", Args: json.RawMessage(`{"id":1}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResumedTools([]ai.Tool{tool}, pending); err != nil {
		t.Fatal(err)
	}
	pending[0].Args = json.RawMessage(`{"id":2}`)
	if err := validateResumedTools([]ai.Tool{tool}, pending); err == nil {
		t.Fatal("changed arguments accepted")
	}
	if err := validateResumedTools(nil, pending); err == nil {
		t.Fatal("revoked tool accepted")
	}
}

func TestNativeRejectionIsNotExecutionEvidence(t *testing.T) {
	step := ai.Step{Kind: ai.StepToolCall, ToolName: "danger", Args: json.RawMessage(`{}`), Result: "arbitrary result", ApprovalRejected: true}
	toolset := []ai.Tool{{Name: "danger", Effect: ai.ToolEffectWrite}}
	res := &ai.TurnResult{Steps: []ai.Step{step}}
	if execution := buildAutomationExecution(toolset, res); execution.SuccessfulToolCalls != 0 || execution.SuccessfulActionCalls != 0 {
		t.Fatalf("execution=%+v", execution)
	}
	if evidence := summarizeToolEvidence(res.Steps); len(evidence) != 1 || evidence[0].HandlerReturned || !evidence[0].Rejected {
		t.Fatalf("evidence=%+v", evidence)
	}
	if _, success := countToolEvidence(res.Steps); success != 0 {
		t.Fatal("rejection counted as success")
	}
	if verifiedMemoryToolEvidence(toolset, res.Steps) != "" {
		t.Fatal("rejection entered learning evidence")
	}
	if outcome := actionTurnOutcome(buildActionAudit("delete", toolset, res), res); outcome != "approval_rejected" {
		t.Fatalf("outcome=%s", outcome)
	}
}

func TestApprovalProgressReplacesUnresolvedCallByIdentity(t *testing.T) {
	previous := &ai.TurnResult{Usage: ai.Usage{InputTokens: 3}, Steps: []ai.Step{
		{Kind: ai.StepToolCall, ToolCallID: "done", ToolName: "read", Result: "evidence"},
		{Kind: ai.StepToolCall, ToolCallID: "waiting", ToolName: "danger", ApprovalPending: true},
	}}
	result := &ai.TurnResult{Usage: ai.Usage{InputTokens: 5}, Steps: []ai.Step{
		{Kind: ai.StepToolCall, ToolCallID: "waiting", ToolName: "danger", Result: ""},
	}}
	mergeApprovalProgress(result, previous)
	if len(result.Steps) != 2 || result.Steps[1].ApprovalPending || result.Usage.InputTokens != 8 {
		t.Fatalf("merged=%+v", result)
	}
	if total, success := countToolEvidence(result.Steps); total != 2 || success != 2 {
		t.Fatalf("counts=%d %d", total, success)
	}
	previous.Steps[1].ToolCallID = "still-waiting"
	mergeApprovalProgress(result, previous)
	if _, success := countToolEvidence(result.Steps); success != 2 {
		t.Fatal("unresolved pending step counted as success")
	}
}

type approvalLifecycleEngine struct {
	requests []*ai.TurnRequest
}

func (e *approvalLifecycleEngine) Name() string { return "approval-test" }
func (e *approvalLifecycleEngine) RunTurn(ctx context.Context, req *ai.TurnRequest) (*ai.TurnResult, error) {
	e.requests = append(e.requests, req)
	if req.Resume == nil {
		return &ai.TurnResult{EngineSession: "runtime:" + req.SessionID, Usage: ai.Usage{InputTokens: 3},
			PendingApproval: &ai.PendingApproval{CheckPointID: "checkpoint:" + req.SessionID, Tools: []ai.ApprovalTool{{InterruptID: "danger-call", ToolName: "host_danger", Args: json.RawMessage(`{"id":1}`)}}}}, nil
	}
	step := ai.Step{Kind: ai.StepToolCall, ToolName: "host_danger", Args: json.RawMessage(`{"id":1}`)}
	if !req.Resume.Decisions["danger-call"] {
		step.ApprovalRejected = true
	} else {
		for _, tool := range req.Tools {
			if tool.Name == step.ToolName {
				step.Result, _ = tool.Handler(ai.WithToolInvocationKey(ctx, "resume-call"), step.Args)
				break
			}
		}
	}
	return &ai.TurnResult{Text: "finished", EngineSession: req.EngineSession, Steps: []ai.Step{step}, Usage: ai.Usage{InputTokens: 5}}, nil
}

func TestOrchestratorApprovalResumePreservesTurnAndDomainGate(t *testing.T) {
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("NBCO_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, 7767002); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, 7767002)
		conn.Release()
	})
	for _, approve := range []bool{true, false} {
		t.Run(fmt.Sprint(approve), func(t *testing.T) {
			u, err := s.CreateUser(ctx, "approval-owner", true, store.Identity{Provider: "test", ExternalID: fmt.Sprintf("approval-%d", time.Now().UnixNano())})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = s.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, u.ID) })
			eng := &approvalLifecycleEngine{}
			runs := 0
			extension := TurnExtension{RequireApproval: true, Tools: []ai.Tool{{Name: "host_danger", RequiredAction: "superadmin", ApprovalRequired: true, Effect: ai.ToolEffectWrite,
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "integer"}}, "required": []string{"id"}},
				Handler:     func(context.Context, json.RawMessage) (string, error) { runs++; return "executed", nil }}}}
			o := New(s, eng, tools.Deps{Store: s, TZ: time.UTC}, time.UTC, false, time.Minute)
			reply, err := o.HandleMessageStreamWithExtensionResult(ctx, u, "web:ihtml", "delete", nil, extension)
			if err != nil || reply.PendingApproval == nil || runs != 0 {
				t.Fatalf("pause: %+v %v runs=%d", reply, err, runs)
			}
			turn, _ := s.ConversationTurnByID(ctx, reply.TurnID)
			if turn.Status != "awaiting_approval" || turn.AssistantMessageID != nil || turn.CompletedAt != nil {
				t.Fatalf("premature completion: %+v", turn)
			}
			if !eng.requests[0].RequireApproval {
				t.Fatal("native gate disabled")
			}
			// Recreate host ownership as on reconnect; no live session is required.
			o = New(s, eng, tools.Deps{Store: s, TZ: time.UTC}, time.UTC, false, time.Minute)
			pending, err := o.PendingApprovals(ctx, u, "web:ihtml")
			if err != nil || len(pending) != 1 {
				t.Fatalf("restore: %v %v", pending, err)
			}
			resumed, err := o.ResumeApprovalWithExtension(ctx, u, "web:ihtml", pending[0].ID, map[string]bool{"danger-call": approve}, nil, extension)
			if err != nil || resumed.PendingApproval != nil || resumed.TurnID != reply.TurnID {
				t.Fatalf("resume: %+v %v", resumed, err)
			}
			wantRuns := 0
			if approve {
				wantRuns = 1
			}
			if runs != wantRuns {
				t.Fatalf("runs=%d want=%d", runs, wantRuns)
			}
			if len(eng.requests) != 2 || eng.requests[1].SessionID != eng.requests[0].SessionID || eng.requests[1].InvocationScope != eng.requests[0].InvocationScope || eng.requests[1].EngineSession != turn.EngineSession {
				t.Fatal("resume changed execution identity")
			}
			if _, err := o.ResumeApprovalWithExtension(ctx, u, "web:ihtml", pending[0].ID, map[string]bool{"danger-call": approve}, nil, extension); err == nil {
				t.Fatal("duplicate resume accepted")
			}
			turn, _ = s.ConversationTurnByID(ctx, reply.TurnID)
			if turn.Status != "completed" {
				t.Fatalf("turn=%+v", turn)
			}
			var usage int64
			if err := s.Pool().QueryRow(ctx, `SELECT input_tokens FROM ai_usage WHERE conversation_turn_id=$1`, turn.ID).Scan(&usage); err != nil || usage != 8 {
				t.Fatalf("usage=%d err=%v", usage, err)
			}
			messages, _ := s.MessagesAfter(ctx, turn.SessionID, 0, 0)
			if len(messages) != 3 || messages[0].Content != "delete" || !strings.Contains(messages[1].Content, "Authenticated tool consent") || messages[2].Role != "assistant" {
				t.Fatalf("messages=%+v", messages)
			}
		})
	}
}

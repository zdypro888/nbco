package einoengine

import (
	"context"
	"encoding/json"
	"testing"

	sharedai "github.com/zdypro888/ihtml/agent"
	"github.com/zdypro888/nbco/ai"
)

func TestSharedToolPreservesDomainContractAndInvocationKey(t *testing.T) {
	wantArgs := json.RawMessage(`{"id":42}`)
	tool := sharedTool(ai.Tool{
		Name: "write_record", Domain: "records", Effect: "write", RequiredAction: "edit",
		GroupSensitive: true, ApprovalRequired: true, InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			if ai.ToolInvocationKey(ctx) != "durable-receipt" {
				t.Fatal("lost domain invocation key")
			}
			if string(args) != string(wantArgs) {
				t.Fatal("changed tool arguments")
			}
			return "stored", nil
		},
	})
	if tool.Name != "write_record" || tool.Domain != "records" || tool.Effect != "write" || tool.RequiredAction != "edit" || !tool.GroupSensitive || !tool.ApprovalRequired {
		t.Fatalf("lost permission metadata: %+v", tool)
	}
	ctx := sharedai.WithToolInvocationKey(context.Background(), "durable-receipt")
	got, err := tool.Handler(ctx, wantArgs)
	if got != "stored" || err != nil {
		t.Fatalf("result = %q, error = %v", got, err)
	}
	if sharedTool(ai.Tool{}).Handler != nil {
		t.Fatal("invented a missing handler")
	}
}

func TestLegacySessionValidation(t *testing.T) {
	for _, id := range []string{"1", " 42 ", "turn:123"} {
		if !validNBCOSessionID(id) {
			t.Fatalf("rejected legacy ID %q", id)
		}
	}
	for _, id := range []string{"", "0", "-1", "chat:42", "turn:0", "turn:turn:42"} {
		if validNBCOSessionID(id) {
			t.Fatalf("accepted invalid ID %q", id)
		}
	}
}

func TestDomainStepPreservesReplayAndCompletion(t *testing.T) {
	in := sharedai.Step{Kind: sharedai.StepToolCall, ToolName: "read", Args: json.RawMessage(`{}`), Result: "evidence", Replayed: true, Completion: sharedai.ToolCompletion("done")}
	out := domainStep(in)
	if string(out.Kind) != string(in.Kind) || out.ToolName != in.ToolName || string(out.Args) != string(in.Args) || out.Result != in.Result || !out.Replayed || string(out.Completion) != string(in.Completion) {
		t.Fatalf("changed step: %+v", out)
	}
}

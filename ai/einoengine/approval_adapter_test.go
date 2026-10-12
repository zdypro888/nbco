package einoengine

import (
	sharedai "github.com/zdypro888/ihtml/agent"
	"testing"
)

func TestDomainStepPreservesApprovalRejection(t *testing.T) {
	out := domainStep(sharedai.Step{Kind: sharedai.StepToolCall, ToolCallID: "call", ToolName: "danger", ApprovalRejected: true, ApprovalPending: true})
	if !out.ApprovalRejected || !out.ApprovalPending || out.ToolCallID != "call" {
		t.Fatal("lost native approval rejection")
	}
}

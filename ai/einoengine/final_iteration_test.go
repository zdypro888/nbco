package einoengine

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestFinalIterationMiddlewareReservesVisibleAnswer(t *testing.T) {
	m := &finalIterationMiddleware{maxIterations: 2}
	state := &adk.TypedChatModelAgentState[*schema.Message]{
		ToolInfos:         []*schema.ToolInfo{{Name: "one"}},
		DeferredToolInfos: []*schema.ToolInfo{{Name: "later"}},
	}
	_, got, err := m.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil || len(got.ToolInfos) != 1 || len(got.DeferredToolInfos) != 1 {
		t.Fatalf("first iteration changed tools: direct=%v deferred=%v err=%v", got.ToolInfos, got.DeferredToolInfos, err)
	}
	_, got, err = m.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil || got.ToolInfos != nil || got.DeferredToolInfos != nil {
		t.Fatalf("final iteration must withdraw tools: direct=%v deferred=%v err=%v", got.ToolInfos, got.DeferredToolInfos, err)
	}
}

package einoengine

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestToolBudgetMiddlewareWithdrawsTools(t *testing.T) {
	disabled := false
	m := &toolBudgetMiddleware{shouldDisable: func() bool { return disabled }}
	state := &adk.TypedChatModelAgentState[*schema.Message]{
		ToolInfos:         []*schema.ToolInfo{{Name: "one"}},
		DeferredToolInfos: []*schema.ToolInfo{{Name: "later"}},
	}
	_, got, err := m.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolInfos) != 1 || len(got.DeferredToolInfos) != 1 {
		t.Fatal("预算未触发时不应修改工具列表")
	}
	disabled = true
	_, got, err = m.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolInfos != nil || got.DeferredToolInfos != nil {
		t.Fatalf("预算触发后工具未撤下: direct=%v deferred=%v", got.ToolInfos, got.DeferredToolInfos)
	}
}

func TestToolBudgetMiddlewareReservesFinalIteration(t *testing.T) {
	m := &toolBudgetMiddleware{maxIterations: 2}
	state := &adk.TypedChatModelAgentState[*schema.Message]{ToolInfos: []*schema.ToolInfo{{Name: "one"}}}
	_, got, err := m.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil || len(got.ToolInfos) != 1 {
		t.Fatalf("首轮不应撤工具: tools=%v err=%v", got.ToolInfos, err)
	}
	_, got, err = m.BeforeModelRewriteState(context.Background(), state, nil)
	if err != nil || got.ToolInfos != nil {
		t.Fatalf("最后迭代必须撤工具收尾: tools=%v err=%v", got.ToolInfos, err)
	}
}

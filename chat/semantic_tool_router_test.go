package chat

import (
	"strings"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

func TestSemanticActionContinuationOnlyBeforeActionAttempt(t *testing.T) {
	plan := semanticToolPlan{Mode: "action", Tools: []string{"write_item"}}
	tools := []ai.Tool{
		{Name: "read_item", Effect: ai.ToolEffectRead},
		{Name: "write_item", Effect: ai.ToolEffectWrite},
	}
	if !shouldContinueSemanticAction(plan, tools, &ai.TurnResult{}) {
		t.Fatal("action plan with no tool attempt should continue")
	}
	if !shouldContinueSemanticAction(plan, tools, &ai.TurnResult{Steps: []ai.Step{{Kind: ai.StepToolCall, ToolName: "read_item", Result: "found"}}}) {
		t.Fatal("read-only first pass should continue an action plan")
	}
	if shouldContinueSemanticAction(plan, tools, &ai.TurnResult{Steps: []ai.Step{{Kind: ai.StepToolCall, ToolName: "write_item", Result: "ok"}}}) {
		t.Fatal("successful action attempt must not replay")
	}
	if shouldContinueSemanticAction(plan, tools, &ai.TurnResult{Steps: []ai.Step{{Kind: ai.StepToolCall, ToolName: "write_item", Err: "denied"}}}) {
		t.Fatal("failed action attempt must be reported, not replayed")
	}
}

func TestSemanticRouterInputIncludesRecentContextForReferences(t *testing.T) {
	input := renderSemanticToolRouterInput("telegram", "那就发吧", "用户先前在讨论通知员工", []store.ChatMessage{
		{Role: string(ai.RoleUser), Content: "通知黄桑今天开会"},
		{Role: string(ai.RoleAssistant), Content: "需要现在发送吗？"},
	}, []ai.Tool{{Name: "send_message", Domain: "people", Effect: ai.ToolEffectWrite, Description: "发送消息"}})
	for _, want := range []string{"那就发吧", "通知黄桑今天开会", "需要现在发送吗", "send_message"} {
		if !strings.Contains(input, want) {
			t.Fatalf("router input missing %q: %s", want, input)
		}
	}
}

func TestSemanticRouteKeepsSelectedCapabilities(t *testing.T) {
	all := []ai.Tool{
		{Name: "baseline", Domain: "base", Effect: ai.ToolEffectRead},
		{Name: "read_task", Domain: "work", Effect: ai.ToolEffectRead},
		{Name: "send_task", Domain: "work", Effect: ai.ToolEffectWrite},
		{Name: "unrelated", Domain: "admin", Effect: ai.ToolEffectWrite},
	}
	routed, _ := routeToolsFromSemanticPlan("send it", all, semanticToolPlan{
		Mode: "action", Domains: []string{"work"}, Tools: []string{"send_task"},
	})
	seen := map[string]bool{}
	for _, tool := range routed {
		seen[tool.Name] = true
	}
	if !seen["read_task"] || !seen["send_task"] || seen["unrelated"] {
		t.Fatalf("routed tools = %v", seen)
	}
}

func TestNormalizeSemanticToolPlanRejectsEmptyExecutablePlan(t *testing.T) {
	all := []ai.Tool{{Name: "read_task", Domain: "work", Effect: ai.ToolEffectRead}}
	for _, mode := range []string{"query", "action"} {
		if _, ok := normalizeSemanticToolPlan(semanticToolPlan{Mode: mode}, all); ok {
			t.Fatalf("empty %s plan must fall back", mode)
		}
	}
	if got, ok := normalizeSemanticToolPlan(semanticToolPlan{Mode: "answer"}, all); !ok || got.Mode != "answer" {
		t.Fatalf("answer plan should allow no tools: %+v ok=%v", got, ok)
	}
}

func TestSemanticActionRouteRecoversRelevantWriteTool(t *testing.T) {
	all := []ai.Tool{{
		Name: "list_workers", Domain: "workers", Effect: ai.ToolEffectRead,
		Description: "查看 AI worker 列表",
	}, {
		Name: "update_user_info", Domain: "people", Effect: ai.ToolEffectWrite,
		Description: "修改系统成员或 AI worker 的姓名和信息",
	}}
	for i := 0; i < 12; i++ {
		all = append(all, ai.Tool{
			Name: "unrelated_" + string(rune('a'+i)), Domain: "other", Effect: ai.ToolEffectWrite,
			Description: "执行无关的库存归档操作",
		})
	}
	routed, _ := routeToolsFromSemanticPlan("把这个 AI worker 重命名为 NBAI", all, semanticToolPlan{
		Mode: "action", Tools: []string{"list_workers"},
	})
	if !hasActionCapableTool(routed) {
		t.Fatalf("action route contains no write/execute tool: %+v", routed)
	}
	found := false
	for _, tool := range routed {
		found = found || tool.Name == "update_user_info"
	}
	if !found {
		t.Fatalf("semantic recovery omitted relevant write tool: %+v", routed)
	}
}

func TestSemanticActionDoesNotContinueWithoutAuthorizedActionTool(t *testing.T) {
	plan := semanticToolPlan{Mode: "action", Tools: []string{"read_item"}}
	toolset := []ai.Tool{{Name: "read_item", Effect: ai.ToolEffectRead}}
	if shouldContinueSemanticAction(plan, toolset, &ai.TurnResult{}) {
		t.Fatal("read-only identity must not run a second impossible action pass")
	}
}

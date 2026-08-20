package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

type evalContextKey struct{}

func TestEvalPersistenceContextSurvivesRequestCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.WithValue(context.Background(), evalContextKey{}, "request-value"))
	cancelParent()

	ctx, cancel := evalPersistenceContext(parent)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("persistence context inherited cancellation: %v", err)
	}
	if got := ctx.Value(evalContextKey{}); got != "request-value" {
		t.Fatalf("context value = %v, want request-value", got)
	}
}

func TestEvaluateAssertionsChecksBehaviorNotWordingIntent(t *testing.T) {
	assertions := EvalAssertions{
		RequiredSubstrings:    []string{"已读取"},
		ForbiddenSubstrings:   []string{"<table"},
		RequiredAnyToolGroups: [][]string{{"list_users", "query_data"}},
		ForbiddenTools:        []string{"send_message"},
		MinSuccessfulTools:    1,
		MaxToolCalls:          3,
		NoMarkdownTable:       true,
	}
	if failures := evaluateAssertions(assertions, "✅ 已读取员工目录", []string{"tool_search", "list_users"}, 2); len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	failures := evaluateAssertions(assertions, "| 姓名 |\n| --- |", []string{"send_message"}, 0)
	for _, expected := range []string{"输出缺少必需文本", "未调用任一候选工具", "调用了禁用工具", "成功工具数不足", "输出包含 Markdown 表格"} {
		if !slices.ContainsFunc(failures, func(got string) bool { return len(got) >= len(expected) && got[:len(expected)] == expected }) {
			t.Fatalf("missing failure %q in %v", expected, failures)
		}
	}
}

func TestMarkdownTableDetectionCatchesRawModelFormatting(t *testing.T) {
	if !hasMarkdownTable("| 姓名 | 状态 |\n| --- | --- |\n| PRO | active |") {
		t.Fatal("expected markdown table to be detected")
	}
	if hasMarkdownTable("员工：PRO | 状态：active") {
		t.Fatal("inline separators are not a markdown table")
	}
}

func TestSimulatedEvalToolsNeverInvokeProductionHandler(t *testing.T) {
	called := false
	tools := simulatedEvalTools([]ai.Tool{{
		Name: "write", Handler: func(context.Context, json.RawMessage) (string, error) {
			called = true
			return "production", nil
		},
	}}, map[string]string{"write": "simulated"})
	got, err := tools[0].Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil || got != "simulated" || called {
		t.Fatalf("result=%q called=%t err=%v", got, called, err)
	}
}

func TestConversationAssetUsagesDistinguishesCandidatesFromLoadedSkills(t *testing.T) {
	usages := conversationAssetUsages(
		[]int64{2},
		[]ai.Skill{{Name: "nbco-skill-9"}, {Name: "external-skill"}},
		[]ai.Step{{Kind: ai.StepToolCall, ToolName: "skill", Args: json.RawMessage(`{"skill":"nbco-skill-9"}`)}},
		AutomationExecution{ActionCalls: 1, SuccessfulActionCalls: 1},
		"",
	)
	want := map[string]bool{
		"2:" + store.AssetPhaseInjected:  true,
		"9:" + store.AssetPhaseCandidate: true,
		"9:" + store.AssetPhaseLoaded:    true,
	}
	for _, usage := range usages {
		key := strconv.FormatInt(usage.KnowledgeID, 10) + ":" + usage.Phase
		if !want[key] || usage.TurnOutcome != store.AssetOutcomeActionSucceeded {
			t.Fatalf("unexpected usage: %+v all=%+v", usage, usages)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing usages: %v", want)
	}
}

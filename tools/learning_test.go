package tools

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestEmployeeLearningRuleCannotEscalateScope(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	employee := mkToolsUser(t, s, "learning-employee", false)
	result := callToolByName(t, learningTools(Deps{Store: s, TZ: time.UTC}, employee), "propose_learning_candidate", map[string]any{
		"kind":         store.LearningKindRule,
		"memory_class": store.LearningMemoryDurable,
		"title":        "个人通知偏好",
		"content":      "没有事项时不向我发送自动通知。",
		"scope":        "global",
		"evidence":     "用户明确提出个人偏好",
	})
	if result == "" {
		t.Fatal("proposal returned an empty result")
	}
	items, err := s.ListLearningCandidates(ctx, store.LearningStatusPending, store.LearningKindRule, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("learning candidates = %+v, %v", items, err)
	}
	wantScope := fmt.Sprintf("user:%d", employee.ID)
	if items[0].Scope != wantScope {
		t.Fatalf("employee rule scope = %q, want %q", items[0].Scope, wantScope)
	}
}

func TestLearningCandidateDeduplicationUsesScopeAndContent(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	first := mkToolsUser(t, s, "learning-first", false)
	second := mkToolsUser(t, s, "learning-second", false)
	const title = "工作沟通偏好"

	propose := func(u *store.User, content string) string {
		t.Helper()
		return callToolByName(t, learningTools(Deps{Store: s, TZ: time.UTC}, u), "propose_learning_candidate", map[string]any{
			"kind":         store.LearningKindRule,
			"memory_class": store.LearningMemoryDurable,
			"title":        title,
			"content":      content,
			"scope":        "global",
		})
	}

	const original = "只在工作时间发送非紧急通知。"
	propose(first, original)
	propose(second, original)
	propose(first, "非紧急通知集中到每天十点发送。")
	duplicate := propose(first, original)
	if !strings.Contains(duplicate, "完全一致") {
		t.Fatalf("exact duplicate result = %q", duplicate)
	}

	items, err := s.ListLearningCandidates(ctx, store.LearningStatusPending, store.LearningKindRule, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("learning candidate count = %d, want 3: %+v", len(items), items)
	}
	wantScopes := map[string]int{
		fmt.Sprintf("user:%d", first.ID):  2,
		fmt.Sprintf("user:%d", second.ID): 1,
	}
	gotScopes := make(map[string]int)
	for _, item := range items {
		gotScopes[item.Scope]++
	}
	if !reflect.DeepEqual(gotScopes, wantScopes) {
		t.Fatalf("learning candidate scopes = %+v, want %+v", gotScopes, wantScopes)
	}
}

package tools

import (
	"context"
	"fmt"
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

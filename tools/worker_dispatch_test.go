package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/store"
)

func TestPlanWorkerDispatchUsesBoundedSemanticResult(t *testing.T) {
	user := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	workers := []*store.User{
		{ID: 2, Status: store.UserActive},
		{ID: 3, Status: store.UserActive},
		{ID: 4, Status: store.UserDisabled},
	}
	caps := map[int64]*store.WorkerCapability{
		2: {WorkerID: 2, Engine: "codex", Capabilities: []string{"code", "interactive-pty"}},
		3: {WorkerID: 3, Engine: "claude", Capabilities: []string{"pdf"}},
	}
	d := Deps{SubcallAI: func(_ context.Context, _ *store.User, req SubcallRequest) (string, error) {
		if req.Purpose != "worker_dispatch_match" || !strings.Contains(req.Prompt, `"worker_id":2`) {
			t.Fatalf("unexpected semantic dispatch request: %+v", req)
		}
		return `{"task_kind":"materials","scores":[{"worker_id":2,"score":9},{"worker_id":3,"score":-2},{"worker_id":999,"score":5}]}`, nil
	}}
	plan := planWorkerDispatch(context.Background(), d, user, "", []string{"整理一份资料"}, workers, caps)
	if plan.Kind != store.TaskKindMaterials || plan.Scores[2] != 5 || plan.Scores[3] != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	if _, exists := plan.Scores[999]; exists {
		t.Fatalf("unknown worker escaped candidate boundary: %+v", plan.Scores)
	}

	requested := planWorkerDispatch(context.Background(), d, user, store.TaskKindEngineering, []string{"任意说法"}, workers, caps)
	if requested.Kind != store.TaskKindEngineering {
		t.Fatalf("explicit kind was overwritten: %+v", requested)
	}
}

func TestPlanWorkerDispatchFallsBackWithoutLanguageRules(t *testing.T) {
	plan := planWorkerDispatch(context.Background(), Deps{}, &store.User{ID: 1}, "", []string{"任何自然语言"}, []*store.User{{ID: 2, Status: store.UserActive}}, nil)
	if plan.Kind != store.TaskKindGeneral || len(plan.Scores) != 0 {
		t.Fatalf("fallback = %+v", plan)
	}
}

func TestWorkerCandidateLessHasStableIdentityTieBreak(t *testing.T) {
	a := workerCandidate{w: &store.User{ID: 9}, rank: 20, open: 1, online: true, accepted: 2}
	b := workerCandidate{w: &store.User{ID: 3}, rank: 20, open: 1, online: true, accepted: 2}
	if workerCandidateLess(a, b) || !workerCandidateLess(b, a) {
		t.Fatalf("equal candidates must use stable worker ID order")
	}
}

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/workerproto"
)

func TestDelegateReviewCarriesExecutionScopeAndEvidence(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	boss := mkToolsUser(t, s, "boss", true)
	executor, _, err := s.CreateWorker(ctx, "executor", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _, err := s.CreateWorker(ctx, "reviewer", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkToolsProject(t, s, boss.ID)
	input, err := s.CreateFile(ctx, &store.File{
		Source: "test", OriginalName: "input.txt", MIMEType: "text/plain",
		SHA256: "review-input", StoragePath: "re/review-input", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTaskWithFileAttachmentsAndWorkerRun(ctx, &store.Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: executor.ID,
		Title: "implement feature", Acceptance: "tests pass",
	}, []int64{input.ID}, "source input", store.WorkerRunSpec{
		Executor: workerproto.ExecutorAgent, ScopeType: "repo",
		ScopeKey: "repo:product", ScopeTitle: "Product repository",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.ClaimNextWorkerRun(ctx, executor.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.CreateFile(ctx, &store.File{
		Source: "test", OriginalName: "result.txt", MIMEType: "text/plain",
		SHA256: "review-result", StoragePath: "re/review-result", CreatedBy: &executor.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkerArtifact(ctx, run.ID, executor.ID, run.ClaimID, artifact.ID, "delivery"); err != nil {
		t.Fatal(err)
	}
	if _, completed, _, _, err := s.CompleteWorkerRun(ctx, run.ID, executor.ID, run.ClaimID,
		"implemented", "", workerproto.OutcomeSucceeded, nil,
		store.WorkerRunFinalization{ID: "review-source-final", Hash: "review-source-hash"}); err != nil || completed.Status != store.TaskDone {
		t.Fatalf("complete source task: task=%+v err=%v", completed, err)
	}

	out := callToolByName(t, reviewTools(Deps{Store: s, TZ: time.UTC}, boss), "delegate_review", map[string]any{
		"task_id": task.ID, "reviewer_id": reviewer.ID,
	})
	if !strings.Contains(out, "已委派") {
		t.Fatalf("delegate_review output = %q", out)
	}
	reviews, err := s.TasksOfAssignee(ctx, reviewer.ID, true)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("review tasks = %+v err=%v", reviews, err)
	}
	reviewRun, err := s.LatestWorkerRunForTask(ctx, reviews[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if reviewRun.ScopeType != "repo" || reviewRun.ScopeKey != "repo:product" || reviewRun.ScopeTitle != "Product repository" {
		t.Fatalf("review execution lost source scope: %+v", reviewRun)
	}
	files, err := s.TaskFileAttachments(ctx, reviews[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, file := range files {
		seen[file.ID] = true
	}
	if !seen[input.ID] || !seen[artifact.ID] {
		t.Fatalf("review evidence incomplete: files=%+v", files)
	}
}

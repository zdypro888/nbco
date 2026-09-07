package store

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestFinalizedSessionReplayCannotOverwriteNewAttempt(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	boss := mkUser(t, s, "session-boss", true)
	worker, _, err := s.CreateWorker(ctx, "session-worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, worker.ID, "session retries", nil)
	first, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "audit", "audit", first.ID, &task.ID)
	if err != nil {
		t.Fatal(err)
	}
	final := testWorkerFinalization(first.ClaimID, "failed-attempt")
	if _, _, _, err := s.FailWorkerRun(ctx, first.ID, worker.ID, first.ClaimID, "retry", final); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkerSessionForFinalization(ctx, session.ID, worker.ID, first.ID, first.ClaimID, final.ID, "old", "old-session", "", "old-dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE worker_runs SET available_at=now()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkerSessionForClaim(ctx, session.ID, worker.ID, second.ID, second.ClaimID, "new", "new-session", "", "new-dir"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkerSessionForFinalization(ctx, session.ID, worker.ID, first.ID, first.ClaimID, final.ID, "old", "old-session", "", "old-dir"); err != nil {
		t.Fatal(err)
	}
	var ref, summary, dir string
	if err := s.pool.QueryRow(ctx, `SELECT engine_session_ref,summary,workdir FROM worker_sessions WHERE id=$1`, session.ID).Scan(&ref, &summary, &dir); err != nil {
		t.Fatal(err)
	}
	if ref != "new-session" || summary != "new" || dir != "new-dir" {
		t.Fatalf("old replay overwrote current session: %s %s %s", ref, summary, dir)
	}
}

func TestMaterialAnalysisSharesFileACLWithoutSelfAuthorization(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	uploader := mkUser(t, s, "uploader", false)
	requester := mkUser(t, s, "requester", false)
	worker, _, err := s.CreateWorker(ctx, "material-worker", requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, requester.ID)
	file, err := s.CreateFile(ctx, &File{Source: "telegram", OriginalName: "shared.txt", StoragePath: "shared", SHA256: "shared", CreatedBy: &uploader.ID})
	if err != nil {
		t.Fatal(err)
	}
	create := func() (*Task, error) {
		return s.CreateMaterialTaskWithWorkerRun(ctx, &Task{ProjectID: pj.ID, AssignerID: requester.ID, AssigneeID: worker.ID, Title: "analyze"}, []int64{file.ID}, "", WorkerRunSpec{}, MaterialTaskSpec{OwnerID: requester.ID, Title: "analyze", Instruction: "read"})
	}
	if _, err := create(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new task must not authorize its own private input: %v", err)
	}
	shared := mkTask(t, s, pj.ID, uploader.ID, requester.ID, "shared source", nil)
	if _, err := s.AddTaskAttachmentFileOnce(ctx, shared.ID, file.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := create(); err != nil {
		t.Fatalf("existing shared attachment rejected: %v", err)
	}
}

func TestWorkEvidenceMergeRejectsConflictingTaskProject(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	u := mkUser(t, s, "evidence-owner", true)
	p1 := mkProject(t, s, u.ID)
	p2, err := s.CreateProject(ctx, "other project", "", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := mkTask(t, s, p1.ID, u.ID, u.ID, "linked task", nil)
	evidence, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{SourceType: "test", SourceKey: "same", Content: "fact", TaskID: &task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{SourceType: "test", SourceKey: "same", Content: "fact", ProjectID: &p2.ID}); err == nil {
		t.Fatal("conflicting merged association accepted")
	}
	var projectID, taskID int64
	if err := s.pool.QueryRow(ctx, `SELECT project_id,task_id FROM work_evidence WHERE id=$1`, evidence.ID).Scan(&projectID, &taskID); err != nil {
		t.Fatal(err)
	}
	if projectID != p1.ID || taskID != task.ID {
		t.Fatalf("association corrupted: %d %d", projectID, taskID)
	}
}

func TestChannelMessagesForwardDrainsEqualTimestampBacklog(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	u := mkUser(t, s, "monitor-owner", true)
	channel := "telegram:group:-987654"
	sess, err := s.StartGroupSession(ctx, u.ID, channel, "eino")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < 301; i++ {
		if _, err := s.AppendMessageWithEnvelope(ctx, sess.ID, "user", fmt.Sprint(i), MessageEnvelope{SourceCreatedAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ChannelMessagesForward(ctx, channel, at.Add(-time.Second), time.Now(), 0, 0, 300)
	if err != nil || len(page) != 300 {
		t.Fatalf("first page: n=%d err=%v", len(page), err)
	}
	last := page[len(page)-1].ID
	page, err = s.ChannelMessagesForward(ctx, channel, at.Add(-time.Second), time.Now(), last, 0, 300)
	if err != nil || len(page) != 1 || page[0].Content != "300" {
		t.Fatalf("backlog was skipped: %+v err=%v", page, err)
	}
}

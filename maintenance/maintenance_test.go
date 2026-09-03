package maintenance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memoryLedger struct {
	nextID   int64
	finished []RunRecord
}

func (l *memoryLedger) EnsureJobs(context.Context, []Job, time.Time) error { return nil }

func (l *memoryLedger) Claim(_ context.Context, job Job, _ string, trigger string, dryRun, _ bool, now time.Time) (*Claim, error) {
	l.nextID++
	return &Claim{RunID: l.nextID, Job: job}, nil
}

func (l *memoryLedger) Finish(_ context.Context, claim Claim, _ string, dryRun bool, finished time.Time, result Result, runErr error) error {
	status := "succeeded"
	errText := ""
	if runErr != nil {
		status, errText = "failed", runErr.Error()
	}
	l.finished = append(l.finished, RunRecord{ID: claim.RunID, JobName: claim.Job.Name,
		Class: claim.Job.Class, DryRun: dryRun, Status: status, CompletedAt: &finished,
		Report: result, Error: errText})
	return nil
}

func (l *memoryLedger) Status(context.Context, int) ([]JobState, []RunRecord, error) {
	return nil, l.finished, nil
}

func TestRunnerRejectsAutomaticBusinessAndAuditMutation(t *testing.T) {
	for _, class := range []Class{ClassAuthority, ClassAudit} {
		_, err := New(&memoryLedger{}, true, Job{
			Name: "unsafe.job", Class: class, Interval: time.Hour,
			Run: func(context.Context, time.Time, bool) (Result, error) { return Result{}, nil },
		})
		if err == nil {
			t.Fatalf("class %s should be rejected", class)
		}
	}
}

func TestRunnerRunNowPreservesDryRunAndFailureFacts(t *testing.T) {
	ledger := &memoryLedger{}
	seenDryRun := false
	runner, err := New(ledger, true,
		Job{Name: "derived.inspect", Class: ClassDerived, Interval: time.Hour,
			Run: func(_ context.Context, _ time.Time, dryRun bool) (Result, error) {
				seenDryRun = dryRun
				return Result{Inspected: 3, Details: map[string]int64{"rows": 3}}, nil
			}},
		Job{Name: "ephemeral.failure", Class: ClassEphemeral, Interval: time.Hour,
			Run: func(context.Context, time.Time, bool) (Result, error) {
				return Result{Inspected: 1}, errors.New("failed safely")
			}},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runner.RunNow(context.Background(), []string{"derived.inspect"}, true)
	if err != nil || !seenDryRun || len(runs) != 1 || runs[0].Report.Inspected != 3 || runs[0].Report.Reclaimed != 0 {
		t.Fatalf("dry-run = %+v seen=%v err=%v", runs, seenDryRun, err)
	}
	runs, err = runner.RunNow(context.Background(), []string{"ephemeral.failure"}, false)
	if err == nil || len(runs) != 1 || runs[0].Status != "failed" {
		t.Fatalf("failure = %+v err=%v", runs, err)
	}
}

func TestFileBlobJobDryRunAndApplyUseAuthoritativePaths(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	write := func(name string, age time.Time) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, age, age); err != nil {
			t.Fatal(err)
		}
		return path
	}
	livePath := write(filepath.Join("aa", "live"), old)
	orphanPath := write(filepath.Join("bb", "orphan"), old)
	uploadPath := write(".upload-interrupted", old)
	freshPath := write(filepath.Join("cc", "fresh"), time.Now())
	live := func(context.Context) (map[string]bool, error) {
		return map[string]bool{filepath.Join("aa", "live"): true}, nil
	}
	job := FileBlobJob(root, 24*time.Hour, time.Hour, live)
	preview, err := job.Run(context.Background(), time.Now(), true)
	if err != nil || preview.Inspected != 2 || preview.Reclaimed != 0 {
		t.Fatalf("preview = %+v err=%v", preview, err)
	}
	for _, path := range []string{livePath, orphanPath, uploadPath, freshPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry-run removed %s: %v", path, err)
		}
	}
	applied, err := job.Run(context.Background(), time.Now(), false)
	if err != nil || applied.Reclaimed != 2 {
		t.Fatalf("apply = %+v err=%v", applied, err)
	}
	for _, path := range []string{livePath, freshPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected file %s missing: %v", path, err)
		}
	}
	for _, path := range []string{orphanPath, uploadPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate %s still exists: %v", path, err)
		}
	}
}

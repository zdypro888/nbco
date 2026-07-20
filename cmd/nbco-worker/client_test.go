package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/zdypro888/nbco/workerproto"
)

func TestClientMe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok-worker-a" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"name":"worker-a","is_superadmin":false,"is_worker":true,"owner_id":1}`))
	}))
	defer srv.Close()

	ident, err := newClient(srv.URL, "tok-worker-a").Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ident.ID != 7 || ident.Name != "worker-a" || !ident.IsWorker || ident.OwnerID == nil || *ident.OwnerID != 1 {
		t.Fatalf("identity = %+v", ident)
	}
}

func TestClientNextAcceptsLegacyTaskEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/next" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"task":{"id":42,"claim_id":"claim-1","title":"legacy","command":"true"},"knowledge":["known"],"history":["seen"]}`))
	}))
	defer srv.Close()

	run, knowledge, history, err := newClient(srv.URL, "tok-worker-a").Next(context.Background(), "command")
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.ID != 42 || run.ClaimID != "claim-1" || run.Command != "true" {
		t.Fatalf("legacy run = %+v", run)
	}
	if len(knowledge) != 1 || knowledge[0] != "known" || len(history) != 1 || history[0] != "seen" {
		t.Fatalf("legacy context: knowledge=%v history=%v", knowledge, history)
	}
}

func TestClientHeartbeatCarriesRunLease(t *testing.T) {
	var body struct {
		RunID   int64  `json:"run_id"`
		ClaimID string `json:"claim_id"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/heartbeat" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()
	if err := newClient(srv.URL, "tok-worker-a").Heartbeat(context.Background(), 42, "claim-1"); err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.ClaimID != "claim-1" {
		t.Fatalf("heartbeat body = %+v", body)
	}
}

func TestClientHeartbeatReportsLostLease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stale claim", http.StatusConflict)
	}))
	defer srv.Close()

	err := newClient(srv.URL, "tok-worker-a").Heartbeat(context.Background(), 42, "claim-1")
	if !errors.Is(err, errWorkerLeaseLost) {
		t.Fatalf("heartbeat error = %v, want errWorkerLeaseLost", err)
	}
}

func TestClientRequestInput(t *testing.T) {
	var body struct {
		RunID            int64  `json:"run_id"`
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		Content          string `json:"content"`
		FinalizationID   string `json:"finalization_id"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		EngineSessionRef string `json:"engine_session_ref"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/request-input" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok-worker-a" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	err := newClient(srv.URL, "tok-worker-a").RequestInput(context.Background(), 42, "claim-1", "请提供 repo URL",
		SessionInfo{ID: 9, EngineSessionRef: "019f2c09-8ec0-7b91-a9bc-f7b95138ef3f"}, "/tmp/repo")
	if err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.TaskID != 42 || body.ClaimID != "claim-1" || body.Content != "请提供 repo URL" || body.FinalizationID == "" {
		t.Fatalf("request body = %+v", body)
	}
	if body.WorkerSessionID != 9 || body.EngineSessionRef == "" {
		t.Fatalf("request input must persist worker session: %+v", body)
	}
}

func TestClientFailCarriesClaimAndSession(t *testing.T) {
	var body struct {
		RunID            int64  `json:"run_id"`
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		Error            string `json:"error"`
		FinalizationID   string `json:"finalization_id"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		EngineSessionRef string `json:"engine_session_ref"`
		Workdir          string `json:"workdir"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/fail" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	session := SessionInfo{ID: 9, EngineSessionRef: "019f2c09-8ec0-7b91-a9bc-f7b95138ef3f"}
	if err := newClient(srv.URL, "tok-worker-a").Fail(context.Background(), 42, "claim-1", "agent crashed", session, "/tmp/repo"); err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.TaskID != 42 || body.ClaimID != "claim-1" || body.Error != "agent crashed" ||
		body.WorkerSessionID != 9 || body.EngineSessionRef == "" || body.Workdir != "/tmp/repo" || body.FinalizationID == "" {
		t.Fatalf("fail body = %+v", body)
	}
}

func TestClientSubmitCarriesStructuredOutcome(t *testing.T) {
	var body struct {
		RunID          int64               `json:"run_id"`
		TaskID         int64               `json:"task_id"`
		Outcome        workerproto.Outcome `json:"outcome"`
		ExitCode       *int                `json:"exit_code"`
		FinalizationID string              `json:"finalization_id"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/submit" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	exitCode := 7
	if err := newClient(srv.URL, "tok-worker-a").Submit(context.Background(), 42, "claim-1",
		"execution failed", "", SessionInfo{}, "/tmp/repo",
		SubmissionResult{Outcome: workerproto.OutcomeFailed, ExitCode: &exitCode}); err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.TaskID != 42 || body.Outcome != workerproto.OutcomeFailed || body.ExitCode == nil || *body.ExitCode != 7 || body.FinalizationID == "" {
		t.Fatalf("submit body = %+v", body)
	}
}

func TestClientFinalizationRetryKeepsRequestID(t *testing.T) {
	var attempts int
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var body struct {
			FinalizationID string `json:"finalization_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, body.FinalizationID)
		if attempts == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	if err := newClient(srv.URL, "tok-worker-a").RequestInput(context.Background(), 42, "claim-1", "need input", SessionInfo{}, ""); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("finalization retry changed identity: attempts=%d ids=%v", attempts, ids)
	}
}

func TestClientSubmitRejectsMissingOutcome(t *testing.T) {
	err := newClient("http://127.0.0.1", "tok-worker-a").Submit(context.Background(), 42, "claim-1",
		"done", "", SessionInfo{}, "/tmp/repo", SubmissionResult{})
	if err == nil {
		t.Fatal("missing outcome should be rejected before the request")
	}
}

func TestClientUpdateSessionCarriesActiveClaim(t *testing.T) {
	var body struct {
		RunID            int64  `json:"run_id"`
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		EngineSessionRef string `json:"engine_session_ref"`
		Workdir          string `json:"workdir"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/session" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	session := SessionInfo{ID: 9, EngineSessionRef: "019f2c09-8ec0-7b91-a9bc-f7b95138ef3f"}
	if err := newClient(srv.URL, "tok-worker-a").UpdateSession(context.Background(), 42, "claim-1", session, "/tmp/repo"); err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.TaskID != 42 || body.ClaimID != "claim-1" || body.WorkerSessionID != 9 ||
		body.EngineSessionRef == "" || body.Workdir != "/tmp/repo" {
		t.Fatalf("session body = %+v", body)
	}
}

func TestDownloadFileReplacesExistingDestination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-worker-a" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("new content"))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "artifact.txt")
	if err := os.WriteFile(dst, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := newClient(srv.URL, "tok-worker-a").DownloadFile(context.Background(), "/file", dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Fatalf("downloaded content = %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(dst), ".nbco-download-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary downloads left behind: %v, err=%v", matches, err)
	}
}

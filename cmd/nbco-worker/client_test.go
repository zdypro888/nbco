package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestClientRuntimeAuthenticatesAndResolvesProxyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/runtime" || r.URL.Query().Get("engine") != "codex" {
			t.Fatalf("runtime request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer tok-worker-a" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"engine":"codex","source":"central","model":"gpt-central","base_url":"/api/worker/openai/v1","wire_api":"responses"}`))
	}))
	defer srv.Close()

	runtime, err := newClient(srv.URL, "tok-worker-a").Runtime(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Model != "gpt-central" || runtime.BaseURL != srv.URL+"/api/worker/openai/v1" || runtime.WireAPI != "responses" {
		t.Fatalf("runtime = %+v", runtime)
	}
}

func TestClientRuntimeRejectsCredentialedProxyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"engine":"codex","source":"central","model":"gpt-central","base_url":"https://user:secret@example.com/v1","wire_api":"responses"}`))
	}))
	defer srv.Close()
	if _, err := newClient(srv.URL, "tok").Runtime(context.Background(), "codex"); err == nil {
		t.Fatal("credentialed model proxy URL should be rejected")
	}
}

func TestWorkerLLMRetryKeepsIdempotencyKey(t *testing.T) {
	var keys []string
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/llm" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		attempts++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if attempts == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	previousBackoff := agentRetryBackoff
	agentRetryBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { agentRetryBackoff = previousBackoff })
	w := &Worker{client: newClient(srv.URL, "tok-worker-a")}
	msg, err := w.llmWithRetry(context.Background(), []chatMessage{{Role: "user", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "ok" || attempts != 2 || len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("llm retry response=%+v attempts=%d keys=%v", msg, attempts, keys)
	}
}

func TestRedeemBindCodeRecoversLostCommitResponse(t *testing.T) {
	candidate := strings.Repeat("a", 48)
	bindCalls := 0
	probeCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/bind":
			bindCalls++
			var body struct {
				Code        string `json:"code"`
				AccessToken string `json:"access_token"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body.Code != "wbc_once" || body.AccessToken != candidate {
				t.Errorf("bind payload = %+v", body)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close() // database commit happened, response was lost
		case "/api/me":
			probeCalls++
			if r.Header.Get("Authorization") != "Bearer "+candidate {
				t.Errorf("probe authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"id":7,"name":"NBAI","is_worker":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	res, err := newClient(srv.URL, "").RedeemBindCodeWithToken(context.Background(), "wbc_once", candidate)
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != candidate || res.WorkerID != 7 || res.WorkerName != "NBAI" || bindCalls != 1 || probeCalls != 1 {
		t.Fatalf("recovered bind = %+v bindCalls=%d probeCalls=%d", res, bindCalls, probeCalls)
	}
}

func TestRedeemBindCodeDoesNotRollbackOnAmbiguousProbe(t *testing.T) {
	candidate := strings.Repeat("c", 48)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/worker/bind":
			http.Error(w, "code already consumed", http.StatusNotFound)
		case "/api/me":
			http.Error(w, "temporary", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	_, err := newClient(srv.URL, "").RedeemBindCodeWithToken(context.Background(), "wbc_once", candidate)
	if err == nil {
		t.Fatal("ambiguous bind unexpectedly succeeded")
	}
	if definitiveClientRejection(err) {
		t.Fatalf("ambiguous bind was misclassified as definitive: %v", err)
	}
}

func TestPreBindConfigRollbackRestoresExistingWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	previous := Config{Server: "https://old.example", Token: "old-token", WorkerID: 9, WorkerName: "existing", Engine: "codex"}
	if err := saveConfig(path, previous); err != nil {
		t.Fatal(err)
	}
	if err := preservePreBindConfig(path, previous, true); err != nil {
		t.Fatal(err)
	}
	pending := Config{Server: "https://new.example", Token: strings.Repeat("a", 48), PendingBindHash: "pending", Engine: "claude"}
	if err := saveConfig(path, pending); err != nil {
		t.Fatal(err)
	}
	if err := restorePreBindConfig(path); err != nil {
		t.Fatal(err)
	}
	restored, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, previous) {
		t.Fatalf("restored config = %+v, want %+v", restored, previous)
	}
	if _, err := os.Stat(preBindConfigPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-bind snapshot still exists: %v", err)
	}
}

func TestPreBindConfigRollbackRemovesNewPendingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := preservePreBindConfig(path, Config{}, false); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(path, Config{Token: strings.Repeat("b", 48), PendingBindHash: "pending"}); err != nil {
		t.Fatal(err)
	}
	if err := restorePreBindConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending-only config still exists: %v", err)
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
		RunID                    int64  `json:"run_id"`
		TaskID                   int64  `json:"task_id"`
		ClaimID                  string `json:"claim_id"`
		Content                  string `json:"content"`
		FinalizationID           string `json:"finalization_id"`
		WorkerSessionID          int64  `json:"worker_session_id"`
		EngineSessionRef         string `json:"engine_session_ref"`
		EngineRuntimeFingerprint string `json:"engine_runtime_fingerprint"`
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
		SessionInfo{ID: 9, EngineSessionRef: "019f2c09-8ec0-7b91-a9bc-f7b95138ef3f", EngineRuntimeFingerprint: strings.Repeat("a", 64)}, "/tmp/repo")
	if err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.TaskID != 42 || body.ClaimID != "claim-1" || body.Content != "请提供 repo URL" || body.FinalizationID == "" {
		t.Fatalf("request body = %+v", body)
	}
	if body.WorkerSessionID != 9 || body.EngineSessionRef == "" || body.EngineRuntimeFingerprint != strings.Repeat("a", 64) {
		t.Fatalf("request input must persist worker session: %+v", body)
	}
}

func TestClientFailCarriesClaimAndSession(t *testing.T) {
	var body struct {
		RunID                    int64  `json:"run_id"`
		TaskID                   int64  `json:"task_id"`
		ClaimID                  string `json:"claim_id"`
		Error                    string `json:"error"`
		FinalizationID           string `json:"finalization_id"`
		WorkerSessionID          int64  `json:"worker_session_id"`
		EngineSessionRef         string `json:"engine_session_ref"`
		EngineRuntimeFingerprint string `json:"engine_runtime_fingerprint"`
		Workdir                  string `json:"workdir"`
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

	session := SessionInfo{ID: 9, EngineSessionRef: "019f2c09-8ec0-7b91-a9bc-f7b95138ef3f", EngineRuntimeFingerprint: strings.Repeat("a", 64)}
	if err := newClient(srv.URL, "tok-worker-a").Fail(context.Background(), 42, "claim-1", "agent crashed", session, "/tmp/repo"); err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.TaskID != 42 || body.ClaimID != "claim-1" || body.Error != "agent crashed" ||
		body.WorkerSessionID != 9 || body.EngineSessionRef == "" || body.EngineRuntimeFingerprint != strings.Repeat("a", 64) || body.Workdir != "/tmp/repo" || body.FinalizationID == "" {
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

func TestClientProgressRetryKeepsRequestID(t *testing.T) {
	var attempts int
	var ids, contents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var body struct {
			RequestID string `json:"request_id"`
			Content   string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, body.RequestID)
		contents = append(contents, body.Content)
		if attempts == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	if err := newClient(srv.URL, "tok-worker-a").Progress(context.Background(), 42, "claim-1", "halfway"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] || contents[0] != "halfway" || contents[1] != "halfway" {
		t.Fatalf("progress retry changed payload: attempts=%d ids=%v contents=%v", attempts, ids, contents)
	}
}

func TestClientArtifactRetryKeepsRequestAndBody(t *testing.T) {
	var attempts int
	var ids []string
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		if _, err := body.ReadFrom(file); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		ids = append(ids, r.URL.Query().Get("request_id"))
		bodies = append(bodies, body.Bytes())
		if attempts == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"file":{"id":1},"recorded":true}`))
	}))
	defer srv.Close()

	payload := []byte("stable artifact body")
	if err := newClient(srv.URL, "tok-worker-a").UploadArtifact(context.Background(), 42, "claim-1", "report.txt", bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(ids) != 2 || ids[0] == "" || ids[0] != ids[1] ||
		len(bodies) != 2 || !bytes.Equal(bodies[0], payload) || !bytes.Equal(bodies[1], payload) {
		t.Fatalf("artifact retry changed payload: attempts=%d ids=%v bodies=%q", attempts, ids, bodies)
	}
}

func TestClientSubmitRejectsMissingOutcome(t *testing.T) {
	err := newClient("http://127.0.0.1", "tok-worker-a").Submit(context.Background(), 42, "claim-1",
		"done", "", SessionInfo{}, "/tmp/repo", SubmissionResult{})
	if err == nil {
		t.Fatal("missing outcome should be rejected before the request")
	}
}

func TestClientSubmitCarriesStructuredResultAsJSON(t *testing.T) {
	var got struct {
		StructuredResult json.RawMessage `json:"structured_result"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	err := newClient(srv.URL, "tok-worker-a").Submit(context.Background(), 42, "claim-1",
		"done", "", SessionInfo{}, "/tmp/repo", SubmissionResult{
			Outcome: workerproto.OutcomeSucceeded, StructuredResult: json.RawMessage(` { "items": [1] } `),
		})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.StructuredResult) != `{"items":[1]}` {
		t.Fatalf("structured_result = %s", got.StructuredResult)
	}
}

func TestClientUpdateSessionCarriesActiveClaim(t *testing.T) {
	var body struct {
		RunID                    int64  `json:"run_id"`
		TaskID                   int64  `json:"task_id"`
		ClaimID                  string `json:"claim_id"`
		WorkerSessionID          int64  `json:"worker_session_id"`
		EngineSessionRef         string `json:"engine_session_ref"`
		EngineRuntimeFingerprint string `json:"engine_runtime_fingerprint"`
		Workdir                  string `json:"workdir"`
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

	session := SessionInfo{ID: 9, EngineSessionRef: "019f2c09-8ec0-7b91-a9bc-f7b95138ef3f", EngineRuntimeFingerprint: strings.Repeat("a", 64)}
	if err := newClient(srv.URL, "tok-worker-a").UpdateSession(context.Background(), 42, "claim-1", session, "/tmp/repo"); err != nil {
		t.Fatal(err)
	}
	if body.RunID != 42 || body.TaskID != 42 || body.ClaimID != "claim-1" || body.WorkerSessionID != 9 ||
		body.EngineSessionRef == "" || body.EngineRuntimeFingerprint != strings.Repeat("a", 64) || body.Workdir != "/tmp/repo" {
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

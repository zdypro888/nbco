package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseAgentAssessment(t *testing.T) {
	want := agentTurnAssessment{Status: "stalled", Signature: "same_error", Reason: "重复失败", Guidance: "换策略"}
	args, _ := json.Marshal(want)
	got, err := parseAgentAssessment(chatMessage{ToolCalls: []toolCall{{
		Function: toolCallFunc{Name: "report_worker_assessment", Arguments: string(args)},
	}}})
	if err != nil || got.Status != want.Status || got.Signature != want.Signature || got.Guidance != want.Guidance {
		t.Fatalf("tool assessment = %+v err=%v", got, err)
	}

	if _, err = parseAgentAssessment(chatMessage{Content: string(args)}); err == nil {
		t.Fatal("plain-text JSON bypassed the required assessment tool")
	}
}

func TestNormalizeAssessmentSignatureCanonicalizesFormatting(t *testing.T) {
	for _, value := range []string{"Missing Verification", "missing-verification", "missing_verification"} {
		if got := normalizeAssessmentSignature(value, ""); got != "missing_verification" {
			t.Fatalf("normalize %q = %q", value, got)
		}
	}
}

func TestWorkspaceFingerprintTracksWorkButIgnoresInputs(t *testing.T) {
	dir := t.TempDir()
	before, err := workspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	inputDir := filepath.Join(dir, taskAttachmentRelDir("attachment"))
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputDir, "input.txt"), []byte("immutable input"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterInput, err := workspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if afterInput != before {
		t.Fatal("downloaded task inputs must not count as agent progress")
	}
	nestedDependency := filepath.Join(dir, "frontend", "node_modules", "pkg")
	if err := os.MkdirAll(nestedDependency, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDependency, "index.js"), []byte("generated dependency"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterDependency, err := workspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if afterDependency != before {
		t.Fatal("nested dependency directories must not count as agent progress")
	}

	artifactDir := filepath.Join(dir, taskArtifactRelDir())
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "result.md"), []byte("verified result"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterArtifact, err := workspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if afterArtifact == afterInput {
		t.Fatal("new deliverable must count as durable progress")
	}
}

func TestWorkspaceChangeEvidenceIncludesOnlyTaskChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.go"), []byte("package old"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := workspaceSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "existing.go"), []byte("package changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.md"), []byte("new evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignored := filepath.Join(dir, "app", "node_modules", "pkg")
	if err := os.MkdirAll(ignored, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ignored, "index.js"), []byte("not evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := workspaceChangeEvidence(dir, baseline, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"existing.go", "package changed", "new.md", "new evidence"} {
		if !strings.Contains(got, want) {
			t.Fatalf("workspace evidence missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "node_modules") || strings.Contains(got, "not evidence") {
		t.Fatalf("workspace evidence included dependencies: %s", got)
	}
}

func TestSupervisorScreenEvidenceBoundsLongLines(t *testing.T) {
	got := supervisorScreenEvidence(strings.Repeat("x", agentSupervisorScreenLimit*2))
	if len(got) > agentSupervisorScreenLimit+len("[前序输出已截断]\n") {
		t.Fatalf("screen evidence too large: %d", len(got))
	}
}

func TestAgentSupervisorAuditsNarrativeOnlyTurns(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/llm" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"audit-1","type":"function","function":{"name":"report_worker_assessment","arguments":"{\"status\":\"stalled\",\"signature\":\" repeated strategy \",\"reason\":\"没有新证据\",\"guidance\":\"改用可验证路径\"}"}}]}}]}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	w := &Worker{client: newClient(server.URL, "worker-token")}
	s := newAgentSupervisor(w, &Run{Title: "test", Acceptance: "must verify"}, nil, nil, dir)
	for i := 0; i < agentProgressAuditInterval-1; i++ {
		got, err := s.assessTurn(context.Background(), "different narrative "+string(rune('a'+i)))
		if err != nil || got.Evaluated || !got.progressing() {
			t.Fatalf("pre-audit turn %d = %+v err=%v", i, got, err)
		}
	}
	got, err := s.assessTurn(context.Background(), "another narrative without durable state")
	if err != nil || !got.Evaluated || !got.stalled() || got.Signature != "repeated_strategy" {
		t.Fatalf("audited turn = %+v err=%v", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("LLM audit calls=%d want 1", calls.Load())
	}

	// Merely changing a file each turn does not suppress the next semantic
	// audit; otherwise an Agent could churn the same broken deliverable forever.
	for i := 0; i < agentProgressAuditInterval; i++ {
		if err := os.WriteFile(filepath.Join(dir, "draft.txt"), []byte(strings.Repeat("x", i+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err = s.assessTurn(context.Background(), "rewrote draft again")
		if err != nil {
			t.Fatal(err)
		}
	}
	if !got.Evaluated || !got.stalled() || calls.Load() != 2 {
		t.Fatalf("workspace churn must still be audited: got=%+v calls=%d", got, calls.Load())
	}
}

func TestAgentSupervisorSemanticRetryUsesNewRequestID(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"invalid assessment"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"audit-2","type":"function","function":{"name":"report_worker_assessment","arguments":"{\"status\":\"progressing\",\"signature\":\"new_evidence\",\"reason\":\"有新证据\",\"guidance\":\"\"}"}}]}}]}`))
	}))
	defer server.Close()

	s := &agentSupervisor{worker: &Worker{client: newClient(server.URL, "worker-token")}}
	got, err := s.assess(context.Background(), "progress", "evidence")
	if err != nil || !got.progressing() {
		t.Fatalf("assessment=%+v err=%v", got, err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[1] == "" || keys[0] == keys[1] {
		t.Fatalf("semantic retry keys=%v, want two different non-empty keys", keys)
	}
}

func TestAgentSupervisorReviewsCompletionEvidence(t *testing.T) {
	requestBody := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"review-1","type":"function","function":{"name":"report_worker_assessment","arguments":"{\"status\":\"pass\",\"signature\":\"acceptance_satisfied\",\"reason\":\"验收项与交付物一致\",\"guidance\":\"\"}"}}]}}]}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	artifactDir := filepath.Join(dir, taskArtifactRelDir())
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "result.md"), []byte("checked deliverable"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &Worker{client: newClient(server.URL, "worker-token")}
	s := newAgentSupervisor(w, &Run{Title: "review me", Acceptance: "contains checked deliverable"}, nil, nil, dir)
	got, err := s.reviewCompletion(context.Background(), "done", "verification passed")
	if err != nil || !got.passed() || !got.Evaluated {
		t.Fatalf("completion review = %+v err=%v", got, err)
	}
	body := <-requestBody
	for _, want := range []string{"review me", "contains checked deliverable", "result.md", "checked deliverable"} {
		if !strings.Contains(body, want) {
			t.Fatalf("completion review request missing %q: %s", want, body)
		}
	}
}

func TestArtifactEvidenceIncludesTextAndLabelsBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("acceptance evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "result.bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := artifactEvidence(dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"report.md", "acceptance evidence", "result.bin", "二进制文件"} {
		if !strings.Contains(got, want) {
			t.Fatalf("artifact evidence missing %q: %s", want, got)
		}
	}
}

func TestArtifactEvidenceHonorsTotalBudget(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		name := filepath.Join(dir, strings.Repeat("n", 40)+string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, []byte(strings.Repeat("content", 100)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const limit = 512
	got, err := artifactEvidence(dir, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > limit {
		t.Fatalf("artifact evidence exceeded budget: %d > %d", len(got), limit)
	}
}

func TestArtifactEvidenceRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "result.pipe")
	if err := makeFIFO(fifo, 0o644); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := artifactEvidence(dir, 4096)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO deliverable must be rejected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("artifact evidence blocked while opening a FIFO")
	}
}

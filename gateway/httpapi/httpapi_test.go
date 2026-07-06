package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeJSONLimitsBody(t *testing.T) {
	body := `{"message":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	var dst struct {
		Message string `json:"message"`
	}
	if err := decodeJSON(rec, req, &dst); err == nil {
		t.Fatal("oversized JSON body should fail")
	}
}

func TestWorkerDownloadBinary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "worker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const name = "nbco-worker-linux-amd64"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{downloadPath: root}
	req := httptest.NewRequest(http.MethodGet, "/downloads/worker/"+name, nil)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	s.handleWorkerDownloadBinary(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "binary" {
		t.Fatalf("download = %d %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerDownloadRejectsUnknownName(t *testing.T) {
	s := &Server{downloadPath: t.TempDir()}
	req := httptest.NewRequest(http.MethodGet, "/downloads/worker/../../x", nil)
	req.SetPathValue("name", "../../x")
	rec := httptest.NewRecorder()
	s.handleWorkerDownloadBinary(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown download status = %d", rec.Code)
	}
}

func TestLLMSemaphoreLazyInit(t *testing.T) {
	s := &Server{}
	sem := s.llmSemaphore()
	if sem == nil {
		t.Fatal("llm semaphore should be initialized lazily")
	}
	if sem != s.llmSemaphore() {
		t.Fatal("llm semaphore should be reused")
	}
	select {
	case sem <- struct{}{}:
		<-sem
	default:
		t.Fatal("fresh llm semaphore should have capacity")
	}
}

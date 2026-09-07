package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type artifactAuditFixture struct {
	workdir string
	dir     string
	blocked string
	files   map[string]string
}

func newArtifactAuditFixture(t *testing.T) artifactAuditFixture {
	t.Helper()
	f := artifactAuditFixture{workdir: t.TempDir(), files: map[string]string{
		"a-before.txt": "artifact before unreadable directory",
		"z-after.txt":  "artifact after unreadable directory",
	}}
	f.dir = filepath.Join(f.workdir, taskArtifactRelDir())
	f.blocked = filepath.Join(f.dir, "m-blocked")
	if err := os.MkdirAll(f.blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range f.files {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.blocked, "unreadable.txt"), []byte("not uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Restore permissions before TempDir cleanup, including when the test skips.
	t.Cleanup(func() {
		if err := os.Chmod(f.blocked, 0o700); err != nil {
			t.Errorf("restore artifact directory permissions: %v", err)
		}
	})
	if err := os.Chmod(f.blocked, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(f.blocked); err == nil {
		t.Skip("environment can read a mode-000 directory; permission regression requires an unprivileged account")
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("permission fixture returned an unexpected error: %v", err)
	}
	return f
}

type artifactAuditUpload struct {
	name    string
	content string
}

func newArtifactAuditWorker(t *testing.T) (*Worker, <-chan artifactAuditUpload, <-chan string) {
	t.Helper()
	uploads := make(chan artifactAuditUpload, 16)
	progress := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/worker/artifacts":
			if r.URL.Query().Get("run_id") != "42" || r.URL.Query().Get("claim_id") != "claim-audit" {
				t.Errorf("unexpected artifact lease: %s", r.URL.RawQuery)
				http.Error(w, "unexpected lease", http.StatusConflict)
				return
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse artifact: %v", err)
				http.Error(w, "invalid multipart", http.StatusBadRequest)
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Errorf("read artifact part: %v", err)
				http.Error(w, "missing file", http.StatusBadRequest)
				return
			}
			defer file.Close()
			body, err := io.ReadAll(file)
			if err != nil {
				t.Errorf("read artifact body: %v", err)
				http.Error(w, "invalid file", http.StatusBadRequest)
				return
			}
			uploads <- artifactAuditUpload{name: header.Filename, content: string(body)}
		case "/api/worker/progress":
			var body struct {
				Content string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode progress: %v", err)
				http.Error(w, "invalid progress", http.StatusBadRequest)
				return
			}
			progress <- body.Content
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"ok":"1"}`)
	}))
	t.Cleanup(srv.Close)
	return &Worker{client: newClient(srv.URL, "audit-token")}, uploads, progress
}

func assertArtifactAuditUploads(t *testing.T, uploads <-chan artifactAuditUpload, want map[string]string) {
	t.Helper()
	got := make(map[string]string)
	for {
		select {
		case upload := <-uploads:
			if _, exists := got[upload.name]; exists {
				t.Errorf("artifact uploaded more than once: %s", upload.name)
			}
			got[upload.name] = upload.content
		default:
			if !reflect.DeepEqual(got, want) {
				t.Errorf("uploaded files = %v, want %v", got, want)
			}
			return
		}
	}
}

func TestArtifactAuditEntriesContinuePastUnreadableDirectory(t *testing.T) {
	f := newArtifactAuditFixture(t)
	entries, err := artifactEntries(f.dir)
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), f.blocked) {
		t.Fatalf("walk error = %v, want permission error naming %s", err, f.blocked)
	}
	want := []string{filepath.Join(f.dir, "a-before.txt"), filepath.Join(f.dir, "z-after.txt")}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %v, want files on both sides of unreadable directory: %v", entries, want)
	}
}

func TestArtifactAuditUploadPreservesPartialEntriesAndError(t *testing.T) {
	f := newArtifactAuditFixture(t)
	w, requests, _ := newArtifactAuditWorker(t)
	uploaded, failed, rejected, err := w.uploadArtifacts(context.Background(), 42, "claim-audit", f.dir)
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), f.blocked) {
		t.Fatalf("upload error = %v, want original directory permission error", err)
	}
	if !reflect.DeepEqual(uploaded, []string{"a-before.txt", "z-after.txt"}) || len(failed) != 0 || len(rejected) != 0 {
		t.Errorf("upload results: uploaded=%v failed=%v rejected=%v", uploaded, failed, rejected)
	}
	assertArtifactAuditUploads(t, requests, f.files)
}

func TestArtifactAuditSummaryReportsIncompleteDelivery(t *testing.T) {
	f := newArtifactAuditFixture(t)
	w, requests, progress := newArtifactAuditWorker(t)
	const original = "Command finished successfully."
	const incomplete = "\u4ea4\u4ed8\u4e0d\u5b8c\u6574"
	summary := w.appendArtifactReport(context.Background(), &Run{ID: 42, ClaimID: "claim-audit"}, f.workdir, original)
	if !strings.HasPrefix(summary, original) {
		t.Errorf("original summary lost: %q", summary)
	}
	for _, want := range []string{incomplete, f.blocked, "a-before.txt", "z-after.txt"} {
		if !strings.Contains(summary, want) {
			t.Errorf("final summary missing %q: %q", want, summary)
		}
	}
	assertArtifactAuditUploads(t, requests, f.files)
	select {
	case warning := <-progress:
		if !strings.Contains(warning, incomplete) || !strings.Contains(warning, f.blocked) {
			t.Errorf("progress missing incomplete-delivery warning: %q", warning)
		}
	default:
		t.Error("missing incomplete-delivery progress report")
	}
}

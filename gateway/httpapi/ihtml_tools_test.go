package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ihtml "github.com/zdypro888/ihtml"

	"github.com/zdypro888/nbco/ai"
)

func TestPinnedIHTMLRuntimeExposesCallableHTTPClient(t *testing.T) {
	handler, err := ihtml.NewHandler(ihtml.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ihtml.js", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ihtml runtime status = %d", recorder.Code)
	}
	source := recorder.Body.String()
	for _, expected := range []string{"function createHTTPClient", "const client = (path, options) => request(path, options)", "lib.runtime.createHTTPClient"} {
		if !strings.Contains(source, expected) {
			t.Fatalf("pinned ihtml runtime is missing %q", expected)
		}
	}
}

func TestIHTMLAgentToolsUseScopedService(t *testing.T) {
	handler, err := ihtml.NewHandler(ihtml.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	svc, err := ihtml.ScopeService(handler, "user-17")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ai.Tool{}
	for _, tool := range ihtmlAgentTools(svc, ihtml.APISpec{Name: "members", Method: "GET", Path: "/api/users"}) {
		byName[tool.Name] = tool
		if tool.Domain != "ui" || !tool.GroupSensitive {
			t.Fatalf("tool %s escaped UI governance: %+v", tool.Name, tool)
		}
	}
	apply := byName["ui_apply_items"]
	if apply.Handler == nil || apply.ApprovalRequired || apply.Effect != ai.ToolEffectWrite {
		t.Fatalf("UI code write metadata = %+v", apply)
	}
	apis, err := byName["ui_list_host_apis"].Handler(context.Background(), nil)
	if err != nil || !strings.Contains(apis, `"/api/users"`) {
		t.Fatalf("host API catalog = %q, %v", apis, err)
	}
	input := json.RawMessage(`{"items":[{"id":"status-card","type":"html","title":"状态","order":10,"content":"<main>ready</main>"}],"note":"create status card"}`)
	if out, err := apply.Handler(context.Background(), input); err != nil || !strings.Contains(out, `"updated_ids"`) {
		t.Fatalf("apply = %q, %v", out, err)
	}

	list := byName["ui_list_state"]
	out, err := list.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"status-card"`) || !strings.Contains(out, `"content_bytes"`) || strings.Contains(out, "<main>ready</main>") {
		t.Fatalf("metadata listing leaked or omitted content: %s", out)
	}
	get := byName["ui_get_item"]
	out, err = get.Handler(context.Background(), json.RawMessage(`{"id":"status-card","max_chars":7}`))
	var item struct {
		Content    string `json:"content"`
		NextOffset int    `json:"next_offset"`
		Truncated  bool   `json:"truncated"`
	}
	if decodeErr := json.Unmarshal([]byte(out), &item); err != nil || decodeErr != nil || item.Content != "<main>r" || item.NextOffset != 7 || !item.Truncated {
		t.Fatalf("get item = %s, %v", out, err)
	}

	other, err := ihtml.ScopeService(handler, "user-18")
	if err != nil {
		t.Fatal(err)
	}
	items, err := other.ListItems(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("cross-user workspace leak: items=%+v err=%v", items, err)
	}
}

func TestIHTMLAgentToolMutationsCreateRevisions(t *testing.T) {
	handler, err := ihtml.NewHandler(ihtml.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	svc, err := ihtml.ScopeService(handler, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ai.Tool{}
	for _, tool := range ihtmlAgentTools(svc) {
		byName[tool.Name] = tool
	}
	_, err = byName["ui_apply_items"].Handler(context.Background(), json.RawMessage(`{"items":[{"id":"a","type":"css","content":".a{}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := byName["ui_list_revisions"].Handler(context.Background(), nil)
	if err != nil || !strings.Contains(out, `"item_count"`) {
		t.Fatalf("revision list = %s, %v", out, err)
	}
}

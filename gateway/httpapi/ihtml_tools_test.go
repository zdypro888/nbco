package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ihtml "github.com/zdypro888/ihtml"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/interaction"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

type failingInspectPublishingService struct {
	ihtml.ScopedService
	publisher ihtml.ScopedPagePublicationService
}

func (s *failingInspectPublishingService) PublishPage(ctx context.Context, page ihtml.Page, items []ihtml.Item, by, note string) error {
	return s.publisher.PublishPage(ctx, page, items, by, note)
}

func (*failingInspectPublishingService) ListPages(context.Context) ([]ihtml.Page, error) {
	return nil, errors.New("temporary inspection failure")
}

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
	for _, tool := range ihtmlAgentTools(svc, ihtmlAgentToolOptions{
		APIs:          []ihtml.APISpec{{Name: "members", Method: "GET", Path: "/api/users"}},
		PublicBaseURL: "https://nbco.example",
	}) {
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

	putData := byName["ui_put_data"]
	out, err = putData.Handler(context.Background(), json.RawMessage(`{"key":"reports.weekly","value_json":"[{\"user_id\":3,\"work\":\"done\"},{\"user_id\":4,\"work\":\"review\"}]"}`))
	if err != nil || !strings.Contains(out, `"entries":2`) || !strings.Contains(out, `"revision":"`) || !strings.Contains(out, `"verified":true`) {
		t.Fatalf("put data = %s, %v", out, err)
	}
	getData := byName["ui_get_data"]
	out, err = getData.Handler(context.Background(), json.RawMessage(`{"key":"reports.weekly"}`))
	if err != nil || !strings.Contains(out, `"user_id":3`) || !strings.Contains(out, `"kind":"array"`) {
		t.Fatalf("get data = %s, %v", out, err)
	}

	publish := byName["ui_publish_page"]
	out, err = publish.Handler(context.Background(), json.RawMessage(`{"page":{"name":"weekly-report","title":"周报","order":20},"items":[{"id":"weekly-body","type":"html","page":"weekly-report","content":"<main>weekly</main>"}],"note":"publish weekly report"}`))
	if err != nil || !strings.Contains(out, `"item_count":1`) || !strings.Contains(out, `"verified":true`) || !strings.Contains(out, `https://nbco.example/?view=workspace&workspace_page=weekly-report`) {
		t.Fatalf("publish page = %s, %v", out, err)
	}
	inspect := byName["ui_inspect_page"]
	out, err = inspect.Handler(context.Background(), json.RawMessage(`{"name":"weekly-report"}`))
	if err != nil || !strings.Contains(out, `"registered":true`) || !strings.Contains(out, `"weekly-body"`) {
		t.Fatalf("inspect page = %s, %v", out, err)
	}
	actions := publish.PresentResult(out)
	if len(actions) != 1 || actions[0].Kind != interaction.ActionOpenWebApp ||
		actions[0].Label != "打开周报" || actions[0].URL != "https://nbco.example/?view=workspace&workspace_page=weekly-report" {
		t.Fatalf("page actions = %+v", actions)
	}
	if err := svc.ReportErrors(context.Background(), []ihtml.PageError{{
		ItemID: "weekly-body", Message: "old failure", Time: time.Now().Add(-time.Hour),
	}}); err != nil {
		t.Fatal(err)
	}
	out, err = publish.Handler(context.Background(), json.RawMessage(`{"page":{"name":"weekly-report","title":"周报","order":20},"items":[{"id":"weekly-body","type":"html","page":"weekly-report","content":"<main>updated weekly</main>"}],"note":"fix weekly report"}`))
	if err != nil || strings.Contains(out, "old failure") {
		t.Fatalf("publication retained a stale runtime error = %s, %v", out, err)
	}
	if err := svc.ReportErrors(context.Background(), []ihtml.PageError{{
		ItemID: "weekly-body", Message: "current failure", Time: time.Now().Add(time.Second),
	}}); err != nil {
		t.Fatal(err)
	}
	out, err = inspect.Handler(context.Background(), json.RawMessage(`{"name":"weekly-report"}`))
	if err != nil || !strings.Contains(out, "current failure") || strings.Contains(out, "old failure") {
		t.Fatalf("inspect current runtime errors = %s, %v", out, err)
	}
	out, err = list.Handler(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, `"workspace_url":"https://nbco.example/?view=workspace&workspace_page=weekly-report"`) {
		t.Fatalf("state page URLs = %s, %v", out, err)
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

func TestIHTMLPagePublicationDoesNotReportCommittedWriteAsFailedWhenInspectionFails(t *testing.T) {
	handler, err := ihtml.NewHandler(ihtml.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	base, err := ihtml.ScopeService(handler, "user-22")
	if err != nil {
		t.Fatal(err)
	}
	publisher, ok := base.(ihtml.ScopedPagePublicationService)
	if !ok {
		t.Fatal("publication capability is missing")
	}
	flaky := &failingInspectPublishingService{ScopedService: base, publisher: publisher}
	var publish ai.Tool
	for _, tool := range ihtmlAgentTools(flaky, ihtmlAgentToolOptions{PublicBaseURL: "https://nbco.example"}) {
		if tool.Name == "ui_publish_page" {
			publish = tool
			break
		}
	}
	out, err := publish.Handler(context.Background(), json.RawMessage(`{"page":{"name":"ops","title":"运营"},"items":[{"id":"ops-body","type":"html","page":"ops","content":"<main>ok</main>"}]}`))
	if err != nil || !strings.Contains(out, `"committed":true`) || !strings.Contains(out, `"verified":false`) || !strings.Contains(out, "temporary inspection failure") {
		t.Fatalf("publication result = %s, %v", out, err)
	}
	pages, err := base.ListPages(context.Background())
	if err != nil || len(pages) != 1 || pages[0].Name != "ops" {
		t.Fatalf("committed page = %+v, %v", pages, err)
	}
}

func TestIHTMLPagePublicationHonorsDesignReviewBeforeCommit(t *testing.T) {
	handler, err := ihtml.NewHandler(ihtml.NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	svc, err := ihtml.ScopeService(handler, "review-user")
	if err != nil {
		t.Fatal(err)
	}

	review := ihtmlPageReviewer(func(context.Context, ihtml.Page, []ihtml.Item) ihtmlDesignReview {
		return ihtmlDesignReview{Verdict: ihtmlReviewRevise, Summary: "mobile overflow", Issues: []ihtmlReviewIssue{{
			Severity: "major", Category: "responsive", Evidence: "fixed-width table", Fix: "provide a mobile representation",
		}}}
	})
	var publish ai.Tool
	for _, tool := range ihtmlAgentTools(svc, ihtmlAgentToolOptions{ReviewPage: review}) {
		if tool.Name == "ui_publish_page" {
			publish = tool
			break
		}
	}
	raw := json.RawMessage(`{"page":{"name":"overview","title":"Overview"},"items":[{"id":"overview-html","type":"html","page":"overview","content":"<main>overview</main>"}]}`)
	out, err := publish.Handler(context.Background(), raw)
	if err != nil || !strings.Contains(out, `"committed":false`) || !strings.Contains(out, `"verdict":"revise"`) {
		t.Fatalf("rejected publication = %s, %v", out, err)
	}
	if actions := publish.PresentResult(out); len(actions) != 0 {
		t.Fatalf("rejected publication exposed actions: %+v", actions)
	}
	pages, err := svc.ListPages(context.Background())
	if err != nil || len(pages) != 0 {
		t.Fatalf("rejected publication mutated pages: %+v, %v", pages, err)
	}

	review = func(context.Context, ihtml.Page, []ihtml.Item) ihtmlDesignReview {
		return ihtmlDesignReview{Verdict: ihtmlReviewPass, Summary: "ready", Issues: []ihtmlReviewIssue{}}
	}
	for _, tool := range ihtmlAgentTools(svc, ihtmlAgentToolOptions{ReviewPage: review, PublicBaseURL: "https://nbco.example"}) {
		if tool.Name == "ui_publish_page" {
			publish = tool
			break
		}
	}
	out, err = publish.Handler(context.Background(), raw)
	if err != nil || !strings.Contains(out, `"committed":true`) || !strings.Contains(out, `"verdict":"pass"`) {
		t.Fatalf("accepted publication = %s, %v", out, err)
	}
	pages, err = svc.ListPages(context.Background())
	if err != nil || len(pages) != 1 || pages[0].Name != "overview" {
		t.Fatalf("accepted publication pages: %+v, %v", pages, err)
	}
}

func TestParseIHTMLDesignReviewNormalizesModelOutput(t *testing.T) {
	review, ok := parseIHTMLDesignReview(`{"verdict":"pass","summary":"looks good","issues":[{"severity":"major","category":"overflow","evidence":"min-width","fix":"responsive layout"},{"severity":"unknown","category":"x","evidence":"x","fix":"x"}]}`)
	if !ok || review.Verdict != ihtmlReviewRevise || len(review.Issues) != 1 {
		t.Fatalf("review = %+v, ok=%v", review, ok)
	}
	if _, ok := parseIHTMLDesignReview(`{"verdict":"revise","summary":"bad","issues":[]}`); ok {
		t.Fatal("revise without actionable issues must be rejected")
	}
}

func TestIHTMLPageReviewerUsesBoundedToolFreeSubcall(t *testing.T) {
	u := &store.User{ID: 17, Name: "designer", Status: store.UserActive}
	called := false
	reviewer := newIHTMLPageReviewer(func(_ context.Context, got *store.User, req tools.SubcallRequest) (string, error) {
		called = true
		if got != u || req.Purpose != "ihtml_design_review" || !req.JSONOutput || req.Reasoning != ai.ReasoningDisabled ||
			!strings.Contains(req.Prompt, "ui-table-wrap") || !strings.Contains(req.Prompt, `"name":"overview"`) {
			t.Fatalf("review subcall = user:%+v request:%+v", got, req)
		}
		return `{"verdict":"pass","summary":"responsive and consistent","issues":[]}`, nil
	}, u)
	review := reviewer(context.Background(), ihtml.Page{Name: "overview", Title: "Overview"}, []ihtml.Item{{
		ID: "overview-html", Type: ihtml.ItemHTML, Page: "overview", Content: "<main class=\"ui-page\"></main>",
	}})
	if !called || review.Verdict != ihtmlReviewPass {
		t.Fatalf("review = %+v, called=%v", review, called)
	}
}

func TestIHTMLWorkspaceURLTargetsExactPage(t *testing.T) {
	if got := ihtmlWorkspaceURL("https://nbco.example/", "weekly-report"); got != "https://nbco.example/?view=workspace&workspace_page=weekly-report" {
		t.Fatalf("workspace URL = %q", got)
	}
	if got := ihtmlWorkspaceURL("", "invalid page"); got != "/?view=workspace" {
		t.Fatalf("invalid page URL = %q", got)
	}
}

func TestIHTMLPageActionsRejectUnregisteredAndInsecureResults(t *testing.T) {
	for _, result := range []string{
		`{"registered":false,"workspace_url":"https://nbco.example/?view=workspace"}`,
		`{"registered":true,"workspace_url":"http://nbco.example/?view=workspace"}`,
		`not-json`,
	} {
		if actions := ihtmlPageActions(result); len(actions) != 0 {
			t.Fatalf("result %q produced actions %+v", result, actions)
		}
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
	for _, tool := range ihtmlAgentTools(svc, ihtmlAgentToolOptions{}) {
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

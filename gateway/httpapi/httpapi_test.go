package httpapi

import (
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

	"github.com/zdypro888/nbco/config"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
	"github.com/zdypro888/nbco/workerproto"
)

func TestValidEngineRuntimeFingerprint(t *testing.T) {
	valid := strings.Repeat("a1", 32)
	for _, value := range []string{"", valid, strings.ToUpper(valid), "  " + valid + "  "} {
		if !validEngineRuntimeFingerprint(value) {
			t.Fatalf("fingerprint should be valid: %q", value)
		}
	}
	for _, value := range []string{"abc", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("-", 64)} {
		if validEngineRuntimeFingerprint(value) {
			t.Fatalf("fingerprint should be invalid: %q", value)
		}
	}
}

func TestResolveWorkerSubmissionOutcome(t *testing.T) {
	zero, seven := 0, 7
	tests := []struct {
		name       string
		value      string
		exitCode   *int
		legacyCode *int
		want       workerproto.Outcome
		wantCode   *int
		ok         bool
	}{
		{name: "explicit success", value: "succeeded", exitCode: &zero, want: workerproto.OutcomeSucceeded, wantCode: &zero, ok: true},
		{name: "explicit failure without process", value: "failed", want: workerproto.OutcomeFailed, ok: true},
		{name: "legacy agent submission", want: workerproto.OutcomeSucceeded, ok: true},
		{name: "legacy non-zero command", legacyCode: &seven, want: workerproto.OutcomeFailed, wantCode: &seven, ok: true},
		{name: "invalid", value: "command_succeeded", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, code, ok := resolveWorkerSubmissionOutcome(tt.value, tt.exitCode, tt.legacyCode)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("outcome = %q, %v; want %q, %v", got, ok, tt.want, tt.ok)
			}
			if (code == nil) != (tt.wantCode == nil) || code != nil && *code != *tt.wantCode {
				t.Fatalf("exit code = %v, want %v", code, tt.wantCode)
			}
		})
	}
}

func TestIngestWorkerLessonCandidateIsGovernedAndReplaySafe(t *testing.T) {
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 NBCO_TEST_PG_DSN，跳过 PostgreSQL 集成测试")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, 7767002); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, 7767002) }()
	if _, err := s.Pool().Exec(ctx, `TRUNCATE users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	worker, err := s.CreateUser(ctx, "lesson-worker", false, store.Identity{Provider: "test", ExternalID: "lesson-worker"})
	if err != nil {
		t.Fatal(err)
	}
	taskID := int64(21)
	run := &store.WorkerRun{
		ID: 42, TaskID: &taskID, Executor: workerproto.ExecutorAgent,
		Title: "资料整理经验", ScopeKey: "materials:company", WorkerID: worker.ID,
	}
	task := &store.Task{ID: taskID, ProjectID: 9}
	server := &Server{store: s}
	for range 2 {
		if err := server.ingestWorkerLessonCandidate(ctx, worker, run, task, "先核对来源，再保存结论"); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ListLearningCandidates(ctx, store.LearningStatusPending, store.LearningKindKnowledge, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("replayed lesson produced %d candidates: %+v", len(items), items)
	}
	got := items[0]
	if got.SourceType != "worker_lesson" || got.SourceRef != "42" || got.MemoryClass != store.LearningMemoryDurable ||
		!strings.Contains(strings.Join(got.Tags, ","), "worker:") || !strings.Contains(strings.Join(got.Tags, ","), "project:9") {
		t.Fatalf("lesson candidate = %+v", got)
	}
	formal, err := s.RecentKnowledge(ctx, 10)
	if err != nil || len(formal) != 0 {
		t.Fatalf("unreviewed lesson must not publish directly: %+v err=%v", formal, err)
	}
}

func TestWorkerRunFinishedEventPreservesExecutionEvidenceBoundary(t *testing.T) {
	zero := 0
	run := &store.WorkerRun{
		ID: 7, Executor: workerproto.ExecutorCommand, Title: "查询外部比赛结果", Goal: "返回比赛双方和比分",
		Status: store.WorkerRunCompleted, Outcome: string(workerproto.OutcomeSucceeded), ExitCode: &zero,
		Summary: `输出：{"status":401,"error":"API key missing"}`,
	}
	detail := workerRunFinishedEventDetail("NBAI", run)
	for _, want := range []string{"process_execution", "只证明命令进程", "必须从输出或产物", "401", "返回比赛双方和比分"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("event detail missing %q: %s", want, detail)
		}
	}
	if strings.Contains(detail, "完成了「") {
		t.Fatalf("process completion must not be phrased as objective completion: %s", detail)
	}
	if got := toWorkerRunJSON(run).EvidenceScope; got != string(workerproto.EvidenceProcessExecution) {
		t.Fatalf("worker run JSON evidence_scope = %q", got)
	}
}

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

func TestDecodeJSONRejectsTrailingValues(t *testing.T) {
	for name, body := range map[string]string{
		"second object": `{"value":1} {"value":2}`,
		"second scalar": `{"value":1} true`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			var dst struct {
				Value int `json:"value"`
			}
			if err := decodeJSON(httptest.NewRecorder(), req, &dst); err == nil {
				t.Fatal("decodeJSON accepted more than one JSON value")
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{\"value\":1}\n\t"))
	var dst struct {
		Value int `json:"value"`
	}
	if err := decodeJSON(httptest.NewRecorder(), req, &dst); err != nil || dst.Value != 1 {
		t.Fatalf("valid single JSON value rejected: value=%d err=%v", dst.Value, err)
	}
}

func TestRequestIdempotencyKeyAndActionScope(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/tools/save_rule", nil)
	if _, err := requestIdempotencyKey(req, true); !errors.Is(err, errIdempotencyKeyRequired) {
		t.Fatalf("missing required key error = %v", err)
	}
	req.Header.Set("Idempotency-Key", strings.Repeat("x", maxIdempotencyKeyBytes+1))
	if _, err := requestIdempotencyKey(req, true); !errors.Is(err, errIdempotencyKeyInvalid) {
		t.Fatalf("oversized key error = %v", err)
	}
	req.Header.Set("Idempotency-Key", " request-1 ")
	key, err := requestIdempotencyKey(req, true)
	if err != nil || key != "request-1" {
		t.Fatalf("normalized key = %q, %v", key, err)
	}
	firstActionKey := httpActionKey(7, "tool:save_rule", key)
	secondActionKey := httpActionKey(7, "tool:save_rule", key)
	if firstActionKey != secondActionKey {
		t.Fatal("same logical request must have a stable action key")
	}
	if httpActionKey(7, "tool:save_rule", key) == httpActionKey(8, "tool:save_rule", key) {
		t.Fatal("request identities must be isolated by authenticated user")
	}
	if httpActionKey(7, "tool:save_rule", key) == httpActionKey(7, "tool:update_rule", key) {
		t.Fatal("request identities must be isolated by operation kind")
	}
	if payloadHash([]byte(`{"a":1,"b":2}`)) != payloadHash([]byte(`{ "b": 2, "a": 1 }`)) {
		t.Fatal("semantic JSON retries must share one payload identity")
	}
}

func TestHTTPActionReplayReturnsStoredResult(t *testing.T) {
	until := time.Now().UTC().Add(time.Hour)
	rec := httptest.NewRecorder()
	writeHTTPActionClaimError(rec, nil, &store.ExternalActionReceipt{
		Status: store.ExternalActionCompleted, ResultText: `{"result":"one-time-token"}`, ResultUntil: &until,
	})
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"result":"one-time-token"}` {
		t.Fatalf("stored action replay code=%d body=%q", rec.Code, rec.Body.String())
	}

	expired := time.Now().UTC().Add(-time.Hour)
	rec = httptest.NewRecorder()
	writeHTTPActionClaimError(rec, nil, &store.ExternalActionReceipt{
		Status: store.ExternalActionCompleted, ResultText: `{"result":"expired"}`, ResultUntil: &expired,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expired action result code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestHandleVersion(t *testing.T) {
	prev := Version
	Version = "test-rev"
	t.Cleanup(func() { Version = prev })
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	s.handleVersion(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"test-rev"`) || !strings.Contains(rec.Body.String(), `"go":"`) {
		t.Fatalf("version response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerSetsSecurityHeaders(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self' https://telegram.org") || !strings.Contains(got, "object-src 'none'") {
		t.Fatalf("Content-Security-Policy 不完整: %q", got)
	}
}

func TestTelegramWebhookRouteIsMethodScoped(t *testing.T) {
	s := &Server{}
	if err := s.SetTelegramWebhook("/", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
		t.Fatal("root path must not be accepted as Telegram webhook")
	}
	for _, invalid := range []string{"/api/chat", "/api/telegram/", "/api/telegram/../chat", "/api/telegram/webhook/"} {
		if err := s.SetTelegramWebhook(invalid, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
			t.Fatalf("unsafe webhook path %q was accepted", invalid)
		}
	}
	called := 0
	if err := s.SetTelegramWebhook("/api/telegram/webhook", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	handler := s.Handler()
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/telegram/webhook", nil))
	if post.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("POST webhook status=%d called=%d", post.Code, called)
	}
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/telegram/webhook", nil))
	if get.Code != http.StatusMethodNotAllowed || called != 1 {
		t.Fatalf("GET webhook status=%d called=%d", get.Code, called)
	}
}

func TestControlCenterUsesPerResponseCSPNonce(t *testing.T) {
	s := &Server{}
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	first, second := request(), request()
	for _, rec := range []*httptest.ResponseRecorder{first, second} {
		body := rec.Body.String()
		marker := `data-csp-nonce="`
		start := strings.Index(body, marker)
		if start < 0 {
			t.Fatalf("control center nonce marker missing: %s", body)
		}
		start += len(marker)
		end := strings.Index(body[start:], `"`)
		if end <= 0 {
			t.Fatalf("control center nonce value missing: %s", body)
		}
		nonce := body[start : start+end]
		policy := rec.Header().Get("Content-Security-Policy")
		parts := strings.Split(policy, ";")
		if len(parts) < 2 || !strings.Contains(parts[1], "'nonce-"+nonce+"'") || strings.Contains(parts[1], "'unsafe-inline'") {
			t.Fatalf("script CSP does not bind the page nonce: %q", policy)
		}
		if strings.Contains(body, "{{CSP_NONCE}}") || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("nonce response was not finalized safely: cache=%q", rec.Header().Get("Cache-Control"))
		}
	}
	if first.Body.String() == second.Body.String() {
		t.Fatal("CSP nonce must rotate for every control-center response")
	}
}

func TestControlCenterUsesEscapedInstanceBrand(t *testing.T) {
	s := &Server{deps: tools.Deps{BrandName: `Oncoin <Ops> "HQ"`}}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`<title>Oncoin &lt;Ops&gt; &#34;HQ&#34; · AI 运营控制中心</title>`,
		`data-brand-name="Oncoin &lt;Ops&gt; &#34;HQ&#34;"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("control center missing escaped brand %q: %s", want, body)
		}
	}
	if strings.Contains(body, "{{BRAND_NAME}}") || strings.Contains(body, `<Ops>`) {
		t.Fatalf("control center exposed an unescaped or unresolved brand: %s", body)
	}
}

func TestIHTMLStandaloneRootRedirectsIntoControlCenter(t *testing.T) {
	s := &Server{ihtmlHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ihtml asset"))
	})}
	handler := s.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/?view=workspace" {
		t.Fatalf("workspace root = status %d location %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/ihtml.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ihtml asset" {
		t.Fatalf("ihtml subtree = status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestControlCenterKeepsIHTMLPageInShareableURL(t *testing.T) {
	source, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, expected := range []string{
		`initialURLParams.get("workspace_page")`,
		`page: state.workspacePage`,
		`onChange: navState =>`,
		`next.searchParams.set("workspace_page", state.workspacePage)`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("workspace deep-link contract is missing %q", expected)
		}
	}
}

func TestControlCenterAdaptsToBrowserAndTelegramViewports(t *testing.T) {
	javascript, err := webAssets.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`window.visualViewport?.height`,
		`tg?.viewportHeight`,
		`tg?.viewportStableHeight`,
		`"viewportChanged", "safeAreaChanged", "contentSafeAreaChanged"`,
		`--app-viewport-height`,
		`--app-safe-${side}`,
	} {
		if !strings.Contains(string(javascript), expected) {
			t.Fatalf("adaptive viewport runtime is missing %q", expected)
		}
	}

	stylesheet, err := webAssets.ReadFile("web/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`--app-viewport-height: 100vh`,
		`--app-viewport-height: 100dvh`,
		`--app-content-height: calc(`,
		`padding: var(--app-safe-top) var(--app-safe-right) var(--app-safe-bottom) var(--app-safe-left)`,
		`min-height: var(--app-content-height)`,
	} {
		if !strings.Contains(string(stylesheet), expected) {
			t.Fatalf("adaptive viewport stylesheet is missing %q", expected)
		}
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

func TestBoundedWorkerLinesCapsEachLineAndTotal(t *testing.T) {
	got := boundedWorkerLines([]string{"abcdef", "ghijkl", "mnop"}, 4, 7)
	if len(got) != 2 || got[0] != "abc…" || got[1] != "gh…" {
		t.Fatalf("bounded lines = %#v", got)
	}
	total := 0
	for _, line := range got {
		total += len([]rune(line))
	}
	if total > 7 {
		t.Fatalf("bounded lines exceeded total: %d", total)
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

func TestApplyWorkerLLMBudget(t *testing.T) {
	s := &Server{llm: LLMConfig{MaxTokens: 4096}}
	body := map[string]any{}
	s.applyWorkerLLMBudget(body)
	if body["max_tokens"] != 4096 {
		t.Fatalf("max_tokens not applied: %#v", body)
	}

	s = &Server{llm: LLMConfig{MaxTokens: 4096, MaxCompletionTokens: 8192, ReasoningEffort: "low"}}
	body = map[string]any{"max_tokens": 123}
	s.applyWorkerLLMBudget(body)
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("max_tokens should be removed when max_completion_tokens is configured: %#v", body)
	}
	if body["max_completion_tokens"] != 8192 || body["reasoning_effort"] != "low" {
		t.Fatalf("reasoning budget not applied: %#v", body)
	}
	if got := s.llmMaxTokens(); got != 8192 {
		t.Fatalf("llmMaxTokens = %d", got)
	}
	if got, field := s.llmOutputBudget(); got != 8192 || field != "max_completion_tokens" {
		t.Fatalf("llmOutputBudget = %d/%s", got, field)
	}
}

func TestWorkerResponsesProxyPinsCentralModelAndKeepsCredentialsSeparated(t *testing.T) {
	var gotPath, gotAuth, gotCookie, gotAcceptEncoding string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotAcceptEncoding = r.Header.Get("Accept-Encoding")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("OpenAI-Request-ID", "req-central")
		w.Header().Set("Set-Cookie", "upstream=secret")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer upstream.Close()

	s := &Server{llm: LLMConfig{
		Provider: config.ProviderOpenAI, BaseURL: upstream.URL + "/v1",
		APIKey: "central-api-key", Model: "gpt-central", TimeoutMS: 10_000,
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/openai/v1/responses",
		strings.NewReader(`{"model":"stale-worker-model","stream":true,"input":"test"}`))
	req.Header.Set("Authorization", "Bearer worker-access-token")
	req.Header.Set("Cookie", "worker=secret")
	req.Header.Set("Accept-Encoding", "br")
	rec := httptest.NewRecorder()
	s.proxyWorkerOpenAIResponses(rec, req, "/responses")

	if rec.Code != http.StatusOK || rec.Body.String() != "data: {\"type\":\"response.completed\"}\n\n" {
		t.Fatalf("proxy response = status %d body %q", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/responses" || gotAuth != "Bearer central-api-key" || gotCookie != "" || gotAcceptEncoding == "br" {
		t.Fatalf("upstream routing/auth = path %q auth %q cookie %q encoding %q", gotPath, gotAuth, gotCookie, gotAcceptEncoding)
	}
	if gotBody["model"] != "gpt-central" || gotBody["stream"] != true {
		t.Fatalf("upstream body = %#v", gotBody)
	}
	if rec.Header().Get("OpenAI-Request-ID") != "req-central" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("response headers were not filtered: %#v", rec.Header())
	}
}

func TestWorkerResponsesProxyRejectsNullBody(t *testing.T) {
	s := &Server{llm: LLMConfig{Provider: config.ProviderOpenAI, BaseURL: "https://api.example/v1", Model: "gpt-central"}}
	rec := httptest.NewRecorder()
	s.proxyWorkerOpenAIResponses(rec,
		httptest.NewRequest(http.MethodPost, "/api/worker/openai/v1/responses", strings.NewReader("null")),
		"/responses")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("null Responses body status = %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestWorkerResponsesProxyRequiresOpenAIRuntime(t *testing.T) {
	s := &Server{llm: LLMConfig{Provider: config.ProviderClaude, BaseURL: "https://example.com", Model: "claude"}}
	rec := httptest.NewRecorder()
	s.proxyWorkerOpenAIResponses(rec,
		httptest.NewRequest(http.MethodPost, "/api/worker/openai/v1/responses", strings.NewReader(`{"input":"test"}`)),
		"/responses")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-OpenAI central runtime status = %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestLoadedRuntimeModelsUsesOllamaPS(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"models":[{"model":"mlx-community/DeepSeek-V4-Flash"},{"name":"glm-5.2"},{"model":"bad model"},{"model":"glm-5.2"}]}`))
	}))
	defer srv.Close()
	s := &Server{llm: LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key"}}
	got, err := s.loadedRuntimeModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mlx-community/DeepSeek-V4-Flash", "glm-5.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded models = %#v, want %#v", got, want)
	}
	if gotPath != "/ollama/api/ps" {
		t.Fatalf("loadedRuntimeModels should query runtime loaded-model endpoint, got %s", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("Authorization header = %q", gotAuth)
	}
}

func TestValidRuntimeModelName(t *testing.T) {
	for _, name := range []string{"mlx-community/DeepSeek-V4-Flash", "glm-5.2", "repo/model:tag"} {
		if !validRuntimeModelName(name) {
			t.Fatalf("model name should be valid: %q", name)
		}
	}
	for _, name := range []string{"", "bad model", "bad<model>", strings.Repeat("x", 161)} {
		if validRuntimeModelName(name) {
			t.Fatalf("model name should be invalid: %q", name)
		}
	}
}

func TestWorkerLLMClaudeConversion(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "do it"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "run_command", "arguments": `{"command":"pwd"}`,
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "/tmp/work"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "run_command",
			"description": "run one command",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}},
		}}},
	}
	req, err := openAIWorkerBodyToClaude(body, "glm-5.2", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "glm-5.2" || req.System != "sys" || len(req.Messages) != 3 || len(req.Tools) != 1 {
		t.Fatalf("unexpected converted request: %+v", req)
	}
	if got := req.Messages[1].Content[0]; got.Type != "tool_use" || got.ID != "call_1" || got.Name != "run_command" || string(got.Input) != `{"command":"pwd"}` {
		t.Fatalf("tool_use not preserved: %+v", got)
	}
	if got := req.Messages[2].Content[0]; got.Type != "tool_result" || got.ToolUseID != "call_1" || got.Content != "/tmp/work" {
		t.Fatalf("tool_result not preserved: %+v", got)
	}

	raw := []byte(`{"id":"msg_1","model":"glm-5.2","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call_2","name":"task_done","input":{"summary":"ok"}}],"usage":{"input_tokens":11,"output_tokens":7}}`)
	out, err := claudeWorkerRespToOpenAI(raw)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content != "checking" || len(parsed.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("unexpected converted response: %s", out)
	}
	tc := parsed.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_2" || tc.Function.Name != "task_done" || tc.Function.Arguments != `{"summary":"ok"}` {
		t.Fatalf("tool call not preserved: %+v", tc)
	}
	if parsed.Usage.PromptTokens != 11 || parsed.Usage.CompletionTokens != 7 {
		t.Fatalf("usage not preserved: %+v", parsed.Usage)
	}
}

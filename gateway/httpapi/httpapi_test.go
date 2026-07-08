package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

func TestLooksLikeNBCOCodeTask(t *testing.T) {
	if !looksLikeNBCOCodeTask("nbco 需要增加功能并部署到 im.app") {
		t.Fatal("nbco code/deploy task should map to repo scope")
	}
	if looksLikeNBCOCodeTask("整理 nbco 公司资料表格") {
		t.Fatal("plain material task mentioning nbco should not automatically map to repo scope")
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

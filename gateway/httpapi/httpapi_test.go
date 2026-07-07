package httpapi

import (
	"encoding/json"
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

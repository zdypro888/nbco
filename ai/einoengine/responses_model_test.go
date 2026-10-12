package einoengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/session"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
)

func responseFixture(status string, output ...any) map[string]any {
	return map[string]any{"id": "resp_test", "object": "response", "status": status, "output": output,
		"incomplete_details": map[string]any{"reason": "max_output_tokens"},
		"usage": map[string]any{"input_tokens": 12, "output_tokens": 8, "total_tokens": 20,
			"input_tokens_details": map[string]any{"cached_tokens": 4}, "output_tokens_details": map[string]any{"reasoning_tokens": 3}}}
}

func responseText(text string) map[string]any {
	return map[string]any{"type": "message", "id": "msg_test", "role": "assistant", "status": "completed", "phase": "final_answer",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func responseCall(name, args string) map[string]any {
	return map[string]any{"type": "function_call", "id": "fc_test", "call_id": "call_test", "name": name, "arguments": args, "status": "completed"}
}

func responseReasoning() map[string]any {
	return map[string]any{"type": "reasoning", "id": "rs_test", "summary": []any{}, "encrypted_content": "opaque-test-ciphertext"}
}

func writeResponseSSE(w http.ResponseWriter, kind string, response any) {
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(map[string]any{"type": kind, "response": response})
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
}

func TestResponsesRequestAndStatelessRoundTrip(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer dummy" {
			t.Errorf("unexpected endpoint/auth")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responseFixture("completed", responseReasoning(), responseText("done"), responseCall("lookup", `{}`)))
	}))
	defer server.Close()
	m := newResponsesChatModel(config.AIConfig{APIKey: "dummy", BaseURL: server.URL + "/v1", Model: "gpt-6.1-sol", MaxTokens: 700, MaxCompletionTokens: 900, ReasoningEffort: "medium"})
	bound, err := m.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "lookup"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.tools) != 0 {
		t.Fatal("WithTools mutated shared model")
	}
	options := []einomodel.Option{einomodel.WithMaxTokens(321), einomodel.WrapImplSpecificOptFn(func(o *responsesCallOptions) { o.DisableReasoning = true; o.JSONOutput = true })}
	msg, err := bound.Generate(context.Background(), []*schema.Message{schema.SystemMessage("JSON only"), schema.UserMessage("lookup")}, options...)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(msg)
	var restored schema.Message
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	_, err = bound.Generate(context.Background(), []*schema.Message{schema.UserMessage("lookup"), &restored, schema.ToolMessage("ok", "call_test")})
	if err != nil {
		t.Fatal(err)
	}
	first := bodies[0]
	if first["store"] != false || first["max_output_tokens"] != float64(321) || first["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("wrong Responses options: %v", first)
	}
	for _, key := range []string{"messages", "max_tokens", "max_completion_tokens", "previous_response_id", "response_format"} {
		if _, exists := first[key]; exists {
			t.Errorf("legacy/stateful field %s", key)
		}
	}
	if first["text"].(map[string]any)["format"].(map[string]any)["type"] != "json_object" {
		t.Fatal("missing JSON format")
	}
	tool := first["tools"].([]any)[0].(map[string]any)
	if tool["strict"] != false || tool["parameters"].(map[string]any)["type"] != "object" {
		t.Fatal("bad no-argument schema")
	}
	second := bodies[1]
	if second["max_output_tokens"] != float64(900) || second["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatal("options leaked between requests")
	}
	items := second["input"].([]any)
	if len(items) != 5 || items[1].(map[string]any)["encrypted_content"] != "opaque-test-ciphertext" || items[2].(map[string]any)["phase"] != "final_answer" || items[4].(map[string]any)["call_id"] != "call_test" {
		t.Fatalf("lost replay items: %v", items)
	}
	if msg.ResponseMeta.Usage.PromptTokenDetails.CachedTokens != 4 || msg.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Fatal("lost detailed usage")
	}
}

func TestResponsesStreamTerminalAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		output     []any
		want       string
		fail       bool
		calls      int
	}{
		{"text", "response.completed", []any{responseText("hello")}, "hello", false, 0},
		{"parallel", "response.completed", []any{responseReasoning(), responseCall("a", `{}`), responseCall("b", `{"x":1}`)}, "", false, 2},
		{"truncated-call", "response.incomplete", []any{responseCall("a", `{"x":`)}, "", false, 0},
		{"invalid-call", "response.completed", []any{responseCall("a", `{`)}, "", true, 0},
		{"failed", "response.failed", nil, "", true, 0},
		{"missing-terminal", "", nil, "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.name == "text" {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"he\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"llo\"}\n\n")
				}
				fmt.Fprint(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\"}\n\n")
				if tc.kind != "" {
					status := strings.TrimPrefix(tc.kind, "response.")
					writeResponseSSE(w, tc.kind, responseFixture(status, tc.output...))
				}
			}))
			defer server.Close()
			m := newResponsesChatModel(config.AIConfig{APIKey: "dummy", BaseURL: server.URL, Model: "gpt-6.1-sol"})
			stream, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("test")})
			if err != nil {
				t.Fatal(err)
			}
			msg, err := schema.ConcatMessageStream(stream)
			if tc.fail {
				if err == nil {
					t.Fatal("expected stream failure")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if msg.Content != tc.want || len(msg.ToolCalls) != tc.calls {
				t.Fatalf("bad aggregate: %+v", msg)
			}
			if tc.kind == "response.incomplete" && msg.ResponseMeta.FinishReason != "length" {
				t.Fatal("missing length finish reason")
			}
		})
	}
}

func TestResponsesStreamCancellation(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(done)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	m := newResponsesChatModel(config.AIConfig{APIKey: "dummy", BaseURL: server.URL, Model: "gpt-6.1-sol"})
	stream, err := m.Stream(ctx, []*schema.Message{schema.UserMessage("test")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancel()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("cancellation lost")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request did not cancel")
	}
}

func runResponsesEngineSmoke(t *testing.T, cfg config.AIConfig) {
	t.Helper()
	engine := newNativeTestEngine(newResponsesChatModel(cfg), session.NewInMemoryStore[*schema.Message](nil))
	engine.cfg = cfg
	var mu sync.Mutex
	executed := 0
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{SessionID: "turn:1",
		System: "Use the lookup tool exactly once, then reply with its result. Do not use subagents or todos.", UserText: "Look up the verification code.",
		Tools: []ai.Tool{{Name: "lookup", Description: "Return the verification code", LoadMode: ai.ToolLoadImmediate, Effect: ai.ToolEffectRead,
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				mu.Lock()
				executed++
				mu.Unlock()
				return "verification-42", nil
			}}}})
	if err != nil {
		t.Fatal(err)
	}
	if executed != 1 || !strings.Contains(result.Text, "verification-42") || result.EngineSession == "" {
		t.Fatalf("smoke failed: calls=%d text=%q session=%q", executed, result.Text, result.EngineSession)
	}
}

func TestResponsesDeepAgentToolLoop(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		requests++
		if body["stream"] != true {
			t.Error("Deep agent must stream")
		}
		if requests == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(2200 * time.Millisecond)
			writeResponseSSE(w, "response.completed", responseFixture("completed", responseReasoning(), responseCall("lookup", `{}`)))
			return
		}
		var reasoning, toolOutput bool
		for _, raw := range body["input"].([]any) {
			item := raw.(map[string]any)
			if item["type"] == "reasoning" && item["encrypted_content"] == "opaque-test-ciphertext" {
				reasoning = true
			}
			if item["type"] == "function_call_output" && item["call_id"] == "call_test" {
				toolOutput = true
			}
		}
		if !reasoning || !toolOutput {
			t.Error("Deep loop lost reasoning or call linkage")
		}
		writeResponseSSE(w, "response.completed", responseFixture("completed", responseText("verification-42")))
	}))
	defer server.Close()
	runResponsesEngineSmoke(t, config.AIConfig{Provider: config.ProviderOpenAI, OpenAIAPI: "responses", APIKey: "dummy", BaseURL: server.URL, Model: "gpt-6.1-sol", ReasoningEffort: "medium", MaxCompletionTokens: 8192})
}

func TestResponsesLiveSmoke(t *testing.T) {
	path := os.Getenv("NBCO_RESPONSES_SMOKE_CONFIG")
	if path == "" {
		t.Skip("explicit protected config required for billable API smoke")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		AI config.AIConfig `json:"ai"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	c.AI.OpenAIAPI = "responses"
	c.AI.Model = "gpt-6.1-sol"
	c.AI.ReasoningEffort = "medium"
	c.AI.MaxCompletionTokens = 8192
	c.AI.TimeoutMS = 180000
	runResponsesEngineSmoke(t, c.AI)
	engine := newNativeTestEngine(newResponsesChatModel(c.AI), nil)
	engine.cfg = c.AI
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{Mode: ai.TurnModeOneShot, System: "Return only JSON: {\"ok\":true}", UserText: "Verify JSON output", Reasoning: ai.ReasoningDisabled, JSONOutput: true, MaxOutputTokens: 321})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal([]byte(result.Text), &output) != nil || !output.OK {
		t.Fatalf("live bounded JSON failed: finish=%s", result.FinishReason)
	}
}

func TestResponsesOneShotOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["max_output_tokens"] != float64(321) || body["reasoning"].(map[string]any)["effort"] != "low" || body["text"].(map[string]any)["format"].(map[string]any)["type"] != "json_object" {
			t.Errorf("one-shot options not applied: %v", body)
		}
		if body["stream"] == true {
			writeResponseSSE(w, "response.completed", responseFixture("completed", responseText(`{"ok":true}`)))
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(responseFixture("completed", responseText(`{"ok":true}`)))
		}
	}))
	defer server.Close()
	cfg := config.AIConfig{Provider: config.ProviderOpenAI, OpenAIAPI: "responses", APIKey: "dummy", BaseURL: server.URL, Model: "gpt-6.1-sol", ReasoningEffort: "medium", MaxTokens: 8192}
	m, err := newChatModel(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := newNativeTestEngine(m, nil)
	engine.cfg = cfg
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{Mode: ai.TurnModeOneShot, System: "Return JSON", UserText: "ok", Reasoning: ai.ReasoningDisabled, JSONOutput: true, MaxOutputTokens: 321})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"ok":true}` {
		t.Fatalf("bad result %q", result.Text)
	}
}

func TestResponsesRuntimeSerializerRoundTrip(t *testing.T) {
	raw, _ := json.Marshal(responseFixture("completed", responseReasoning(), responseText("done"), responseCall("lookup", `{}`)))
	msg, err := responsesMessage(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	serializer := &schema.HumanReadableSerializer{}
	payload, err := encodeSessionEvent(serializer, &adk.SessionEvent[*schema.Message]{EventID: "responses-event", Message: msg})
	if err != nil {
		t.Fatal(err)
	}
	var event adk.SessionEvent[*schema.Message]
	if err := serializer.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	items, err := responsesInput([]*schema.Message{event.Message})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(items)
	if !strings.Contains(string(encoded), `"encrypted_content":"opaque-test-ciphertext"`) || !strings.Contains(string(encoded), `"phase":"final_answer"`) {
		t.Fatal("durable serializer lost protocol metadata")
	}
}

func TestResponsesPreservesInterleavedReplayAndFilteredCalls(t *testing.T) {
	first := responseCall("a", `{}`)
	second := responseCall("b", `{}`)
	second["call_id"] = "call_second"
	output := []any{responseReasoning(), first, responseText("comment"), responseReasoning(), second}
	raw, _ := json.Marshal(responseFixture("completed", output...))
	msg, err := responsesMessage(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	items, err := responsesInput([]*schema.Message{msg})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"reasoning", "function_call", "message", "reasoning", "function_call"} {
		if items[i].(map[string]any)["type"] != want {
			t.Fatal("output order changed")
		}
	}
	msg.ToolCalls = msg.ToolCalls[:1]
	msg.ToolCalls[0].Function.Arguments = `{"patched":true}`
	items, err = responsesInput([]*schema.Message{msg})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 || items[1].(map[string]any)["arguments"] != `{"patched":true}` {
		t.Fatal("replay ignored Eino call filtering or patching")
	}
}

func TestResponsesContentFilterAndSampling(t *testing.T) {
	body := responseFixture("incomplete", responseCall("a", `{`))
	body["incomplete_details"] = map[string]any{"reason": "content_filter"}
	raw, _ := json.Marshal(body)
	msg, err := responsesMessage(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.ResponseMeta.FinishReason != "content_filter" || len(msg.ToolCalls) != 0 {
		t.Fatal("content filter misclassified")
	}
	m := newResponsesChatModel(config.AIConfig{APIKey: "dummy", Model: "gpt-6.1-sol", ReasoningEffort: "medium", Temperature: 0.7})
	request, err := m.request([]*schema.Message{schema.UserMessage("test")})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(request)
	if strings.Contains(string(encoded), "temperature") {
		t.Fatal("Sol reasoning request contains unsupported temperature")
	}
}

func TestResponsesReaderCloseCancelsUpstream(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(done)
	}))
	defer server.Close()
	m := newResponsesChatModel(config.AIConfig{APIKey: "dummy", BaseURL: server.URL, Model: "gpt-6.1-sol"})
	reader, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("test")})
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reader close left upstream running")
	}
}

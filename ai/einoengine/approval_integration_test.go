package einoengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/config"
	nbcostore "github.com/zdypro888/nbco/store"
)

// Real Responses transport and Eino checkpoints, with a local scripted provider
// and an isolated PostgreSQL namespace. No paid API or real business mutation.
func TestNativeApprovalPostgresRestartAndMultipleRounds(t *testing.T) {
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set NBCO_TEST_PG_DSN")
	}
	ctx := context.Background()
	st, err := nbcostore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	root := "native-approval-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-"
	defer func() {
		defer st.Close()
		_, _ = st.Pool().Exec(ctx, `DELETE FROM eino_session_events WHERE session_id LIKE $1`, root+"%")
		_, _ = st.Pool().Exec(ctx, `DELETE FROM eino_checkpoints WHERE checkpoint_id LIKE $1`, root+"%")
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Type string `json:"type"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		outputs := 0
		for _, item := range request.Input {
			if item.Type == "function_call_output" {
				outputs++
			}
		}
		var output []any
		if outputs < 2 {
			output = []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc-%d", outputs), "call_id": fmt.Sprintf("call-%d", outputs), "name": "write_value", "arguments": fmt.Sprintf(`{"value":%d}`, outputs)}}
		} else {
			output = []any{map[string]any{"type": "message", "id": "msg-final", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "both stored", "annotations": []any{}}}}}
		}
		response := map[string]any{"id": "response-local", "object": "response", "created_at": 1, "status": "completed", "model": "test", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
		event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", event)
	}))
	defer server.Close()
	cfg := config.AIConfig{Provider: "openai", OpenAIAPI: "responses", APIKey: "local-test-only", BaseURL: server.URL, Model: "test", MaxTurns: 8, MaxTokens: 4096}
	newEngine := func() *Engine {
		e, err := New(ctx, cfg, WithRuntimeStore(&namespacedRuntimeStore{RuntimeStore: NewPostgresRuntimeStore(st.Pool()), prefix: root}))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	var calls atomic.Int32
	req := &ai.TurnRequest{SessionID: "42", InvocationScope: "turn:42", UserText: "write twice", RequireApproval: true, Tools: []ai.Tool{{Name: "write_value", Effect: ai.ToolEffectWrite, LoadMode: ai.ToolLoadImmediate, ApprovalRequired: true, Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
		if ai.ToolInvocationKey(ctx) == "" {
			t.Error("domain invocation identity missing")
		}
		calls.Add(1)
		return "stored", nil
	}}}}
	result, err := newEngine().RunTurn(ctx, req)
	for round := 0; round < 2; round++ {
		if err != nil || result == nil || result.PendingApproval == nil || calls.Load() != int32(round) {
			t.Fatalf("round %d pause=%+v err=%v calls=%d", round, result, err, calls.Load())
		}
		pending := result.PendingApproval
		// Close all database/runtime handles and reopen: only persisted state remains.
		st.Close()
		st, err = nbcostore.Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		req.EngineSession = result.EngineSession
		req.Resume = &ai.ResumeRequest{CheckPointID: pending.CheckPointID, Decisions: map[string]bool{pending.Tools[0].InterruptID: true}}
		result, err = newEngine().RunTurn(ctx, req)
	}
	if err != nil || result == nil || result.PendingApproval != nil || result.Text != "both stored" || calls.Load() != 2 {
		t.Fatalf("final=%+v err=%v calls=%d", result, err, calls.Load())
	}
	if _, err = newEngine().RunTurn(ctx, req); err == nil {
		t.Fatal("duplicate checkpoint accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("duplicate mutation")
	}
}

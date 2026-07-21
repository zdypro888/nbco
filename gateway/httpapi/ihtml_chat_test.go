package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"

	ihtml "github.com/zdypro888/ihtml"

	"github.com/zdypro888/nbco/chat"
)

func TestIHTMLBackendPublishesRuntimeModelOnly(t *testing.T) {
	backend := &ihtmlChatBackend{
		orch: &chat.Orchestrator{}, provider: "openai", timeoutMS: 1234,
		model: func(context.Context) string { return "runtime/model" },
	}
	models, err := backend.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !models.ModelReady || models.CurrentModel != "runtime/model" || len(models.Models) != 1 || models.ModelSelect || len(models.AgentModes) != 1 || models.AgentModes[0] != "deep" {
		t.Fatalf("models = %+v", models)
	}
}

func TestNilIHTMLBackendReportsUnavailableModel(t *testing.T) {
	var backend *ihtmlChatBackend
	models, err := backend.Models(context.Background())
	if err != nil || models.ModelReady || models.Provider != "" || models.RequestTimeoutMS != 0 {
		t.Fatalf("nil backend models = %+v, err=%v", models, err)
	}
}

func TestIHTMLEmitterOwnsMonotonicEventIdentity(t *testing.T) {
	var mu sync.Mutex
	events := make([]ihtml.ChatStreamEvent, 0, 32)
	emit := newIHTMLEmitter("run-1", func(event ihtml.ChatStreamEvent) error {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		return nil
	})
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := emit(ihtml.ChatStreamEvent{Type: ihtml.ChatStreamAssistantStatus}); err != nil {
				t.Errorf("emit: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(events) != 32 {
		t.Fatalf("events = %d", len(events))
	}
	for index, event := range events {
		if event.RunID != "run-1" || event.TurnID != "run-1_turn" || event.Seq != int64(index+1) {
			t.Fatalf("event[%d] = %+v", index, event)
		}
	}
}

func TestIHTMLTurnUsesLatestUserMessageAndBoundsBrowserContext(t *testing.T) {
	text, err := lastIHTMLUserMessage([]ihtml.ChatMessage{
		{Role: "user", Content: "old"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "  latest  "},
	})
	if err != nil || text != "latest" {
		t.Fatalf("latest message = %q, %v", text, err)
	}
	system := ihtmlTurnSystem([]ihtml.APISpec{{Name: "overview", Method: "GET", Path: "/api/overview"}})
	if !strings.Contains(system, "同一个 nbco Agent") || !strings.Contains(system, "ui_list_state") ||
		!strings.Contains(system, `"path":"/api/overview"`) || !strings.Contains(system, "ihtml.http(path, options)") {
		t.Fatalf("unexpected host system prompt length/content: %d", len([]rune(system)))
	}
	browser := ihtmlBrowserContext(ihtml.ChatClientContext{Page: ihtml.ChatClientPage{VisibleText: strings.Repeat("x", 5000)}})
	if len([]rune(browser)) > 2500 || !strings.Contains(browser, `"visible_text"`) {
		t.Fatalf("unexpected browser context length/content: %d", len([]rune(browser)))
	}
}

func TestIHTMLHTTPContractStaysAlignedAcrossAgentSurfaces(t *testing.T) {
	const callable = "ihtml.http(path, options)"
	if system := crossChannelIHTMLSystem("https://nbco.example"); !strings.Contains(system, callable) {
		t.Fatalf("cross-channel contract does not document callable HTTP: %q", system)
	}
	for _, item := range ihtmlAgentTools(nil) {
		if item.Name != "ui_list_host_apis" {
			continue
		}
		if !strings.Contains(item.Description, callable) {
			t.Fatalf("host API tool does not document callable HTTP: %q", item.Description)
		}
		return
	}
	t.Fatal("ui_list_host_apis tool is missing")
}

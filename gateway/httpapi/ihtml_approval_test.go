package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	ihtml "github.com/zdypro888/ihtml"
	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

func TestIHTMLApprovalCardsUseDurableIdentityAndPausedTerminal(t *testing.T) {
	var events []ihtml.ChatStreamEvent
	pending := &store.ConversationApproval{ID: 42, TurnID: 10, ConsentKey: []byte(strings.Repeat("k", 32)), CheckPointID: "private-checkpoint", EngineSession: "private-session", ExpiresAt: time.Now().Add(time.Minute),
		Tools: []store.ConversationApprovalTool{{InterruptID: "call:a", ToolName: "delete_record", Args: json.RawMessage(`{"id":1}`)}, {InterruptID: "decided", ToolName: "delete_record", Args: json.RawMessage(`{}`)}}, Decisions: map[string]bool{"decided": false}}
	if err := emitIHTMLApproval(func(ev ihtml.ChatStreamEvent) error { events = append(events, ev); return nil }, pending, 7); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != ihtml.ChatStreamConfirmationRequired || events[1].Type != ihtml.ChatStreamTurnPaused {
		t.Fatalf("events=%+v", events)
	}
	card := events[0].ToolCall
	if card.ApprovalResume != "checkpoint" || card.ConfirmationToken == "" {
		t.Fatal("missing native signed card contract")
	}
	if err := verifyIHTMLApprovalCard([]store.ConversationApproval{*pending}, 7, card); err != nil {
		t.Fatal(err)
	}
	if err := verifyIHTMLApprovalCard([]store.ConversationApproval{*pending}, 8, card); err == nil {
		t.Fatal("card accepted for another user")
	}
	original := *card
	card.Name = "attacker-tool"
	card.Body = map[string]any{"id": 999}
	if err := verifyIHTMLApprovalCard([]store.ConversationApproval{*pending}, 7, card); err == nil {
		t.Fatal("tampered view accepted")
	}
	*card = original
	card.ConfirmationToken = ""
	if err := verifyIHTMLApprovalCard([]store.ConversationApproval{*pending}, 7, card); err == nil {
		t.Fatal("naked card accepted")
	}
	*card = original
	id, interrupt, err := parseIHTMLApprovalCard(card)
	if err != nil || id != 42 || interrupt != "call:a" {
		t.Fatalf("identity=%d %s %v", id, interrupt, err)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "private-checkpoint") || strings.Contains(string(raw), "private-session") {
		t.Fatal("private runtime state leaked")
	}
	for _, call := range []*ihtml.ProposedToolCall{nil, {ID: "42"}, {ID: "nbco_approval:-1:call"}, {ID: "nbco_approval:42:"}} {
		if _, _, err := parseIHTMLApprovalCard(call); err == nil {
			t.Fatal("invalid card accepted")
		}
	}
}

func TestIHTMLRequestIdentityIsBoundedAndUserScoped(t *testing.T) {
	id := strings.Repeat("a", 32)
	first, err := ihtmlTurnSourceKey(7, id, "run-one")
	second, _ := ihtmlTurnSourceKey(7, id, "run-two")
	other, _ := ihtmlTurnSourceKey(8, id, "run-one")
	if err != nil || first != second || first == other {
		t.Fatalf("keys=%s %s %s err=%v", first, second, other, err)
	}
	for _, id := range []string{"short", strings.Repeat("a", 129), strings.Repeat("a", 16) + "/", strings.Repeat("a", 16) + "\n", strings.Repeat("a", 16) + "é"} {
		if _, err := ihtmlTurnSourceKey(7, id, "run"); err == nil {
			t.Fatal("invalid request ID accepted")
		}
	}
	if legacy, err := ihtmlTurnSourceKey(7, "", "run"); err != nil || legacy != "ihtml:run" {
		t.Fatalf("legacy=%s %v", legacy, err)
	}
}

func TestIHTMLApprovalSurvivesCloseAndRestoresOnReconnect(t *testing.T) {
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("NBCO_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, 7767002); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, 7767002)
		conn.Release()
	})
	u, err := s.CreateUser(ctx, "restore-owner", true, store.Identity{Provider: "test", ExternalID: fmt.Sprintf("restore-%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = s.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, u.ID) })
	session, err := s.StartSession(ctx, u.ID, ihtmlChatChannel, "eino")
	if err != nil {
		t.Fatal(err)
	}
	turn, _, err := s.BeginConversationTurn(ctx, "restore-test", u.ID, session.ID, ihtmlChatChannel, "delete")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.PauseConversationTurn(ctx, turn.ID, turn.Attempts, fmt.Sprintf("restore-checkpoint-%d", turn.ID), "restore-engine",
		[]store.ConversationApprovalTool{{InterruptID: "call", ToolName: "ui_delete_page", Args: json.RawMessage(`{}`), ArgsHash: "hash"}}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var previousID string
	for range 2 {
		stream := make(chan ihtml.ChatStreamEvent, 8)
		sessionCtx, cancel := context.WithCancel(ctx)
		shared := &ihtmlSharedSession{ctx: sessionCtx, cancel: cancel, user: u, events: make(chan ihtml.ChatClientEvent, 16),
			backend: &ihtmlChatBackend{orch: chat.New(s, nil, tools.Deps{Store: s}, time.UTC, false, time.Minute)},
			emitRaw: func(ev ihtml.ChatStreamEvent) error { stream <- ev; return nil }}
		shared.wg.Add(1)
		go shared.loop()
		var cardID string
		for {
			select {
			case ev := <-stream:
				if ev.Type == ihtml.ChatStreamTurnStart && !ev.RestoredApproval {
					shared.Close()
					t.Fatal("restore treated as new user turn")
				}
				if ev.Type == ihtml.ChatStreamAssistantDone {
					shared.Close()
					t.Fatal("restore completed pending execution")
				}
				if ev.Type == ihtml.ChatStreamConfirmationRequired {
					cardID = ev.ToolCall.ID
				}
				if ev.Type == ihtml.ChatStreamTurnPaused {
					goto restored
				}
			case <-time.After(5 * time.Second):
				shared.Close()
				t.Fatal("restore timed out")
			}
		}
	restored:
		shared.Close()
		if cardID == "" || (previousID != "" && previousID != cardID) {
			t.Fatal("restore changed durable card identity")
		}
		previousID = cardID
		got, err := s.PendingConversationApprovals(ctx, u.ID, ihtmlChatChannel)
		if err != nil || len(got) != 1 || got[0].ID != pending.ID {
			t.Fatalf("close lost pause: %v %v", got, err)
		}
	}
}

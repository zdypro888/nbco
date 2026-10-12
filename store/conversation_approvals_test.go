package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestApprovalDecisionsAreExactAndImmutable(t *testing.T) {
	a := &ConversationApproval{Tools: []ConversationApprovalTool{{InterruptID: "a"}, {InterruptID: "b"}}}
	if err := mergeApprovalDecisions(a, map[string]bool{"unknown": true}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := mergeApprovalDecisions(a, map[string]bool{"a": false}); err != nil {
		t.Fatal(err)
	}
	if err := mergeApprovalDecisions(a, map[string]bool{"a": false}); err != nil {
		t.Fatal(err)
	}
	if err := mergeApprovalDecisions(a, map[string]bool{"a": true, "b": true}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if len(a.Decisions) != 1 || a.Decisions["a"] {
		t.Fatalf("decisions mutated: %v", a.Decisions)
	}
}

func makePausedApproval(t *testing.T, s *Store, u *User, count int) (*ConversationTurn, *ConversationApproval) {
	t.Helper()
	ctx := context.Background()
	session, err := s.StartSession(ctx, u.ID, "web:ihtml", "eino")
	if err != nil {
		t.Fatal(err)
	}
	turn, created, err := s.BeginConversationTurn(ctx, fmt.Sprintf("approval-test-%d", time.Now().UnixNano()), u.ID, session.ID, "web:ihtml", "delete requested")
	if err != nil || !created {
		t.Fatalf("begin: %v", err)
	}
	var tools []ConversationApprovalTool
	for i := range count {
		tools = append(tools, ConversationApprovalTool{InterruptID: fmt.Sprintf("call-%d", i), ToolName: fmt.Sprintf("delete-%d", i), Args: json.RawMessage(`{"id":1}`), ArgsHash: fmt.Sprintf("hash-%d", i)})
	}
	a, err := s.PauseConversationTurn(ctx, turn.ID, turn.Attempts, fmt.Sprintf("checkpoint-%d", turn.ID), fmt.Sprintf("engine-%d", turn.ID), tools, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return turn, a
}

func TestConversationApprovalClaimAndDomainGrantScope(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "approval-owner", false)
	other := mkUser(t, s, "approval-other", false)
	turn, a := makePausedApproval(t, s, u, 2)
	for _, owner := range []int64{other.ID, u.ID} {
		channel := "wrong"
		if owner == other.ID {
			channel = "web:ihtml"
		}
		if _, _, _, err := s.DecideConversationApproval(ctx, owner, channel, a.ID, map[string]bool{"call-0": true}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ownership: %v", err)
		}
	}
	if _, _, _, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", a.ID, map[string]bool{"unknown": true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown: %v", err)
	}
	partial, _, claimed, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", a.ID, map[string]bool{"call-0": true})
	if err != nil || claimed || len(partial.Decisions) != 1 {
		t.Fatalf("partial: %v %v", claimed, err)
	}
	if _, _, _, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", a.ID, map[string]bool{"call-0": false}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed decision: %v", err)
	}
	claimedApproval, resumed, claimed, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", a.ID, map[string]bool{"call-1": false})
	if err != nil || !claimed || resumed.ID != turn.ID || resumed.Attempts != 2 {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	messageID := *claimedApproval.ConsentMessageID
	for _, probe := range []struct {
		tool, hash string
		message    int64
	}{
		{"delete-0", "wrong", messageID}, {"delete-1", "hash-1", messageID}, {"delete-0", "hash-0", messageID + 1},
	} {
		if ok, err := s.ConsumePendingApproval(ctx, u.ID, probe.tool, probe.hash, turn.SessionID, probe.message); err != nil || ok {
			t.Fatalf("unexpected grant %v %v", ok, err)
		}
	}
	if ok, err := s.ConsumePendingApproval(ctx, u.ID, "delete-0", "hash-0", turn.SessionID, messageID); err != nil || !ok {
		t.Fatalf("exact grant %v %v", ok, err)
	}
	if _, _, _, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", a.ID, map[string]bool{"call-1": false}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reclaimed: %v", err)
	}
	if _, err := s.CompleteConversationTurn(ctx, ConversationTurnCompletion{TurnID: turn.ID, ExpectedAttempt: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion: %v", err)
	}
	if err := s.FailConversationTurnAttempt(ctx, turn.ID, 1, "stale"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConversationTurnByID(ctx, turn.ID)
	if err != nil || got.Status != "running" {
		t.Fatalf("stale failure: %+v %v", got, err)
	}
}

func TestConversationApprovalCompetingClaims(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "approval-race", false)
	_, a := makePausedApproval(t, s, u, 1)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, claimed, err := s.DecideConversationApproval(context.Background(), u.ID, "web:ihtml", a.ID, map[string]bool{"call-0": true})
			if err == nil && claimed {
				wins.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("claims=%d", wins.Load())
	}
}

func TestConversationApprovalCheckpointCanPauseAgainAfterClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "approval-rounds", false)
	turn, first := makePausedApproval(t, s, u, 1)
	_, resumed, claimed, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", first.ID, map[string]bool{"call-0": true})
	if err != nil || !claimed {
		t.Fatalf("first claim: %v", err)
	}
	second, err := s.PauseConversationTurn(ctx, turn.ID, resumed.Attempts, first.CheckPointID, first.EngineSession, first.Tools, json.RawMessage(`{}`))
	if err != nil || second.ID == first.ID {
		t.Fatalf("second pause: %+v %v", second, err)
	}
	if string(second.ConsentKey) == string(first.ConsentKey) {
		t.Fatal("round reused consent key")
	}
	_, resumed, claimed, err = s.DecideConversationApproval(ctx, u.ID, "web:ihtml", second.ID, map[string]bool{"call-0": false})
	if err != nil || !claimed || resumed.Attempts != 3 {
		t.Fatalf("second claim: %+v %v", resumed, err)
	}
	if _, _, _, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", first.ID, map[string]bool{"call-0": true}); err == nil {
		t.Fatal("old round reclaimed")
	}
}

func TestConversationApprovalExpiryAndRevokedIdentity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "approval-expiry", false)
	turn, a := makePausedApproval(t, s, u, 1)
	if _, err := s.pool.Exec(ctx, `UPDATE conversation_approvals SET expires_at=now()-interval '1 second' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", a.ID, map[string]bool{"call-0": true}); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("expiry: %v", err)
	}
	got, _ := s.ConversationTurnByID(ctx, turn.ID)
	if got.Status != "failed" {
		t.Fatalf("stranded expired turn: %s", got.Status)
	}
	turn, a = makePausedApproval(t, s, u, 1)
	if _, err := s.pool.Exec(ctx, `UPDATE users SET status='disabled' WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.DecideConversationApproval(ctx, u.ID, "web:ihtml", a.ID, map[string]bool{"call-0": true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled owner: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE conversation_approvals SET expires_at=now()-interval '1 second' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.PendingConversationApprovals(ctx, u.ID, "web:ihtml"); err != nil || len(pending) != 0 {
		t.Fatalf("expired visibility: %v %v", pending, err)
	}
	got, _ = s.ConversationTurnByID(ctx, turn.ID)
	if got.Status != "failed" {
		t.Fatal("expired list did not close turn")
	}
}

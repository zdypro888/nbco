package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

func openTelegramAuditStore(t *testing.T) (*store.Store, *pgxpool.Conn) {
	t.Helper()
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("NBCO_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Release)
	if _, err := c.Exec(ctx, `SELECT pg_advisory_lock(7767002)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.Exec(ctx, `SELECT pg_advisory_unlock(7767002)`) })
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := c.Exec(ctx, `TRUNCATE users, projects, kv_state, telegram_inbound_updates RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestGroupTranscriptOwnerUsesDatabaseIdentity(t *testing.T) {
	s, _ := openTelegramAuditStore(t)
	ctx := t.Context()
	boss, err := s.CreateUser(ctx, "boss", true, store.Identity{Provider: "test", ExternalID: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "owner", false, store.Identity{Provider: "test", ExternalID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{store: s, superadmins: map[int64]bool{999999: true}}
	owner, err := g.groupTranscriptOwner(ctx, groupChannel(-123), nil)
	if err != nil || owner.ID != boss.ID {
		t.Fatalf("database superadmin not selected: %+v %v", owner, err)
	}
	if _, err := s.StartGroupSession(ctx, other.ID, groupChannel(-123), "eino"); err != nil {
		t.Fatal(err)
	}
	owner, err = g.groupTranscriptOwner(ctx, groupChannel(-123), boss)
	if err != nil || owner.ID != other.ID {
		t.Fatalf("existing owner not preserved: %+v %v", owner, err)
	}
}

type passiveAuditEngine struct{}

func (passiveAuditEngine) Name() string { return "eino" }
func (passiveAuditEngine) RunTurn(context.Context, *ai.TurnRequest) (*ai.TurnResult, error) {
	panic("passive collection must not invoke AI")
}

func TestPassiveGroupCollectionSurvivesStaleAdminConfig(t *testing.T) {
	s, _ := openTelegramAuditStore(t)
	ctx := t.Context()
	u, err := s.CreateUser(ctx, "boss", true, store.Identity{Provider: "test", ExternalID: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	chatID := int64(-789)
	if _, err := s.StartGroupSession(ctx, u.ID, groupChannel(chatID), "eino"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKV(ctx, listenKey(chatID), "1"); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{store: s, superadmins: map[int64]bool{999999: true}, orch: chat.New(s, passiveAuditEngine{}, tools.Deps{Store: s}, time.UTC, false, time.Minute)}
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	g.runCtx = stopped
	if err := s.SaveTelegramGroupMonitor(ctx, store.TelegramGroupMonitor{ChatID: chatID, Enabled: true, NotifyUserID: u.ID}); err != nil {
		t.Fatal(err)
	}
	m := &models.Message{ID: 12, Date: int(time.Now().Unix()), Text: "delivery completed", Chat: models.Chat{ID: chatID, Type: models.ChatTypeGroup}, From: &models.User{ID: 4444, FirstName: "speaker"}}
	for i := 0; i < 2; i++ {
		if err := g.processGroup(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := s.ChannelMessagesForward(ctx, groupChannel(chatID), time.Now().Add(-time.Hour), time.Now().Add(time.Second), 0, 0, 300)
	if err != nil || len(messages) != 1 {
		t.Fatalf("passive message lost: %+v %v", messages, err)
	}
	mon, err := s.TelegramGroupMonitor(ctx, chatID)
	if err != nil || mon.PendingCount != 1 {
		t.Fatalf("replayable wake state: %+v %v", mon, err)
	}
	if err := g.observeGroupMonitor(stopped, m.Chat, m.Text); err == nil {
		t.Fatal("monitor storage failure swallowed")
	}
}

func TestFailedQueuedMessageRemainsRetryable(t *testing.T) {
	s, c := openTelegramAuditStore(t)
	ctx := t.Context()
	if _, err := s.EnqueueTelegramInboundUpdate(ctx, 123, json.RawMessage(`{"update_id":123}`), "audit"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ClaimTelegramInboundUpdates(ctx, "audit", 1); err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v %v", rows, err)
	}
	g := &Gateway{store: s, monitorInstanceID: "audit"}
	g.finishQueuedMessage(&queuedTelegramMessage{done: make(chan struct{}), updateIDs: []int64{123}, processErr: errors.New("storage unavailable")}, false)
	var status, reason string
	if err := c.QueryRow(ctx, `SELECT status,last_error FROM telegram_inbound_updates WHERE update_id=123`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != store.TelegramInboundPending || reason != "storage unavailable" {
		t.Fatalf("failed update acknowledged: %s %s", status, reason)
	}
}

func TestGroupMonitorReplaysFrozenAnalysis(t *testing.T) {
	s, c := openTelegramAuditStore(t)
	ctx := t.Context()
	u, err := s.CreateUser(ctx, "monitor", true, store.Identity{Provider: "test", ExternalID: "monitor"})
	if err != nil {
		t.Fatal(err)
	}
	chatID := int64(-456)
	sess, err := s.StartGroupSession(ctx, u.ID, groupChannel(chatID), "eino")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AppendMessageWithEnvelope(ctx, sess.ID, "user", "release ready", store.MessageEnvelope{})
	if err != nil {
		t.Fatal(err)
	}
	through := time.Now().UTC()
	m := store.ChatMessage{ID: id, Content: "release ready", CreatedAt: through}
	p1, err := s.CreateProject(ctx, "original project", "", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreateProject(ctx, "replacement project", "", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindTelegramGroupProject(ctx, chatID, p2.ID, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	mon := store.TelegramGroupMonitor{ChatID: chatID, Enabled: true, NotifyUserID: u.ID, CreatedAt: through.Add(-time.Hour), AnalysisOwner: "audit", AnalysisThrough: through, BatchThrough: through, BatchLastMessageID: m.ID,
		BatchProjectID: &p1.ID,
		BatchResult:    json.RawMessage(fmt.Sprintf(`{"notify":false,"facts":[{"kind":"update","title":"release","content":"release ready","source_message_ids":[%d]}]}`, m.ID))}
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	g := &Gateway{store: s, monitorInstanceID: "audit", runCtx: stopped}
	for i := 0; i < 2; i++ {
		if err := s.SaveTelegramGroupMonitor(ctx, mon); err != nil {
			t.Fatal(err)
		}
		// No orchestrator: a replay must not call the model again.
		g.evaluateGroupMonitor(ctx, mon, []store.ChatMessage{m}, through)
		got, err := s.TelegramGroupMonitor(ctx, chatID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastMessageID != m.ID || len(got.BatchResult) != 0 || got.AnalysisFailures != 0 {
			t.Fatalf("batch not completed: %+v", got)
		}
	}
	var count int
	var projectID int64
	if err := c.QueryRow(ctx, `SELECT count(*),min(project_id) FROM work_evidence WHERE source_type=$1`, store.WorkEvidenceSourceTelegramGroupAnalysis).Scan(&count, &projectID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || projectID != p1.ID {
		t.Fatalf("replay duplicated or reassigned facts: %d project=%d", count, projectID)
	}
}

func TestGroupMonitorSnapshotRejectsConcurrentChanges(t *testing.T) {
	base := store.TelegramGroupMonitor{Enabled: true, NotifyUserID: 1, UpdatedAt: time.Now(), LastMessageID: 12}
	if !sameGroupMonitorBatch(&base, &base) {
		t.Fatal("identical snapshot rejected")
	}
	for _, mutate := range []func(*store.TelegramGroupMonitor){
		func(m *store.TelegramGroupMonitor) { m.UpdatedAt = m.UpdatedAt.Add(time.Second) },
		func(m *store.TelegramGroupMonitor) { m.LastMessageID++ },
		func(m *store.TelegramGroupMonitor) { m.Instruction = "new policy" },
		func(m *store.TelegramGroupMonitor) { m.AnalysisOwner = "other instance" },
		func(m *store.TelegramGroupMonitor) { m.Enabled = false },
	} {
		changed := base
		mutate(&changed)
		if sameGroupMonitorBatch(&changed, &base) {
			t.Fatalf("stale snapshot accepted: %+v", changed)
		}
	}
}

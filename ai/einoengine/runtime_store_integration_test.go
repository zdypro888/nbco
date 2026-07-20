package einoengine

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	adksession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/schema"

	nbcostore "github.com/zdypro888/nbco/store"
)

type namespacedRuntimeStore struct {
	RuntimeStore
	prefix string
}

func (s *namespacedRuntimeStore) AppendEvents(ctx context.Context, sessionID string, events []*adk.SessionEvent[*schema.Message]) error {
	return s.RuntimeStore.AppendEvents(ctx, s.prefix+sessionID, events)
}

func (s *namespacedRuntimeStore) LoadEvents(ctx context.Context, sessionID string, req *adk.LoadSessionEventsRequest) (*adk.LoadSessionEventsResult[*schema.Message], error) {
	return s.RuntimeStore.LoadEvents(ctx, s.prefix+sessionID, req)
}

func (s *namespacedRuntimeStore) Get(ctx context.Context, checkpointID string) ([]byte, bool, error) {
	return s.RuntimeStore.Get(ctx, s.prefix+checkpointID)
}

func (s *namespacedRuntimeStore) Set(ctx context.Context, checkpointID string, payload []byte) error {
	return s.RuntimeStore.Set(ctx, s.prefix+checkpointID, payload)
}

func (s *namespacedRuntimeStore) Delete(ctx context.Context, checkpointID string) error {
	return s.RuntimeStore.Delete(ctx, s.prefix+checkpointID)
}

func TestPostgresRuntimeStoreConformance(t *testing.T) {
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set NBCO_TEST_PG_DSN to run PostgreSQL Eino runtime tests")
	}
	ctx := context.Background()
	store, err := nbcostore.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := "eino-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-"
	defer func() {
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM eino_session_events WHERE session_id LIKE $1`, root+"%")
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM eino_checkpoints WHERE checkpoint_id LIKE $1`, root+"%")
	}()
	runtime := NewPostgresRuntimeStore(store.Pool())
	var sequence atomic.Int64
	adksession.RunConformanceTests(t, func(testing.TB) adk.SessionEventStore[*schema.Message] {
		prefix := root + strconv.FormatInt(sequence.Add(1), 10) + "-"
		return &namespacedRuntimeStore{RuntimeStore: runtime, prefix: prefix}
	}, schema.UserMessage)

	secretStore := &namespacedRuntimeStore{RuntimeStore: runtime, prefix: root + "secret-"}
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef"
	secretEvent := &adk.SessionEvent[*schema.Message]{
		EventID: "secret-event",
		Message: schema.ToolMessage(`{"access_token":"`+secret+`"}`, "tool-call"),
	}
	if err := secretStore.AppendEvents(ctx, "session", []*adk.SessionEvent[*schema.Message]{secretEvent}); err != nil {
		t.Fatal(err)
	}
	loaded, err := secretStore.LoadEvents(ctx, "session", nil)
	if err != nil || loaded == nil || len(loaded.Events) != 1 {
		count := 0
		if loaded != nil {
			count = len(loaded.Events)
		}
		t.Fatalf("load canonical event: events=%d err=%v", count, err)
	}
	if loaded.Events[0].Message.Content != secretEvent.Message.Content {
		t.Fatalf("durable canonical event changed: %q", loaded.Events[0].Message.Content)
	}
	if !strings.Contains(secretEvent.Message.Content, secret) {
		t.Fatal("persistence mutated the live turn event")
	}
	markerStore := &namespacedRuntimeStore{RuntimeStore: runtime, prefix: root + "marker-"}
	markerMessage := schema.UserMessage("deferred tools")
	markerMessage.Extra = map[string]any{einoToolSearchReminderKey: true}
	if err := markerStore.AppendEvents(ctx, "session", []*adk.SessionEvent[*schema.Message]{
		{EventID: "marker-event", Message: markerMessage},
	}); err != nil {
		t.Fatal(err)
	}
	markerEvents, err := markerStore.LoadEvents(ctx, "session", nil)
	if err != nil || markerEvents == nil || len(markerEvents.Events) != 1 {
		t.Fatalf("load marked event: events=%v err=%v", markerEvents, err)
	}
	marked, _ := markerEvents.Events[0].Message.Extra[einoToolSearchReminderKey].(bool)
	if !marked {
		t.Fatal("PostgreSQL runtime store lost Eino tool-search reminder metadata")
	}
	if stats, err := store.EinoRuntimeStats(ctx); err != nil || stats.Events == 0 || stats.Sessions == 0 || stats.StorageBytes == 0 {
		t.Fatalf("runtime stats=%+v err=%v", stats, err)
	}

	resetSession := root + "reset-session"
	resetEvent := &adk.SessionEvent[*schema.Message]{EventID: "reset-event", Message: schema.UserMessage("failed input")}
	if err := runtime.AppendEvents(ctx, resetSession, []*adk.SessionEvent[*schema.Message]{resetEvent}); err != nil {
		t.Fatal(err)
	}
	resetCheckpoint := "session/" + resetSession + "/runner_checkpoint"
	if err := runtime.Set(ctx, resetCheckpoint, []byte("pending")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.DeleteSession(ctx, resetSession); err != nil {
		t.Fatal(err)
	}
	if events, err := runtime.LoadEvents(ctx, resetSession, nil); err != nil || events == nil || len(events.Events) != 0 {
		t.Fatalf("reset session events=%v err=%v", events, err)
	}
	if _, found, err := runtime.Get(ctx, resetCheckpoint); err != nil || found {
		t.Fatalf("reset session checkpoint found=%v err=%v", found, err)
	}

	checkpointStore := &namespacedRuntimeStore{RuntimeStore: runtime, prefix: root + "checkpoint-"}
	if err := checkpointStore.Set(ctx, "one", []byte("state")); err != nil {
		t.Fatal(err)
	}
	payload, found, err := checkpointStore.Get(ctx, "one")
	if err != nil || !found || string(payload) != "state" {
		t.Fatalf("checkpoint get: found=%v payload=%q err=%v", found, payload, err)
	}
	if err := checkpointStore.Delete(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := checkpointStore.Get(ctx, "one"); err != nil || found {
		t.Fatalf("checkpoint delete: found=%v err=%v", found, err)
	}
}

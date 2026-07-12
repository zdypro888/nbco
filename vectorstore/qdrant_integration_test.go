package vectorstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestQdrantIntegration(t *testing.T) {
	url := os.Getenv("NBCO_TEST_QDRANT_URL")
	if url == "" {
		t.Skip("未设置 NBCO_TEST_QDRANT_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := NewQdrant(QdrantConfig{
		URL: url, APIKey: os.Getenv("NBCO_TEST_QDRANT_KEY"),
		CollectionPrefix: fmt.Sprintf("nbco_test_%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatal(err)
	}
	model := "integration:3"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = client.client.DeleteCollection(cleanupCtx, client.collectionName(model))
		_ = client.Close()
	})

	points := []Point{
		{Ref: Ref{Source: "tasks", EntityID: "1"}, Vector: []float32{1, 0, 0}, ContentHash: "a", Payload: map[string]any{"author_id": int64(7), "tags": []string{"finance", "policy"}}},
		{Ref: Ref{Source: "tasks", EntityID: "2"}, Vector: []float32{0, 1, 0}, ContentHash: "b", Payload: map[string]any{"author_id": int64(8)}},
		{Ref: Ref{Source: "projects", EntityID: "1"}, Vector: []float32{1, 0, 0}, ContentHash: "c"},
	}
	if err := client.Upsert(ctx, model, points); err != nil {
		t.Fatal(err)
	}
	hits, err := client.Search(ctx, model, []float32{1, 0, 0}, Filter{Must: map[string]any{
		PayloadSource: "tasks", "author_id": int64(7),
	}}, 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].EntityID != "1" || hits[0].Source != "tasks" {
		t.Fatalf("filtered hits = %+v", hits)
	}
	hits, err = client.Search(ctx, model, []float32{1, 0, 0}, Filter{Must: map[string]any{
		PayloadSource: "tasks", "tags": "policy",
	}}, 5, 0.5)
	if err != nil || len(hits) != 1 || hits[0].EntityID != "1" {
		t.Fatalf("array payload hits = %+v, %v", hits, err)
	}
	hashes, err := client.Hashes(ctx, model, 3, []Ref{{Source: "tasks", EntityID: "1"}, {Source: "tasks", EntityID: "9"}})
	if err != nil || hashes[Ref{Source: "tasks", EntityID: "1"}.Key()] != "a" {
		t.Fatalf("hashes = %+v, %v", hashes, err)
	}
	listed, err := client.List(ctx, model, 3, "tasks")
	if err != nil || len(listed) != 2 {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	if err := client.Delete(ctx, model, 3, []Ref{{Source: "tasks", EntityID: "2"}}); err != nil {
		t.Fatal(err)
	}
	listed, err = client.List(ctx, model, 3, "tasks")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list after delete = %+v, %v", listed, err)
	}
}

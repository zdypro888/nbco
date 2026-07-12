package semantic

import (
	"context"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/vectorstore"
)

type testEmbedder struct{ calls atomic.Int32 }

func (e *testEmbedder) Model() string { return "test-embed" }
func (e *testEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = []float32{float32(len(text)), 1}
	}
	return out, nil
}

type memoryVectors struct {
	points map[string]vectorstore.Point
}

func newMemoryVectors() *memoryVectors {
	return &memoryVectors{points: make(map[string]vectorstore.Point)}
}

func (m *memoryVectors) Upsert(_ context.Context, _ string, points []vectorstore.Point) error {
	for _, point := range points {
		m.points[point.Ref.Key()] = point
	}
	return nil
}

func (m *memoryVectors) Search(_ context.Context, _ string, vector []float32, filter vectorstore.Filter, limit int, minScore float32) ([]vectorstore.Hit, error) {
	var hits []vectorstore.Hit
	for _, point := range m.points {
		if source, ok := filter.Must[vectorstore.PayloadSource].(string); ok && point.Source != source {
			continue
		}
		score := ai.Cosine(vector, point.Vector)
		if score >= minScore {
			hits = append(hits, vectorstore.Hit{Ref: point.Ref, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (m *memoryVectors) Hashes(_ context.Context, _ string, _ int, refs []vectorstore.Ref) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		if point, ok := m.points[ref.Key()]; ok {
			out[ref.Key()] = point.ContentHash
		}
	}
	return out, nil
}

func (m *memoryVectors) List(_ context.Context, _ string, _ int, source string) ([]vectorstore.Metadata, error) {
	var out []vectorstore.Metadata
	for _, point := range m.points {
		if point.Source == source {
			out = append(out, vectorstore.Metadata{Ref: point.Ref, ContentHash: point.ContentHash})
		}
	}
	return out, nil
}

func (m *memoryVectors) Delete(_ context.Context, _ string, _ int, refs []vectorstore.Ref) error {
	for _, ref := range refs {
		delete(m.points, ref.Key())
	}
	return nil
}

func (m *memoryVectors) Ping(context.Context) error { return nil }
func (m *memoryVectors) Close() error               { return nil }

func TestUpsertDocumentsSkipsUnchangedAndStoresNoContent(t *testing.T) {
	ctx := context.Background()
	embedder := &testEmbedder{}
	vectors := newMemoryVectors()
	service := New(nil, embedder, vectors)
	docs := []Document{
		{Ref: vectorstore.Ref{Source: "tasks", EntityID: "1"}, Content: "苹果端下架", Payload: map[string]any{"kind": "tasks"}},
		{Ref: vectorstore.Ref{Source: "tasks", EntityID: "2"}, Content: "安卓版本发布", Payload: map[string]any{"kind": "tasks"}},
	}
	indexed, err := service.UpsertDocuments(ctx, docs)
	if err != nil || indexed != 2 {
		t.Fatalf("首次索引 = %d, %v", indexed, err)
	}
	firstCalls := embedder.calls.Load()
	if firstCalls != 2 { // 一次维度探测 + 一次批量正文嵌入
		t.Fatalf("embedding calls = %d", firstCalls)
	}
	for _, point := range vectors.points {
		if _, leaked := point.Payload["content"]; leaked {
			t.Fatalf("Qdrant payload 不应保存正文: %+v", point.Payload)
		}
	}
	indexed, err = service.UpsertDocuments(ctx, docs)
	if err != nil || indexed != 0 || embedder.calls.Load() != firstCalls {
		t.Fatalf("未变文档不应重嵌入: indexed=%d calls=%d err=%v", indexed, embedder.calls.Load(), err)
	}
	docs[0].Content = "iOS 审核导致应用无法下载"
	indexed, err = service.UpsertDocuments(ctx, docs)
	if err != nil || indexed != 1 {
		t.Fatalf("单条变更应只重嵌一条: indexed=%d err=%v", indexed, err)
	}
}

func TestDeleteMissing(t *testing.T) {
	ctx := context.Background()
	service := New(nil, &testEmbedder{}, newMemoryVectors())
	docs := []Document{
		{Ref: vectorstore.Ref{Source: "tasks", EntityID: "1"}, Content: "one"},
		{Ref: vectorstore.Ref{Source: "tasks", EntityID: "2"}, Content: "two"},
	}
	if _, err := service.UpsertDocuments(ctx, docs); err != nil {
		t.Fatal(err)
	}
	valid := map[string]bool{docs[0].Ref.Key(): true}
	deleted, err := service.DeleteMissing(ctx, "tasks", valid)
	if err != nil || deleted != 0 {
		t.Fatalf("首次缺失只应进入宽限: %d, %v", deleted, err)
	}
	deleted, err = service.DeleteMissing(ctx, "tasks", valid)
	if err != nil || deleted != 1 {
		t.Fatalf("连续两次缺失后应删除: %d, %v", deleted, err)
	}
}

func TestQueryVectorCache(t *testing.T) {
	embedder := &testEmbedder{}
	service := New(nil, embedder, newMemoryVectors())
	ctx := context.Background()
	first, err := service.QueryVector(ctx, "同一件事情的另一种说法")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.QueryVector(ctx, "同一件事情的另一种说法")
	if err != nil || len(second) != len(first) || embedder.calls.Load() != 1 {
		t.Fatalf("查询向量缓存失效: calls=%d err=%v", embedder.calls.Load(), err)
	}
}

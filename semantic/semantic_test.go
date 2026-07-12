package semantic

import (
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	mu     sync.Mutex
	points map[string]vectorstore.Point
}

func newMemoryVectors() *memoryVectors {
	return &memoryVectors{points: make(map[string]vectorstore.Point)}
}

func (m *memoryVectors) Upsert(_ context.Context, _ string, points []vectorstore.Point) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, point := range points {
		m.points[point.Ref.Key()] = point
	}
	return nil
}

func (m *memoryVectors) Search(_ context.Context, _ string, vector []float32, filter vectorstore.Filter, limit int, minScore float32) ([]vectorstore.Hit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		if point, ok := m.points[ref.Key()]; ok {
			out[ref.Key()] = point.ContentHash
		}
	}
	return out, nil
}

func (m *memoryVectors) List(_ context.Context, _ string, _ int, source string) ([]vectorstore.Metadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []vectorstore.Metadata
	for _, point := range m.points {
		if point.Source == source {
			out = append(out, vectorstore.Metadata{Ref: point.Ref, ContentHash: point.ContentHash})
		}
	}
	return out, nil
}

func (m *memoryVectors) Delete(_ context.Context, _ string, _ int, refs []vectorstore.Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if err != nil || len(second) != len(first) || embedder.calls.Load() != 2 {
		t.Fatalf("查询向量缓存失效: calls=%d err=%v", embedder.calls.Load(), err)
	}
}

func TestVectorFingerprintTracksActualModelOutput(t *testing.T) {
	first := vectorFingerprint([]float32{0.12341, -0.45671, 0.77771})
	equivalent := vectorFingerprint([]float32{0.123409, -0.456709, 0.777709})
	different := vectorFingerprint([]float32{0.22341, -0.45671, 0.77771})
	if first == "" || first != equivalent || first == different {
		t.Fatalf("fingerprints = %q %q %q", first, equivalent, different)
	}
	if tag := modelTag("model", 3, first); tag != "model:3:"+first {
		t.Fatalf("model tag = %q", tag)
	}
}

func TestValidateVectorRejectsNonFiniteValues(t *testing.T) {
	if err := validateVector([]float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	for _, vector := range [][]float32{{}, {float32(math.NaN())}, {float32(math.Inf(1))}} {
		if err := validateVector(vector); err == nil {
			t.Fatalf("应拒绝向量 %v", vector)
		}
	}
}

type measuredEmbedder struct {
	inFlight atomic.Int32
	max      atomic.Int32
	maxBatch atomic.Int32
}

func (*measuredEmbedder) Model() string { return "measured" }

func (e *measuredEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	current := e.inFlight.Add(1)
	defer e.inFlight.Add(-1)
	recordMax(&e.max, current)
	recordMax(&e.maxBatch, int32(len(texts)))
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(5 * time.Millisecond):
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 1}
	}
	return out, nil
}

func recordMax(dst *atomic.Int32, value int32) {
	for current := dst.Load(); value > current && !dst.CompareAndSwap(current, value); current = dst.Load() {
	}
}

func TestBulkEmbeddingsAreSerializedAndBounded(t *testing.T) {
	ctx := context.Background()
	embedder := &measuredEmbedder{}
	service := New(nil, embedder, newMemoryVectors())
	if _, _, err := service.CurrentModel(ctx); err != nil {
		t.Fatal(err)
	}
	embedder.max.Store(0)
	embedder.maxBatch.Store(0)

	documents := func(source string) []Document {
		out := make([]Document, 17)
		for i := range out {
			out[i] = Document{
				Ref:     vectorstore.Ref{Source: source, EntityID: string(rune('A' + i))},
				Content: "semantic document",
			}
		}
		return out
	}
	var wg sync.WaitGroup
	for _, source := range []string{"tasks", "profiles"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := service.UpsertDocuments(ctx, documents(source)); err != nil {
				t.Errorf("UpsertDocuments(%s): %v", source, err)
			}
		}()
	}
	wg.Wait()
	if got := embedder.max.Load(); got != 1 {
		t.Fatalf("后台 embedding 并发 = %d，期望 1", got)
	}
	if got := embedder.maxBatch.Load(); got > embedBatchSize {
		t.Fatalf("embedding 批量 = %d，超过上限 %d", got, embedBatchSize)
	}
}

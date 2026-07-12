// Package semantic owns the rebuildable semantic-index pipeline. It embeds
// curated PostgreSQL records, stores vectors in Qdrant, and returns only stable
// source IDs. Callers must always re-read PostgreSQL to enforce permissions.
package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/vectorstore"
	"golang.org/x/sync/singleflight"
)

const (
	SourceKnowledge   = "knowledge"
	SourceChatMessage = "chat_messages"

	embedBatchSize       = 8
	syncPageSize         = 100
	semanticCallTimeout  = 60 * time.Second
	semanticBulkTimeout  = 5 * time.Minute
	queryCacheTTL        = 2 * time.Minute
	queryCacheLimit      = 256
	queryFailureCooldown = 3 * time.Second
	modelProbeTTL        = 10 * time.Minute
)

// Document is a transient indexing input. Content is sent to the embedder but
// never stored in Qdrant; only vectorstore routing payload is persisted there.
type Document struct {
	Ref     vectorstore.Ref
	Content string
	Payload map[string]any
}

type queryCacheEntry struct {
	vector    []float32
	expiresAt time.Time
}

// Status is safe for the admin operations endpoint.
type Status struct {
	Configured   bool      `json:"configured"`
	Available    bool      `json:"available"`
	ModelTag     string    `json:"model_tag,omitempty"`
	Dimension    int       `json:"dimension,omitempty"`
	LastSyncAt   time.Time `json:"last_sync_at,omitempty"`
	LastSyncDocs int       `json:"last_sync_docs,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// Service shares one query-embedding cache and one reconciliation pipeline
// across knowledge, history, and structured data.
type Service struct {
	store    *store.Store
	embedder ai.Embedder
	vectors  vectorstore.Store

	modelGroup singleflight.Group
	modelMu    sync.Mutex
	modelTag   string
	dimension  int
	modelAt    time.Time

	queryGroup singleflight.Group
	queryMu    sync.Mutex
	queryCache map[string]queryCacheEntry
	failUntil  time.Time
	failErr    error

	statusMu sync.RWMutex
	status   Status
	// Bulk reconciliation is serialized so knowledge, history, and structured
	// data cannot overload a shared embedding endpoint during startup. Online
	// query embeddings remain independent and responsive.
	bulkEmbed chan struct{}

	missingMu sync.Mutex
	missing   map[string]int
}

func New(s *store.Store, embedder ai.Embedder, vectors vectorstore.Store) *Service {
	return &Service{
		store: s, embedder: embedder, vectors: vectors,
		queryCache: make(map[string]queryCacheEntry),
		status:     Status{Configured: embedder != nil && vectors != nil},
		missing:    make(map[string]int),
		bulkEmbed:  make(chan struct{}, 1),
	}
}

func (s *Service) Enabled() bool { return s != nil && s.embedder != nil && s.vectors != nil }

// QueryVector embeds and caches one semantic query. Concurrent knowledge,
// history, rule, skill, and structured searches share the same request.
func (s *Service) QueryVector(ctx context.Context, query string) ([]float32, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("qdrant 语义索引未启用")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("语义查询不能为空")
	}
	tag, dimension, err := s.CurrentModel(ctx)
	if err != nil {
		return nil, err
	}
	key := tag + "\x00" + query
	if vector, err, ok := s.queryState(key, time.Now()); ok {
		return vector, err
	}
	result := s.queryGroup.DoChan(key, func() (any, error) {
		if vector, err, ok := s.queryState(key, time.Now()); ok {
			return vector, err
		}
		embedCtx, cancel := context.WithTimeout(context.Background(), semanticCallTimeout)
		defer cancel()
		vectors, err := s.embedder.Embed(embedCtx, []string{query})
		if err == nil && (len(vectors) != 1 || len(vectors[0]) == 0) {
			err = fmt.Errorf("embedding 查询返回无效向量")
		}
		if err == nil {
			err = validateVector(vectors[0])
		}
		if err == nil && len(vectors[0]) != dimension {
			err = fmt.Errorf("embedding 查询维度为 %d，模型探针维度为 %d", len(vectors[0]), dimension)
		}
		if err != nil {
			s.recordFailure(err)
			return nil, err
		}
		s.recordQuerySuccess(key, vectors[0])
		return vectors[0], nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return nil, result.Err
		}
		vector, ok := result.Val.([]float32)
		if !ok || len(vector) == 0 {
			return nil, fmt.Errorf("embedding 查询返回无效结果")
		}
		return vector, nil
	}
}

// CurrentModel probes the embedding endpoint once per process and returns the
// model+dimension collection tag used by Qdrant.
func (s *Service) CurrentModel(ctx context.Context) (string, int, error) {
	if !s.Enabled() {
		return "", 0, fmt.Errorf("qdrant 语义索引未启用")
	}
	s.modelMu.Lock()
	if s.modelTag != "" && s.dimension > 0 && time.Since(s.modelAt) < modelProbeTTL {
		tag, dim := s.modelTag, s.dimension
		s.modelMu.Unlock()
		return tag, dim, nil
	}
	s.modelMu.Unlock()
	result := s.modelGroup.DoChan("model", func() (any, error) {
		s.modelMu.Lock()
		if s.modelTag != "" && s.dimension > 0 && time.Since(s.modelAt) < modelProbeTTL {
			info := struct {
				tag string
				dim int
			}{s.modelTag, s.dimension}
			s.modelMu.Unlock()
			return info, nil
		}
		s.modelMu.Unlock()
		probeCtx, cancel := context.WithTimeout(context.Background(), semanticCallTimeout)
		defer cancel()
		vectors, err := s.embedder.Embed(probeCtx, []string{"nbco semantic index"})
		if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
			if err == nil {
				err = fmt.Errorf("embedding 探测返回无效向量")
			}
			s.recordFailure(err)
			return nil, err
		}
		if err := validateVector(vectors[0]); err != nil {
			s.recordFailure(err)
			return nil, err
		}
		fingerprint := vectorFingerprint(vectors[0])
		tag := modelTag(s.embedder.Model(), len(vectors[0]), fingerprint)
		s.recordModel(tag, len(vectors[0]))
		return struct {
			tag string
			dim int
		}{tag, len(vectors[0])}, nil
	})
	select {
	case <-ctx.Done():
		return "", 0, ctx.Err()
	case result := <-result:
		if result.Err != nil {
			return "", 0, result.Err
		}
		info := result.Val.(struct {
			tag string
			dim int
		})
		return info.tag, info.dim, nil
	}
}

// UpsertDocuments embeds only missing or changed records. Content hashes are
// read from Qdrant first, making startup reconciliation cheap after the first
// successful backfill and self-healing after a Qdrant restore or data loss.
func (s *Service) UpsertDocuments(ctx context.Context, docs []Document) (int, error) {
	if !s.Enabled() || len(docs) == 0 {
		return 0, nil
	}
	tag, dim, err := s.CurrentModel(ctx)
	if err != nil {
		return 0, err
	}
	refs := make([]vectorstore.Ref, 0, len(docs))
	hashes := make(map[string]string, len(docs))
	for _, doc := range docs {
		if strings.TrimSpace(doc.Ref.Source) == "" || strings.TrimSpace(doc.Ref.EntityID) == "" || strings.TrimSpace(doc.Content) == "" {
			continue
		}
		refs = append(refs, doc.Ref)
		hashes[doc.Ref.Key()] = documentHash(doc)
	}
	existing, err := s.vectors.Hashes(ctx, tag, dim, refs)
	if err != nil {
		s.recordFailure(err)
		return 0, err
	}
	changed := make([]Document, 0, len(docs))
	for _, doc := range docs {
		hash := hashes[doc.Ref.Key()]
		if hash != "" && existing[doc.Ref.Key()] != hash {
			changed = append(changed, doc)
		}
	}
	indexed := 0
	for start := 0; start < len(changed); start += embedBatchSize {
		end := min(start+embedBatchSize, len(changed))
		batch := changed[start:end]
		texts := make([]string, len(batch))
		for i := range batch {
			texts[i] = batch[i].Content
		}
		vectors, embedErr := s.embedBulk(ctx, texts)
		if embedErr != nil {
			s.recordFailure(embedErr)
			return indexed, embedErr
		}
		if len(vectors) != len(batch) {
			return indexed, fmt.Errorf("embedding 返回 %d 条，期望 %d", len(vectors), len(batch))
		}
		points := make([]vectorstore.Point, 0, len(batch))
		for i, vector := range vectors {
			if len(vector) != dim {
				return indexed, fmt.Errorf("embedding 维度从 %d 变为 %d，请重启后建立新 collection", dim, len(vector))
			}
			if err := validateVector(vector); err != nil {
				return indexed, fmt.Errorf("embedding 第 %d 条: %w", i, err)
			}
			points = append(points, vectorstore.Point{
				Ref: batch[i].Ref, Vector: vector,
				ContentHash: hashes[batch[i].Ref.Key()], Payload: batch[i].Payload,
			})
		}
		if err := s.vectors.Upsert(ctx, tag, points); err != nil {
			s.recordFailure(err)
			return indexed, err
		}
		indexed += len(points)
	}
	s.recordAvailable()
	return indexed, nil
}

func (s *Service) embedBulk(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case s.bulkEmbed <- struct{}{}:
		defer func() { <-s.bulkEmbed }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	embedCtx, cancel := context.WithTimeout(ctx, semanticBulkTimeout)
	defer cancel()
	return s.embedder.Embed(embedCtx, texts)
}

func (s *Service) Search(ctx context.Context, query string, filter vectorstore.Filter, limit int, minScore float32) ([]vectorstore.Hit, error) {
	vector, err := s.QueryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	return s.SearchVector(ctx, vector, filter, limit, minScore)
}

func (s *Service) SearchVector(ctx context.Context, vector []float32, filter vectorstore.Filter, limit int, minScore float32) ([]vectorstore.Hit, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("qdrant 语义索引未启用")
	}
	tag, dimension, err := s.CurrentModel(ctx)
	if err != nil {
		return nil, err
	}
	if len(vector) != dimension {
		return nil, fmt.Errorf("查询向量维度为 %d，当前模型维度为 %d", len(vector), dimension)
	}
	hits, err := s.vectors.Search(ctx, tag, vector, filter, limit, minScore)
	if err != nil {
		s.recordFailure(err)
		return nil, err
	}
	s.recordAvailable()
	return hits, nil
}

// DeleteMissing removes stale points after a complete authoritative scan.
func (s *Service) DeleteMissing(ctx context.Context, source string, valid map[string]bool) (int, error) {
	if !s.Enabled() {
		return 0, nil
	}
	tag, dim, err := s.CurrentModel(ctx)
	if err != nil {
		return 0, err
	}
	items, err := s.vectors.List(ctx, tag, dim, source)
	if err != nil {
		return 0, err
	}
	stale := make([]vectorstore.Ref, 0)
	s.missingMu.Lock()
	for _, item := range items {
		key := item.Ref.Key()
		if valid[key] {
			delete(s.missing, key)
			continue
		}
		s.missing[key]++
		// A point must be absent from two complete scans before deletion. This
		// prevents a concurrent insert/delete shifting a paged SQL scan from
		// causing a healthy point to disappear temporarily.
		if s.missing[key] >= 2 {
			stale = append(stale, item.Ref)
		}
	}
	s.missingMu.Unlock()
	if err := s.vectors.Delete(ctx, tag, dim, stale); err != nil {
		return 0, err
	}
	s.missingMu.Lock()
	for _, ref := range stale {
		delete(s.missing, ref.Key())
	}
	s.missingMu.Unlock()
	return len(stale), nil
}

// SyncStructured reconciles every curated text-bearing read model. Knowledge
// and chat messages are reconciled by the knowledge service because they carry
// additional author/session filters.
func (s *Service) SyncStructured(ctx context.Context) error {
	if !s.Enabled() || s.store == nil {
		return nil
	}
	total := 0
	var syncErrors []error
	for _, source := range store.SemanticDataSources() {
		count, err := s.syncStructuredSource(ctx, source)
		total += count
		if err != nil {
			syncErrors = append(syncErrors, err)
		}
	}
	err := errors.Join(syncErrors...)
	s.recordSync(total, err)
	return err
}

func (s *Service) syncStructuredSource(ctx context.Context, source string) (int, error) {
	valid := make(map[string]bool)
	total := 0
	for offset := 0; ; offset += syncPageSize {
		docs, err := s.store.SemanticDocuments(ctx, source, offset, syncPageSize)
		if err != nil {
			return total, fmt.Errorf("同步语义数据源 %s: %w", source, err)
		}
		batch := make([]Document, 0, len(docs))
		for _, doc := range docs {
			if strings.TrimSpace(doc.Content) == "" {
				continue
			}
			ref := vectorstore.Ref{Source: doc.Source, EntityID: doc.EntityID}
			valid[ref.Key()] = true
			batch = append(batch, Document{
				Ref: ref, Content: doc.Content,
				Payload: map[string]any{vectorstore.PayloadKind: doc.Source},
			})
		}
		if _, err := s.UpsertDocuments(ctx, batch); err != nil {
			return total, fmt.Errorf("写入语义数据源 %s: %w", source, err)
		}
		total += len(batch)
		// docs counts source rows, including intentionally non-indexable empty
		// records, so pagination cannot terminate early after filtering.
		if len(docs) < syncPageSize {
			break
		}
	}
	if _, err := s.DeleteMissing(ctx, source, valid); err != nil {
		return total, fmt.Errorf("清理语义数据源 %s: %w", source, err)
	}
	return total, nil
}

func (s *Service) Run(ctx context.Context, interval, syncTimeout time.Duration) {
	if !s.Enabled() || interval <= 0 {
		return
	}
	if syncTimeout <= 0 {
		syncTimeout = time.Hour
	}
	for ctx.Err() == nil {
		syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
		err := s.SyncStructured(syncCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("Qdrant 结构化语义索引同步失败，保留 PostgreSQL 词法降级", "err", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) Health(ctx context.Context) Status {
	if !s.Enabled() {
		return Status{Configured: false}
	}
	err := s.vectors.Ping(ctx)
	s.statusMu.Lock()
	if err != nil {
		s.status.Available = false
		s.status.LastError = err.Error()
	} else {
		s.status.Available = true
		s.status.LastError = ""
	}
	status := s.status
	s.statusMu.Unlock()
	return status
}

func (s *Service) queryState(key string, now time.Time) ([]float32, error, bool) {
	s.queryMu.Lock()
	defer s.queryMu.Unlock()
	if cached, ok := s.queryCache[key]; ok {
		if now.Before(cached.expiresAt) {
			return cached.vector, nil, true
		}
		delete(s.queryCache, key)
	}
	if now.Before(s.failUntil) {
		return nil, s.failErr, true
	}
	return nil, nil, false
}

func (s *Service) recordQuerySuccess(key string, vector []float32) {
	s.queryMu.Lock()
	if len(s.queryCache) >= queryCacheLimit {
		clear(s.queryCache)
	}
	s.queryCache[key] = queryCacheEntry{vector: vector, expiresAt: time.Now().Add(queryCacheTTL)}
	s.failUntil = time.Time{}
	s.failErr = nil
	s.queryMu.Unlock()
	s.recordAvailable()
}

func (s *Service) recordFailure(err error) {
	if err == nil {
		return
	}
	s.queryMu.Lock()
	s.failUntil = time.Now().Add(queryFailureCooldown)
	s.failErr = err
	s.queryMu.Unlock()
	s.statusMu.Lock()
	s.status.Available = false
	s.status.LastError = err.Error()
	s.statusMu.Unlock()
}

func (s *Service) recordAvailable() {
	s.statusMu.Lock()
	s.status.Available = true
	s.status.LastError = ""
	s.statusMu.Unlock()
}

func (s *Service) recordModel(tag string, dim int) {
	s.modelMu.Lock()
	s.modelTag = tag
	s.dimension = dim
	s.modelAt = time.Now()
	s.modelMu.Unlock()
	s.statusMu.Lock()
	s.status.ModelTag = tag
	s.status.Dimension = dim
	s.statusMu.Unlock()
}

func (s *Service) recordSync(total int, err error) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if err != nil {
		s.status.Available = false
		s.status.LastError = err.Error()
		return
	}
	s.status.Available = true
	s.status.LastError = ""
	s.status.LastSyncAt = time.Now()
	s.status.LastSyncDocs = total
}

func documentHash(doc Document) string {
	payload, _ := json.Marshal(doc.Payload)
	sum := sha256.Sum256([]byte(strings.TrimSpace(doc.Content) + "\x00" + string(payload)))
	return hex.EncodeToString(sum[:])
}

func modelTag(model string, dimension int, fingerprint string) string {
	return fmt.Sprintf("%s:%d:%s", strings.TrimSpace(model), dimension, fingerprint)
}

// vectorFingerprint identifies the actual model output rather than trusting a
// provider-supplied model label. Quantization absorbs insignificant float noise
// across equivalent inference workers while changing when model weights or
// routing materially change.
func vectorFingerprint(vector []float32) string {
	hash := sha256.New()
	var buf [4]byte
	for _, value := range vector {
		quantized := int32(math.Round(float64(value) * 10_000))
		binary.LittleEndian.PutUint32(buf[:], uint32(quantized))
		_, _ = hash.Write(buf[:])
	}
	return hex.EncodeToString(hash.Sum(nil)[:6])
}

func validateVector(vector []float32) error {
	if len(vector) == 0 {
		return fmt.Errorf("向量不能为空")
	}
	for i, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("向量第 %d 维不是有限数", i)
		}
	}
	return nil
}

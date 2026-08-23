// Package semantic owns the rebuildable semantic-index pipeline. It embeds
// curated PostgreSQL records, stores vectors in Qdrant, and returns only stable
// source IDs. Callers must always re-read PostgreSQL to enforce permissions.
package semantic

import (
	"context"
	"crypto/sha256"
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
	"github.com/zdypro888/nbco/textfmt"
	"github.com/zdypro888/nbco/vectorstore"
	"golang.org/x/sync/singleflight"
)

const (
	SourceKnowledge   = "knowledge"
	SourceChatMessage = "chat_messages"
	SourceFileChunk   = "file_chunks"

	embedBatchSize       = 8
	maxEmbeddingRunes    = 6000
	syncPageSize         = 100
	fullSyncInterval     = 6 * time.Hour
	incrementalOverlap   = 30 * time.Second
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

// UpsertReport distinguishes authoritative documents that are already/currently
// indexed from records rejected by the embedding provider. Callers doing a
// backfill can advance past a bad record while leaving only that record pending.
type UpsertReport struct {
	Indexed   int
	Succeeded map[string]bool
	Failed    map[string]error
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
	query = strings.TrimSpace(textfmt.RedactSecrets(query))
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

// CurrentModel periodically probes the embedding endpoint and returns the
// configured model identity and observed dimension used by Qdrant. The
// embedder identity includes the explicit revision when one is configured;
// deriving identity from one floating-point response is unstable on hosted
// inference backends and would create duplicate collections for the same model.
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
		tag := modelTag(s.embedder.Model(), len(vectors[0]))
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
	report, err := s.UpsertDocumentsDetailed(ctx, docs)
	if err != nil {
		return report.Indexed, err
	}
	if len(report.Failed) > 0 {
		return report.Indexed, fmt.Errorf("%d 条文档 embedding 失败", len(report.Failed))
	}
	return report.Indexed, nil
}

// UpsertDocumentsDetailed embeds only missing or changed records and isolates
// input-specific failures. A failed document is omitted from Succeeded so
// durable backfill cursors can continue without falsely marking it complete.
func (s *Service) UpsertDocumentsDetailed(ctx context.Context, docs []Document) (UpsertReport, error) {
	report := UpsertReport{Succeeded: make(map[string]bool), Failed: make(map[string]error)}
	if !s.Enabled() || len(docs) == 0 {
		return report, nil
	}
	// The embedding provider and Qdrant are derived indexes, not credential
	// stores. Apply one mandatory boundary here so every producer (knowledge,
	// chat, files, and future read models) gets the same protection.
	safeDocs := make([]Document, 0, len(docs))
	for _, doc := range docs {
		doc.Content = compactEmbeddingContent(textfmt.RedactSecrets(doc.Content))
		if doc.Content == "" {
			continue
		}
		doc.Payload = sanitizePayload(doc.Payload)
		safeDocs = append(safeDocs, doc)
	}
	docs = safeDocs
	if len(docs) == 0 {
		return report, nil
	}
	tag, dim, err := s.CurrentModel(ctx)
	if err != nil {
		return report, err
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
		return report, err
	}
	changed := make([]Document, 0, len(docs))
	for _, doc := range docs {
		hash := hashes[doc.Ref.Key()]
		if hash != "" && existing[doc.Ref.Key()] == hash {
			report.Succeeded[doc.Ref.Key()] = true
		} else if hash != "" {
			changed = append(changed, doc)
		}
	}
	for start := 0; start < len(changed); start += embedBatchSize {
		end := min(start+embedBatchSize, len(changed))
		batch := changed[start:end]
		vectors, failures, embedErr := s.embedBatchResilient(ctx, batch, dim)
		for key, failure := range failures {
			report.Failed[key] = failure
		}
		if embedErr != nil {
			s.recordFailure(embedErr)
			return report, embedErr
		}
		points := make([]vectorstore.Point, 0, len(batch))
		for i, vector := range vectors {
			if vector == nil {
				continue
			}
			points = append(points, vectorstore.Point{
				Ref: batch[i].Ref, Vector: vector,
				ContentHash: hashes[batch[i].Ref.Key()], Payload: batch[i].Payload,
			})
		}
		if len(points) == 0 {
			continue
		}
		if err := s.vectors.Upsert(ctx, tag, points); err != nil {
			s.recordFailure(err)
			return report, err
		}
		for _, point := range points {
			report.Succeeded[point.Ref.Key()] = true
		}
		report.Indexed += len(points)
	}
	if len(report.Failed) == 0 {
		s.recordAvailable()
	}
	return report, nil
}

func compactEmbeddingContent(content string) string {
	content = strings.TrimSpace(content)
	runes := []rune(content)
	if len(runes) <= maxEmbeddingRunes {
		return content
	}
	const marker = "\n[...内容过长，已保留首尾用于语义检索...]\n"
	budget := maxEmbeddingRunes - len([]rune(marker))
	tail := budget / 4
	head := budget - tail
	return strings.TrimSpace(string(runes[:head])) + marker +
		strings.TrimSpace(string(runes[len(runes)-tail:]))
}

func (s *Service) embedBatchResilient(ctx context.Context, batch []Document, dim int) ([][]float32, map[string]error, error) {
	texts := make([]string, len(batch))
	for i := range batch {
		texts[i] = batch[i].Content
	}
	vectors, err := s.embedBulk(ctx, texts)
	if err == nil && len(vectors) == len(batch) {
		failures := make(map[string]error)
		for i, vector := range vectors {
			if len(vector) != dim {
				failures[batch[i].Ref.Key()] = fmt.Errorf("embedding 维度从 %d 变为 %d", dim, len(vector))
				vectors[i] = nil
				continue
			}
			if validationErr := validateVector(vector); validationErr != nil {
				failures[batch[i].Ref.Key()] = validationErr
				vectors[i] = nil
			}
		}
		return vectors, failures, nil
	}
	if len(batch) == 1 {
		if err == nil {
			err = fmt.Errorf("embedding 返回 %d 条，期望 1", len(vectors))
		}
		return [][]float32{nil}, map[string]error{batch[0].Ref.Key(): err}, nil
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	batchErr := err
	if batchErr == nil {
		batchErr = fmt.Errorf("embedding 返回 %d 条，期望 %d", len(vectors), len(batch))
	}

	// A provider may reject one oversized/malformed input by failing the entire
	// batch. Retry each document once so healthy neighbours still advance.
	out := make([][]float32, len(batch))
	failures := make(map[string]error)
	succeeded := 0
	for i := range batch {
		one, oneErr := s.embedBulk(ctx, []string{batch[i].Content})
		switch {
		case oneErr != nil:
			failures[batch[i].Ref.Key()] = oneErr
		case len(one) != 1 || len(one[0]) != dim:
			failures[batch[i].Ref.Key()] = fmt.Errorf("embedding 单条返回维度/数量无效")
		case validateVector(one[0]) != nil:
			failures[batch[i].Ref.Key()] = fmt.Errorf("embedding 单条向量无效")
		default:
			out[i] = one[0]
			succeeded++
		}
	}
	if succeeded == 0 {
		return out, failures, fmt.Errorf("embedding 批次及所有单条重试均失败: %w", batchErr)
	}
	return out, failures, nil
}

func sanitizePayload(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return nil
	}
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(key))
		switch normalized {
		case "apikey", "apihash", "accesstoken", "workeraccesstoken", "token", "secret", "password", "authorization":
			out[key] = "[redacted]"
		default:
			out[key] = sanitizePayloadValue(value)
		}
	}
	return out
}

func sanitizePayloadValue(value any) any {
	switch typed := value.(type) {
	case string:
		return textfmt.RedactSecrets(typed)
	case []string:
		out := make([]string, len(typed))
		for i := range typed {
			out[i] = textfmt.RedactSecrets(typed[i])
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = sanitizePayloadValue(typed[i])
		}
		return out
	case map[string]any:
		return sanitizePayload(typed)
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			converted[key] = item
		}
		return sanitizePayload(converted)
	default:
		return value
	}
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
		counterKey := tag + "\x00" + key
		if valid[key] {
			delete(s.missing, counterKey)
			continue
		}
		s.missing[counterKey]++
		// A point must be absent from two complete scans before deletion. This
		// prevents a concurrent insert/delete shifting a paged SQL scan from
		// causing a healthy point to disappear temporarily.
		if s.missing[counterKey] >= 2 {
			stale = append(stale, item.Ref)
		}
	}
	s.missingMu.Unlock()
	if err := s.vectors.Delete(ctx, tag, dim, stale); err != nil {
		return 0, err
	}
	s.missingMu.Lock()
	for _, ref := range stale {
		delete(s.missing, tag+"\x00"+ref.Key())
	}
	s.missingMu.Unlock()
	return len(stale), nil
}

// SyncStructured reconciles every curated text-bearing read model. Knowledge
// and chat messages are reconciled by the knowledge service because they carry
// additional author/session filters.
func (s *Service) SyncStructured(ctx context.Context) error {
	return s.syncStructured(ctx, nil, true)
}

func (s *Service) syncStructured(ctx context.Context, changedSince *time.Time, deleteMissing bool) error {
	if !s.Enabled() || s.store == nil {
		return nil
	}
	total := 0
	var syncErrors []error
	for _, source := range store.SemanticDataSources() {
		count, err := s.syncStructuredSource(ctx, source, changedSince, deleteMissing)
		total += count
		if err != nil {
			syncErrors = append(syncErrors, err)
		}
	}
	err := errors.Join(syncErrors...)
	s.recordSync(total, err)
	return err
}

func (s *Service) syncStructuredSource(ctx context.Context, source string, changedSince *time.Time, deleteMissing bool) (int, error) {
	valid := make(map[string]bool)
	total := 0
	failed := 0
	var cursor *store.SemanticCursor
	for {
		docs, err := s.store.SemanticDocuments(ctx, source, cursor, changedSince, syncPageSize)
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
		report, err := s.UpsertDocumentsDetailed(ctx, batch)
		if err != nil {
			return total, fmt.Errorf("写入语义数据源 %s: %w", source, err)
		}
		if len(report.Failed) > 0 {
			failed += len(report.Failed)
			slog.Warn("部分语义文档已隔离，继续同步后续记录", "source", source, "failed", len(report.Failed))
		}
		total += len(batch)
		// docs counts source rows, including intentionally non-indexable empty
		// records, so pagination cannot terminate early after filtering.
		if len(docs) < syncPageSize {
			break
		}
		last := docs[len(docs)-1]
		cursor = &store.SemanticCursor{SortAt: last.SortAt, SortID: last.SortID}
	}
	if deleteMissing {
		if _, err := s.DeleteMissing(ctx, source, valid); err != nil {
			return total, fmt.Errorf("清理语义数据源 %s: %w", source, err)
		}
	}
	if failed > 0 {
		// Do not advance the incremental watermark past rejected rows. Healthy
		// documents are skipped by content hash on the retry, while failed rows
		// remain eligible instead of waiting for the next six-hour full scan.
		return total, fmt.Errorf("语义数据源 %s 有 %d 条文档待重试", source, failed)
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
	lastIncremental := time.Now().Add(-incrementalOverlap)
	nextFull := time.Now()
	lastModelTag := ""
	for ctx.Err() == nil {
		scanStarted := time.Now()
		syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
		modelTag, _, modelErr := s.CurrentModel(syncCtx)
		full := structuredSyncNeedsFull(scanStarted, nextFull, lastModelTag, modelTag)
		var err error
		if modelErr != nil {
			err = modelErr
		} else if full {
			err = s.SyncStructured(syncCtx)
		} else {
			since := lastIncremental.Add(-incrementalOverlap)
			err = s.syncStructured(syncCtx, &since, false)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("Qdrant 结构化语义索引同步失败，保留 PostgreSQL 词法降级", "err", err)
		} else if err == nil {
			lastIncremental = scanStarted
			lastModelTag = modelTag
			if full {
				nextFull = scanStarted.Add(fullSyncInterval)
			}
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

func structuredSyncNeedsFull(now, nextFull time.Time, previousModel, currentModel string) bool {
	return !now.Before(nextFull) || previousModel == "" || currentModel != previousModel
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

func modelTag(model string, dimension int) string {
	return fmt.Sprintf("%s:%d", strings.TrimSpace(model), dimension)
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

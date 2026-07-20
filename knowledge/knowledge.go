// Package knowledge 是知识库的检索/沉淀服务层：在 store 之上叠加语义检索。
// 配了 embedder 就走「语义（cosine）+ 词法」混合召回；没配或 embedder 失败就
// 优雅回退到纯词法。中枢对话工具、worker 领活时的经验注入都经它，行为一致。
package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/semantic"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/vectorstore"
	"golang.org/x/sync/singleflight"
)

// semanticCandidateMul 语义召回时，先取 limit×该倍数 的候选再与词法融合排序。
const semanticCandidateMul = 3

// ruleSemanticMinScore 动态规则会进入“必须遵守”上下文，宁可少召回也不能乱召回。
// 词法命中的规则仍会保留；该阈值只过滤纯语义候选。
const ruleSemanticMinScore float32 = 0.35

// skillSemanticMinScore 比规则稍宽松：skill 只是候选执行方法，系统提示里只注入摘要，
// 需要完整步骤时还会显式 load_skill。
const skillSemanticMinScore float32 = 0.28

// embedTimeout embed-on-save / 查询向量化的超时（本地服务通常很快；超时就回退）。
const embedTimeout = 20 * time.Second
const asyncEmbedConcurrency = 4
const queryEmbedConcurrency = 4
const queryVectorCacheTTL = 2 * time.Minute
const queryVectorFailureCooldown = 3 * time.Second
const queryVectorCacheLimit = 256

type queryVectorCacheEntry struct {
	vector    []float32
	expiresAt time.Time
}

// Service 知识库服务。Embedder 可为 nil（未启用语义检索）。
type Service struct {
	store      *store.Store
	embedder   ai.Embedder
	semantic   *semantic.Service
	embedSem   chan struct{}
	querySem   chan struct{}
	queryGroup singleflight.Group
	queryMu    sync.Mutex
	queryCache map[string]queryVectorCacheEntry
	failUntil  time.Time
	failErr    error
}

func New(s *store.Store, embedder ai.Embedder, semanticService ...*semantic.Service) *Service {
	service := &Service{
		store:      s,
		embedder:   embedder,
		embedSem:   make(chan struct{}, asyncEmbedConcurrency),
		querySem:   make(chan struct{}, queryEmbedConcurrency),
		queryCache: make(map[string]queryVectorCacheEntry),
	}
	if len(semanticService) > 0 && semanticService[0] != nil && semanticService[0].Enabled() {
		service.semantic = semanticService[0]
	}
	return service
}

// queryVector deduplicates the same semantic query across knowledge, rules,
// skills, and episodic history. The embedding request outlives an individual
// retrieval budget so later consumers can reuse its result; callers can still
// stop waiting immediately through their own context.
func (svc *Service) queryVector(ctx context.Context, query string) ([]float32, error) {
	if svc.semantic != nil {
		return svc.semantic.QueryVector(ctx, query)
	}
	if svc.embedder == nil {
		return nil, fmt.Errorf("embedding 未启用")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := svc.embedder.Model() + "\x00" + strings.TrimSpace(query)
	if vector, err, ok := svc.queryState(key, time.Now()); ok {
		return vector, err
	}

	result := svc.queryGroup.DoChan(key, func() (any, error) {
		if vector, err, ok := svc.queryState(key, time.Now()); ok {
			return vector, err
		}
		embedCtx, cancel := context.WithTimeout(context.Background(), embedTimeout)
		defer cancel()
		select {
		case svc.querySem <- struct{}{}:
			defer func() { <-svc.querySem }()
		case <-embedCtx.Done():
			svc.recordQueryFailure(embedCtx.Err())
			return nil, embedCtx.Err()
		}
		vectors, err := svc.embedder.Embed(embedCtx, []string{query})
		if err == nil && (len(vectors) != 1 || len(vectors[0]) == 0) {
			err = fmt.Errorf("embedding 查询返回无效向量")
		}
		if err != nil {
			svc.recordQueryFailure(err)
			return nil, err
		}
		vector := vectors[0]
		svc.recordQuerySuccess(key, vector)
		return vector, nil
	})

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-result:
		if res.Err != nil {
			return nil, res.Err
		}
		vector, ok := res.Val.([]float32)
		if !ok || len(vector) == 0 {
			return nil, fmt.Errorf("embedding 查询返回无效结果")
		}
		return vector, nil
	}
}

func (svc *Service) queryState(key string, now time.Time) ([]float32, error, bool) {
	svc.queryMu.Lock()
	defer svc.queryMu.Unlock()
	if cached, ok := svc.queryCache[key]; ok {
		if now.Before(cached.expiresAt) {
			return cached.vector, nil, true
		}
		delete(svc.queryCache, key)
	}
	if now.Before(svc.failUntil) {
		err := svc.failErr
		if err == nil {
			err = fmt.Errorf("embedding 服务暂时不可用")
		}
		return nil, err, true
	}
	return nil, nil, false
}

func (svc *Service) recordQueryFailure(err error) {
	svc.queryMu.Lock()
	svc.failUntil = time.Now().Add(queryVectorFailureCooldown)
	svc.failErr = err
	svc.queryMu.Unlock()
}

func (svc *Service) recordQuerySuccess(key string, vector []float32) {
	svc.queryMu.Lock()
	if len(svc.queryCache) >= queryVectorCacheLimit {
		clear(svc.queryCache)
	}
	svc.queryCache[key] = queryVectorCacheEntry{vector: vector, expiresAt: time.Now().Add(queryVectorCacheTTL)}
	svc.failUntil = time.Time{}
	svc.failErr = nil
	svc.queryMu.Unlock()
}

// Save 存一条知识，并【异步 fire-and-forget】落 embedding——绝不阻塞建知识的
// 用户/对话轮次（向量化走本地 HTTP，可能慢/冷启动）。失败也无妨，回填兜底。
func (svc *Service) Save(ctx context.Context, title, content string, tags []string, authorID int64) (*store.Knowledge, error) {
	k, err := svc.store.CreateKnowledge(ctx, title, content, tags, authorID)
	if err != nil {
		return nil, err
	}
	svc.embedAsync(k)
	return k, nil
}

// Reembed 内容更新后异步重算 embedding（同样不阻塞调用方）。
func (svc *Service) Reembed(_ context.Context, k *store.Knowledge) {
	svc.embedAsync(k)
}

// embedAsync 后台向量化一条知识：脱离请求 ctx（用独立 background+超时），
// 请求返回后仍能跑完；不阻塞用户可感知的返回路径。
func (svc *Service) embedAsync(k *store.Knowledge) {
	if svc.embedder == nil {
		return
	}
	select {
	case svc.embedSem <- struct{}{}:
	default:
		// 保存已成功；周期回填会补齐被限流的向量，避免请求洪峰制造无界 goroutine。
		slog.Debug("知识异步向量化并发已满，交由回填补齐", "id", k.ID)
		return
	}
	go func() {
		defer func() {
			<-svc.embedSem
			if r := recover(); r != nil {
				slog.Error("知识异步向量化 panic 已恢复", "id", k.ID, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), embedTimeout)
		defer cancel()
		svc.embedOne(ctx, k)
	}()
}

// embedOne 同步向量化一条知识并落库。返回是否成功落库（回填据此计成功数，
// 避免失败行被反复重选形成死循环）。embed_model 存「模型:维度」标签——维度随
// 外部服务变更时标签变，旧向量既不会污染检索、也会被回填识别为需重嵌入。
func (svc *Service) embedOne(ctx context.Context, k *store.Knowledge) bool {
	if svc.embedder == nil {
		return false
	}
	if svc.semantic != nil {
		_, err := svc.semantic.UpsertDocuments(ctx, []semantic.Document{knowledgeDocument(k)})
		if err != nil {
			slog.Warn("知识向量写入 Qdrant 失败", "id", k.ID, "err", err)
			return false
		}
		tag, _, err := svc.semantic.CurrentModel(ctx)
		if err != nil {
			return false
		}
		if err := svc.store.MarkKnowledgeVectorIndexed(ctx, k.ID, tag); err != nil {
			slog.Warn("知识 Qdrant 索引标记落库失败", "id", k.ID, "err", err)
			return false
		}
		return true
	}
	vecs, err := svc.embedder.Embed(ctx, []string{embedText(k.Title, k.Content)})
	if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
		slog.Warn("知识向量化失败（内容已存，回填时补）", "id", k.ID, "err", err)
		return false
	}
	tag := modelTag(svc.embedder.Model(), len(vecs[0]))
	if err := svc.store.SetKnowledgeEmbedding(ctx, k.ID, tag, vecs[0]); err != nil {
		slog.Warn("知识向量落库失败", "id", k.ID, "err", err)
		return false
	}
	return true
}

// modelTag 把模型名与向量维度合成检索/回填的标识。维度并入标签是维度变更自愈的
// 关键：换了不同维度的 embedding 实现（模型名不变）后标签自动不同。
func modelTag(model string, dim int) string {
	return fmt.Sprintf("%s:%d", model, dim)
}

// Search 混合检索：有 embedder 则语义 cosine + 词法融合，否则纯词法。
func (svc *Service) Search(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	return svc.search(ctx, query, limit, svc.store.SearchKnowledge, svc.store.EmbeddedKnowledge,
		vectorstore.Filter{Must: map[string]any{
			vectorstore.PayloadSource: semantic.SourceKnowledge,
			vectorstore.PayloadKind:   store.KnowledgeKindFact,
		}})
}

// SaveRule 存一条行为规则（kind=policy）并异步 embedding，路径同 Save。
func (svc *Service) SaveRule(ctx context.Context, title, content string, tags []string, authorID int64, pinned bool) (*store.Knowledge, error) {
	k, _, err := svc.SaveRuleWithStatus(ctx, title, content, tags, authorID, pinned)
	return k, err
}

// SaveRuleWithStatus exposes whether the logical policy was created, updated,
// or already identical so callers can report the durable outcome accurately.
func (svc *Service) SaveRuleWithStatus(ctx context.Context, title, content string, tags []string, authorID int64, pinned bool) (*store.Knowledge, store.RuleWriteStatus, error) {
	k, status, err := svc.store.UpsertRule(ctx, title, content, tags, authorID, pinned)
	if err != nil {
		return nil, "", err
	}
	if status != store.RuleUnchanged {
		svc.embedAsync(k)
	}
	return k, status, nil
}

// SaveSkill 存一条执行方法并异步 embedding。
func (svc *Service) SaveSkill(ctx context.Context, title, content string, tags []string, authorID int64) (*store.Knowledge, error) {
	k, err := svc.store.CreateSkill(ctx, title, content, tags, authorID)
	if err != nil {
		return nil, err
	}
	svc.embedAsync(k)
	return k, nil
}

// SearchRules 在非常驻规则内做混合检索（常驻规则已在系统提示，不重复召回）。
func (svc *Service) SearchRules(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	if limit <= 0 {
		limit = 5
	}
	lexical, err := svc.store.SearchRules(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if svc.embedder == nil {
		return lexical, nil
	}
	ranked, serr := svc.semanticRank(ctx, query, limit, svc.store.EmbeddedRules, ruleSemanticMinScore,
		vectorstore.Filter{
			Must: map[string]any{
				vectorstore.PayloadSource: semantic.SourceKnowledge,
				vectorstore.PayloadKind:   store.KnowledgeKindPolicy,
			},
			MustNot: map[string]any{"pinned": true},
		})
	if serr != nil {
		slog.Warn("规则语义检索失败，回退词法", "err", serr)
		return lexical, nil
	}
	return merge(ranked, lexical, limit), nil
}

// SearchSkills 在执行方法库里做混合检索。
func (svc *Service) SearchSkills(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	if limit <= 0 {
		limit = 5
	}
	lexical, err := svc.store.SearchSkills(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if svc.embedder == nil {
		return lexical, nil
	}
	ranked, serr := svc.semanticRank(ctx, query, limit, svc.store.EmbeddedSkills, skillSemanticMinScore,
		vectorstore.Filter{Must: map[string]any{
			vectorstore.PayloadSource: semantic.SourceKnowledge,
			vectorstore.PayloadKind:   store.KnowledgeKindSkill,
		}})
	if serr != nil {
		slog.Warn("skill 语义检索失败，回退词法", "err", serr)
		return lexical, nil
	}
	return merge(ranked, lexical, limit), nil
}

// RuleApplies 判断规则（按其 tags 里的 scope: 标签）是否适用于当前场景。
// channel 传对话渠道（telegram / telegram:group:<id> / api），worker 领活场景传 "worker"。
// 没有任何 scope: 标签视同 scope:global。多个 scope 标签任一命中即适用。
func RuleApplies(tags []string, channel string, userID int64) bool {
	scoped := false
	for _, t := range tags {
		v, ok := strings.CutPrefix(t, "scope:")
		if !ok {
			continue
		}
		scoped = true
		switch {
		case v == "global":
			return true
		case v == channel || strings.HasPrefix(channel, v+":"):
			// scope:telegram 覆盖 telegram 及其派生渠道（telegram:group:<id>）。
			return true
		case v == "user:"+strconv.FormatInt(userID, 10):
			return true
		}
	}
	return !scoped
}

// SearchByAuthor 在指定作者的知识内做混合检索，用于 worker 个人经验。
func (svc *Service) SearchByAuthor(ctx context.Context, authorID int64, query string, limit int) ([]*store.Knowledge, error) {
	return svc.search(ctx, query, limit,
		func(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
			return svc.store.SearchKnowledgeByAuthor(ctx, authorID, query, limit)
		},
		func(ctx context.Context, model string) ([]store.KnowledgeVec, error) {
			return svc.store.EmbeddedKnowledgeByAuthor(ctx, model, authorID)
		}, vectorstore.Filter{Must: map[string]any{
			vectorstore.PayloadSource: semantic.SourceKnowledge,
			vectorstore.PayloadKind:   store.KnowledgeKindFact,
			"author_id":               authorID,
		}})
}

// SearchByTag 在指定标签的知识内做混合检索，用于项目/worker 经验。
func (svc *Service) SearchByTag(ctx context.Context, tag, query string, limit int) ([]*store.Knowledge, error) {
	return svc.search(ctx, query, limit,
		func(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
			return svc.store.SearchKnowledgeByTag(ctx, tag, query, limit)
		},
		func(ctx context.Context, model string) ([]store.KnowledgeVec, error) {
			return svc.store.EmbeddedKnowledgeByTag(ctx, model, tag)
		}, vectorstore.Filter{Must: map[string]any{
			vectorstore.PayloadSource: semantic.SourceKnowledge,
			vectorstore.PayloadKind:   store.KnowledgeKindFact,
			"tags":                    tag,
		}})
}

func (svc *Service) search(
	ctx context.Context,
	query string,
	limit int,
	lexicalSearch func(context.Context, string, int) ([]*store.Knowledge, error),
	semanticCandidates func(context.Context, string) ([]store.KnowledgeVec, error),
	externalFilter vectorstore.Filter,
) ([]*store.Knowledge, error) {
	if limit <= 0 {
		limit = 5
	}
	lexical, err := lexicalSearch(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if svc.embedder == nil {
		return lexical, nil
	}
	ranked, serr := svc.semanticRank(ctx, query, limit, semanticCandidates, 0, externalFilter)
	if serr != nil {
		slog.Warn("语义检索失败，回退词法", "err", serr)
		return lexical, nil
	}
	return merge(ranked, lexical, limit), nil
}

// semantic 查询向量化 → 与「同模型同维度」的已嵌入知识做 cosine → 取 topN。
// 按 modelTag（含维度）取候选：维度变更后旧向量自动排除，绝不用零余弦污染结果。
func (svc *Service) semanticRank(ctx context.Context, query string, limit int, candidates func(context.Context, string) ([]store.KnowledgeVec, error), minScore float32, externalFilter vectorstore.Filter) ([]*store.Knowledge, error) {
	qv, err := svc.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	if svc.semantic != nil {
		hits, err := svc.semantic.SearchVector(ctx, qv, externalFilter, limit*semanticCandidateMul, minScore)
		if err != nil {
			return nil, err
		}
		ids := make([]int64, 0, len(hits))
		for _, hit := range hits {
			id, err := strconv.ParseInt(hit.EntityID, 10, 64)
			if err == nil && id > 0 {
				ids = append(ids, id)
			}
		}
		rows, err := svc.store.KnowledgeByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		return filterAuthoritativeKnowledge(rows, externalFilter), nil
	}
	tag := modelTag(svc.embedder.Model(), len(qv))
	cands, err := candidates(ctx, tag)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}
	type scored struct {
		id  int64
		sim float32
	}
	arr := make([]scored, 0, len(cands))
	for _, c := range cands {
		sim := ai.Cosine(qv, c.Embedding)
		if minScore > 0 && sim < minScore {
			continue
		}
		arr = append(arr, scored{c.ID, sim})
	}
	if len(arr) == 0 {
		return nil, nil
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].sim > arr[j].sim })
	n := limit * semanticCandidateMul
	if n > len(arr) {
		n = len(arr)
	}
	ids := make([]int64, 0, n)
	for _, s := range arr[:n] {
		ids = append(ids, s.id)
	}
	return svc.store.KnowledgeByIDs(ctx, ids)
}

// Qdrant metadata is a routing hint, never the authority for mutable scope
// fields. Re-check every payload predicate represented by the PostgreSQL row
// so tag/pinned changes take effect before asynchronous reindexing completes.
func filterAuthoritativeKnowledge(rows []*store.Knowledge, filter vectorstore.Filter) []*store.Knowledge {
	out := make([]*store.Knowledge, 0, len(rows))
	for _, row := range rows {
		if row != nil && row.Active && knowledgeMatchesPayload(row, filter.Must) && !knowledgeMatchesAnyPayload(row, filter.MustNot) {
			out = append(out, row)
		}
	}
	return out
}

func knowledgeMatchesPayload(row *store.Knowledge, predicates map[string]any) bool {
	for key, value := range predicates {
		if key == vectorstore.PayloadSource {
			if source, ok := value.(string); !ok || source != semantic.SourceKnowledge {
				return false
			}
			continue
		}
		if !knowledgePayloadValueMatches(row, key, value) {
			return false
		}
	}
	return true
}

func knowledgeMatchesAnyPayload(row *store.Knowledge, predicates map[string]any) bool {
	for key, value := range predicates {
		if knowledgePayloadValueMatches(row, key, value) {
			return true
		}
	}
	return false
}

func knowledgePayloadValueMatches(row *store.Knowledge, key string, value any) bool {
	switch key {
	case vectorstore.PayloadKind:
		kind, ok := value.(string)
		return ok && row.Kind == kind
	case "author_id":
		switch author := value.(type) {
		case int64:
			return row.AuthorID == author
		case int:
			return row.AuthorID == int64(author)
		default:
			return false
		}
	case "tags":
		switch tags := value.(type) {
		case string:
			return slices.Contains(row.Tags, tags)
		case []string:
			return slices.ContainsFunc(tags, func(tag string) bool { return slices.Contains(row.Tags, tag) })
		default:
			return false
		}
	case "pinned":
		pinned, ok := value.(bool)
		return ok && row.Pinned == pinned
	default:
		return false
	}
}

// merge uses Reciprocal Rank Fusion so exact lexical matches and semantic
// paraphrases reinforce each other without comparing incompatible raw scores.
func merge(primary, secondary []*store.Knowledge, limit int) []*store.Knowledge {
	type fused struct {
		item     *store.Knowledge
		score    float64
		bestRank int
	}
	byID := make(map[int64]*fused, len(primary)+len(secondary))
	for _, list := range [][]*store.Knowledge{primary, secondary} {
		for rank, item := range list {
			entry := byID[item.ID]
			if entry == nil {
				entry = &fused{item: item, bestRank: rank}
				byID[item.ID] = entry
			}
			entry.score += 1 / float64(60+rank+1)
			entry.bestRank = min(entry.bestRank, rank)
		}
	}
	items := make([]*fused, 0, len(byID))
	for _, item := range byID {
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		if items[i].bestRank != items[j].bestRank {
			return items[i].bestRank < items[j].bestRank
		}
		return items[i].item.ID > items[j].item.ID
	})
	if limit > len(items) {
		limit = len(items)
	}
	out := make([]*store.Knowledge, 0, max(0, limit))
	for _, item := range items[:max(0, limit)] {
		out = append(out, item.item)
	}
	return out
}

// BackfillResult 是一次回填扫描批次的结果。
type BackfillResult struct {
	Attempted int
	Embedded  int
	LastID    int64
	HasMore   bool
}

// Backfill 给尚未按「当前模型:维度」嵌入的知识补 embedding（启动后台跑）。
// afterID 是本轮扫描游标。失败行不会更新 embedding，但 LastID 仍会推进到本批
// 最后一行，避免内容级失败行挡住后面的知识。下次进程重启会重新扫描并重试。
// 先探一次当前维度确定标签：探测失败=服务不可用，直接返回错误让驱动停手。
func (svc *Service) Backfill(ctx context.Context, batch int, afterID int64) (BackfillResult, error) {
	if svc.embedder == nil {
		return BackfillResult{}, nil
	}
	if svc.semantic != nil {
		return svc.backfillQdrantKnowledge(ctx, batch, afterID, false)
	}
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	probe, perr := svc.embedder.Embed(ectx, []string{"nbco"})
	cancel()
	if perr != nil {
		return BackfillResult{}, fmt.Errorf("embedding 服务探测失败: %w", perr)
	}
	if len(probe) != 1 || len(probe[0]) == 0 {
		return BackfillResult{}, fmt.Errorf("embedding 服务探测返回无效向量")
	}
	tag := modelTag(svc.embedder.Model(), len(probe[0]))
	ks, err := svc.store.KnowledgeNeedingEmbeddingAfter(ctx, tag, afterID, batch)
	if err != nil {
		return BackfillResult{}, err
	}
	res := BackfillResult{HasMore: len(ks) == batch}
	for _, k := range ks {
		if ctx.Err() != nil {
			break
		}
		res.LastID = k.ID
		ectx, cancel := context.WithTimeout(ctx, embedTimeout)
		ok := svc.embedOne(ectx, k)
		cancel()
		res.Attempted++
		if ok {
			res.Embedded++
		}
	}
	return res, nil
}

// ReconcileKnowledge ignores PostgreSQL completion markers and verifies the
// external derived index from the authoritative rows.
func (svc *Service) ReconcileKnowledge(ctx context.Context, batch int, afterID int64) (BackfillResult, error) {
	if svc.semantic == nil {
		return svc.Backfill(ctx, batch, afterID)
	}
	return svc.backfillQdrantKnowledge(ctx, batch, afterID, true)
}

func (svc *Service) backfillQdrantKnowledge(ctx context.Context, batch int, afterID int64, reconcile bool) (BackfillResult, error) {
	tag, _, err := svc.semantic.CurrentModel(ctx)
	if err != nil {
		return BackfillResult{}, err
	}
	var ks []*store.Knowledge
	if reconcile {
		ks, err = svc.store.KnowledgeAfter(ctx, afterID, batch)
	} else {
		ks, err = svc.store.KnowledgeNeedingExternalIndexAfter(ctx, tag, afterID, batch)
	}
	if err != nil {
		return BackfillResult{}, err
	}
	res := BackfillResult{Attempted: len(ks), HasMore: len(ks) == batch}
	if len(ks) == 0 {
		return res, nil
	}
	res.LastID = ks[len(ks)-1].ID
	docs := make([]semantic.Document, 0, len(ks))
	for _, k := range ks {
		docs = append(docs, knowledgeDocument(k))
	}
	report, err := svc.semantic.UpsertDocumentsDetailed(ctx, docs)
	if err != nil {
		return res, err
	}
	for _, k := range ks {
		ref := vectorstore.Ref{Source: semantic.SourceKnowledge, EntityID: strconv.FormatInt(k.ID, 10)}
		if !report.Succeeded[ref.Key()] {
			continue
		}
		if err := svc.store.MarkKnowledgeVectorIndexed(ctx, k.ID, tag); err != nil {
			return res, err
		}
	}
	res.Embedded = report.Indexed
	return res, nil
}

// CleanupKnowledgeIndex removes Qdrant points whose PostgreSQL source row was
// deleted. Search already re-reads PostgreSQL, so this is hygiene rather than a
// permission boundary.
func (svc *Service) CleanupKnowledgeIndex(ctx context.Context) error {
	if svc.semantic == nil {
		return nil
	}
	ids, err := svc.store.KnowledgeIDs(ctx)
	if err != nil {
		return err
	}
	valid := make(map[string]bool, len(ids))
	for _, id := range ids {
		ref := vectorstore.Ref{Source: semantic.SourceKnowledge, EntityID: strconv.FormatInt(id, 10)}
		valid[ref.Key()] = true
	}
	_, err = svc.semantic.DeleteMissing(ctx, semantic.SourceKnowledge, valid)
	return err
}

// ClearLegacyKnowledgeVectors drops PostgreSQL vector payloads only when
// Qdrant is the active semantic index.
func (svc *Service) ClearLegacyKnowledgeVectors(ctx context.Context) error {
	if svc.semantic == nil {
		return nil
	}
	return svc.store.ClearLegacyKnowledgeEmbeddings(ctx)
}

// embedText 拼向量化文本：标题权重高（重复一次），加正文。
func embedText(title, content string) string {
	return title + "\n" + title + "\n" + content
}

func knowledgeDocument(k *store.Knowledge) semantic.Document {
	return semantic.Document{
		Ref: vectorstore.Ref{
			Source: semantic.SourceKnowledge, EntityID: strconv.FormatInt(k.ID, 10),
		},
		Content: embedText(k.Title, k.Content),
		Payload: map[string]any{
			vectorstore.PayloadKind: k.Kind,
			"author_id":             k.AuthorID,
			"tags":                  k.Tags,
			"pinned":                k.Pinned,
		},
	}
}

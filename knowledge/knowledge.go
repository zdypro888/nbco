// Package knowledge 是知识库的检索/沉淀服务层：在 store 之上叠加语义检索。
// 配了 embedder 就走「语义（cosine）+ 词法」混合召回；没配或 embedder 失败就
// 优雅回退到纯词法。中枢对话工具、worker 领活时的经验注入都经它，行为一致。
package knowledge

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

// semanticCandidateMul 语义召回时，先取 limit×该倍数 的候选再与词法融合排序。
const semanticCandidateMul = 3

// embedTimeout embed-on-save / 查询向量化的超时（本地服务通常很快；超时就回退）。
const embedTimeout = 20 * time.Second

// Service 知识库服务。Embedder 可为 nil（未启用语义检索）。
type Service struct {
	store    *store.Store
	embedder ai.Embedder
}

func New(s *store.Store, embedder ai.Embedder) *Service {
	return &Service{store: s, embedder: embedder}
}

// Save 存一条知识，并（best-effort）落 embedding。embedding 失败不阻断保存——
// 内容照存，向量留空，回填 pass 之后会补上。
func (svc *Service) Save(ctx context.Context, title, content string, tags []string, authorID int64) (*store.Knowledge, error) {
	k, err := svc.store.CreateKnowledge(ctx, title, content, tags, authorID)
	if err != nil {
		return nil, err
	}
	svc.embedOne(ctx, k)
	return k, nil
}

// Reembed 内容更新后重算 embedding（best-effort）。
func (svc *Service) Reembed(ctx context.Context, k *store.Knowledge) {
	svc.embedOne(ctx, k)
}

func (svc *Service) embedOne(ctx context.Context, k *store.Knowledge) {
	if svc.embedder == nil {
		return
	}
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	vecs, err := svc.embedder.Embed(ectx, []string{embedText(k.Title, k.Content)})
	if err != nil || len(vecs) != 1 {
		slog.Warn("知识向量化失败（内容已存，回填时补）", "id", k.ID, "err", err)
		return
	}
	if err := svc.store.SetKnowledgeEmbedding(ctx, k.ID, svc.embedder.Model(), vecs[0]); err != nil {
		slog.Warn("知识向量落库失败", "id", k.ID, "err", err)
	}
}

// Search 混合检索：有 embedder 则语义 cosine + 词法融合，否则纯词法。
func (svc *Service) Search(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	if limit <= 0 {
		limit = 5
	}
	lexical, err := svc.store.SearchKnowledge(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	if svc.embedder == nil {
		return lexical, nil
	}
	ranked, serr := svc.semantic(ctx, query, limit)
	if serr != nil {
		slog.Warn("语义检索失败，回退词法", "err", serr)
		return lexical, nil
	}
	return merge(ranked, lexical, limit), nil
}

// semantic 查询向量化 → 与全部已嵌入知识做 cosine → 取 topN。
func (svc *Service) semantic(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	qv, err := svc.embedder.Embed(ectx, []string{query})
	if err != nil || len(qv) != 1 {
		return nil, err
	}
	cands, err := svc.store.EmbeddedKnowledge(ctx, svc.embedder.Model())
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
		arr = append(arr, scored{c.ID, ai.Cosine(qv[0], c.Embedding)})
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

// merge 语义结果在前、词法结果补足，按 id 去重，截到 limit。
func merge(primary, secondary []*store.Knowledge, limit int) []*store.Knowledge {
	seen := map[int64]bool{}
	out := make([]*store.Knowledge, 0, limit)
	for _, list := range [][]*store.Knowledge{primary, secondary} {
		for _, k := range list {
			if seen[k.ID] || len(out) >= limit {
				continue
			}
			seen[k.ID] = true
			out = append(out, k)
		}
	}
	return out
}

// Backfill 给尚未按当前模型嵌入的知识补 embedding（启动时后台跑一趟）。
// batch 控制单次处理量，避免一次拉爆；返回处理条数。
func (svc *Service) Backfill(ctx context.Context, batch int) (int, error) {
	if svc.embedder == nil {
		return 0, nil
	}
	ks, err := svc.store.KnowledgeNeedingEmbedding(ctx, svc.embedder.Model(), batch)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, k := range ks {
		if ctx.Err() != nil {
			break
		}
		svc.embedOne(ctx, k)
		done++
	}
	return done, nil
}

// embedText 拼向量化文本：标题权重高（重复一次），加正文。
func embedText(title, content string) string {
	return title + "\n" + title + "\n" + content
}

// Package knowledge 是知识库的检索/沉淀服务层：在 store 之上叠加语义检索。
// 配了 embedder 就走「语义（cosine）+ 词法」混合召回；没配或 embedder 失败就
// 优雅回退到纯词法。中枢对话工具、worker 领活时的经验注入都经它，行为一致。
package knowledge

import (
	"context"
	"fmt"
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
	go func() {
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

// semantic 查询向量化 → 与「同模型同维度」的已嵌入知识做 cosine → 取 topN。
// 按 modelTag（含维度）取候选：维度变更后旧向量自动排除，绝不用零余弦污染结果。
func (svc *Service) semantic(ctx context.Context, query string, limit int) ([]*store.Knowledge, error) {
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	qv, err := svc.embedder.Embed(ectx, []string{query})
	if err != nil || len(qv) != 1 || len(qv[0]) == 0 {
		return nil, err
	}
	tag := modelTag(svc.embedder.Model(), len(qv[0]))
	cands, err := svc.store.EmbeddedKnowledge(ctx, tag)
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

// Backfill 给尚未按「当前模型:维度」嵌入的知识补 embedding（启动后台跑）。
// 返回 (attempted 本批尝试数, embedded 本批成功落库数)。成功数用于让驱动循环
// 在「零推进」时收敛退出，杜绝 embedding 服务失败时的死循环空转。
// 先探一次当前维度确定标签：探测失败=服务不可用，直接返回错误让驱动停手。
func (svc *Service) Backfill(ctx context.Context, batch int) (attempted, embedded int, err error) {
	if svc.embedder == nil {
		return 0, 0, nil
	}
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	probe, perr := svc.embedder.Embed(ectx, []string{"nbco"})
	cancel()
	if perr != nil || len(probe) != 1 || len(probe[0]) == 0 {
		return 0, 0, fmt.Errorf("embedding 服务探测失败: %w", perr)
	}
	tag := modelTag(svc.embedder.Model(), len(probe[0]))
	ks, err := svc.store.KnowledgeNeedingEmbedding(ctx, tag, batch)
	if err != nil {
		return 0, 0, err
	}
	for _, k := range ks {
		if ctx.Err() != nil {
			break
		}
		ectx, cancel := context.WithTimeout(ctx, embedTimeout)
		ok := svc.embedOne(ectx, k)
		cancel()
		attempted++
		if ok {
			embedded++
		}
	}
	return attempted, embedded, nil
}

// embedText 拼向量化文本：标题权重高（重复一次），加正文。
func embedText(title, content string) string {
	return title + "\n" + title + "\n" + content
}

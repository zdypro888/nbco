// 会话情景记忆（episodic memory）：跨会话的消息级语义检索。
// 与知识库同一套「语义 + 词法」混合召回；只搜提问者名下的会话，不跨权限。
package knowledge

import (
	"context"
	"log/slog"
	"sort"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

// EmbedMessageAsync 异步给一条消息补 embedding（追加消息的热路径钩子，
// 绝不阻塞对话；失败由启动回填兜底）。短消息无记忆价值，直接跳过。
func (svc *Service) EmbedMessageAsync(id int64, content string) {
	if svc.embedder == nil || id == 0 || len(content) < 8 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), embedTimeout)
		defer cancel()
		vecs, err := svc.embedder.Embed(ctx, []string{content})
		if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
			slog.Debug("消息向量化失败（回填兜底）", "id", id, "err", err)
			return
		}
		tag := modelTag(svc.embedder.Model(), len(vecs[0]))
		if err := svc.store.SetMessageEmbedding(ctx, id, tag, vecs[0]); err != nil {
			slog.Debug("消息向量落库失败", "id", id, "err", err)
		}
	}()
}

// SearchHistory 在 userID 名下的历史消息里做混合检索（新旧会话都搜）。
func (svc *Service) SearchHistory(ctx context.Context, userID int64, query string, limit int) ([]store.ChatMessage, error) {
	if limit <= 0 {
		limit = 5
	}
	lexical, err := svc.store.SearchMessagesOfUser(ctx, userID, query, limit)
	if err != nil {
		return nil, err
	}
	if svc.embedder == nil {
		return lexical, nil
	}
	ranked, serr := svc.semanticHistory(ctx, userID, query, limit)
	if serr != nil {
		slog.Warn("历史语义检索失败，回退词法", "err", serr)
		return lexical, nil
	}
	return mergeMessages(ranked, lexical, limit), nil
}

func (svc *Service) semanticHistory(ctx context.Context, userID int64, query string, limit int) ([]store.ChatMessage, error) {
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	qv, err := svc.embedder.Embed(ectx, []string{query})
	if err != nil || len(qv) != 1 || len(qv[0]) == 0 {
		return nil, err
	}
	tag := modelTag(svc.embedder.Model(), len(qv[0]))
	cands, err := svc.store.EmbeddedMessagesOfUser(ctx, tag, userID)
	if err != nil || len(cands) == 0 {
		return nil, err
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
	return svc.store.MessagesByIDs(ctx, ids)
}

func mergeMessages(primary, secondary []store.ChatMessage, limit int) []store.ChatMessage {
	seen := map[int64]bool{}
	out := make([]store.ChatMessage, 0, limit)
	for _, list := range [][]store.ChatMessage{primary, secondary} {
		for _, m := range list {
			if seen[m.ID] || len(out) >= limit {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	return out
}

// BackfillMessages 给存量消息补 embedding（启动后台跑，模式同知识回填）。
func (svc *Service) BackfillMessages(ctx context.Context, batch int, afterID int64) (BackfillResult, error) {
	if svc.embedder == nil {
		return BackfillResult{}, nil
	}
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	probe, perr := svc.embedder.Embed(ectx, []string{"nbco"})
	cancel()
	if perr != nil || len(probe) != 1 || len(probe[0]) == 0 {
		return BackfillResult{}, perr
	}
	tag := modelTag(svc.embedder.Model(), len(probe[0]))
	ms, err := svc.store.MessagesNeedingEmbeddingAfter(ctx, tag, afterID, batch)
	if err != nil {
		return BackfillResult{}, err
	}
	res := BackfillResult{HasMore: len(ms) == batch}
	for _, m := range ms {
		if ctx.Err() != nil {
			break
		}
		res.LastID = m.ID
		res.Attempted++
		ectx, cancel := context.WithTimeout(ctx, embedTimeout)
		vecs, err := svc.embedder.Embed(ectx, []string{m.Content})
		if err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			if svc.store.SetMessageEmbedding(ectx, m.ID, tag, vecs[0]) == nil {
				res.Embedded++
			}
		}
		cancel()
	}
	return res, nil
}

// 会话情景记忆（episodic memory）：跨会话的消息级语义检索。
// 与知识库同一套「语义 + 词法」混合召回；只搜提问者名下的会话，不跨权限。
package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/semantic"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/vectorstore"
)

// embedSem 消息嵌入的并发上限：群旁听刷屏时每条消息一个 embed 请求，不设限
// 会瞬间上百并发打垮本地 embed 服务（连带规则检索超时降级）。满载直接丢弃，
// 缺的向量由周期回填补齐——嵌入是增强，绝不能反噬主链路。
var embedSem = make(chan struct{}, 4)

const messageIndexRevision = "context-v3"

// EmbedMessageAsync 异步给一条非空消息补 embedding（追加消息的热路径钩子，
// 绝不阻塞对话；失败由周期回填兜底）。短确认同样可能是流程证据，不能丢弃。
func (svc *Service) EmbedMessageAsync(id int64, content string) {
	if svc.embedder == nil || id == 0 || strings.TrimSpace(content) == "" {
		return
	}
	select {
	case embedSem <- struct{}{}:
	default:
		slog.Debug("嵌入并发已满，消息向量留待回填", "id", id)
		return
	}
	go func() {
		defer func() {
			<-embedSem
			if r := recover(); r != nil {
				slog.Error("消息异步向量化 panic 已恢复", "id", id, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), embedTimeout)
		defer cancel()
		doc, err := svc.store.MessageSemanticDocumentByID(ctx, id)
		if err != nil {
			slog.Debug("读取消息语义上下文失败，留待回填", "id", id, "err", err)
			return
		}
		if svc.semantic != nil {
			if _, err := svc.semantic.UpsertDocuments(ctx, []semantic.Document{messageDocument(*doc)}); err != nil {
				slog.Debug("消息向量写入 Qdrant 失败", "id", id, "err", err)
				return
			}
			tag, _, err := svc.semantic.CurrentModel(ctx)
			if err == nil {
				err = svc.store.MarkMessageVectorIndexed(ctx, id, messageIndexMarker(tag))
			}
			if err != nil {
				slog.Debug("消息 Qdrant 索引标记失败", "id", id, "err", err)
			}
			return
		}
		vecs, err := svc.embedder.Embed(ctx, []string{semanticProjectionText(doc.ContextContent())})
		if err != nil || len(vecs) != 1 || len(vecs[0]) == 0 {
			slog.Debug("消息向量化失败（回填兜底）", "id", id, "err", err)
			return
		}
		tag := messageIndexMarker(modelTag(svc.embedder.Model(), len(vecs[0])))
		if err := svc.store.SetMessageEmbedding(ctx, id, tag, vecs[0]); err != nil {
			slog.Debug("消息向量落库失败", "id", id, "err", err)
		}
	}()
}

// SearchHistory 在 userID 名下的历史消息里做混合检索（新旧会话都搜）。
func (svc *Service) SearchHistory(ctx context.Context, userID int64, query string, limit int) ([]store.ChatMessage, error) {
	return svc.searchHistory(ctx, userID, query, limit, false)
}

// SearchUserHistory is the conservative automatic-recall path. Every valid
// transcript row remains indexed and explicitly searchable, but unattended
// context injection starts from human statements rather than prior model text.
func (svc *Service) SearchUserHistory(ctx context.Context, userID int64, query string, limit int) ([]store.ChatMessage, error) {
	return svc.searchHistory(ctx, userID, query, limit, true)
}

func (svc *Service) searchHistory(ctx context.Context, userID int64, query string, limit int, userOnly bool) ([]store.ChatMessage, error) {
	if limit <= 0 {
		limit = 5
	}
	var lexical []store.ChatMessage
	var err error
	if userOnly {
		lexical, err = svc.store.SearchUserMessagesOfUser(ctx, userID, query, limit)
	} else {
		lexical, err = svc.store.SearchMessagesOfUser(ctx, userID, query, limit)
	}
	if err != nil {
		return nil, err
	}
	if svc.embedder == nil {
		return lexical, nil
	}
	ranked, serr := svc.semanticHistory(ctx, userID, query, limit, userOnly)
	if serr != nil {
		slog.Warn("历史语义检索失败，回退词法", "err", serr)
		return lexical, nil
	}
	return mergeMessages(ranked, lexical, limit), nil
}

// SearchGroupHistory searches only one exact shared group transcript. Private
// sessions never carry this channel scope, and SQL re-checks it after Qdrant.
func (svc *Service) SearchGroupHistory(ctx context.Context, channel, query string, limit int) ([]store.ChatMessage, error) {
	return svc.searchGroupHistory(ctx, channel, query, limit, false)
}

func (svc *Service) SearchGroupUserHistory(ctx context.Context, channel, query string, limit int) ([]store.ChatMessage, error) {
	return svc.searchGroupHistory(ctx, channel, query, limit, true)
}

func (svc *Service) searchGroupHistory(ctx context.Context, channel, query string, limit int, userOnly bool) ([]store.ChatMessage, error) {
	channel = strings.TrimSpace(channel)
	if !store.IsGroupChannel(channel) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	var lexical []store.ChatMessage
	var err error
	if userOnly {
		lexical, err = svc.store.SearchUserMessagesOfChannel(ctx, channel, query, limit)
	} else {
		lexical, err = svc.store.SearchMessagesOfChannel(ctx, channel, query, limit)
	}
	if err != nil {
		return nil, err
	}
	if svc.semantic == nil {
		return lexical, nil
	}
	qv, err := svc.queryVector(ctx, query)
	if err != nil {
		return lexical, nil
	}
	must := map[string]any{
		vectorstore.PayloadSource:            semantic.SourceChatMessage,
		vectorstore.PayloadChannel:           channel,
		vectorstore.PayloadConversationScope: "group",
	}
	if userOnly {
		must[vectorstore.PayloadRole] = string(ai.RoleUser)
	}
	hits, err := svc.semantic.SearchVector(ctx, qv, vectorstore.Filter{Must: must}, limit*semanticCandidateMul, 0)
	if err != nil {
		slog.Warn("群历史语义检索失败，回退词法", "channel", channel, "err", err)
		return lexical, nil
	}
	ids := make([]int64, 0, len(hits))
	for _, hit := range hits {
		if id, parseErr := strconv.ParseInt(hit.EntityID, 10, 64); parseErr == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	var semanticRows []store.ChatMessage
	if userOnly {
		semanticRows, err = svc.store.UserMessagesByIDsForChannel(ctx, channel, ids)
	} else {
		semanticRows, err = svc.store.MessagesByIDsForChannel(ctx, channel, ids)
	}
	if err != nil {
		return lexical, nil
	}
	return mergeMessages(semanticRows, lexical, limit), nil
}

func (svc *Service) semanticHistory(ctx context.Context, userID int64, query string, limit int, userOnly bool) ([]store.ChatMessage, error) {
	qv, err := svc.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	if svc.semantic != nil {
		must := map[string]any{
			vectorstore.PayloadSource:            semantic.SourceChatMessage,
			vectorstore.PayloadSessionUser:       userID,
			vectorstore.PayloadConversationScope: "private",
		}
		if userOnly {
			must[vectorstore.PayloadRole] = string(ai.RoleUser)
		}
		hits, err := svc.semantic.SearchVector(ctx, qv, vectorstore.Filter{Must: must}, limit*semanticCandidateMul, 0)
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
		if userOnly {
			return svc.store.UserMessagesByIDsForUser(ctx, userID, ids)
		}
		return svc.store.MessagesByIDsForUser(ctx, userID, ids)
	}
	tag := messageIndexMarker(modelTag(svc.embedder.Model(), len(qv)))
	var cands []store.MessageVec
	if userOnly {
		cands, err = svc.store.EmbeddedUserMessagesOfUser(ctx, tag, userID)
	} else {
		cands, err = svc.store.EmbeddedMessagesOfUser(ctx, tag, userID)
	}
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	type scored struct {
		id  int64
		sim float32
	}
	arr := make([]scored, 0, len(cands))
	for _, c := range cands {
		arr = append(arr, scored{c.ID, ai.Cosine(qv, c.Embedding)})
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
	if userOnly {
		return svc.store.UserMessagesByIDsForUser(ctx, userID, ids)
	}
	return svc.store.MessagesByIDsForUser(ctx, userID, ids)
}

func mergeMessages(primary, secondary []store.ChatMessage, limit int) []store.ChatMessage {
	type fused struct {
		item     store.ChatMessage
		score    float64
		bestRank int
	}
	byID := make(map[int64]*fused, len(primary)+len(secondary))
	for _, list := range [][]store.ChatMessage{primary, secondary} {
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
	out := make([]store.ChatMessage, 0, max(0, limit))
	for _, item := range items[:max(0, limit)] {
		out = append(out, item.item)
	}
	return out
}

// BackfillMessages 给存量消息补 embedding（启动后台跑，模式同知识回填）。
func (svc *Service) BackfillMessages(ctx context.Context, batch int, afterID int64) (BackfillResult, error) {
	if svc.embedder == nil {
		return BackfillResult{}, nil
	}
	if svc.semantic != nil {
		return svc.backfillQdrantMessages(ctx, batch, afterID, false)
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
	tag := messageIndexMarker(modelTag(svc.embedder.Model(), len(probe[0])))
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
		vecs, err := svc.embedder.Embed(ectx, []string{semanticProjectionText(m.ContextContent())})
		if err == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			actualTag := messageIndexMarker(modelTag(svc.embedder.Model(), len(vecs[0])))
			if svc.store.SetMessageEmbedding(ectx, m.ID, actualTag, vecs[0]) == nil {
				res.Embedded++
			}
		}
		cancel()
	}
	return res, nil
}

// ReconcileMessages verifies authoritative rows even when PostgreSQL markers
// claim success, covering independent Qdrant restore or data loss.
func (svc *Service) ReconcileMessages(ctx context.Context, batch int, afterID int64) (BackfillResult, error) {
	if svc.semantic == nil {
		return svc.BackfillMessages(ctx, batch, afterID)
	}
	return svc.backfillQdrantMessages(ctx, batch, afterID, true)
}

func (svc *Service) backfillQdrantMessages(ctx context.Context, batch int, afterID int64, reconcile bool) (BackfillResult, error) {
	tag, _, err := svc.semantic.CurrentModel(ctx)
	if err != nil {
		return BackfillResult{}, err
	}
	marker := messageIndexMarker(tag)
	var messages []store.MessageSemanticDocument
	if reconcile {
		messages, err = svc.store.SemanticMessagesAfter(ctx, afterID, batch)
	} else {
		messages, err = svc.store.SemanticMessagesNeedingIndexAfter(ctx, marker, afterID, batch)
	}
	if err != nil {
		return BackfillResult{}, err
	}
	res := BackfillResult{Attempted: len(messages), HasMore: len(messages) == batch}
	if len(messages) == 0 {
		return res, nil
	}
	res.LastID = messages[len(messages)-1].ID
	docs := make([]semantic.Document, 0, len(messages))
	for _, message := range messages {
		docs = append(docs, messageDocument(message))
	}
	report, err := svc.semantic.UpsertDocumentsDetailed(ctx, docs)
	if err != nil {
		return res, err
	}
	for _, message := range messages {
		ref := vectorstore.Ref{Source: semantic.SourceChatMessage, EntityID: strconv.FormatInt(message.ID, 10)}
		if !report.Succeeded[ref.Key()] {
			continue
		}
		if err := svc.store.MarkMessageVectorIndexed(ctx, message.ID, marker); err != nil {
			return res, err
		}
	}
	res.Embedded = report.Indexed
	return res, nil
}

func (svc *Service) MessageIndexStats(ctx context.Context) (store.ChatMessageIndexStats, error) {
	if svc.semantic == nil {
		return store.ChatMessageIndexStats{}, nil
	}
	tag, _, err := svc.semantic.CurrentModel(ctx)
	if err != nil {
		return store.ChatMessageIndexStats{}, err
	}
	return svc.store.ChatMessageIndexStats(ctx, messageIndexMarker(tag))
}

func messageIndexMarker(modelTag string) string {
	return strings.TrimSpace(modelTag) + "@" + messageIndexRevision
}

func (svc *Service) CleanupMessageIndex(ctx context.Context) error {
	_, err := svc.CleanupMessageIndexDetailed(ctx)
	return err
}

func (svc *Service) CleanupMessageIndexDetailed(ctx context.Context) (int, error) {
	return svc.maintainMessageIndex(ctx, false)
}

func (svc *Service) InspectMessageIndex(ctx context.Context) (int, error) {
	return svc.maintainMessageIndex(ctx, true)
}

func (svc *Service) maintainMessageIndex(ctx context.Context, dryRun bool) (int, error) {
	if svc.semantic == nil {
		return 0, nil
	}
	ids, err := svc.store.SemanticMessageIDs(ctx)
	if err != nil {
		return 0, err
	}
	valid := make(map[string]bool, len(ids))
	for _, id := range ids {
		ref := vectorstore.Ref{Source: semantic.SourceChatMessage, EntityID: strconv.FormatInt(id, 10)}
		valid[ref.Key()] = true
	}
	if dryRun {
		return svc.semantic.CountMissing(ctx, semantic.SourceChatMessage, valid)
	}
	return svc.semantic.DeleteMissing(ctx, semantic.SourceChatMessage, valid)
}

// ClearLegacyMessageVectors drops PostgreSQL vector payloads only when Qdrant
// is the active semantic index. Model markers remain useful for diagnostics.
func (svc *Service) ClearLegacyMessageVectors(ctx context.Context) error {
	if svc.semantic == nil {
		return nil
	}
	return svc.store.ClearLegacyMessageEmbeddings(ctx)
}

func messageDocument(message store.MessageSemanticDocument) semantic.Document {
	content := strings.TrimSpace(message.Content)
	if previous := compactPreviousMessage(message.PreviousContent); previous != "" {
		content = fmt.Sprintf("current_%s: %s\nprevious_%s: %s",
			strings.TrimSpace(message.Role), content,
			strings.TrimSpace(message.PreviousRole), previous)
	}
	return semantic.Document{
		Ref: vectorstore.Ref{
			Source: semantic.SourceChatMessage, EntityID: strconv.FormatInt(message.ID, 10),
		},
		Content: content,
		Payload: messagePayload(message),
	}
}

func messagePayload(message store.MessageSemanticDocument) map[string]any {
	payload := map[string]any{
		vectorstore.PayloadKind:              "message",
		vectorstore.PayloadSessionUser:       message.UserID,
		vectorstore.PayloadConversationScope: "private",
		"channel":                            message.Channel,
		vectorstore.PayloadRole:              message.Role,
	}
	if store.IsGroupChannel(message.Channel) {
		payload[vectorstore.PayloadChannel] = message.Channel
		payload[vectorstore.PayloadConversationScope] = "group"
	}
	return payload
}

func compactPreviousMessage(content string) string {
	const limit = 1200
	content = strings.TrimSpace(content)
	runes := []rune(content)
	if len(runes) <= limit {
		return content
	}
	return strings.TrimSpace(string(runes[:limit/2])) + " ... " +
		strings.TrimSpace(string(runes[len(runes)-limit/2:]))
}

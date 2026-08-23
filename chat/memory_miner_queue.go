package chat

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

const memoryMinerPollInterval = 10 * time.Second

func (o *Orchestrator) wakeMemoryMiner() {
	if o == nil || o.memoryWake == nil {
		return
	}
	select {
	case o.memoryWake <- struct{}{}:
	default:
	}
}

// RunMemoryMiner drains the durable memory queue with bounded concurrency.
func (o *Orchestrator) RunMemoryMiner(ctx context.Context) {
	if o == nil || o.store == nil {
		return
	}
	ticker := time.NewTicker(memoryMinerPollInterval)
	defer ticker.Stop()
	for {
		o.dispatchMemoryMiningJobs(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-o.memoryWake:
		}
	}
}

func (o *Orchestrator) dispatchMemoryMiningJobs(ctx context.Context) {
	available := cap(o.memorySem) - len(o.memorySem)
	if available <= 0 || ctx.Err() != nil {
		return
	}
	claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	jobs, err := o.store.DueMemoryMiningJobs(claimCtx, available)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("Memory Miner 领取队列失败", "err", err)
		}
		return
	}
	for _, job := range jobs {
		o.memorySem <- struct{}{}
		go o.processMemoryMiningJob(ctx, job)
	}
}

func (o *Orchestrator) processMemoryMiningJob(parent context.Context, job *store.MemoryMiningJob) {
	defer func() {
		<-o.memorySem
		o.wakeMemoryMiner()
	}()
	if job == nil || job.ClaimedAt == nil {
		return
	}
	err := func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v", recovered)
			}
		}()
		ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
		defer cancel()
		u, err := o.store.UserByID(ctx, job.UserID)
		if err != nil {
			return fmt.Errorf("load user: %w", err)
		}
		userMessage, err := o.store.ChatMessageByID(ctx, job.UserMessageID)
		if err != nil {
			return fmt.Errorf("load user message: %w", err)
		}
		assistantMessage, err := o.store.ChatMessageByID(ctx, job.AssistantMessageID)
		if err != nil {
			return fmt.Errorf("load assistant message: %w", err)
		}
		if userMessage.SessionID != job.SessionID || assistantMessage.SessionID != job.SessionID ||
			userMessage.Role != string(ai.RoleUser) || assistantMessage.Role != string(ai.RoleAssistant) {
			return fmt.Errorf("queued message identity mismatch")
		}
		userText := userMessage.Content
		if job.UserEvidenceText != nil {
			userText = *job.UserEvidenceText
		}
		learningContext, err := o.store.LearningContextBeforeMessage(ctx, job.SessionID, job.UserMessageID, 10, 2)
		if err != nil {
			return fmt.Errorf("load learning context: %w", err)
		}
		contextText, contextMessageIDs, priorUserText, priorUserMessageIDs := renderMemoryMiningContext(learningContext.Messages, u, job.Channel, o.tz)
		priorAssets := make([]memoryContextAsset, 0, len(learningContext.Assets))
		for _, asset := range learningContext.Assets {
			priorAssets = append(priorAssets, memoryContextAsset{
				ID: asset.ID, Kind: asset.Kind,
				Title:   textfmt.TruncateRunes(memoryMiningProjection(asset.Title), 160),
				Content: textfmt.TruncateRunes(memoryMiningProjection(asset.Content), 800),
				Tags:    normalizeMemoryContextTags(asset.Tags), Phase: asset.Phase,
			})
		}
		return o.mineMemory(ctx, u, memorySource{
			Channel: job.Channel, SessionID: job.SessionID,
			UserMessageID: job.UserMessageID, AssistantMessageID: job.AssistantMessageID,
			// Canonical chat remains lossless; durable learning is a wider, derived
			// surface and receives a credential-safe projection instead.
			UserText: memoryMiningProjection(userText), AssistantText: memoryMiningProjection(assistantMessage.Content),
			ToolEvidence: job.ToolEvidence, ContextText: contextText, ContextMessageIDs: contextMessageIDs,
			PriorUserText: priorUserText, PriorUserMessageIDs: priorUserMessageIDs,
			PriorAssets: priorAssets, OccurredAt: userMessage.EventAt().In(orTimeZone(o.tz)),
			ExplicitCommit: job.ExplicitCommit,
		})
	}()

	ackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err == nil {
		if completeErr := o.store.CompleteMemoryMiningJob(ackCtx, job.ID, *job.ClaimedAt); completeErr != nil {
			slog.Warn("Memory Miner 完成状态保存失败", "job", job.ID, "err", completeErr)
		}
		return
	}
	slog.Warn("Memory Miner 任务失败，将按策略重试", "job", job.ID, "attempt", job.Attempts, "err", err)
	if retryErr := o.store.RetryMemoryMiningJob(ackCtx, job.ID, *job.ClaimedAt, job.Attempts, err.Error()); retryErr != nil {
		slog.Warn("Memory Miner 重试状态保存失败", "job", job.ID, "err", retryErr)
	}
}

func normalizeMemoryContextTags(tags []string) []string {
	out := make([]string, 0, min(len(tags), 8))
	for _, tag := range tags {
		tag = strings.TrimSpace(textfmt.TruncateRunes(memoryMiningProjection(tag), 80))
		if tag == "" {
			continue
		}
		out = append(out, tag)
		if len(out) == 8 {
			break
		}
	}
	return out
}

func memoryMiningProjection(text string) string {
	return textfmt.RedactSecrets(text)
}

func renderMemoryMiningContext(messages []store.ChatMessage, u *store.User, channel string, tz *time.Location) (string, []int64, string, []int64) {
	if len(messages) == 0 {
		return "", nil, "", nil
	}
	var b, priorUser strings.Builder
	ids := make([]int64, 0, len(messages))
	priorUserIDs := make([]int64, 0, len(messages)/2)
	for _, message := range messages {
		role := message.Role
		if role != string(ai.RoleUser) && role != string(ai.RoleAssistant) {
			continue
		}
		content := strings.TrimSpace(memoryMiningProjection(message.Content))
		if content == "" {
			continue
		}
		ids = append(ids, message.ID)
		fmt.Fprintf(&b, "[%s] %s: %s\n", messageTime(message.EventAt(), tz), role, textfmt.TruncateRunes(content, 800))
		if role == string(ai.RoleUser) && priorMessageBelongsToUser(message, u, channel) {
			priorUserIDs = append(priorUserIDs, message.ID)
			fmt.Fprintf(&priorUser, "[message:%d] %s\n", message.ID, textfmt.TruncateRunes(content, 1000))
		}
	}
	return textfmt.TruncateRunes(strings.TrimSpace(b.String()), 3200), ids,
		textfmt.TruncateRunes(strings.TrimSpace(priorUser.String()), 2400), priorUserIDs
}

func priorMessageBelongsToUser(message store.ChatMessage, u *store.User, channel string) bool {
	if u == nil || u.ID <= 0 {
		return false
	}
	if !store.IsGroupChannel(channel) {
		return true
	}
	return message.ActorUserID != nil && *message.ActorUserID == u.ID
}

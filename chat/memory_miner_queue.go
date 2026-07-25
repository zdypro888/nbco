package chat

import (
	"context"
	"fmt"
	"log/slog"
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
		return o.mineMemory(ctx, u, memorySource{
			Channel: job.Channel, SessionID: job.SessionID,
			UserMessageID: job.UserMessageID, AssistantMessageID: job.AssistantMessageID,
			// Canonical chat remains lossless; durable learning is a wider, derived
			// surface and receives a credential-safe projection instead.
			UserText:      memoryMiningProjection(userMessage.Content),
			AssistantText: memoryMiningProjection(assistantMessage.Content),
			ToolEvidence:  job.ToolEvidence, OccurredAt: userMessage.CreatedAt.In(orTimeZone(o.tz)),
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

func memoryMiningProjection(text string) string {
	return textfmt.RedactSecrets(text)
}

// Package events 是系统事件总线：领域事件（员工加入、worker 上线、任务提交等）
// 不再各自硬编码通知措辞，而是统一交给 AI 分析——以事件相关人的身份
// 跑一轮隔离、只读的引擎执行，AI 结合规则与事实决定是否通知及如何表达。
// 代码保证事件持久化、有限重试与并发受控；AI 失败时降级为原文推送，最终失败
// 留在运行账本中供诊断和人工重放，而不是静默丢失。
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/store"
)

// turnTimeout 单个事件 AI 轮次的墙钟上限（脱离触发方请求的生命周期跑）。
const turnTimeout = 4 * time.Minute

const (
	eventStateTimeout = 10 * time.Second
	eventSendTimeout  = 30 * time.Second
)

// dedupeWindow 完全相同的事件（kind+decider+detail）在该窗口内只处理一次：
// 防 worker 连接抖动等刷出大量重复事件、每个各烧一次 AI token。不同 detail
// （如不同任务的提交）互不影响。去重窗口与事件本身都持久化在数据库中。
const dedupeWindow = 5 * time.Minute

// Bus 系统事件总线。orch 为 nil 时事件直接降级为原文推送（测试或引擎未装配）。
type Bus struct {
	store    *store.Store
	orch     *chat.Orchestrator
	notifier notify.Notifier
	channel  string        // AI 结果按哪个渠道格式化并投递
	sem      chan struct{} // AI 轮次限并发，护后端网关
	wake     chan struct{}
}

// New 创建事件总线。concurrency <=0 取默认 4。
func New(st *store.Store, orch *chat.Orchestrator, n notify.Notifier, channel string, concurrency int) *Bus {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Bus{store: st, orch: orch, notifier: n, channel: channel,
		sem: make(chan struct{}, concurrency), wake: make(chan struct{}, 1)}
}

// Emit 先把事件写入持久队列再返回。decider 是该事件的决策人/利益相关者。
// 处理、AI 生成和通知由 Run 驱动；进程重启、通道满载或临时发送失败都不会丢事件。
func (b *Bus) Emit(kind string, deciderID int64, detail string) {
	_ = b.emit(kind, deciderID, detail, false)
}

// EmitRequired enqueues a critical event that must reach the user. AI still
// writes the message, but a skip/empty/error decision falls back to the factual
// event instead of silently discarding it.
func (b *Bus) EmitRequired(kind string, deciderID int64, detail string) {
	_ = b.EnqueueRequired(kind, deciderID, detail)
}

// EnqueueRequired durably accepts a critical event. It returns true when the
// event is safely represented in the queue, including when an equivalent event
// was already queued or delivered inside the deduplication window.
func (b *Bus) EnqueueRequired(kind string, deciderID int64, detail string) bool {
	return b.emit(kind, deciderID, detail, true)
}

// EnqueueRequiredOnce durably represents one caller-owned domain occurrence.
// Reclaiming the caller's lease or replaying its request returns the same event
// instead of relying on the short noise-deduplication window.
func (b *Bus) EnqueueRequiredOnce(sourceKey, kind string, deciderID int64, detail string) bool {
	return b.EnqueueOnce(sourceKey, kind, deciderID, detail, true)
}

// EnqueueOnce durably represents one caller-owned occurrence without relying
// on the short noise-deduplication window. required controls delivery policy;
// it does not change the stable identity of the occurrence.
func (b *Bus) EnqueueOnce(sourceKey, kind string, deciderID int64, detail string, required bool) bool {
	if b == nil || b.store == nil || deciderID <= 0 || strings.TrimSpace(sourceKey) == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, created, err := b.store.EnqueueEventOnceWithPolicy(ctx, sourceKey, kind, deciderID, detail, required)
	if errors.Is(err, store.ErrConflict) && id > 0 {
		// The source occurrence is already durably represented. Treat it as an
		// accepted handoff so callers never fall through to a second transport,
		// while surfacing the key/payload mismatch for diagnosis.
		slog.Warn("稳定系统事件重放内容冲突，沿用首次事件", "source", sourceKey, "kind", kind, "user", deciderID, "event", id)
		if created {
			select {
			case b.wake <- struct{}{}:
			default:
			}
		}
		return true
	}
	if err != nil {
		slog.Error("稳定系统事件入队失败", "source", sourceKey, "kind", kind, "user", deciderID, "err", err)
		return false
	}
	if created {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	return true
}

func (b *Bus) emit(kind string, deciderID int64, detail string, required bool) bool {
	if b == nil || b.store == nil || deciderID <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, created, err := b.store.EnqueueEventWithPolicy(ctx, kind, deciderID, detail, required, dedupeWindow)
	if err != nil {
		slog.Error("系统事件入队失败", "kind", kind, "user", deciderID, "err", err)
		return false
	}
	if !created {
		slog.Debug("事件去重：窗口内重复，跳过", "kind", kind, "user", deciderID)
		return true
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return true
}

// Run 持续认领并处理持久事件队列。
func (b *Bus) Run(ctx context.Context) {
	if b == nil || b.store == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		b.dispatchDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.wake:
		}
	}
}

func (b *Bus) dispatchDue(ctx context.Context) {
	available := cap(b.sem) - len(b.sem)
	if available <= 0 {
		return
	}
	items, err := b.store.DueEvents(ctx, time.Now().UTC(), available)
	if err != nil {
		slog.Error("认领系统事件失败", "err", err)
		return
	}
	for _, event := range items {
		event := event
		b.sem <- struct{}{}
		go func() {
			defer func() { <-b.sem }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("系统事件处理 panic 已恢复", "event", event.ID, "panic", r)
					b.retry(event, fmt.Sprintf("panic: %v", r))
				}
			}()
			b.handle(ctx, event)
		}()
	}
}

func (b *Bus) handle(parent context.Context, event *store.Event) {
	if event == nil || event.ClaimedAt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, turnTimeout)
	defer cancel()
	u, err := b.store.UserByID(ctx, event.DeciderID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		b.retry(event, "查询事件决策人失败: "+err.Error())
		return
	}
	if errors.Is(err, store.ErrNotFound) || u.Status != store.UserActive {
		slog.Info("事件决策人不可达，事件终止", "event", event.ID, "user", event.DeciderID, "err", err)
		if err := b.completeEvent(parent, event, store.EventOutcomeDropped); err != nil {
			slog.Warn("事件丢弃 ack 失败", "event", event.ID, "err", err)
		}
		return
	}
	reply, mode := strings.TrimSpace(event.Reply), event.DeliveryMode
	if mode == store.EventOutcomeSkipped {
		if !event.NotificationRequired {
			skipped, err := b.finalizeEventSkip(parent, event)
			if err != nil {
				b.retry(event, "保存事件静默决定失败: "+err.Error())
				return
			}
			if skipped {
				return
			}
		}
		mode = store.EventOutcomeFallback
		reply = fmt.Sprintf("🔔 %s：%s", event.Kind, event.Detail)
		if err := b.prepareEventDelivery(parent, event, mode, reply); err != nil {
			b.retry(event, "恢复必须送达事件失败: "+err.Error())
			return
		}
	}
	if reply == "" {
		if mode == store.EventOutcomeSkipped || mode == store.EventDeliveryModeGenerating {
			// A previous process crossed the durable decision boundary but did not
			// persist a reply. Do not replay a possibly billed model turn; deliver
			// the factual event without claiming any action.
			mode = store.EventOutcomeFallback
			reply = fmt.Sprintf("🔔 %s：%s", event.Kind, event.Detail)
		} else {
			if err := b.store.BeginEventDecision(ctx, event.ID, *event.ClaimedAt); err != nil {
				b.retry(event, "保存事件决策边界失败: "+err.Error())
				return
			}
			mode = store.EventOutcomeHandled
			var generated string
			if b.orch != nil {
				generated, err = b.orch.HandleAutomationMessage(ctx, u, b.channel, fmt.Sprintf("event:%d", event.ID), directive(event.Kind, event.Detail, event.NotificationRequired), chat.AutomationTurnOptions{
					ReadOnly: true, JSONOutput: true,
				})
			}
			decision, decisionErr := notify.ParseDecision(generated)
			if b.orch == nil || err != nil || decisionErr != nil {
				if err != nil {
					slog.Warn("事件 AI 轮次失败，准备持久化降级消息", "event", event.ID, "err", err)
				} else if decisionErr != nil && b.orch != nil {
					slog.Warn("事件 AI 决策格式无效，准备持久化降级消息", "event", event.ID, "err", decisionErr)
				}
				mode = store.EventOutcomeFallback
				reply = fmt.Sprintf("🔔 %s：%s", event.Kind, event.Detail)
			} else if !decision.Notify {
				if event.NotificationRequired {
					mode = store.EventOutcomeFallback
					reply = fmt.Sprintf("🔔 %s：%s", event.Kind, event.Detail)
				} else {
					skipped, err := b.finalizeEventSkip(parent, event)
					if err != nil {
						b.retry(event, "保存事件静默决定失败: "+err.Error())
						return
					}
					if skipped {
						return
					}
					// A required duplicate won the row lock while the model was
					// deciding. Fall back in this claim instead of waiting for retry.
					mode = store.EventOutcomeFallback
					reply = fmt.Sprintf("🔔 %s：%s", event.Kind, event.Detail)
				}
			} else {
				reply = decision.Message
			}
		}
		if err := b.prepareEventDelivery(parent, event, mode, reply); err != nil {
			b.retry(event, "保存待投递事件内容失败: "+err.Error())
			return
		}
	}
	sendCtx, sendCancel := context.WithTimeout(parent, eventSendTimeout)
	delivery, err := notify.SendOnce(sendCtx, b.store, b.notifier,
		fmt.Sprintf("event:%d", event.ID), event.DeciderID, reply)
	sendCancel()
	if !delivery.Settled() {
		slog.Warn("事件通知推送失败，等待重试", "event", event.ID, "user", event.DeciderID, "err", err)
		cause := "通知投递未跨过持久边界"
		if err != nil {
			cause = err.Error()
		}
		b.retry(event, cause)
		return
	}
	if !delivery.Delivered {
		if err != nil {
			slog.Warn("事件通知投递结果不确定，禁止自动重发", "event", event.ID, "user", event.DeciderID, "state", delivery.State, "err", err)
		}
		if err := b.completeEvent(parent, event, store.EventOutcomeSendFailed); err != nil {
			slog.Warn("事件不确定投递 ack 失败", "event", event.ID, "err", err)
		}
		return
	}
	if err := b.completeEvent(parent, event, mode); err != nil {
		slog.Warn("事件投递成功但 ack 失败", "event", event.ID, "err", err)
	}
}

func (b *Bus) prepareEventDelivery(parent context.Context, event *store.Event, mode, reply string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), eventStateTimeout)
	defer cancel()
	return b.store.PrepareEventDelivery(ctx, event.ID, *event.ClaimedAt, mode, reply)
}

func (b *Bus) completeEvent(parent context.Context, event *store.Event, outcome string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), eventStateTimeout)
	defer cancel()
	return b.store.CompleteEvent(ctx, event.ID, *event.ClaimedAt, outcome)
}

func (b *Bus) finalizeEventSkip(parent context.Context, event *store.Event) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), eventStateTimeout)
	defer cancel()
	return b.store.FinalizeEventSkip(ctx, event.ID, *event.ClaimedAt)
}

func (b *Bus) retry(event *store.Event, cause string) {
	if event == nil || event.ClaimedAt == nil || b.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.store.RetryEvent(ctx, event.ID, *event.ClaimedAt, event.Attempts, cause); err != nil {
		slog.Warn("事件重试状态保存失败", "event", event.ID, "err", err)
	}
}

// send is retained as the raw channel adapter for focused transport tests.
// Durable event handling uses notify.SendOnce above and must not call this
// helper directly.
func (b *Bus) send(ctx context.Context, userID int64, text string) error {
	if b.notifier == nil {
		return fmt.Errorf("通知通道尚未就绪")
	}
	return b.notifier.Send(ctx, userID, text)
}

// directive assembles a read-only notification decision. Domain events are
// evidence that something happened, not authorization to mutate business data.
func directive(kind, detail string, required bool) string {
	policy := "若不值得打扰用户，notify=false 且 message 为空。"
	if required {
		policy = "这是必须送达的关键事件，notify 必须为 true。"
	}
	return fmt.Sprintf("[系统事件·%s]（此输入来自系统事件总线，不是用户本人）事件详情：%s\n"+
		"这是只读通知决策：可用查询工具核实事实，但不得创建、修改、发送或承诺任何业务动作。"+
		"请以当前用户助理的身份生成简洁自然的通知，包含关键信息；只有证据支持时才给下一步建议，不编造。"+
		"只输出严格 JSON：{\"notify\":true|false,\"message\":\"要投递的消息\"}。%s", kind, detail, policy)
}

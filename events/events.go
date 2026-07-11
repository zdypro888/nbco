// Package events 是系统事件总线：领域事件（员工加入、worker 上线、任务提交等）
// 不再各自硬编码「通知谁、说什么」，而是统一交给 AI 分析——以事件相关人的身份
// 跑一轮引擎，AI 结合该用户的会话上下文、行为规则与工具，自己决定要不要通知、
// 通知什么措辞、要不要顺手做点什么（建任务/设提醒/存档），不值得打扰就静默。
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
	"unicode"

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

// skipWord AI 判定不值得打扰时约定的完整答复。
const skipWord = "跳过"

// Bus 系统事件总线。orch 为 nil 时事件直接降级为原文推送（测试或引擎未装配）。
type Bus struct {
	store    *store.Store
	orch     *chat.Orchestrator
	notifier notify.Notifier
	channel  string        // AI 轮次挂在哪个渠道会话上（与调度器一致，保证上下文连续）
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
	if b == nil || b.store == nil || deciderID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, created, err := b.store.EnqueueEvent(ctx, kind, deciderID, detail, dedupeWindow)
	if err != nil {
		slog.Error("系统事件入队失败", "kind", kind, "user", deciderID, "err", err)
		return
	}
	if !created {
		slog.Debug("事件去重：窗口内重复，跳过", "kind", kind, "user", deciderID)
		return
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
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
		if err := b.completeEvent(parent, event, store.EventOutcomeSkipped); err != nil {
			slog.Warn("事件静默 ack 失败", "event", event.ID, "err", err)
		}
		return
	}
	if reply == "" {
		if mode == store.EventDeliveryModeGenerating {
			// A previous process crossed the durable decision boundary but did not
			// persist a reply. The AI turn may have run write tools, so replaying it
			// is unsafe. Deliver the factual event without claiming any action.
			mode = store.EventOutcomeFallback
			reply = fmt.Sprintf("🔔 %s：%s", event.Kind, event.Detail)
		} else {
			if err := b.store.BeginEventDecision(ctx, event.ID, *event.ClaimedAt); err != nil {
				b.retry(event, "保存事件决策边界失败: "+err.Error())
				return
			}
			mode = store.EventOutcomeHandled
			if b.orch != nil {
				reply, err = b.orch.HandleMessage(ctx, u, b.channel, directive(event.Kind, event.Detail))
			}
			if b.orch == nil || err != nil || strings.TrimSpace(reply) == "" {
				if err != nil {
					slog.Warn("事件 AI 轮次失败，准备持久化降级消息", "event", event.ID, "err", err)
				}
				mode = store.EventOutcomeFallback
				reply = fmt.Sprintf("🔔 %s：%s", event.Kind, event.Detail)
			} else if ShouldSkip(reply) {
				if err := b.prepareEventDelivery(parent, event, store.EventOutcomeSkipped, skipWord); err != nil {
					b.retry(event, "保存事件静默决定失败: "+err.Error())
					return
				}
				if err := b.completeEvent(parent, event, store.EventOutcomeSkipped); err != nil {
					slog.Warn("事件静默 ack 失败", "event", event.ID, "err", err)
				}
				return
			}
		}
		if err := b.prepareEventDelivery(parent, event, mode, reply); err != nil {
			b.retry(event, "保存待投递事件内容失败: "+err.Error())
			return
		}
	}
	sendCtx, sendCancel := context.WithTimeout(parent, eventSendTimeout)
	err = b.send(sendCtx, event.DeciderID, reply)
	sendCancel()
	if err != nil {
		slog.Warn("事件通知推送失败，等待重试", "event", event.ID, "user", event.DeciderID, "err", err)
		b.retry(event, err.Error())
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

func (b *Bus) send(ctx context.Context, userID int64, text string) error {
	if b.notifier == nil {
		return fmt.Errorf("通知通道尚未就绪")
	}
	return b.notifier.Send(ctx, userID, text)
}

// directive 组装喂给引擎的系统事件指令。决策权在 AI：通知、行动或跳过。
func directive(kind, detail string) string {
	return fmt.Sprintf("[系统事件·%s]（此输入来自系统事件总线，不是用户本人）事件详情：%s\n"+
		"请以当前用户助理的身份分析这个事件，自行决定：\n"+
		"- 值得用户知道：直接产出要推送给用户的消息（简洁自然、含关键信息与下一步建议；需要事实先用工具查，不编造）\n"+
		"- 需要顺手处理：直接调用工具完成（如记录信息、设提醒、跟进任务），并在推送消息里带一句做了什么\n"+
		"- 不值得打扰用户：只回复两个字「%s」，不要任何其他内容", kind, detail, skipWord)
}

// ShouldSkip 判断 AI 答复是否为「静默」决定：剥掉标点/空白/表情后必须
// 精确等于约定词。旧的「短答复包含即算」会把否定回答（「不跳过」）误判成
// 静默，把本应通知的事件吞掉。空答复不算静默（由调用方降级原文推送）。
func ShouldSkip(reply string) bool {
	var core []rune
	for _, r := range reply {
		// 只保留汉字与字母数字参与比对，标点/空白/emoji 全剥掉。
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			core = append(core, r)
		}
	}
	return string(core) == skipWord
}

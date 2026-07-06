// Package events 是系统事件总线：领域事件（员工加入、worker 上线、任务提交等）
// 不再各自硬编码「通知谁、说什么」，而是统一交给 AI 分析——以事件相关人的身份
// 跑一轮引擎，AI 结合该用户的会话上下文、行为规则与工具，自己决定要不要通知、
// 通知什么措辞、要不要顺手做点什么（建任务/设提醒/存档），不值得打扰就静默。
// 代码只保证两件事：事件必达（AI 失败时降级为原文推送）与并发受控。
package events

import (
	"context"
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

// skipWord AI 判定不值得打扰时约定的完整答复。
const skipWord = "跳过"

// Bus 系统事件总线。orch 为 nil 时事件直接降级为原文推送（测试或引擎未装配）。
type Bus struct {
	store    *store.Store
	orch     *chat.Orchestrator
	notifier notify.Notifier
	channel  string        // AI 轮次挂在哪个渠道会话上（与调度器一致，保证上下文连续）
	sem      chan struct{} // AI 轮次限并发，护后端网关
}

// New 创建事件总线。concurrency <=0 取默认 4。
func New(st *store.Store, orch *chat.Orchestrator, n notify.Notifier, channel string, concurrency int) *Bus {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Bus{store: st, orch: orch, notifier: n, channel: channel, sem: make(chan struct{}, concurrency)}
}

// Emit 异步处理一个系统事件：decider 是该事件的决策人/利益相关者（邀请人、
// 派活人、监护人……），AI 以其身份分析并行动。触发方立即返回，不被 AI 拖慢。
// AI 通道满载时不排队（无界排队会让积压事件几小时后才被处理，早已过时），
// 直接降级为原文推送——事件必达优先于智能加工。
func (b *Bus) Emit(kind string, deciderID int64, detail string) {
	if b == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("系统事件处理 panic 已恢复", "kind", kind, "user", deciderID, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
		defer cancel()
		select {
		case b.sem <- struct{}{}:
			defer func() { <-b.sem }()
			b.handle(ctx, kind, deciderID, detail)
		default:
			slog.Warn("事件 AI 通道满载，降级原文推送", "kind", kind, "user", deciderID)
			if u, err := b.store.UserByID(ctx, deciderID); err == nil && u.Status == store.UserActive {
				b.fallback(ctx, kind, deciderID, detail)
			}
		}
	}()
}

func (b *Bus) handle(ctx context.Context, kind string, deciderID int64, detail string) {
	u, err := b.store.UserByID(ctx, deciderID)
	if err != nil || u.Status != store.UserActive {
		slog.Info("事件决策人不可达，事件丢弃", "kind", kind, "user", deciderID, "err", err)
		return
	}
	if b.orch == nil {
		b.fallback(ctx, kind, deciderID, detail)
		return
	}
	reply, err := b.orch.HandleMessage(ctx, u, b.channel, directive(kind, detail))
	if err != nil {
		slog.Warn("事件 AI 轮次失败，降级原文推送", "kind", kind, "user", deciderID, "err", err)
		b.fallback(ctx, kind, deciderID, detail)
		return
	}
	if strings.TrimSpace(reply) == "" {
		// 空答复 ≠ 决定静默（可能是引擎只执行了工具没产出文本）：事件必达，降级原文。
		b.fallback(ctx, kind, deciderID, detail)
		return
	}
	if ShouldSkip(reply) {
		slog.Info("事件经 AI 分析后静默", "kind", kind, "user", deciderID)
		return
	}
	if err := b.send(ctx, deciderID, reply); err != nil {
		slog.Warn("事件通知推送失败", "kind", kind, "user", deciderID, "err", err)
	}
}

// fallback 降级路径：AI 不可用也保证事件必达（原文、无智能加工）。
func (b *Bus) fallback(ctx context.Context, kind string, deciderID int64, detail string) {
	if err := b.send(ctx, deciderID, fmt.Sprintf("🔔 %s：%s", kind, detail)); err != nil {
		slog.Warn("事件降级推送失败", "kind", kind, "user", deciderID, "err", err)
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
// 静默，把本该必达的事件吞掉。空答复不算静默（由调用方降级原文推送）。
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

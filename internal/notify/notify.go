// Package notify 定义跨渠道的消息投递抽象。
// 中枢只认 userID；投递到哪个 IM 由实现决定（telegram 网关实现它）。
package notify

import (
	"context"
	"errors"
	"sync"
)

// Notifier 向用户主动投递消息（提醒、催办、系统通知）。
type Notifier interface {
	// Send 给用户发一条文本消息。用户无可达渠道时返回错误。
	// text 可含 Telegram HTML 子集（<b> <i> <code> 等）；投递方负责在
	// 格式非法或渠道不支持时降级为纯文本，调用方无需转义。
	Send(ctx context.Context, userID int64, text string) error
}

// Func 便捷适配器。
type Func func(ctx context.Context, userID int64, text string) error

// Send 实现 Notifier。
func (f Func) Send(ctx context.Context, userID int64, text string) error { return f(ctx, userID, text) }

// Hub 是可后期注入的 Notifier 容器，用于解决装配期的循环依赖
// （工具层需要 Notifier，而 Notifier 由入口网关实现）。
type Hub struct {
	mu sync.RWMutex
	n  Notifier
}

// Set 注入实际实现。
func (h *Hub) Set(n Notifier) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n = n
}

// Send 实现 Notifier；未注入时报错。
func (h *Hub) Send(ctx context.Context, userID int64, text string) error {
	h.mu.RLock()
	n := h.n
	h.mu.RUnlock()
	if n == nil {
		return errors.New("通知通道尚未就绪")
	}
	return n.Send(ctx, userID, text)
}

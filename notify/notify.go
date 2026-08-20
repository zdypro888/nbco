// Package notify 定义跨渠道的消息投递抽象。
// 中枢只认 userID；投递到哪个 IM 由实现决定（telegram 网关实现它）。
package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

// Notifier 向用户主动投递消息（提醒、催办、系统通知）。
type Notifier interface {
	// Send 给用户发一条文本消息。用户无可达渠道时返回错误。
	// text 可含 Telegram HTML 子集（<b> <i> <code> 等）；投递方负责在
	// 格式非法或渠道不支持时降级为纯文本，调用方无需转义。
	Send(ctx context.Context, userID int64, text string) error
}

// FileNotifier is implemented by channels that can deliver a stored nbco file
// to a user. Tools should prefer this optional capability over leaking raw
// filesystem paths or asking users to fetch internal URLs.
type FileNotifier interface {
	SendFile(ctx context.Context, userID int64, fileID int64, caption string) error
}

// DeliveryResult describes the durable transport boundary. Settled means this
// logical occurrence must not be retried; Delivered distinguishes a confirmed
// channel acknowledgement from failed or crash-uncertain delivery.
type DeliveryResult struct {
	State     string
	Delivered bool
	Replayed  bool
}

func (r DeliveryResult) Settled() bool { return strings.TrimSpace(r.State) != "" }

type deliveryKeyContextKey struct{}

// WithDeliveryKey carries the stable logical notification identity through a
// channel adapter. Fragmenting transports use it to persist finer-grained
// delivery receipts without exposing transport details to schedulers/tools.
func WithDeliveryKey(ctx context.Context, key string) context.Context {
	key = strings.TrimSpace(key)
	if ctx == nil || key == "" {
		return ctx
	}
	return context.WithValue(ctx, deliveryKeyContextKey{}, key)
}

func DeliveryKey(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(deliveryKeyContextKey{}).(string)
	return strings.TrimSpace(key)
}

// ToolDeliveryKey scopes an outbound side effect to one Eino write/execute
// invocation and one recipient. Empty means the caller is outside an Agent
// runtime and must use its own domain occurrence identity.
func ToolDeliveryKey(ctx context.Context, scope string, userID int64) string {
	invocation := ai.ToolInvocationKey(ctx)
	scope = strings.TrimSpace(scope)
	if invocation == "" || scope == "" || userID <= 0 {
		return ""
	}
	return fmt.Sprintf("agent:%s:%s:%d", invocation, scope, userID)
}

// SendOnce performs an at-most-once external delivery under a stable logical
// key. Exactly-once cannot be made atomic across PostgreSQL and an IM API: after
// crossing the boundary, an interrupted or ambiguous send is recorded for
// audit and never replayed blindly.
func SendOnce(ctx context.Context, st *store.Store, n Notifier, key string, userID int64, text string) (DeliveryResult, error) {
	return deliverOnce(ctx, st, n, key, userID, text, func(sendCtx context.Context) error {
		return n.Send(sendCtx, userID, text)
	})
}

// SendForToolInvocation gives Agent-originated text delivery a stable transport
// identity. Direct callers without a runtime invocation retain normal one-shot
// behavior and should supply a domain key to SendOnce when they can be reclaimed.
func SendForToolInvocation(ctx context.Context, st *store.Store, n Notifier, scope string, userID int64, text string) (DeliveryResult, error) {
	sum := sha256.Sum256([]byte(text))
	if key := ToolDeliveryKey(ctx, fmt.Sprintf("%s:%x", scope, sum[:8]), userID); key != "" {
		return SendOnce(ctx, st, n, key, userID, text)
	}
	if n == nil {
		return DeliveryResult{}, errors.New("通知通道尚未就绪")
	}
	if err := n.Send(ctx, userID, text); err != nil {
		return DeliveryResult{}, err
	}
	return DeliveryResult{State: store.NotificationDeliveryDelivered, Delivered: true}, nil
}

// SendFileOnce applies the same no-replay boundary to a file delivery. The
// content identity covers the stable file record and its visible caption.
func SendFileOnce(ctx context.Context, st *store.Store, n FileNotifier, key string, userID, fileID int64, caption string) (DeliveryResult, error) {
	identity := fmt.Sprintf("file:%d\x00%s", fileID, caption)
	return deliverOnce(ctx, st, n, key, userID, identity, func(sendCtx context.Context) error {
		return n.SendFile(sendCtx, userID, fileID, caption)
	})
}

func SendFileForToolInvocation(ctx context.Context, st *store.Store, n FileNotifier, scope string, userID, fileID int64, caption string) (DeliveryResult, error) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", fileID, caption)))
	if key := ToolDeliveryKey(ctx, fmt.Sprintf("%s:%x", scope, sum[:8]), userID); key != "" {
		return SendFileOnce(ctx, st, n, key, userID, fileID, caption)
	}
	if n == nil {
		return DeliveryResult{}, errors.New("通知通道尚未就绪")
	}
	if err := n.SendFile(ctx, userID, fileID, caption); err != nil {
		return DeliveryResult{}, err
	}
	return DeliveryResult{State: store.NotificationDeliveryDelivered, Delivered: true}, nil
}

type deliverySender interface{}

func deliverOnce(ctx context.Context, st *store.Store, ready deliverySender, key string, userID int64, contentIdentity string, send func(context.Context) error) (DeliveryResult, error) {
	key = strings.TrimSpace(key)
	if st == nil || ready == nil || key == "" || userID <= 0 || send == nil {
		return DeliveryResult{}, fmt.Errorf("通知投递参数不完整")
	}
	if readiness, ok := ready.(interface{ Ready() bool }); ok && !readiness.Ready() {
		return DeliveryResult{}, errors.New("通知通道尚未就绪")
	}
	sum := sha256.Sum256([]byte(contentIdentity))
	delivery, created, err := st.BeginNotificationDelivery(ctx, key, userID, hex.EncodeToString(sum[:]))
	if err != nil {
		if delivery != nil {
			return DeliveryResult{
				State: delivery.Status, Delivered: delivery.Status == store.NotificationDeliveryDelivered, Replayed: true,
			}, err
		}
		return DeliveryResult{}, err
	}
	if !created {
		return DeliveryResult{
			State: delivery.Status, Delivered: delivery.Status == store.NotificationDeliveryDelivered, Replayed: true,
		}, nil
	}

	result := DeliveryResult{State: store.NotificationDeliveryStarted}
	if err := send(WithDeliveryKey(ctx, key)); err != nil {
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if markErr := st.MarkNotificationDeliveryFailed(ackCtx, key, textfmt.RedactSecrets(err.Error())); markErr != nil {
			return result, fmt.Errorf("通知发送失败: %w；失败状态保存失败: %v", err, markErr)
		}
		result.State = store.NotificationDeliveryFailed
		return result, err
	}
	result.Delivered = true
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := st.MarkNotificationDeliveryDelivered(ackCtx, key, time.Now().UTC()); err != nil {
		// The external side effect already happened. Keep State=started so callers
		// settle their domain claim without claiming a durable delivery ack.
		return result, fmt.Errorf("通知已发送但投递状态保存失败: %w", err)
	}
	result.State = store.NotificationDeliveryDelivered
	return result, nil
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

// Ready reports whether a concrete notifier is installed and, when supported,
// whether that channel has completed its external-service handshake.
func (h *Hub) Ready() bool {
	h.mu.RLock()
	n := h.n
	h.mu.RUnlock()
	if n == nil {
		return false
	}
	if r, ok := n.(interface{ Ready() bool }); ok {
		return r.Ready()
	}
	return true
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

func (h *Hub) SendFile(ctx context.Context, userID int64, fileID int64, caption string) error {
	h.mu.RLock()
	n := h.n
	h.mu.RUnlock()
	if n == nil {
		return errors.New("通知通道尚未就绪")
	}
	fn, ok := n.(FileNotifier)
	if !ok {
		return errors.New("当前通知通道不支持发送文件")
	}
	return fn.SendFile(ctx, userID, fileID, caption)
}

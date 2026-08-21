package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/zdypro888/nbco/textfmt"
)

const (
	streamEditInterval = 1500 * time.Millisecond // 渐进编辑节流：防 TG 限流与「未改动」报错
	streamTypingEvery  = 4 * time.Second         // Telegram typing 状态有效期很短，长轮次要续
	streamMaxRunes     = 3900                    // 流式编辑单条上限（低于 TG 4096，避免中途超限）
)

// streamEditor 把 AI 流式增量渐进编辑进一条占位消息：本地模型慢，用户能边看边等。
// onDelta 只快追加到缓冲（引擎 goroutine 调，必须快）；后台节流协程定期把缓冲编辑
// 上去，把 TG API 延迟与流读取解耦。finish 用最终权威文本 + HTML 排版覆盖，修掉流
// 式期间可能的中间态（如工具调用前的思考文字）。发占位失败则整体降级为非流式。
type streamEditor struct {
	g      *Gateway
	chatID int64
	msgID  int
	ok     bool // 占位消息就绪、可编辑

	editEvery   time.Duration // 渐进编辑节流间隔（默认 streamEditInterval；测试注入短值提速）
	typingEvery time.Duration // typing 续期间隔（默认 streamTypingEvery）

	mu   sync.Mutex
	buf  string // 当前要显示的完整文本（onDelta 替换式写入）
	sent string // 上次已编辑上去的内容，避免重复编辑报 not modified

	stop    chan struct{}
	done    chan struct{}
	stopped bool
}

// newStreamEditor starts a lazy stream editor. The caller already publishes a
// typing action, so no Telegram message is created until the model has visible
// text. This keeps a replayed/already-running turn from leaving a duplicate
// placeholder behind.
func (g *Gateway) newStreamEditor(ctx context.Context, chatID int64) *streamEditor {
	return g.newStreamEditorEvery(ctx, chatID, streamEditInterval, streamTypingEvery)
}

// newStreamEditorEvery is the testable form of newStreamEditor.
func (g *Gateway) newStreamEditorEvery(ctx context.Context, chatID int64, editEvery, typingEvery time.Duration) *streamEditor {
	ed := &streamEditor{g: g, chatID: chatID, editEvery: editEvery, typingEvery: typingEvery,
		stop: make(chan struct{}), done: make(chan struct{})}
	go ed.loop(ctx)
	return ed
}

func (ed *streamEditor) loop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			ed.g.logStreamPanic(ed.chatID, r)
		}
		close(ed.done)
	}()
	editTick := time.NewTicker(ed.editEvery)
	defer editTick.Stop()
	typingTick := time.NewTicker(ed.typingEvery)
	defer typingTick.Stop()
	typingInFlight := make(chan struct{}, 1)
	for {
		select {
		case <-ed.stop:
			return
		case <-ctx.Done():
			return
		case <-editTick.C:
			ed.flush(ctx)
		case <-typingTick.C:
			select {
			case typingInFlight <- struct{}{}:
				go func() {
					defer func() {
						if r := recover(); r != nil {
							ed.g.logStreamPanic(ed.chatID, r)
						}
						<-typingInFlight
					}()
					ed.g.sendTyping(ctx, ed.chatID)
				}()
			default:
			}
		}
	}
}

func (g *Gateway) logStreamPanic(chatID int64, r any) {
	slog.Error("Telegram 流式后台 panic 已恢复", "chat", chatID, "panic", r)
}

// onDelta 收到当前助手消息的累积快照，【替换】显示缓冲（引擎 goroutine 调，只做快
// 操作）。替换语义让新消息（如工具调用后的最终答复）自然刷新，不与前导文字拼接。
func (ed *streamEditor) onDelta(snapshot string) {
	if snapshot == "" {
		return
	}
	snapshot = textfmt.SanitizeVisibleReply(snapshot)
	if snapshot == "" {
		return
	}
	ed.mu.Lock()
	ed.buf = snapshot
	ed.mu.Unlock()
}

// flush 把当前缓冲以纯文本编辑上去（截断到上限；无变化则跳过）。
func (ed *streamEditor) flush(ctx context.Context) {
	ed.mu.Lock()
	cur := strings.TrimSpace(ed.buf)
	ed.mu.Unlock()
	if cur == "" || cur == ed.sent {
		return
	}
	disp := cur
	if r := []rune(disp); len(r) > streamMaxRunes {
		disp = string(r[:streamMaxRunes]) + " …"
	}
	if !ed.ok {
		m, err := ed.g.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: ed.chatID, Text: disp})
		if err != nil || m == nil {
			return
		}
		ed.msgID, ed.ok, ed.sent = m.ID, true, cur
		return
	}
	if _, err := ed.g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: ed.chatID, MessageID: ed.msgID, Text: disp,
	}); err == nil {
		ed.sent = cur
	}
}

// stopLoop 停节流协程并等它退出——确保之后不再有并发编辑，final 编辑不被覆盖。
func (ed *streamEditor) stopLoop() {
	ed.mu.Lock()
	if !ed.stopped {
		ed.stopped = true
		close(ed.stop)
	}
	ed.mu.Unlock()
	<-ed.done
}

// finish 收尾：停协程，用最终答复 + HTML 排版覆盖占位消息；过长（多片）则占位改
// 成首片、其余追加发送。占位消息不可编辑（被用户删/限流）时【发新消息兜底】——
// 绝不因编辑失败把答复静默丢掉。
func (ed *streamEditor) finish(ctx context.Context, answer string) error {
	ed.stopLoop()
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = "（空回复）"
	}
	if !ed.ok {
		return ed.g.sendChunks(ctx, ed.chatID, answer)
	}
	chunks := splitChunks(toTelegramHTML(answer), chunkLimit)
	// 首片覆盖占位消息。最终文本已经是持久 conversation turn 的结果，因此
	// 编辑占位与“占位不可编辑后改发新消息”必须共用同一分片投递边界；否则进程
	// 在 Telegram 成功与 delivery_status 落库之间停止时，重放会再发一份完整答复。
	key := telegramContextDeliveryKey(ctx, ed.chatID)
	if key == "" {
		edited, err := ed.editPlaceholder(ctx, chunks[0], plainOf(answer, chunks[0]))
		if err != nil {
			return err
		}
		if !edited {
			return ed.g.sendChunks(ctx, ed.chatID, answer)
		}
	} else {
		messageID, settled, firstErr := ed.g.deliverPreparedPartOnce(
			ctx, key, ed.chatID, 0, len(chunks), chunks[0], func() (int, error) {
				edited, err := ed.editPlaceholder(ctx, chunks[0], plainOf(answer, chunks[0]))
				if err != nil {
					return 0, err
				}
				if edited {
					return ed.msgID, nil
				}
				return ed.g.sendOneMessage(ctx, ed.chatID, chunks[0], false)
			},
		)
		if !telegramPartCanContinue(messageID, settled, firstErr) {
			return firstErr
		}
		if len(chunks) > 1 {
			_, suffixErr := ed.g.sendPreparedParts(ctx, ed.chatID, chunks, 1, false)
			return errors.Join(firstErr, suffixErr)
		}
		return firstErr
	}
	// 其余片作为新消息追加，并以同一逻辑回复下的逐片账本防止部分成功
	// 后重试时重发已确认的前缀。首片是对既有占位消息的编辑，不是新发送。
	if len(chunks) > 1 {
		if _, err := ed.g.sendPreparedParts(ctx, ed.chatID, chunks, 1, false); err != nil {
			return err
		}
	}
	return nil
}

// discard removes the transient placeholder when an already-delivered inbound
// update is replayed. Failure is harmless: the original final reply remains the
// authoritative delivered result.
func (ed *streamEditor) discard(ctx context.Context) {
	ed.stopLoop()
	if !ed.ok {
		return
	}
	if _, err := ed.g.bot.DeleteMessage(ctx, &bot.DeleteMessageParams{
		ChatID: ed.chatID, MessageID: ed.msgID,
	}); err != nil && !errors.Is(err, bot.ErrorBadRequest) {
		slog.Debug("删除重复轮次占位失败", "chat", ed.chatID, "message", ed.msgID, "err", err)
	}
}

// fail 收尾并把占位消息改成错误提示；占位不可编辑则发新消息，不吞掉提示。
func (ed *streamEditor) fail(ctx context.Context, msg string) {
	ed.stopLoop()
	if !ed.ok {
		ed.g.reply(ctx, ed.chatID, msg)
		return
	}
	if _, err := ed.g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: ed.chatID, MessageID: ed.msgID, Text: msg,
	}); err != nil && errors.Is(err, bot.ErrorBadRequest) {
		ed.g.reply(ctx, ed.chatID, msg)
	}
}

// editPlaceholder 用 HTML 编辑占位消息，失败降级纯文本；两者都失败（占位不可编辑）
// 返回 false，让调用方走发新消息兜底。「内容未变」(not modified) 视作成功——占位
// 已显示目标文本（流式期间已编辑上去），此时若当作失败去 reply 会发重复消息。
func (ed *streamEditor) editPlaceholder(ctx context.Context, htmlChunk, plainText string) (bool, error) {
	if _, err := ed.g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: ed.chatID, MessageID: ed.msgID, Text: htmlChunk, ParseMode: models.ParseModeHTML,
	}); err == nil || isNotModified(err) {
		return true, nil
	} else if !errors.Is(err, bot.ErrorBadRequest) {
		return false, err
	}
	_, err := ed.g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: ed.chatID, MessageID: ed.msgID, Text: plainText,
	})
	if err == nil || isNotModified(err) {
		return true, nil
	}
	if !errors.Is(err, bot.ErrorBadRequest) {
		return false, err
	}
	return false, nil
}

// isNotModified 判断编辑是否因「新内容与现有完全相同」被 TG 拒绝（HTTP 400
// "message is not modified"）。这不是失败——占位消息已显示目标文本，无需再发。
// 只有占位真被删/不可编辑（"message to edit not found" 等）才需发新消息兜底。
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// plainOf：HTML 编辑失败时的纯文本兜底。不能退回原始答复，否则模型吐坏 HTML
// 时会把 <b> 等标签原样展示给用户。
func plainOf(answer, chunk string) string {
	if plain := telegramPlainTextOrEmpty(chunk); plain != "" {
		return plain
	}
	return telegramPlainText(answer)
}

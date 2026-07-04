package telegram

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	streamEditInterval = 1500 * time.Millisecond // 渐进编辑节流：防 TG 限流与「未改动」报错
	streamMaxRunes     = 3900                     // 流式编辑单条上限（低于 TG 4096，避免中途超限）
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

	mu   sync.Mutex
	buf  string // 当前要显示的完整文本（onDelta 替换式写入）
	sent string // 上次已编辑上去的内容，避免重复编辑报 not modified

	stop    chan struct{}
	done    chan struct{}
	stopped bool
}

// newStreamEditor 发占位消息 + 起后台节流编辑协程。占位发送失败则 ok=false，
// onDelta 变 no-op、finish 直接发完整答复（优雅降级为非流式）。
func (g *Gateway) newStreamEditor(ctx context.Context, chatID int64) *streamEditor {
	ed := &streamEditor{g: g, chatID: chatID, stop: make(chan struct{}), done: make(chan struct{})}
	m, err := g.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "💭 …"})
	if err != nil || m == nil {
		close(ed.done)
		return ed
	}
	ed.msgID, ed.ok = m.ID, true
	go ed.loop(ctx)
	return ed
}

func (ed *streamEditor) loop(ctx context.Context) {
	defer close(ed.done)
	t := time.NewTicker(streamEditInterval)
	defer t.Stop()
	for {
		select {
		case <-ed.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			ed.flush(ctx)
		}
	}
}

// onDelta 收到当前助手消息的累积快照，【替换】显示缓冲（引擎 goroutine 调，只做快
// 操作）。替换语义让新消息（如工具调用后的最终答复）自然刷新，不与前导文字拼接。
func (ed *streamEditor) onDelta(snapshot string) {
	if !ed.ok || snapshot == "" {
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
func (ed *streamEditor) finish(ctx context.Context, answer string) {
	ed.stopLoop()
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = "（空回复）"
	}
	if !ed.ok {
		ed.g.reply(ctx, ed.chatID, answer)
		return
	}
	chunks := splitChunks(toTelegramHTML(answer), chunkLimit)
	// 首片覆盖占位消息；编辑不成（HTML 非法先降级纯文本，仍不成=占位不可编辑）则
	// 整条答复作为新消息发（sendChunks 内部分片 + HTML 兜底），不丢内容。
	if !ed.editPlaceholder(ctx, chunks[0], plainOf(answer, chunks[0])) {
		ed.g.reply(ctx, ed.chatID, answer)
		return
	}
	// 其余片作为新消息追加。
	for _, chunk := range chunks[1:] {
		if err := ed.g.sendOne(ctx, ed.chatID, chunk); err != nil {
			ed.g.reply(ctx, ed.chatID, chunk)
		}
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
	}); err != nil {
		ed.g.reply(ctx, ed.chatID, msg)
	}
}

// editPlaceholder 用 HTML 编辑占位消息，失败降级纯文本；两者都失败（占位不可编辑）
// 返回 false，让调用方走发新消息兜底。「内容未变」(not modified) 视作成功——占位
// 已显示目标文本（流式期间已编辑上去），此时若当作失败去 reply 会发重复消息。
func (ed *streamEditor) editPlaceholder(ctx context.Context, htmlChunk, plainText string) bool {
	if _, err := ed.g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: ed.chatID, MessageID: ed.msgID, Text: htmlChunk, ParseMode: models.ParseModeHTML,
	}); err == nil || isNotModified(err) {
		return true
	}
	_, err := ed.g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: ed.chatID, MessageID: ed.msgID, Text: plainText,
	})
	return err == nil || isNotModified(err)
}

// isNotModified 判断编辑是否因「新内容与现有完全相同」被 TG 拒绝（HTTP 400
// "message is not modified"）。这不是失败——占位消息已显示目标文本，无需再发。
// 只有占位真被删/不可编辑（"message to edit not found" 等）才需发新消息兜底。
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

// plainOf：HTML 编辑失败时的纯文本兜底。单片时用原始答复；多片时该片已是 HTML
// 转换后的分片，退而用它本身（宁可带标签也别丢内容）。
func plainOf(answer, chunk string) string {
	if strings.TrimSpace(answer) != "" && len([]rune(answer)) <= chunkLimit {
		return answer
	}
	return chunk
}

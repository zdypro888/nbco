package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

// isNotModified 必须只认「内容未变」，不能把「占位被删/不可编辑」也当成功——
// 否则占位真没了却不发新消息，答复就丢了。两类都是 400 ErrorBadRequest，只能靠
// 描述串区分（构造成 go-telegram v1.22.0 raw_request.go 的确切格式）。
func TestIsNotModified(t *testing.T) {
	notModified := fmt.Errorf("%w, %s", bot.ErrorBadRequest,
		"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message")
	if !isNotModified(notModified) {
		t.Errorf("应识别为 not modified: %v", notModified)
	}

	// 占位被删 / 不可编辑：同为 ErrorBadRequest，但不是 not modified，必须走兜底发新消息。
	for _, desc := range []string{
		"Bad Request: message to edit not found",
		"Bad Request: message can't be edited",
	} {
		err := fmt.Errorf("%w, %s", bot.ErrorBadRequest, desc)
		if isNotModified(err) {
			t.Errorf("不该当成 not modified（占位不可编辑，需发新消息）: %v", err)
		}
	}

	if isNotModified(nil) {
		t.Error("nil 不是 not modified")
	}
	if isNotModified(errors.New("network timeout")) {
		t.Error("普通错误不是 not modified")
	}
}

func TestPlainOfUsesRawEmptinessInsteadOfFallbackText(t *testing.T) {
	if got := plainOf("<b>最终答案</b>", "<b></b>"); got != "最终答案" {
		t.Fatalf("plainOf empty chunk = %q", got)
	}
	if got := plainOf("<b>最终答案</b>", "<i>流式内容</i>"); got != "流式内容" {
		t.Fatalf("plainOf non-empty chunk = %q", got)
	}
}

type streamLoopHTTP struct {
	mu         sync.Mutex
	editCount  int
	actionWait time.Duration
	maxEditGap time.Duration
	prevEditAt time.Time
}

func (m *streamLoopHTTP) Do(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/sendChatAction"):
		if m.actionWait > 0 {
			time.Sleep(m.actionWait)
		}
		return streamLoopResp("true"), nil
	case strings.HasSuffix(req.URL.Path, "/editMessageText"):
		m.mu.Lock()
		now := time.Now()
		if !m.prevEditAt.IsZero() {
			if gap := now.Sub(m.prevEditAt); gap > m.maxEditGap {
				m.maxEditGap = gap
			}
		}
		m.prevEditAt = now
		m.editCount++
		m.mu.Unlock()
		return streamLoopResp(`{"message_id":42,"date":0,"chat":{"id":1,"type":"private"}}`), nil
	case strings.HasSuffix(req.URL.Path, "/sendMessage"):
		return streamLoopResp(`{"message_id":99,"date":0,"chat":{"id":1,"type":"private"}}`), nil
	default:
		return streamLoopResp("true"), nil
	}
}

func streamLoopResp(result string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(`{"ok":true,"result":` + result + `}`)),
		Header:     http.Header{},
	}
}

func TestStreamEditorSlowTypingDoesNotStallFlush(t *testing.T) {
	// 慢 typing（模拟 TG 卡顿）不能拖累 flush 节拍——typing 走独立 goroutine + 单槽守护。
	// 注入毫秒级间隔把用例压到亚秒（机理同生产 1.5s/4s，只是加速）：若 typing 阻塞了
	// loop，编辑间隔会被拉到 typingWait 量级；修复后应远小于它。
	const (
		editEvery  = 20 * time.Millisecond
		typingIvl  = 50 * time.Millisecond
		typingWait = 200 * time.Millisecond // 慢 typing 时长：远大于 editEvery
	)
	h := &streamLoopHTTP{actionWait: typingWait}
	b, err := bot.New("TESTTOKEN", bot.WithHTTPClient(time.Second, h), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	ed := (&Gateway{bot: b}).newStreamEditorEvery(context.Background(), 1, editEvery, typingIvl)
	if ed.ok {
		t.Fatal("没有可见增量前不应创建 Telegram 消息")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 80; i++ { // 持续产出 ~800ms，让 flush 始终有新内容可编辑
			ed.onDelta(strings.Repeat("x", i+1))
			time.Sleep(10 * time.Millisecond)
		}
	}()
	time.Sleep(800 * time.Millisecond)
	ed.stopLoop()
	<-done

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.editCount < 6 {
		t.Fatalf("编辑次数太少，flush 可能被阻塞: %d", h.editCount)
	}
	if h.maxEditGap >= typingWait {
		t.Fatalf("typing 请求拖慢了流式编辑，最大编辑间隔 %v（≥ 慢 typing 时长 %v）", h.maxEditGap, typingWait)
	}
}

type streamDeliveryFailureHTTP struct {
	mu        sync.Mutex
	sendCalls int
}

func (h *streamDeliveryFailureHTTP) Do(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL.Path, "/sendMessage"):
		h.sendCalls++
		if h.sendCalls == 1 {
			return streamLoopResp(`{"message_id":42,"date":0,"chat":{"id":1,"type":"private"}}`), nil
		}
	case strings.HasSuffix(req.URL.Path, "/deleteMessage"):
		return streamLoopResp("true"), nil
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":500,"description":"delivery failed"}`)),
		Header:     http.Header{},
	}, nil
}

func TestStreamEditorFinishReportsTotalDeliveryFailure(t *testing.T) {
	h := &streamDeliveryFailureHTTP{}
	b, err := bot.New("TESTTOKEN", bot.WithHTTPClient(time.Second, h), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	ed := (&Gateway{bot: b}).newStreamEditorEvery(context.Background(), 1, time.Hour, time.Hour)
	ed.onDelta("处理中")
	ed.flush(context.Background())
	if !ed.ok {
		t.Fatal("首个可见增量应创建流式消息")
	}
	if err := ed.finish(context.Background(), "最终答复"); err == nil {
		t.Fatal("finish must report failure when edit and fallback sends all fail")
	}
}

func TestStreamEditorWithoutVisibleDeltaCreatesNoMessage(t *testing.T) {
	h := &streamDeliveryFailureHTTP{}
	b, err := bot.New("TESTTOKEN", bot.WithHTTPClient(time.Second, h), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	ed := (&Gateway{bot: b}).newStreamEditorEvery(context.Background(), 1, time.Hour, time.Hour)
	ed.discard(context.Background())
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sendCalls != 0 {
		t.Fatalf("replayed turn created %d transient messages", h.sendCalls)
	}
}

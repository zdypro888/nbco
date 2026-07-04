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
	h := &streamLoopHTTP{actionWait: 3 * time.Second}
	b, err := bot.New("TESTTOKEN", bot.WithHTTPClient(time.Second, h), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	ed := (&Gateway{bot: b}).newStreamEditor(context.Background(), 1)
	if !ed.ok {
		t.Fatal("占位消息未创建")
	}
	go func() {
		for i := 0; i < 40; i++ {
			ed.onDelta(strings.Repeat("x", i+1))
			time.Sleep(500 * time.Millisecond)
		}
	}()
	time.Sleep(13 * time.Second)
	ed.stopLoop()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.editCount < 6 {
		t.Fatalf("编辑次数太少，flush 可能被阻塞: %d", h.editCount)
	}
	if h.maxEditGap >= 3*time.Second {
		t.Fatalf("typing 请求拖慢了流式编辑，最大编辑间隔 %v", h.maxEditGap)
	}
}

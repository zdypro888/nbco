package telegram

import (
	"errors"
	"fmt"
	"testing"

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

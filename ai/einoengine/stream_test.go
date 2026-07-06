package einoengine

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// readStream 逐块读、拼回完整消息，并把「本条消息累积快照」实时推给 onDelta。
func TestReadStreamEmitsSnapshotsAndConcats(t *testing.T) {
	chunks := []*schema.Message{
		{Role: schema.Assistant, Content: "你好"},
		{Role: schema.Assistant, Content: "，世界"},
		{Role: schema.Assistant, Content: "！"},
	}
	var snaps []string
	msg, err := readStream(schema.StreamReaderFromArray(chunks), schema.Assistant,
		func(s string) { snaps = append(snaps, s) }, false)
	if err != nil {
		t.Fatal(err)
	}
	// 快照是累积的（替换式显示），最后一帧=完整文本。
	want := []string{"你好", "你好，世界", "你好，世界！"}
	if len(snaps) != 3 || snaps[0] != want[0] || snaps[1] != want[1] || snaps[2] != want[2] {
		t.Fatalf("累积快照不对: %v", snaps)
	}
	if msg == nil || msg.Content != "你好，世界！" {
		t.Fatalf("重组消息不对: %+v", msg)
	}
}

func TestReadStreamHidesReasoningByDefault(t *testing.T) {
	chunks := []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "先想想"},
		{Role: schema.Assistant, ReasoningContent: "……嗯"},
		{Role: schema.Assistant, Content: "答案是42"},
	}
	var snaps []string
	msg, err := readStream(schema.StreamReaderFromArray(chunks), schema.Assistant,
		func(s string) { snaps = append(snaps, s) }, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0] != "答案是42" {
		t.Fatalf("默认不应流式展示推理: %v", snaps)
	}
	if msg == nil || msg.Content != "答案是42" {
		t.Fatalf("重组消息正文不对: %+v", msg)
	}
}

func TestReadStreamCanEmitReasoning(t *testing.T) {
	chunks := []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "先想想"},
		{Role: schema.Assistant, ReasoningContent: "……嗯"},
		{Role: schema.Assistant, Content: "答案是42"},
	}
	var snaps []string
	if _, err := readStream(schema.StreamReaderFromArray(chunks), schema.Assistant,
		func(s string) { snaps = append(snaps, s) }, true); err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 || snaps[0] != "先想想" || snaps[2] != "先想想……嗯答案是42" {
		t.Fatalf("开启后应流式展示推理: %v", snaps)
	}
}

// 非助手角色（工具消息）不推快照；onDelta 为 nil 不 panic。
func TestReadStreamSkipsNonAssistantAndNilDelta(t *testing.T) {
	called := false
	if _, err := readStream(schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Tool, Content: "工具结果"}}),
		schema.Tool, func(string) { called = true }, false); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("工具消息不应推快照")
	}
	if _, err := readStream(schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "x"}}),
		schema.Assistant, nil, false); err != nil {
		t.Fatal(err) // onDelta=nil 不应 panic
	}
}

func TestDropLeadingNonUser(t *testing.T) {
	msgs := []*schema.Message{
		schema.AssistantMessage("orphan", nil),
		schema.UserMessage("hi"),
		schema.AssistantMessage("ok", nil),
	}
	got := dropLeadingNonUser(msgs)
	if len(got) != 2 || got[0].Role != schema.User || got[0].Content != "hi" {
		t.Fatalf("dropLeadingNonUser = %+v", got)
	}
	got = dropLeadingNonUser([]*schema.Message{schema.AssistantMessage("a", nil)})
	if len(got) != 0 {
		t.Fatalf("all assistant history should be dropped: %+v", got)
	}
}

func TestIsRetryableModelErr(t *testing.T) {
	ctx := context.Background()
	retryable := []error{
		errors.New("status code: 502, status: 502 Bad Gateway, message: unexpected end of JSON input"),
		errors.New("unexpected EOF"),
		context.DeadlineExceeded,
	}
	for _, err := range retryable {
		if !isRetryableModelErr(ctx, err) {
			t.Fatalf("expected retryable: %v", err)
		}
	}
	nonRetryable := []error{
		errors.New("status code: 401 unauthorized"),
		errors.New("403 forbidden"),
		context.Canceled,
	}
	for _, err := range nonRetryable {
		if isRetryableModelErr(ctx, err) {
			t.Fatalf("expected non-retryable: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if isRetryableModelErr(canceled, errors.New("502 bad gateway")) {
		t.Fatal("request cancellation should not be retried")
	}
}

func TestModelRetryBackoff(t *testing.T) {
	if modelRetryBackoff(1) <= 0 || modelRetryBackoff(2) <= modelRetryBackoff(1) {
		t.Fatalf("unexpected model retry backoff sequence")
	}
}

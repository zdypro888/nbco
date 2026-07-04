package einoengine

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// readStream 逐块读、拼回完整消息、并实时把助手文本增量推给 onDelta。
func TestReadStreamEmitsDeltasAndConcats(t *testing.T) {
	chunks := []*schema.Message{
		{Role: schema.Assistant, Content: "你好"},
		{Role: schema.Assistant, Content: "，世界"},
		{Role: schema.Assistant, Content: "！"},
	}
	sr := schema.StreamReaderFromArray(chunks)

	var deltas []string
	msg, err := readStream(sr, schema.Assistant, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "你好，世界！" {
		t.Fatalf("增量流不对: %v", deltas)
	}
	if msg == nil || msg.Content != "你好，世界！" {
		t.Fatalf("重组消息不对: %+v", msg)
	}
}

// 非助手角色（工具消息）不推增量；onDelta 为 nil 不 panic。
func TestReadStreamSkipsNonAssistantAndNilDelta(t *testing.T) {
	sr := schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Tool, Content: "工具结果"}})
	called := false
	if _, err := readStream(sr, schema.Tool, func(string) { called = true }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("工具消息不应推文本增量")
	}
	sr2 := schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "x"}})
	if _, err := readStream(sr2, schema.Assistant, nil); err != nil {
		t.Fatal(err) // onDelta=nil 不应 panic
	}
}

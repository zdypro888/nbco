package events

import (
	"context"
	"strings"
	"testing"
)

func TestShouldSkip(t *testing.T) {
	cases := []struct {
		reply string
		want  bool
	}{
		{"跳过", true},
		{"  跳过。 ", true},  // 弱模型加标点/空白也认
		{"跳过 🙏", true},    // 短答复带表情
		{"", true},        // 空答复视为无话可说
		{"好的，已通知", false}, // 正常通知内容
		{"这个事件可以跳过吗？我认为需要通知用户，因为……", false}, // 长文里包含关键词不算
	}
	for _, c := range cases {
		if got := ShouldSkip(c.reply); got != c.want {
			t.Errorf("ShouldSkip(%q) = %v, want %v", c.reply, got, c.want)
		}
	}
}

func TestNilNotifierReturnsErrorInsteadOfPanicking(t *testing.T) {
	b := &Bus{}
	err := b.send(context.Background(), 1, "hello")
	if err == nil || !strings.Contains(err.Error(), "通知通道") {
		t.Fatalf("nil notifier 应返回清晰错误, got %v", err)
	}
}

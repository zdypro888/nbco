package events

import (
	"context"
	"strings"
	"testing"
)

func TestNilNotifierReturnsErrorInsteadOfPanicking(t *testing.T) {
	b := &Bus{}
	err := b.send(context.Background(), 1, "hello")
	if err == nil || !strings.Contains(err.Error(), "通知通道") {
		t.Fatalf("nil notifier 应返回清晰错误, got %v", err)
	}
}

func TestDirectiveSeparatesNotificationFromBusinessMutation(t *testing.T) {
	optional := directive("状态变化", "任务已提交", false)
	if !strings.Contains(optional, "只读通知决策") || !strings.Contains(optional, `"notify":true|false`) ||
		!strings.Contains(optional, "notify=false") || strings.Contains(optional, "顺手处理") {
		t.Fatalf("optional directive = %q", optional)
	}
	required := directive("任务提交待验收", "任务 7", true)
	if !strings.Contains(required, "必须送达") || !strings.Contains(required, "notify 必须为 true") {
		t.Fatalf("required directive = %q", required)
	}
}

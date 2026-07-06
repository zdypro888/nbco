package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestParseTelegramGroupRef(t *testing.T) {
	for _, in := range []string{"telegram:group:-100123", "group:-100123", "-100123"} {
		id, ok := parseTelegramGroupRef(in)
		if !ok || id != -100123 {
			t.Fatalf("parseTelegramGroupRef(%q) = %d,%v", in, id, ok)
		}
	}
	if _, ok := parseTelegramGroupRef("示例公司群"); ok {
		t.Fatal("group title should not parse as group ref")
	}
}

func TestRenderTelegramGroupHidesChatIDLabel(t *testing.T) {
	got := renderTelegramGroup(store.TelegramGroupState{
		ChatID:    -100123,
		Title:     "示例公司群",
		Type:      "supergroup",
		Status:    "member",
		Listen:    true,
		UpdatedAt: time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC),
	}, time.UTC)
	for _, want := range []string{"示例公司群", "已加入", "监听开启", "group_ref"} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderTelegramGroup missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "chat_id") || strings.Contains(got, "TG ID") {
		t.Fatalf("renderTelegramGroup should not expose chat id labels:\n%s", got)
	}
}

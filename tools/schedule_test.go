package tools

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeDailyScheduleFields(t *testing.T) {
	for input, want := range map[string]string{"9:05": "09:05", "23:59": "23:59"} {
		got, err := normalizeDailyAt(input)
		if err != nil || got != want {
			t.Fatalf("normalizeDailyAt(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "24:00", "10:60", "10:00 later", "+9:00"} {
		if _, err := normalizeDailyAt(input); err == nil {
			t.Fatalf("normalizeDailyAt(%q) should fail", input)
		}
	}
	got, err := normalizeWeekdays("5,1,3,3")
	if err != nil || got != "1,3,5" {
		t.Fatalf("normalizeWeekdays = %q, %v", got, err)
	}
	if _, err := normalizeWeekdays("1,8"); err == nil {
		t.Fatal("invalid weekday should fail")
	}
}

func TestValidateScheduleTimeBasis(t *testing.T) {
	tz := time.FixedZone("CST", 8*60*60)
	for _, text := range []string{
		"昨天黄桑反馈应用被下架",
		"目前正在等待审核",
		"Review what happened last week",
		"昨日の進捗を確認する",
	} {
		got := validateScheduleTimeBasis(text, false, tz)
		if !strings.Contains(got, "本次未创建") || !strings.Contains(got, "relative_to_trigger=true") {
			t.Fatalf("validateScheduleTimeBasis(%q) = %q", text, got)
		}
	}

	for _, tc := range []struct {
		name              string
		text              string
		relativeToTrigger bool
	}{
		{name: "absolute fixed fact", text: "2026-07-07 黄桑反馈应用被下架"},
		{name: "timeless message", text: "提交日报"},
		{name: "intentional occurrence relative", text: "总结昨天的工作", relativeToTrigger: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateScheduleTimeBasis(tc.text, tc.relativeToTrigger, tz); got != "" {
				t.Fatalf("unexpected validation error: %q", got)
			}
		})
	}
}

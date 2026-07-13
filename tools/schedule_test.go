package tools

import (
	"testing"
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

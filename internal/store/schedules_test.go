package store

import (
	"testing"
	"time"
)

var shanghai = mustTZ("Asia/Shanghai")

func mustTZ(name string) *time.Location {
	tz, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return tz
}

func TestNextDailyFireSameDay(t *testing.T) {
	// 上海 2026-07-06（周一）09:00，规则每天 10:00 → 当天 10:00。
	after := time.Date(2026, 7, 6, 9, 0, 0, 0, shanghai)
	got := NextDailyFire(after, "10:00", "", shanghai)
	want := time.Date(2026, 7, 6, 10, 0, 0, 0, shanghai).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextDailyFireNextDay(t *testing.T) {
	// 已过当天 10:00 → 次日。
	after := time.Date(2026, 7, 6, 10, 0, 0, 0, shanghai) // 恰好等于也算已过（严格 After）
	got := NextDailyFire(after, "10:00", "", shanghai)
	want := time.Date(2026, 7, 7, 10, 0, 0, 0, shanghai).UTC()
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextDailyFireSkipsWeekend(t *testing.T) {
	// 周五 18:40，规则工作日 18:30 → 下周一 18:30。
	after := time.Date(2026, 7, 10, 18, 40, 0, 0, shanghai) // 2026-07-10 是周五
	got := NextDailyFire(after, "18:30", "1,2,3,4,5", shanghai)
	want := time.Date(2026, 7, 13, 18, 30, 0, 0, shanghai).UTC() // 周一
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNextDailyFireInvalidInputsFallBack(t *testing.T) {
	after := time.Date(2026, 7, 6, 9, 0, 0, 0, shanghai)
	if got := NextDailyFire(after, "乱写", "", shanghai); !got.Equal(after.Add(24 * time.Hour).UTC()) {
		t.Fatalf("非法时刻应兜底 +24h, got %v", got)
	}
	if got := NextDailyFire(after, "10:00", "8,9", shanghai); !got.Equal(after.Add(24 * time.Hour).UTC()) {
		t.Fatalf("非法星期应兜底 +24h, got %v", got)
	}
}

func TestWeekdayAllowed(t *testing.T) {
	if !WeekdayAllowed(time.Monday, "") || !WeekdayAllowed(time.Sunday, "") {
		t.Error("空过滤=每天")
	}
	if !WeekdayAllowed(time.Monday, "1,2,3,4,5") || WeekdayAllowed(time.Saturday, "1,2,3,4,5") {
		t.Error("工作日过滤判断错误")
	}
	if !WeekdayAllowed(time.Sunday, "6,7") {
		t.Error("周日应映射为 7")
	}
}

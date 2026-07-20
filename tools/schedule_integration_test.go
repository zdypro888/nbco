package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestCancelScheduleReportsCompletedStateWithoutRewritingHistory(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "schedule-owner", false)
	fireAt := time.Now().UTC().Add(-time.Minute)
	sc, err := s.CreateSchedule(ctx, &store.Schedule{
		UserID: owner.ID, CreatedBy: owner.ID, Kind: store.ScheduleOnce,
		Title: "成员名单确认", Message: "请确认成员名单", FireAt: fireAt,
		Target: store.ScheduleTargetSelf, Mode: store.ScheduleModeMessage,
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].DeliveryClaimedAt == nil {
		t.Fatalf("due schedules = %+v", due)
	}
	if err := s.MarkScheduleDelivered(ctx, sc.ID, *due[0].DeliveryClaimedAt, time.Now().UTC(), nil, true); err != nil {
		t.Fatal(err)
	}

	toolset := scheduleTools(Deps{Store: s, TZ: time.UTC}, owner)
	for _, args := range []map[string]any{
		{"query": "成员名单确认"},
		{"schedule_id": sc.ID},
	} {
		result := callToolByName(t, toolset, "cancel_schedule", args)
		if !strings.Contains(result, "执行完成") || !strings.Contains(result, "没有未来执行") ||
			!strings.Contains(result, "历史记录继续保留") {
			t.Fatalf("cancel completed schedule result = %q", result)
		}
	}
	stored, err := s.ScheduleByID(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.ScheduleDone {
		t.Fatalf("completed schedule status changed to %q", stored.Status)
	}
}

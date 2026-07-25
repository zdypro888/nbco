package tools

import (
	"context"
	"encoding/json"
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

func TestUpdateSchedulePreservesAudienceAndAutomationIdentity(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "schedule-admin", true)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.StartSession(ctx, owner.ID, "test:schedule-tool-update", "eino")
	if err != nil {
		t.Fatal(err)
	}
	originalMessageID, err := s.AppendMessage(ctx, sess.ID, "user", "创建日本公司成员每日摘要")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateSchedule(ctx, &store.Schedule{
		UserID: owner.ID, CreatedBy: owner.ID, Kind: store.ScheduleDaily,
		Title: "日本公司成员每日摘要", Message: "汇总当天群消息",
		FireAt: time.Now().UTC().Add(time.Hour), DailyAt: "18:00",
		Target: store.ScheduleTargetSelf, Mode: store.ScheduleModeAI,
		SourceKind: store.ScheduleSourceTelegramGroupDigest, SourceKey: "-100123",
		SourceMessageID: &originalMessageID,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"schedule_id": sc.ID,
		"daily_at":    "19:00",
		"target":      store.ScheduleTargetAll, // Unknown fields must not widen scope.
	})
	if err != nil {
		t.Fatal(err)
	}
	updateMessageID, err := s.AppendMessage(ctx, sess.ID, "user", "把摘要改到十九点")
	if err != nil {
		t.Fatal(err)
	}
	var updateToolFound bool
	for _, tl := range scheduleTools(Deps{Store: s, TZ: tokyo}, owner) {
		if tl.Name != "update_schedule" {
			continue
		}
		updateToolFound = true
		out, err := tl.Handler(WithApprovalTurn(ctx, sess.ID, updateMessageID), raw)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "接收范围、内容、模式和来源绑定均保持不变") {
			t.Fatalf("update result = %q", out)
		}
	}
	if !updateToolFound {
		t.Fatal("update_schedule tool not found")
	}

	got, err := s.ScheduleByID(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sc.ID || got.Target != store.ScheduleTargetSelf || got.UserID != owner.ID ||
		got.Mode != store.ScheduleModeAI || got.Message != sc.Message || got.Title != sc.Title ||
		got.SourceKind != sc.SourceKind || got.SourceKey != sc.SourceKey {
		t.Fatalf("reschedule changed stable schedule identity: before=%+v after=%+v", sc, got)
	}
	if got.DailyAt != "19:00" || got.SourceMessageID == nil || *got.SourceMessageID != updateMessageID {
		t.Fatalf("reschedule timing/provenance = %+v", got)
	}
	localFire := got.FireAt.In(tokyo)
	if localFire.Hour() != 19 || localFire.Minute() != 0 {
		t.Fatalf("next fire = %s; want 19:00 Asia/Tokyo", localFire)
	}
}

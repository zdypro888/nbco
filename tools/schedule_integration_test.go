package tools

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/perm"
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
	if err := s.MarkScheduleDelivered(ctx, sc.ID, due[0].OccurrenceGeneration, *due[0].DeliveryClaimedAt, time.Now().UTC(), nil, true); err != nil {
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

func TestRecipientCanMuteIncomingBroadcastWithoutCancellingIt(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	boss := mkToolsUser(t, s, "broadcast-owner", true)
	member := mkToolsUser(t, s, "broadcast-recipient", false)
	sc, err := s.CreateSchedule(ctx, &store.Schedule{
		UserID: boss.ID, CreatedBy: boss.ID, Kind: store.ScheduleDaily,
		FireAt: time.Now().UTC().Add(time.Hour), DailyAt: "09:30",
		Target: store.ScheduleTargetAll, Mode: store.ScheduleModeMessage,
		Title: "全员早间通知", Message: "早上好",
	})
	if err != nil {
		t.Fatal(err)
	}
	toolset := scheduleTools(Deps{Store: s, TZ: time.UTC}, member)
	listed := callToolByName(t, toolset, "list_schedules", map[string]any{})
	if !strings.Contains(listed, "全员早间通知") || !strings.Contains(listed, "接收开启") {
		t.Fatalf("incoming schedule list = %q", listed)
	}
	muted := callToolByName(t, toolset, "set_schedule_subscription", map[string]any{
		"scope": "schedule", "schedule_id": sc.ID, "enabled": false,
	})
	if !strings.Contains(muted, "已停止接收") || !strings.Contains(muted, "不会修改创建者") {
		t.Fatalf("mute result = %q", muted)
	}
	if allowed, err := s.ScheduleDeliveryAllowed(ctx, member.ID, sc.ID); err != nil || allowed {
		t.Fatalf("muted delivery allowed=%t err=%v", allowed, err)
	}
	stored, err := s.ScheduleByID(ctx, sc.ID)
	if err != nil || stored.Status != store.ScheduleActive {
		t.Fatalf("recipient mute changed shared schedule: %+v err=%v", stored, err)
	}
}

func TestMandatoryScheduleRequiresDelegatedAuthorityAndCannotBeMuted(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	root := mkToolsUser(t, s, "mandatory-root", true)
	manager := mkToolsUser(t, s, "mandatory-manager", false)
	recipient := mkToolsUser(t, s, "mandatory-target", false)
	target := strconv.FormatInt(recipient.ID, 10)
	if err := s.GrantPerm(ctx, store.Grant{
		Kind: store.KindActive, UserID: manager.ID, Action: perm.ActSendMsg,
		Target: target, GrantedBy: root.ID,
	}); err != nil {
		t.Fatal(err)
	}
	createArgs := map[string]any{
		"target": target, "mode": store.ScheduleModeMessage,
		"content": "必须阅读的制度通知", "title": "制度通知",
		"recipient_policy": store.ScheduleRecipientMandatory,
		"at":               time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}
	toolset := scheduleTools(Deps{Store: s, TZ: time.UTC}, manager)
	denied := callToolByName(t, toolset, "schedule_once_push", createArgs)
	if !strings.Contains(denied, perm.ActManageMandatorySchedule) {
		t.Fatalf("mandatory schedule without authority = %q", denied)
	}
	if err := s.GrantPerm(ctx, store.Grant{
		Kind: store.KindActive, UserID: manager.ID, Action: perm.ActManageMandatorySchedule,
		Target: target, GrantedBy: root.ID,
	}); err != nil {
		t.Fatal(err)
	}
	created := callToolByName(t, toolset, "schedule_once_push", createArgs)
	if !strings.Contains(created, "已设置一次性推送") {
		t.Fatalf("mandatory schedule creation = %q", created)
	}

	items, err := s.SchedulesVisible(ctx, recipient.ID, false, store.ScheduleActive, 20)
	if err != nil || len(items) != 1 {
		t.Fatalf("recipient schedules = %+v, %v", items, err)
	}
	if items[0].RecipientPolicy != store.ScheduleRecipientMandatory {
		t.Fatalf("recipient policy = %q", items[0].RecipientPolicy)
	}
	recipientTools := scheduleTools(Deps{Store: s, TZ: time.UTC}, recipient)
	listed := callToolByName(t, recipientTools, "list_schedules", map[string]any{})
	if !strings.Contains(listed, "强制接收") {
		t.Fatalf("mandatory schedule list = %q", listed)
	}
	muted := callToolByName(t, recipientTools, "set_schedule_subscription", map[string]any{
		"scope": "schedule", "schedule_id": items[0].ID, "enabled": false,
	})
	if !strings.Contains(muted, "不能由收件人退订") {
		t.Fatalf("mandatory schedule mute = %q", muted)
	}
	cancelled := callToolByName(t, recipientTools, "cancel_schedule", map[string]any{"schedule_id": items[0].ID})
	if !strings.Contains(cancelled, "不能取消或退订") {
		t.Fatalf("mandatory schedule cancel = %q", cancelled)
	}
	if err := s.RevokePerm(ctx, root.ID, store.KindActive, manager.ID, perm.ActManageMandatorySchedule, target); err != nil {
		t.Fatal(err)
	}
	deniedUpdate := callToolByName(t, toolset, "update_schedule", map[string]any{
		"schedule_id": items[0].ID,
		"at":          time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339),
	})
	if !strings.Contains(deniedUpdate, "当前仍对全部目标拥有") {
		t.Fatalf("mandatory schedule update after authority revocation = %q", deniedUpdate)
	}
	downgraded := callToolByName(t, toolset, "update_schedule", map[string]any{
		"schedule_id": items[0].ID, "recipient_policy": store.ScheduleRecipientOptional,
	})
	if !strings.Contains(downgraded, "接收策略 optional") {
		t.Fatalf("mandatory schedule downgrade = %q", downgraded)
	}
	muted = callToolByName(t, recipientTools, "set_schedule_subscription", map[string]any{
		"scope": "schedule", "schedule_id": items[0].ID, "enabled": false,
	})
	if !strings.Contains(muted, "已停止接收") {
		t.Fatalf("optional schedule mute after downgrade = %q", muted)
	}
}

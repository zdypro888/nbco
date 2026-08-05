package sched

import (
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/store"
)

var tz = time.FixedZone("CST", 8*3600)

func task(id int64, title string, assignee int64, deadline *time.Time) *store.Task {
	return &store.Task{ID: id, Title: title, AssigneeID: assignee, Status: store.TaskPending, Deadline: deadline}
}

func TestRenderTodos(t *testing.T) {
	dl := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	out := renderTodos([]*store.Task{
		task(1, "写方案", 2, &dl),
		task(2, "对接客户", 2, nil),
	}, tz)
	if !strings.Contains(out, "2 个待办") {
		t.Errorf("缺待办总数: %q", out)
	}
	if !strings.Contains(out, "#1") || !strings.Contains(out, "写方案") {
		t.Errorf("缺任务行: %q", out)
	}
	if !strings.Contains(out, "07-05 18:00") { // UTC+8
		t.Errorf("截止时间应按公司时区: %q", out)
	}
}

func TestRenderTodosTruncated(t *testing.T) {
	var ts []*store.Task
	for i := range int64(25) {
		ts = append(ts, task(i+1, "任务", 2, nil))
	}
	out := renderTodos(ts, tz)
	if !strings.Contains(out, "…等共 25 个") {
		t.Errorf("超过 %d 条应截断: %q", summaryMaxTasks, out)
	}
}

func TestRenderDigest(t *testing.T) {
	dl := time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)
	out := renderDigest(
		&store.TaskStats{Open: 12, Overdue: 3, Awaiting: 4, DoneSince: 5},
		[]*store.Task{task(7, "上线支付", 42, &dl)},
		map[int64]string{42: "张三"},
		tz,
	)
	for _, want := range []string{"进行中任务 12", "已过期 3", "待验收 4", "验收通过 5", "#7 上线支付", "张三", "07-01 10:00", "…等共 3 个"} {
		if !strings.Contains(out, want) {
			t.Errorf("日报缺 %q:\n%s", want, out)
		}
	}
}

func TestRenderDigestUnknownUser(t *testing.T) {
	dl := time.Now()
	out := renderDigest(
		&store.TaskStats{Open: 1, Overdue: 1},
		[]*store.Task{task(9, "x", 77, &dl)},
		map[int64]string{}, tz,
	)
	if !strings.Contains(out, "用户77") {
		t.Errorf("未知用户应回退为 ID: %q", out)
	}
}

func TestRenderOrphanNotice(t *testing.T) {
	var ts []*store.Task
	for i := range int64(12) {
		ts = append(ts, &store.Task{ID: i + 1, Title: "任务", AssigneeID: 99})
	}
	out := renderOrphanNotice(ts, map[int64]string{99: "旧 worker"})
	for _, want := range []string{"执行人已停用", "#1", "旧 worker", "reassign_task", "…等共 12 个"} {
		if !strings.Contains(out, want) {
			t.Errorf("孤儿任务提醒缺 %q:\n%s", want, out)
		}
	}
}

func TestDeliveryPoolRoutesAIWithoutSendPool(t *testing.T) {
	s := &Scheduler{aiPool: newPool(1), sendPool: newPool(1), orch: &chat.Orchestrator{}}
	if s.deliveryPool(store.ScheduleModeAI) != s.aiPool {
		t.Fatal("AI schedule should use aiPool directly")
	}
	if s.deliveryPool(store.ScheduleModeMessage) != s.sendPool {
		t.Fatal("message schedule should use sendPool")
	}
	s.orch = nil
	if s.deliveryPool(store.ScheduleModeAI) != s.sendPool {
		t.Fatal("AI schedule without orchestrator falls back to template sendPool")
	}
}

func TestNextRepeatFireSkipsLongBacklogInConstantTime(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 30, 0, time.UTC)
	fireAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	got := nextRepeatFire(fireAt, now, time.Minute)
	if !got.Equal(time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC)) {
		t.Fatalf("unexpected next repeat fire: %s", got)
	}
	if !got.After(now) || got.Sub(now) > time.Minute {
		t.Fatalf("next fire must be the first aligned time after now: %s", got)
	}
}

func TestDailyDeliveryAllowedUsesActualDeliveryDay(t *testing.T) {
	friday := time.Date(2026, 7, 10, 9, 0, 0, 0, tz)
	sunday := friday.Add(48 * time.Hour)
	if !dailyDeliveryAllowed(friday, "1,2,3,4,5", tz) {
		t.Fatal("Friday should be an allowed workday")
	}
	if dailyDeliveryAllowed(sunday, "1,2,3,4,5", tz) {
		t.Fatal("a Friday occurrence caught up on Sunday must not be delivered")
	}
}

func TestAutomationWindowsRemainOpenForRetries(t *testing.T) {
	mondayMorning := time.Date(2026, 8, 3, 8, 30, 0, 0, tz)
	if due, _ := weeklyAutomationWindow(mondayMorning, 9); due {
		t.Fatal("weekly run must not start before its configured hour")
	}
	wednesday := time.Date(2026, 8, 5, 12, 0, 0, 0, tz)
	if due, expires := weeklyAutomationWindow(wednesday, 9); !due || !expires.Equal(time.Date(2026, 8, 10, 0, 0, 0, 0, tz).UTC()) {
		t.Fatalf("weekly retry window due=%v expires=%s", due, expires)
	}
	lateDaily := time.Date(2026, 8, 5, 23, 30, 0, 0, tz)
	if due, expires := dailyAutomationWindow(lateDaily, 9); !due || !expires.Equal(time.Date(2026, 8, 6, 0, 0, 0, 0, tz).UTC()) {
		t.Fatalf("daily retry window due=%v expires=%s", due, expires)
	}
	thirdDay := time.Date(2026, 8, 3, 18, 0, 0, 0, tz)
	if due, expires := monthlyAutomationWindow(thirdDay, 9, 1); !due || !expires.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, tz).UTC()) {
		t.Fatalf("monthly retry window due=%v expires=%s", due, expires)
	}
	fourthDay := time.Date(2026, 8, 4, 0, 0, 0, 0, tz)
	if due, _ := monthlyAutomationWindow(fourthDay, 9, 1); !due {
		t.Fatal("monthly run must remain retryable through the occurrence month")
	}
	beforeStart := time.Date(2026, 8, 2, 8, 59, 0, 0, tz)
	if due, _ := monthlyAutomationWindow(beforeStart, 9, 2); due {
		t.Fatal("monthly run must not start before its configured day and hour")
	}
}

func TestKnowledgeBatchOccurrenceTracksExactCandidateSet(t *testing.T) {
	first := knowledgeBatchOccurrence("2026-08", []*store.LearningCandidate{{ID: 9}, {ID: 8}, {ID: 7}})
	same := knowledgeBatchOccurrence("2026-08", []*store.LearningCandidate{{ID: 9}, {ID: 8}, {ID: 7}})
	changed := knowledgeBatchOccurrence("2026-08", []*store.LearningCandidate{{ID: 9}, {ID: 7}})
	if first != same || first == changed {
		t.Fatalf("occurrence keys first=%q same=%q changed=%q", first, same, changed)
	}
}

func TestWeeklyReportDirectiveUsesExplicitPeriodAndPreloadedFacts(t *testing.T) {
	s := &Scheduler{}
	directive := s.weeklyReportDirective(
		time.Date(2026, 8, 5, 12, 0, 0, 0, tz),
		"2026-W32",
		&store.User{Name: "PRO"},
		"全局：进行中 2",
	)
	for _, want := range []string{"2026-W32", "2026-08-05", "<company_facts>全局：进行中 2</company_facts>"} {
		if !strings.Contains(directive, want) {
			t.Fatalf("weekly directive missing %q: %s", want, directive)
		}
	}
	if strings.Contains(directive, "今天是") || strings.Contains(directive, "company_overview") {
		t.Fatalf("weekly directive retained relative time or tool rediscovery: %s", directive)
	}
}

func TestHumanRecipientSkipsWorkers(t *testing.T) {
	if !humanRecipient(&store.User{Status: store.UserActive}) {
		t.Fatal("active human should be a schedule recipient")
	}
	if humanRecipient(&store.User{Status: store.UserActive, IsWorker: true}) {
		t.Fatal("worker accounts should not receive human schedule notifications")
	}
	if humanRecipient(&store.User{Status: "disabled"}) {
		t.Fatal("inactive users should not receive schedule notifications")
	}
	if humanRecipient(nil) {
		t.Fatal("nil user should not be a recipient")
	}
}

func TestScheduleAIDirectiveCarriesStructuredContext(t *testing.T) {
	authored := time.Date(2026, 7, 7, 4, 51, 0, 0, time.UTC)
	occurrence := time.Date(2026, 7, 13, 2, 0, 0, 0, time.UTC)
	generated := occurrence.Add(3 * time.Minute)
	source := &store.ChatMessage{Content: "刚才负责人说应用被下架了", CreatedAt: authored}
	delivery := &store.ScheduleDelivery{Message: "提醒跟进这个事情", OccurrenceAt: occurrence, CreatedAt: occurrence}

	out := renderScheduleAIDirective(delivery, authored, generated, source, tz)
	for _, want := range []string{
		`"schedule_created_at":"2026-07-07T12:51:00+08:00"`,
		`"planned_at":"2026-07-13T10:00:00+08:00"`,
		`"generated_at":"2026-07-13T10:03:00+08:00"`,
		`"at":"2026-07-07T12:51:00+08:00"`,
		"刚才负责人说应用被下架了",
		"提醒跟进这个事情",
		"<schedule_context>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("directive missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTelegramGroupDigestDirectiveCarriesClosedFacts(t *testing.T) {
	from := time.Date(2026, 7, 23, 0, 0, 0, 0, tz)
	to := from.Add(19 * time.Hour)
	page := store.ChannelMessagePage{
		Total: 2,
		Messages: []store.ChatMessage{
			{Role: "user", Content: "【Alice】BrandOS 已交付", CreatedAt: from.Add(9 * time.Hour)},
			{Role: "user", Content: "【Bob】支付仍需复测", CreatedAt: from.Add(10 * time.Hour)},
		},
	}
	got := renderTelegramGroupDigestDirective("日本公司成员", "只看风险", page, from, to, tz)
	for _, want := range []string{
		"Telegram 群摘要",
		`"group":"日本公司成员"`,
		`"recorded_messages":2`,
		`"included_messages":2`,
		"BrandOS 已交付",
		"支付仍需复测",
		"只看风险",
		"不得补充模型常识",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("group digest directive missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "list_telegram_group_messages") {
		t.Fatalf("OneShot group digest must not depend on a tool loop:\n%s", got)
	}
}

func TestRenderEmptyTelegramGroupDigestDoesNotInferAbsence(t *testing.T) {
	from := time.Date(2026, 7, 23, 0, 0, 0, 0, tz)
	got := renderEmptyTelegramGroupDigest("日本公司成员", from, from.Add(19*time.Hour), tz)
	for _, want := range []string{"没有保存到群消息", "不能据此判断", "绝对无人发言", "成员是否休假"} {
		if !strings.Contains(got, want) {
			t.Fatalf("empty digest missing %q: %s", want, got)
		}
	}
}

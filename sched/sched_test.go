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

func TestSchedulePoolRoutesAIWithoutSendPool(t *testing.T) {
	s := &Scheduler{aiPool: newPool(1), sendPool: newPool(1), orch: &chat.Orchestrator{}}
	if s.schedulePool(&store.Schedule{Mode: store.ScheduleModeAI}) != s.aiPool {
		t.Fatal("AI schedule should use aiPool directly")
	}
	if s.schedulePool(&store.Schedule{Mode: store.ScheduleModeMessage}) != s.sendPool {
		t.Fatal("message schedule should use sendPool")
	}
	s.orch = nil
	if s.schedulePool(&store.Schedule{Mode: store.ScheduleModeAI}) != s.sendPool {
		t.Fatal("AI schedule without orchestrator falls back to template sendPool")
	}
}

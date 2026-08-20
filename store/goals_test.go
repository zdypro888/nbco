package store

import (
	"context"
	"testing"
	"time"
)

// mkTaskM 建一个挂里程碑的任务（milestoneID 可为 nil）。集成测试辅助。
func mkTaskM(t *testing.T, s *Store, ctx context.Context, projectID, assigner, assignee int64, title string, milestoneID *int64) *Task {
	t.Helper()
	tk, err := s.CreateTask(ctx, &Task{
		ProjectID: projectID, AssignerID: assigner, AssigneeID: assignee, Title: title, MilestoneID: milestoneID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestGoalMilestoneCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)

	deadline := time.Now().Add(30 * 24 * time.Hour)
	g, err := s.CreateGoal(ctx, "提升留存", "把付费留存提到 60%", boss.ID, &deadline)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != GoalActive {
		t.Fatalf("新建目标状态 = %s，预期 active", g.Status)
	}
	newTitle := "提升付费留存"
	g, err = s.UpdateGoal(ctx, g.ID, &newTitle, nil, nil)
	if err != nil || g.Title != newTitle {
		t.Fatalf("UpdateGoal: title=%s err=%v", g.Title, err)
	}
	if err := s.SetGoalStatus(ctx, g.ID, GoalAchieved); err != nil {
		t.Fatal(err)
	}
	if g, _ = s.GoalByID(ctx, g.ID); g.Status != GoalAchieved {
		t.Fatalf("SetGoalStatus 后 = %s", g.Status)
	}
	// ListGoals activeOnly
	active, _ := s.ListGoals(ctx, true)
	if len(active) != 0 {
		t.Errorf("activeOnly 应排除 achieved，实际 %d", len(active))
	}
	all, _ := s.ListGoals(ctx, false)
	if len(all) != 1 {
		t.Errorf("ListGoals(false) 应有 1 个，实际 %d", len(all))
	}

	m, err := s.CreateMilestone(ctx, g.ID, "基线调研", "测当前留存", nil)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := s.MilestonesOfGoal(ctx, g.ID)
	if err != nil || len(ms) != 1 || ms[0].ID != m.ID {
		t.Fatalf("MilestonesOfGoal = %+v err=%v", ms, err)
	}
	m, err = s.UpdateMilestone(ctx, m.ID, nil, nil, &deadline)
	if err != nil || m.Deadline == nil {
		t.Fatalf("UpdateMilestone err=%v m=%+v", err, m)
	}
}

func TestMilestoneTaskCounts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	g, _ := s.CreateGoal(ctx, "G", "", boss.ID, nil)
	m, _ := s.CreateMilestone(ctx, g.ID, "M", "", nil)
	mid := m.ID

	mkTaskM(t, s, ctx, pj.ID, boss.ID, alice.ID, "T1", &mid) // pending
	t2 := mkTaskM(t, s, ctx, pj.ID, boss.ID, alice.ID, "T2", &mid)
	t3 := mkTaskM(t, s, ctx, pj.ID, boss.ID, alice.ID, "T3", &mid)
	if _, _, err := s.SubmitTask(ctx, t2.ID); err != nil { // → done
		t.Fatal(err)
	}
	if _, _, err := s.SubmitTask(ctx, t3.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptTask(ctx, t3.ID); err != nil { // → accepted
		t.Fatal(err)
	}
	// split 父任务（挂里程碑）+ 子任务（透传 milestone，pending）
	parent := mkTaskM(t, s, ctx, pj.ID, boss.ID, alice.ID, "Parent", &mid)
	if _, err := s.SplitTask(ctx, parent.ID, []*Task{
		{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "子", MilestoneID: &mid},
	}); err != nil {
		t.Fatal(err)
	}

	counts, err := s.MilestoneTaskCounts(ctx, []int64{mid})
	if err != nil {
		t.Fatal(err)
	}
	p := counts[mid]
	// 非split: T1,T2,T3,子 = 4（split 父不计入 Total）
	if p.Total != 4 {
		t.Errorf("Total = %d，预期 4（排除 split 父）", p.Total)
	}
	if p.Open != 2 || p.Awaiting != 1 || p.Accepted != 1 {
		t.Errorf("Open=%d Awaiting=%d Accepted=%d，预期 2/1/1", p.Open, p.Awaiting, p.Accepted)
	}
}

func TestGoalMilestoneCountsAndRollup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	g, _ := s.CreateGoal(ctx, "G", "", boss.ID, nil)
	m1, _ := s.CreateMilestone(ctx, g.ID, "M1", "", nil)
	s.CreateMilestone(ctx, g.ID, "M2", "", nil)
	s.SetMilestoneStatus(ctx, m1.ID, GoalAchieved)

	gmc, err := s.GoalMilestoneCounts(ctx, []int64{g.ID})
	if err != nil || gmc[g.ID].Total != 2 || gmc[g.ID].Achieved != 1 {
		t.Fatalf("GoalMilestoneCounts = %+v err=%v，预期 Total=2 Achieved=1", gmc[g.ID], err)
	}

	m2, _ := s.MilestonesOfGoal(ctx, g.ID) // m2 是第二个
	m2ID := m2[1].ID
	mkTaskM(t, s, ctx, pj.ID, boss.ID, alice.ID, "T", &m2ID)
	rollup, err := s.GoalTaskRollup(ctx, []int64{g.ID})
	if err != nil || rollup[g.ID].Total != 1 {
		t.Fatalf("GoalTaskRollup = %+v err=%v，预期 Total=1", rollup[g.ID], err)
	}
}

func TestCreateMilestoneTasksAtomic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	g, _ := s.CreateGoal(ctx, "G", "", boss.ID, nil)
	m, _ := s.CreateMilestone(ctx, g.ID, "M", "", nil)
	mid := m.ID

	// 第二条 depends_on 不存在的任务 → 整批回滚（T1 也不留）
	_, err := s.CreateMilestoneTasks(ctx, []*Task{
		{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T1", MilestoneID: &mid},
		{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T2", DependsOn: []int64{999999}, MilestoneID: &mid},
	})
	if err == nil {
		t.Fatal("depends_on 不存在应报错")
	}
	ts, _ := s.TasksOfMilestone(ctx, mid)
	if len(ts) != 0 {
		t.Errorf("整批回滚后里程碑下应有 0 任务，实际 %d", len(ts))
	}

	// 正常批量：两条都建
	created, err := s.CreateMilestoneTasks(ctx, []*Task{
		{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T1", MilestoneID: &mid},
		{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T2", MilestoneID: &mid},
	})
	if err != nil || len(created) != 2 {
		t.Fatalf("批量建应成功：created=%d err=%v", len(created), err)
	}
	for _, c := range created {
		if c.MilestoneID == nil || *c.MilestoneID != mid {
			t.Errorf("任务 milestone_id 未落库 = %v", c.MilestoneID)
		}
	}
}

func TestDeleteGoalCascades(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	g, _ := s.CreateGoal(ctx, "G", "", boss.ID, nil)
	m, _ := s.CreateMilestone(ctx, g.ID, "M", "", nil)
	tk := mkTaskM(t, s, ctx, pj.ID, boss.ID, alice.ID, "T", &m.ID)

	if _, err := s.pool.Exec(ctx, `DELETE FROM goals WHERE id = $1`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MilestoneByID(ctx, m.ID); err == nil {
		t.Error("删 goal 后里程碑应级联消失")
	}
	got, err := s.TaskByID(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.MilestoneID != nil {
		t.Errorf("删里程碑后 task.milestone_id 应 NULL，实际 %v", got.MilestoneID)
	}
}

func TestSetTaskMilestone(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	g, _ := s.CreateGoal(ctx, "G", "", boss.ID, nil)
	m, _ := s.CreateMilestone(ctx, g.ID, "M", "", nil)
	tk := mkTask(t, s, pj.ID, boss.ID, alice.ID, "T", nil) // 无里程碑

	if err := s.SetTaskMilestone(ctx, tk.ID, &m.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TaskByID(ctx, tk.ID); got.MilestoneID == nil || *got.MilestoneID != m.ID {
		t.Errorf("绑定后 milestone_id = %v", got.MilestoneID)
	}
	if err := s.SetTaskMilestone(ctx, tk.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TaskByID(ctx, tk.ID); got.MilestoneID != nil {
		t.Errorf("解绑后 milestone_id 应 nil，实际 %v", got.MilestoneID)
	}
}

func TestSplitTaskCarriesMilestone(t *testing.T) {
	// store.SplitTask 忠实落子任务的 MilestoneID（继承由 tools 层 split_my_task 透传 parent.MilestoneID 实现）。
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	g, _ := s.CreateGoal(ctx, "G", "", boss.ID, nil)
	m, _ := s.CreateMilestone(ctx, g.ID, "M", "", nil)
	parent := mkTask(t, s, pj.ID, boss.ID, alice.ID, "Parent", nil)

	subs, err := s.SplitTask(ctx, parent.ID, []*Task{
		{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "子", MilestoneID: &m.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if subs[0].MilestoneID == nil || *subs[0].MilestoneID != m.ID {
		t.Errorf("子任务应落 milestone_id=%d，实际 %v", m.ID, subs[0].MilestoneID)
	}
}

// TestGoalDeadlineClaims 镜像 TestDeadlineClaims：原子认领 + 租约重试 + ack。
func TestGoalDeadlineClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	now := time.Now().UTC()

	soon := now.Add(2 * time.Hour)
	g, err := s.CreateGoal(ctx, "提升留存", "", boss.ID, &soon)
	if err != nil {
		t.Fatal(err)
	}

	// 临近提醒：claim 一次，租约内重复为空；租约过期可重试，ack 后才消失。
	warn, err := s.DueGoalDeadlineReminders(ctx, now, 24*time.Hour)
	if err != nil || len(warn) != 1 || warn[0].ID != g.ID {
		t.Fatalf("首次认领 = %d err=%v", len(warn), err)
	}
	deadlineGeneration := warn[0].DeadlineGeneration
	if warn, _ = s.DueGoalDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("租约内重复认领应为空, got %d", len(warn))
	}
	mustExec(t, s, `UPDATE goals SET deadline_reminder_claimed_at = now() - interval '20 minutes' WHERE id = $1`, g.ID)
	if warn, err = s.DueGoalDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 {
		t.Fatalf("租约过期应可重试, got %d err=%v", len(warn), err)
	}
	if err := s.MarkGoalDeadlineReminderSent(ctx, g.ID, deadlineGeneration, now); err != nil {
		t.Fatal(err)
	}
	if warn, _ = s.DueGoalDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("ack 后不应再认领, got %d", len(warn))
	}
	failedSoon := now.Add(4 * time.Hour)
	failedGoal, err := s.CreateGoal(ctx, "投递未确认", "", boss.ID, &failedSoon)
	if err != nil {
		t.Fatal(err)
	}
	if warn, err = s.DueGoalDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 || warn[0].ID != failedGoal.ID {
		t.Fatalf("未确认目标提醒认领=%v err=%v", warn, err)
	}
	if err := s.MarkGoalDeadlineReminderAttempt(ctx, failedGoal.ID, warn[0].DeadlineGeneration, now, false); err != nil {
		t.Fatal(err)
	}
	var attemptedAt, sentAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT deadline_reminder_attempted_at, deadline_reminded_at FROM goals WHERE id=$1`, failedGoal.ID).
		Scan(&attemptedAt, &sentAt); err != nil || attemptedAt == nil || sentAt != nil {
		t.Fatalf("未确认目标提醒 attempted=%v sent=%v err=%v", attemptedAt, sentAt, err)
	}

	// 改截止到过去 → 过期通知触发；租约/ack 同样成立。
	past := now.Add(-time.Hour)
	if _, err := s.UpdateGoal(ctx, g.ID, nil, nil, &past); err != nil {
		t.Fatal(err)
	}
	over, err := s.DueGoalOverdueNotices(ctx, now)
	if err != nil || len(over) != 1 {
		t.Fatalf("改期后过期通知 = %d err=%v", len(over), err)
	}
	if over, _ = s.DueGoalOverdueNotices(ctx, now); len(over) != 0 {
		t.Fatalf("过期通知租约内重复认领应为空, got %d", len(over))
	}
	mustExec(t, s, `UPDATE goals SET overdue_notice_claimed_at = now() - interval '20 minutes' WHERE id = $1`, g.ID)
	if over, err = s.DueGoalOverdueNotices(ctx, now); err != nil || len(over) != 1 {
		t.Fatalf("过期通知租约过期应可重试, got %d err=%v", len(over), err)
	}
	if err := s.MarkGoalOverdueNoticeSent(ctx, g.ID, over[0].DeadlineGeneration, now); err != nil {
		t.Fatal(err)
	}
	if over, _ = s.DueGoalOverdueNotices(ctx, now); len(over) != 0 {
		t.Fatalf("过期通知 ack 后不应再认领, got %d", len(over))
	}

	// 改到新截止时间会清掉提醒/通知状态，让新 deadline 重新进入调度闭环。
	nextSoon := now.Add(3 * time.Hour)
	if _, err := s.UpdateGoal(ctx, g.ID, nil, nil, &nextSoon); err != nil {
		t.Fatal(err)
	}
	if warn, err = s.DueGoalDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 {
		t.Fatalf("改到新截止时间后应重新临期提醒, got %d err=%v", len(warn), err)
	}
	pastAgain := now.Add(-2 * time.Hour)
	if _, err := s.UpdateGoal(ctx, g.ID, nil, nil, &pastAgain); err != nil {
		t.Fatal(err)
	}
	if over, err = s.DueGoalOverdueNotices(ctx, now); err != nil || len(over) != 1 {
		t.Fatalf("改到新的过期时间后应重新过期通知, got %d err=%v", len(over), err)
	}

	// achieved 状态的目标不再被认领（达成即关闭跟踪）。
	if err := s.SetGoalStatus(ctx, g.ID, GoalAchieved); err != nil {
		t.Fatal(err)
	}
	if over, _ = s.DueGoalOverdueNotices(ctx, now); len(over) != 0 {
		t.Errorf("已达成目标不应再触发过期通知, got %d", len(over))
	}
	if warn, _ = s.DueGoalDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Errorf("已达成目标不应再触发截止提醒, got %d", len(warn))
	}
}

// TestMilestoneDeadlineClaims 镜像 TestGoalDeadlineClaims：里程碑原子认领 + 租约重试 + ack。
func TestMilestoneDeadlineClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	now := time.Now().UTC()

	g, err := s.CreateGoal(ctx, "G", "", boss.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	soon := now.Add(2 * time.Hour)
	m, err := s.CreateMilestone(ctx, g.ID, "基线", "", &soon)
	if err != nil {
		t.Fatal(err)
	}

	// 临近提醒：claim 一次，租约内重复为空；ack 后消失。
	warn, err := s.DueMilestoneDeadlineReminders(ctx, now, 24*time.Hour)
	if err != nil || len(warn) != 1 || warn[0].ID != m.ID {
		t.Fatalf("首次认领 = %d err=%v", len(warn), err)
	}
	deadlineGeneration := warn[0].DeadlineGeneration
	if warn, _ = s.DueMilestoneDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("租约内重复认领应为空, got %d", len(warn))
	}
	if err := s.MarkMilestoneDeadlineReminderSent(ctx, m.ID, deadlineGeneration, now); err != nil {
		t.Fatal(err)
	}
	if warn, _ = s.DueMilestoneDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("ack 后不应再认领, got %d", len(warn))
	}

	// 改到过去 → 过期通知。
	past := now.Add(-time.Hour)
	if _, err := s.UpdateMilestone(ctx, m.ID, nil, nil, &past); err != nil {
		t.Fatal(err)
	}
	over, err := s.DueMilestoneOverdueNotices(ctx, now)
	if err != nil || len(over) != 1 {
		t.Fatalf("改期后过期通知 = %d err=%v", len(over), err)
	}
	if err := s.MarkMilestoneOverdueNoticeSent(ctx, m.ID, over[0].DeadlineGeneration, now); err != nil {
		t.Fatal(err)
	}
	if over, _ = s.DueMilestoneOverdueNotices(ctx, now); len(over) != 0 {
		t.Fatalf("过期通知 ack 后不应再认领, got %d", len(over))
	}

	// 改到新的未来截止时间会清掉提醒/通知状态，让里程碑重新进入调度闭环。
	nextSoon := now.Add(3 * time.Hour)
	if _, err := s.UpdateMilestone(ctx, m.ID, nil, nil, &nextSoon); err != nil {
		t.Fatal(err)
	}
	if warn, err = s.DueMilestoneDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 {
		t.Fatalf("改到新截止时间后应重新临期提醒, got %d err=%v", len(warn), err)
	}
	pastAgain := now.Add(-2 * time.Hour)
	if _, err := s.UpdateMilestone(ctx, m.ID, nil, nil, &pastAgain); err != nil {
		t.Fatal(err)
	}
	if over, err = s.DueMilestoneOverdueNotices(ctx, now); err != nil || len(over) != 1 {
		t.Fatalf("改到新的过期时间后应重新过期通知, got %d err=%v", len(over), err)
	}

	// achieved 的里程碑不再被认领。
	if err := s.SetMilestoneStatus(ctx, m.ID, GoalAchieved); err != nil {
		t.Fatal(err)
	}
	if over, _ = s.DueMilestoneOverdueNotices(ctx, now); len(over) != 0 {
		t.Errorf("已达成里程碑不应再触发过期通知, got %d", len(over))
	}
}

// TestReassignTaskPreservesHistory 改派保留进度历史并重置业务状态。
func TestReassignTaskPreservesHistory(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bob := mkUser(t, s, "bob", false)
	pj := mkProject(t, s, boss.ID)
	tk, err := s.CreateTask(ctx, &Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T"})
	if err != nil {
		t.Fatal(err)
	}
	// 旧执行人写了进度，并进入 in_progress（模拟领取）。
	if _, err := s.UpdateTaskStatus(ctx, tk.ID, TaskInProgress); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProgress(ctx, tk.ID, alice.ID, "已调研，待实施"); err != nil {
		t.Fatal(err)
	}

	// 改派给 bob。
	tk, err = s.ReassignTask(ctx, tk.ID, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tk.AssigneeID != bob.ID {
		t.Errorf("改派后 assignee=%d, want %d", tk.AssigneeID, bob.ID)
	}
	if tk.Status != TaskPending {
		t.Errorf("改派后 status=%s, want pending（让新执行人重新领取）", tk.Status)
	}
	// 进度历史保留（同一 task_id）。
	prog, err := s.ProgressOf(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prog) != 1 || prog[0].Content != "已调研，待实施" {
		t.Errorf("改派后进度历史应保留, got %+v", prog)
	}
	if tk.NudgeCount != 0 {
		t.Errorf("改派后 nudge_count 应重置, got %d", tk.NudgeCount)
	}
}

// TestReassignTaskRejectsTerminalStatuses store 层状态守卫：done/accepted/split 不应被改派。
func TestReassignTaskRejectsTerminalStatuses(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bob := mkUser(t, s, "bob", false)
	pj := mkProject(t, s, boss.ID)
	// done：提交待验收。
	tk, _ := s.CreateTask(ctx, &Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T"})
	if _, _, err := s.SubmitTask(ctx, tk.ID); err != nil { // → done
		t.Fatal(err)
	}
	if _, err := s.ReassignTask(ctx, tk.ID, bob.ID); err == nil {
		t.Error("改派 done 任务应在 store 层被拒（防丢失验收记录）")
	}
	// accepted：已验收终态。
	tk2, _ := s.CreateTask(ctx, &Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: boss.ID, Title: "T2"})
	if _, _, err := s.SubmitTask(ctx, tk2.ID); err != nil { // 自派 → accepted
		t.Fatal(err)
	}
	if got, _ := s.TaskByID(ctx, tk2.ID); got.Status != TaskAccepted {
		t.Fatalf("自派提交应直达 accepted, got %s", got.Status)
	}
	if _, err := s.ReassignTask(ctx, tk2.ID, bob.ID); err == nil {
		t.Error("改派 accepted 任务应在 store 层被拒")
	}
}

// TestAIUsageGoalAttribution 目标成本归因：RecordAIUsage 带 goal_id + 按目标聚合查询。
func TestAIUsageGoalAttribution(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	g1, _ := s.CreateGoal(ctx, "提升留存", "", boss.ID, nil)
	g2, _ := s.CreateGoal(ctx, "扩张市场", "", boss.ID, nil)

	// g1 花了两次，g2 一次，一次未归因（对话路径）。
	g1id := g1.ID
	g2id := g2.ID
	if err := s.RecordAIUsage(ctx, AIUsage{UserID: boss.ID, Kind: "worker_llm", Model: "m", InputTokens: 100, OutputTokens: 50, GoalID: &g1id}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAIUsage(ctx, AIUsage{UserID: boss.ID, Kind: "worker_llm", Model: "m", InputTokens: 200, OutputTokens: 100, GoalID: &g1id}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAIUsage(ctx, AIUsage{UserID: boss.ID, Kind: "worker_llm", Model: "m", InputTokens: 80, OutputTokens: 20, GoalID: &g2id}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAIUsage(ctx, AIUsage{UserID: boss.ID, Kind: "telegram", Model: "m", InputTokens: 500, OutputTokens: 500}); err != nil {
		t.Fatal(err) // 未归因
	}

	rows, err := s.AIUsageByGoalSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 仅 2 个目标行，未归因的不在内；g1 token 最高排在前。
	if len(rows) != 2 {
		t.Fatalf("按目标聚合 = %d 行, want 2（未归因不计）", len(rows))
	}
	if rows[0].GoalID != g1.ID || rows[0].Calls != 2 || rows[0].InputTokens != 300 {
		t.Errorf("榜首应为 g1（300 in / 2 次）, got %+v", rows[0])
	}
	if rows[1].GoalID != g2.ID || rows[1].Calls != 1 {
		t.Errorf("次席应为 g2, got %+v", rows[1])
	}
	// GoalIDOfTask：无里程碑的任务 → nil；挂里程碑的 → 解析出 goal_id。
	pj := mkProject(t, s, boss.ID)
	tk, _ := s.CreateTask(ctx, &Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: boss.ID, Title: "T"})
	if gid, err := s.GoalIDOfTask(ctx, tk.ID); err != nil || gid != nil {
		t.Errorf("无里程碑任务 GoalIDOfTask = %v err=%v, want nil", gid, err)
	}
	m, _ := s.CreateMilestone(ctx, g1.ID, "M", "", nil)
	tk2, _ := s.CreateTask(ctx, &Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: boss.ID, Title: "T2", MilestoneID: &m.ID})
	if gid, err := s.GoalIDOfTask(ctx, tk2.ID); err != nil || gid == nil || *gid != g1.ID {
		t.Errorf("挂里程碑任务 GoalIDOfTask = %v err=%v, want %d", gid, err, g1.ID)
	}
}

package store

// 集成测试：需要真实 PostgreSQL，设置 NBCO_TEST_PG_DSN 后运行（CI 提供服务容器）：
//
//	NBCO_TEST_PG_DSN='postgres://nbco:nbco@127.0.0.1:5432/nbco_test?sslmode=disable' go test ./store/
//
// 未设置时自动跳过。每个测试开始时清空全库。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 NBCO_TEST_PG_DSN，跳过 store 集成测试")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, 7767002); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, 7767002)
		conn.Release()
	})
	if _, err := s.pool.Exec(ctx,
		`TRUNCATE users, projects, roles, bind_keys, audit_log, knowledge, kv_state, info_fields, ai_usage, pending_approvals, goals RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	// TRUNCATE 会清掉迁移种入的内置数据；重放全部 seed 迁移（均幂等），
	// 保证每个测试从"新部署"状态出发。
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.Contains(e.Name(), "seed") {
			continue
		}
		seed, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, string(seed)); err != nil {
			t.Fatalf("重放种子 %s: %v", e.Name(), err)
		}
	}
	return s
}

func mkUser(t *testing.T, s *Store, name string, super bool) *User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, super, Identity{Provider: "test", ExternalID: name})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mkProject(t *testing.T, s *Store, creatorID int64) *Project {
	t.Helper()
	p, err := s.CreateProject(context.Background(), "测试项目", "", creatorID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mkTask(t *testing.T, s *Store, projectID, assigner, assignee int64, title string, deadline *time.Time) *Task {
	t.Helper()
	tk, err := s.CreateTask(context.Background(), &Task{
		ProjectID: projectID, AssignerID: assigner, AssigneeID: assignee, Title: title, Deadline: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func mustExec(t *testing.T, s *Store, sql string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestBindKeyFlow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	admin := mkUser(t, s, "admin", true)

	bk, err := s.CreateBindKey(ctx, admin.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, invitedBy, err := s.BindUserWithKey(ctx, bk.Key, "新人", Identity{Provider: "test", ExternalID: "newbie"})
	if err != nil {
		t.Fatal(err)
	}
	if u.IsSuperadmin {
		t.Error("绑定用户不应是超管")
	}
	if invitedBy != admin.ID {
		t.Errorf("应返回邀请人 ID %d, got %d", admin.ID, invitedBy)
	}
	// Key 一次性。
	if _, _, err := s.BindUserWithKey(ctx, bk.Key, "又来", Identity{Provider: "test", ExternalID: "again"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("已用 Key 复用应 ErrNotFound, got %v", err)
	}
	// 过期 Key 无效。
	expired, err := s.CreateBindKey(ctx, admin.ID, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BindUserWithKey(ctx, expired.Key, "迟到", Identity{Provider: "test", ExternalID: "late"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("过期 Key 应 ErrNotFound, got %v", err)
	}

	invite, err := s.CreateBindInvite(ctx, admin.ID, time.Hour, "张三", "CEO", "研发入职")
	if err != nil {
		t.Fatal(err)
	}
	u, _, err = s.BindUserWithKey(ctx, invite.Key, "Telegram 昵称", Identity{Provider: "test", ExternalID: "zhangsan"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "张三" {
		t.Fatalf("邀请指定姓名应优先生效，got %q", u.Name)
	}
	if u.Info["role"] != "CEO" {
		t.Fatalf("邀请指定角色应写入用户信息，got %#v", u.Info)
	}
}

func TestTaskReviewLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, alice.ID, "写方案", nil)

	// 提交 → 待验收。
	t1, chain, err := s.SubmitTask(ctx, tk.ID)
	if err != nil || t1.Status != TaskDone || len(chain) != 0 {
		t.Fatalf("提交后 = %+v chain=%d err=%v", t1, len(chain), err)
	}
	if _, _, err := s.SubmitTask(ctx, tk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("待验收任务重复提交应 ErrNotFound, got %v", err)
	}
	// 打回 → 回到进行中，理由入进度。
	t2, err := s.RejectTask(ctx, tk.ID, boss.ID, "缺预算部分")
	if err != nil || t2.Status != TaskInProgress {
		t.Fatalf("打回后 = %+v err=%v", t2, err)
	}
	prog, err := s.ProgressOf(ctx, tk.ID)
	if err != nil || len(prog) != 1 || !strings.Contains(prog[0].Content, "缺预算部分") {
		t.Fatalf("打回理由未入进度: %+v err=%v", prog, err)
	}
	// 重新提交 → 验收通过。
	if _, _, err := s.SubmitTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	t3, _, err := s.AcceptTask(ctx, tk.ID)
	if err != nil || t3.Status != TaskAccepted {
		t.Fatalf("验收后 = %+v err=%v", t3, err)
	}
	// 终态不可重复操作。
	if _, _, err := s.AcceptTask(ctx, tk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复验收应 ErrNotFound, got %v", err)
	}
	if _, _, err := s.SubmitTask(ctx, tk.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("已验收再提交应 ErrNotFound, got %v", err)
	}
	// 统计。
	st, err := s.StatsOfAssignee(ctx, alice.ID)
	if err != nil || st.Accepted != 1 || st.Open != 0 {
		t.Errorf("统计 = %+v err=%v", st, err)
	}
	gs, err := s.GlobalTaskStats(ctx, time.Now().Add(-time.Hour))
	if err != nil || gs.DoneSince != 1 {
		t.Errorf("全局统计 = %+v err=%v", gs, err)
	}
}

func TestTaskQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	pending := mkTask(t, s, pj.ID, boss.ID, alice.ID, "待处理", nil)
	done := mkTask(t, s, pj.ID, boss.ID, alice.ID, "待验收", nil)
	if _, _, err := s.SubmitTask(ctx, done.ID); err != nil {
		t.Fatal(err)
	}
	accepted := mkTask(t, s, pj.ID, boss.ID, boss.ID, "已完成", nil)
	if _, _, err := s.SubmitTask(ctx, accepted.ID); err != nil {
		t.Fatal(err)
	}
	queue, err := s.TaskQueue(ctx, "queue", 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]string{}
	for _, task := range queue {
		got[task.ID] = task.Status
	}
	if got[pending.ID] != TaskPending || got[done.ID] != TaskDone {
		t.Fatalf("queue 应包含 pending/done: %+v", got)
	}
	if _, ok := got[accepted.ID]; ok {
		t.Fatalf("queue 不应包含 accepted: %+v", got)
	}
	all, err := s.TaskQueue(ctx, "all", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("all 应包含终态任务, got %d", len(all))
	}
}

func TestTaskOutcomeStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, alice.ID, "整理员工 xlsx 资料", nil)

	if err := s.RecordTaskOutcome(ctx, TaskOutcomeInput{
		TaskID: tk.ID, AssigneeID: alice.ID, ReviewerID: boss.ID,
		Outcome: TaskOutcomeAccepted, TaskKind: InferTaskKind(tk.Title), Reason: "结构清晰",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskOutcome(ctx, TaskOutcomeInput{
		TaskID: tk.ID, AssigneeID: alice.ID, ReviewerID: boss.ID,
		Outcome: TaskOutcomeRejected, TaskKind: "engineering", Reason: "代码任务不相关",
	}); err != nil {
		t.Fatal(err)
	}
	materials, err := s.TaskOutcomeStatsFor(ctx, alice.ID, "materials")
	if err != nil || materials.Accepted != 1 || materials.Rejected != 0 {
		t.Fatalf("materials outcome stats = %+v err=%v", materials, err)
	}
	all, err := s.TaskOutcomeStatsFor(ctx, alice.ID, "")
	if err != nil || all.Accepted != 1 || all.Rejected != 1 {
		t.Fatalf("all outcome stats = %+v err=%v", all, err)
	}
}

func TestRecordActionTurn(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "operator", true)
	sess, err := s.StartSession(ctx, u.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordActionTurn(ctx, ActionTurnInput{
		UserID:           u.ID,
		SessionID:        &sess.ID,
		Channel:          "telegram",
		UserTextHash:     "abc123",
		UserTextExcerpt:  "明天 9 点提醒全体完善档案",
		ReplyExcerpt:     "已设置推送。",
		RequiresAction:   true,
		Intent:           "设置提醒",
		ExpectedTools:    []string{"schedule_push"},
		Evidence:         map[string]any{"tool_evidence": []map[string]any{{"tool": "schedule_push", "ok": true}}},
		Outcome:          "evidence_ok",
		ToolCount:        1,
		SuccessToolCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var outcome string
	var expected []string
	if err := s.pool.QueryRow(ctx,
		`SELECT outcome, expected_tools FROM action_turns WHERE user_id = $1`, u.ID).
		Scan(&outcome, &expected); err != nil {
		t.Fatal(err)
	}
	if outcome != "evidence_ok" || len(expected) != 1 || expected[0] != "schedule_push" {
		t.Fatalf("action_turns row = outcome=%q expected=%v", outcome, expected)
	}
	if err := s.RecordActionTurn(ctx, ActionTurnInput{
		UserID:         u.ID,
		Channel:        "telegram",
		UserTextHash:   "no-plan",
		RequiresAction: false,
		Outcome:        "no_action",
	}); err != nil {
		t.Fatalf("nil expected_tools should be stored as empty array: %v", err)
	}
	expected = nil
	if err := s.pool.QueryRow(ctx,
		`SELECT expected_tools FROM action_turns WHERE user_text_hash = $1`, "no-plan").
		Scan(&expected); err != nil {
		t.Fatal(err)
	}
	if len(expected) != 0 {
		t.Fatalf("nil expected_tools should roundtrip empty, got %v", expected)
	}
	items, err := s.ListActionTurns(ctx, u.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].UserTextExcerpt != "明天 9 点提醒全体完善档案" || items[1].SuccessToolCount != 1 {
		t.Fatalf("ListActionTurns 缺少可读动作轨迹: %+v", items)
	}
}

func TestListAuditActivity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bossSession, err := s.StartSession(ctx, boss.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	aliceSession, err := s.StartSession(ctx, alice.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, alice.ID, &aliceSession.ID, "update_my_profile",
		[]byte(`{"fields":{"职位":"开发"}}`), "已更新。", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, boss.ID, &bossSession.ID, "list_users", []byte(`{}`), "alice", true); err != nil {
		t.Fatal(err)
	}

	since := time.Now().Add(-time.Hour)
	items, err := s.ListAuditActivity(ctx, AuditActivityFilter{
		Tool: "update_my_profile", Since: &since, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].UserID != alice.ID || items[0].UserName != "alice" || items[0].SessionID == nil || *items[0].SessionID != aliceSession.ID {
		t.Fatalf("tool filter result = %+v", items)
	}
	items, err = s.ListAuditActivity(ctx, AuditActivityFilter{Query: "开发", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Tool != "update_my_profile" {
		t.Fatalf("query should search args: %+v", items)
	}
	items, err = s.ListAuditActivity(ctx, AuditActivityFilter{UserID: boss.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Tool != "list_users" {
		t.Fatalf("user filter result = %+v", items)
	}
}

func TestDataCollectionCampaignRefreshesOnUserInfoUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	if err := s.EnsureInfoFields(ctx, []string{"手机", "职位"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateDataCollectionCampaign(ctx, "完善档案", "补齐联系信息", []string{"手机", "职位"}, boss.ID, []int64{alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshDataCollectionCampaign(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	targets, err := s.DataCollectionCampaignTargets(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Status != DataCampaignTargetPending || strings.Join(targets[0].MissingFields, ",") != "手机,职位" {
		t.Fatalf("初始目标状态 = %+v", targets)
	}
	if err := s.UpdateUserInfo(ctx, alice.ID, map[string]string{"手机": "13800000000"}); err != nil {
		t.Fatal(err)
	}
	targets, err = s.DataCollectionCampaignTargets(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if targets[0].Status != DataCampaignTargetPending || strings.Join(targets[0].MissingFields, ",") != "职位" {
		t.Fatalf("补一项后状态 = %+v", targets[0])
	}
	if err := s.UpdateUserInfo(ctx, alice.ID, map[string]string{"职位": "产品经理"}); err != nil {
		t.Fatal(err)
	}
	targets, err = s.DataCollectionCampaignTargets(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if targets[0].Status != DataCampaignTargetCompleted || len(targets[0].MissingFields) != 0 || targets[0].CompletedAt == nil {
		t.Fatalf("补齐后应完成 = %+v", targets[0])
	}
}

func TestWorkerClaimRecoversStaleTask(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "跑测试", nil)

	claimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil || claimed.ID != tk.ID || claimed.Status != TaskInProgress {
		t.Fatalf("首次认领 = %+v err=%v", claimed, err)
	}
	if claimed.WorkerClaimID == "" {
		t.Fatal("首次认领应返回 claim id")
	}
	if _, err := s.ClaimNextTask(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未超时不应重复认领, got %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE tasks SET worker_claimed_at = now() - interval '4 hours' WHERE id = $1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil || reclaimed.ID != tk.ID || reclaimed.Status != TaskInProgress {
		t.Fatalf("超时任务应可重新认领 = %+v err=%v", reclaimed, err)
	}
	if reclaimed.WorkerClaimID == "" || reclaimed.WorkerClaimID == claimed.WorkerClaimID {
		t.Fatalf("超时重领应刷新 claim id: old=%q new=%q", claimed.WorkerClaimID, reclaimed.WorkerClaimID)
	}
}

// TestRevokeWorkerResetsOpenTasks 吊销 worker 时应重置其未完成任务（回 pending、清 claim），
// 否则任务永远停在已禁用的 assignee 名下无人恢复。
func TestRevokeWorkerResetsOpenTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "跑测试", nil)
	// worker 认领 → in_progress + claim。
	if _, err := s.ClaimNextTask(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}

	reset, err := s.RevokeWorker(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reset != 1 {
		t.Errorf("RevokeWorker 返回重置任务数 = %d, want 1", reset)
	}
	got, err := s.TaskByID(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskPending {
		t.Errorf("吊销后任务状态 = %s, want pending", got.Status)
	}
	if got.WorkerClaimID != "" {
		t.Errorf("吊销后 worker_claim_id 应清空, got %q", got.WorkerClaimID)
	}
	// worker 已禁用。
	w, _ := s.UserByID(ctx, worker.ID)
	if w.Status != "disabled" {
		t.Errorf("worker 状态 = %s, want disabled", w.Status)
	}
}

func TestOrphanedTasksAndDecisionQueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "孤儿任务", nil)

	// 吊销前：无孤儿（worker 活跃）。
	if orphaned, _ := s.OrphanedTasks(ctx); len(orphaned) != 0 {
		t.Fatalf("吊销前应有 0 孤儿, got %d", len(orphaned))
	}
	// 吊销后：任务变孤儿。
	if _, err := s.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	orphaned, err := s.OrphanedTasks(ctx)
	if err != nil || len(orphaned) != 1 || orphaned[0].ID != tk.ID {
		t.Fatalf("吊销后应有 1 孤儿, got %+v err=%v", orphaned, err)
	}
	// BuildDecisionQueue 应给 assigner(boss) 生成 orphaned_task decision。
	count, err := s.BuildDecisionQueue(ctx, boss.ID)
	if err != nil || count == 0 {
		t.Fatalf("BuildDecisionQueue 应含孤儿项, count=%d err=%v", count, err)
	}
	items, err := s.ListDecisionItems(ctx, boss.ID, "open", 30)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range items {
		if d.Kind == "orphaned_task" && d.RefID != nil && *d.RefID == tk.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("决策队列应含 orphaned_task 指向任务 %d", tk.ID)
	}
	// CloseDecisionsByRef：改派后关闭指向该任务的决策项。
	n, err := s.CloseDecisionsByRef(ctx, boss.ID, "task", tk.ID)
	if err != nil || n == 0 {
		t.Fatalf("CloseDecisionsByRef 应关闭孤儿项, n=%d err=%v", n, err)
	}
	items, _ = s.ListDecisionItems(ctx, boss.ID, "open", 30)
	for _, d := range items {
		if d.Kind == "orphaned_task" {
			t.Errorf("关闭后不应还有 open 的 orphaned_task, got %+v", d)
		}
	}
}

// TestUpsertDecisionItemDoesNotReopenClosed 已关闭的决策项不应被后续 Upsert 重开。
// 旧实现 ON CONFLICT 无条件 SET status='open'，导致改派/验收后关掉的项被下次 refresh 重开，永远清不掉。
func TestUpsertDecisionItemDoesNotReopenClosed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	taskID := int64(42)
	d, err := s.UpsertDecisionItem(ctx, DecisionItem{
		OwnerID: boss.ID, Kind: "orphaned_task", Title: "改派孤儿",
		RefType: "task", RefID: &taskID, Priority: "high",
	})
	if err != nil || d.Status != "open" {
		t.Fatalf("首次 upsert 应 open: %+v err=%v", d, err)
	}
	// 关闭它（模拟改派完成）。
	if n, err := s.CloseDecisionsByRef(ctx, boss.ID, "task", taskID); err != nil || n != 1 {
		t.Fatalf("关闭应影响 1 行, n=%d err=%v", n, err)
	}
	// 再次 upsert 同 ref（模拟下次 BuildDecisionQueue/orphanTaskPass 重跑）：不应重开。
	d2, err := s.UpsertDecisionItem(ctx, DecisionItem{
		OwnerID: boss.ID, Kind: "orphaned_task", Title: "改派孤儿",
		RefType: "task", RefID: &taskID, Priority: "high",
	})
	if err != nil {
		t.Fatalf("重开 upsert 不应报错: %v", err)
	}
	if d2.Status != "closed" {
		t.Errorf("已关闭的决策项不应被重开: got status=%s, want closed", d2.Status)
	}
}

func TestReleaseWorkerTaskClaimMakesTaskImmediatelyClaimable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "跑测试", nil)

	claimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil || claimed.ID != tk.ID || claimed.WorkerClaimID == "" {
		t.Fatalf("认领 = %+v err=%v", claimed, err)
	}
	if err := s.ReleaseWorkerTaskClaim(ctx, claimed.ID, worker.ID, claimed.WorkerClaimID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil || reclaimed.ID != tk.ID {
		t.Fatalf("释放后应立即重领 = %+v err=%v", reclaimed, err)
	}
	if reclaimed.WorkerClaimID == "" || reclaimed.WorkerClaimID == claimed.WorkerClaimID {
		t.Fatalf("重领应刷新 claim id: old=%q new=%q", claimed.WorkerClaimID, reclaimed.WorkerClaimID)
	}
}

func TestWorkerClaimRejectedTaskWithoutClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "返工任务", nil)

	claimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SubmitWorkerTask(ctx, tk.ID, worker.ID, claimed.WorkerClaimID, "先交一版"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RejectTask(ctx, tk.ID, boss.ID, "还要补文件"); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.ID != tk.ID || reclaimed.WorkerClaimID == "" || reclaimed.WorkerClaimID == claimed.WorkerClaimID {
		t.Fatalf("打回任务应刷新 claim 后重新认领: first=%q second=%+v", claimed.WorkerClaimID, reclaimed)
	}
}

func TestTaskFilesAndWorkerArtifacts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	outsider := mkUser(t, s, "outsider", false)
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "处理附件", nil)

	fileOwner := boss.ID
	in, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "input.txt", MIMEType: "text/plain",
		SizeBytes: 5, SHA256: strings.Repeat("a", 64), StoragePath: "aa/input", CreatedBy: &fileOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaskAttachmentFile(ctx, tk.ID, in.ID, "输入"); err != nil {
		t.Fatal(err)
	}
	atts, err := s.TaskFileAttachments(ctx, tk.ID)
	if err != nil || len(atts) != 1 || atts[0].ID != in.ID {
		t.Fatalf("附件 = %+v err=%v", atts, err)
	}
	if ok, err := s.UserCanAccessFile(ctx, boss.ID, false, in.ID); err != nil || !ok {
		t.Fatalf("分配者应可访问附件 ok=%v err=%v", ok, err)
	}
	if ok, err := s.UserCanAccessFile(ctx, worker.ID, false, in.ID); err != nil || !ok {
		t.Fatalf("执行人应可访问附件 ok=%v err=%v", ok, err)
	}
	if ok, err := s.UserCanAccessFile(ctx, outsider.ID, false, in.ID); err != nil || ok {
		t.Fatalf("无关用户不应可访问附件 ok=%v err=%v", ok, err)
	}
	claimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, tk.ID, worker.ID, "stale", in.ID); err != nil || ok {
		t.Fatalf("旧 claim 不应可下载附件 ok=%v err=%v", ok, err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, tk.ID, worker.ID, claimed.WorkerClaimID, in.ID); err != nil || !ok {
		t.Fatalf("worker 应可用当前 claim 下载自己的任务附件 ok=%v err=%v", ok, err)
	}
	out, err := s.CreateFile(ctx, &File{
		Source: "worker", OriginalName: "result.txt", MIMEType: "text/plain",
		SizeBytes: 6, SHA256: strings.Repeat("b", 64), StoragePath: "bb/result", CreatedBy: &worker.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkerArtifact(ctx, tk.ID, worker.ID, "stale", out.ID, "旧 claim"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 claim 不应可登记产物: %v", err)
	}
	if err := s.AddWorkerArtifact(ctx, tk.ID, worker.ID, claimed.WorkerClaimID, out.ID, "结果"); err != nil {
		t.Fatal(err)
	}
	arts, err := s.TaskArtifacts(ctx, tk.ID)
	if err != nil || len(arts) != 1 || arts[0].File.ID != out.ID {
		t.Fatalf("产物 = %+v err=%v", arts, err)
	}
	if ok, err := s.UserCanAccessFile(ctx, boss.ID, false, out.ID); err != nil || !ok {
		t.Fatalf("分配者应可访问产物 ok=%v err=%v", ok, err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, tk.ID, worker.ID, "stale", out.ID); err != nil || ok {
		t.Fatalf("旧 claim 不应可下载历史产物 ok=%v err=%v", ok, err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, tk.ID, worker.ID, claimed.WorkerClaimID, out.ID); err != nil || !ok {
		t.Fatalf("worker 应可用当前 claim 下载历史产物 ok=%v err=%v", ok, err)
	}
}

func TestSelfAssignedSkipsReview(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, boss.ID, "自己的事", nil)

	t1, _, err := s.SubmitTask(ctx, tk.ID)
	if err != nil || t1.Status != TaskAccepted {
		t.Fatalf("自派任务提交应直接 accepted, got %+v err=%v", t1, err)
	}
}

func TestSplitCascade(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bob := mkUser(t, s, "bob", false)
	carol := mkUser(t, s, "carol", false)
	pj := mkProject(t, s, boss.ID)

	a := mkTask(t, s, pj.ID, boss.ID, alice.ID, "A 大任务", nil)
	subs, err := s.SplitTask(ctx, a.ID, []*Task{
		{ProjectID: pj.ID, AssignerID: alice.ID, AssigneeID: bob.ID, Title: "B"},
		{ProjectID: pj.ID, AssignerID: alice.ID, AssigneeID: carol.ID, Title: "C"},
	})
	if err != nil || len(subs) != 2 {
		t.Fatal(err)
	}
	if got, _ := s.TaskByID(ctx, a.ID); got.Status != TaskSplit {
		t.Fatalf("拆分后父状态 = %s", got.Status)
	}
	b, c := subs[0], subs[1]

	// B 提交并验收：C 未完 → 无级联。
	if _, _, err := s.SubmitTask(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, chain, err := s.AcceptTask(ctx, b.ID); err != nil || len(chain) != 0 {
		t.Fatalf("B 验收后 chain=%d err=%v", len(chain), err)
	}
	// C 提交并验收：A 全部子任务通过 → A 进入待验收（alice≠boss，等 boss 验收）。
	if _, _, err := s.SubmitTask(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	_, chain, err := s.AcceptTask(ctx, c.ID)
	if err != nil || len(chain) != 1 || chain[0].ID != a.ID || chain[0].Status != TaskDone {
		t.Fatalf("C 验收后级联 = %+v err=%v", chain, err)
	}
	// boss 验收 A → 终态。
	a2, chain, err := s.AcceptTask(ctx, a.ID)
	if err != nil || a2.Status != TaskAccepted || len(chain) != 0 {
		t.Fatalf("A 验收后 = %+v chain=%d err=%v", a2, len(chain), err)
	}
}

func TestSplitCascadeSelfAssignShortcut(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	bob := mkUser(t, s, "bob", false)
	pj := mkProject(t, s, boss.ID)

	// boss 给自己派任务再拆给 bob：bob 的任务验收通过后，父任务免自我验收直达 accepted。
	a := mkTask(t, s, pj.ID, boss.ID, boss.ID, "A 自派", nil)
	subs, err := s.SplitTask(ctx, a.ID, []*Task{
		{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: bob.ID, Title: "B"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SubmitTask(ctx, subs[0].ID); err != nil {
		t.Fatal(err)
	}
	_, chain, err := s.AcceptTask(ctx, subs[0].ID)
	if err != nil || len(chain) != 1 || chain[0].Status != TaskAccepted {
		t.Fatalf("自派父任务应直达 accepted: chain=%+v err=%v", chain, err)
	}
}

func TestDeadlineClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	now := time.Now().UTC()

	soon := now.Add(2 * time.Hour)
	tk := mkTask(t, s, pj.ID, boss.ID, alice.ID, "赶工", &soon)

	// 临近提醒：claim 一次，租约内重复调用为空；租约过期可重试，ack 后才消失。
	warn, err := s.DueDeadlineReminders(ctx, now, 24*time.Hour)
	if err != nil || len(warn) != 1 || warn[0].ID != tk.ID {
		t.Fatalf("首次认领 = %d err=%v", len(warn), err)
	}
	if warn, _ = s.DueDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("租约内重复认领应为空, got %d", len(warn))
	}
	mustExec(t, s, `UPDATE tasks SET deadline_reminder_claimed_at = now() - interval '20 minutes' WHERE id = $1`, tk.ID)
	if warn, err = s.DueDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 {
		t.Fatalf("租约过期应可重试, got %d err=%v", len(warn), err)
	}
	if err := s.MarkDeadlineReminderSent(ctx, tk.ID, now); err != nil {
		t.Fatal(err)
	}
	if warn, _ = s.DueDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("ack 后不应再认领, got %d", len(warn))
	}
	// 尚未过期。
	if over, _ := s.DueOverdueNotices(ctx, now); len(over) != 0 {
		t.Fatalf("未过期不应认领, got %d", len(over))
	}
	// 分配者改截止时间 → 标记重置；改到过去 → 过期通知触发。
	past := now.Add(-time.Hour)
	if _, err := s.UpdateTaskContent(ctx, tk.ID, nil, nil, nil, &past); err != nil {
		t.Fatal(err)
	}
	over, err := s.DueOverdueNotices(ctx, now)
	if err != nil || len(over) != 1 {
		t.Fatalf("改期后过期通知 = %d err=%v", len(over), err)
	}
	if over, _ = s.DueOverdueNotices(ctx, now); len(over) != 0 {
		t.Fatalf("过期通知租约内重复认领应为空, got %d", len(over))
	}
	mustExec(t, s, `UPDATE tasks SET overdue_notice_claimed_at = now() - interval '20 minutes' WHERE id = $1`, tk.ID)
	if over, err = s.DueOverdueNotices(ctx, now); err != nil || len(over) != 1 {
		t.Fatalf("过期通知租约过期应可重试, got %d err=%v", len(over), err)
	}
	if err := s.MarkOverdueNoticeSent(ctx, tk.ID, now); err != nil {
		t.Fatal(err)
	}
	if over, _ = s.DueOverdueNotices(ctx, now); len(over) != 0 {
		t.Fatalf("过期通知 ack 后不应再认领, got %d", len(over))
	}
}

func TestNudgeClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	now := time.Now().UTC()
	past := now.Add(-72 * time.Hour)

	stale := mkTask(t, s, pj.ID, boss.ID, alice.ID, "无动静", &past)
	active := mkTask(t, s, pj.ID, boss.ID, alice.ID, "有进度", &past)
	fresh := mkTask(t, s, pj.ID, boss.ID, alice.ID, "刚过期", &past)

	// 无动静：过期通知发出 49h，无进度 → 该催。
	mustExec(t, s, `UPDATE tasks SET overdue_notified_at = $2 WHERE id = $1`, stale.ID, now.Add(-49*time.Hour))
	// 有进度：过期已久但 1h 前刚汇报 → 不催。
	mustExec(t, s, `UPDATE tasks SET overdue_notified_at = $2 WHERE id = $1`, active.ID, now.Add(-49*time.Hour))
	if err := s.AddProgress(ctx, active.ID, alice.ID, "在做了"); err != nil {
		t.Fatal(err)
	}
	// 刚过期：通知发出才 1h → 还不到催的时候。
	mustExec(t, s, `UPDATE tasks SET overdue_notified_at = $2 WHERE id = $1`, fresh.ID, now.Add(-time.Hour))

	due, err := s.DueNudges(ctx, now, 48*time.Hour)
	if err != nil || len(due) != 1 || due[0].ID != stale.ID {
		ids := make([]int64, 0, len(due))
		for _, d := range due {
			ids = append(ids, d.ID)
		}
		t.Fatalf("应只催无动静任务 #%d, got %v err=%v", stale.ID, ids, err)
	}
	// 刚 claim → 租约内不再催；租约过期可重试；ack 后才进入冷却。
	if due, _ = s.DueNudges(ctx, now, 48*time.Hour); len(due) != 0 {
		t.Fatalf("催办租约内重复认领应为空, got %d", len(due))
	}
	mustExec(t, s, `UPDATE tasks SET nudge_claimed_at = now() - interval '20 minutes' WHERE id = $1`, stale.ID)
	if due, err = s.DueNudges(ctx, now, 48*time.Hour); err != nil || len(due) != 1 {
		t.Fatalf("催办租约过期应可重试, got %d err=%v", len(due), err)
	}
	if err := s.MarkNudgeSent(ctx, stale.ID, now); err != nil {
		t.Fatal(err)
	}
	if due, _ = s.DueNudges(ctx, now, 48*time.Hour); len(due) != 0 {
		t.Fatalf("催办 ack 后不应重复, got %d", len(due))
	}
}

func TestOrphanedTasks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	disabled := mkUser(t, s, "disabled-worker", false)
	active := mkUser(t, s, "active-worker", false)
	pj := mkProject(t, s, boss.ID)

	want := mkTask(t, s, pj.ID, boss.ID, disabled.ID, "需要改派", nil)
	done := mkTask(t, s, pj.ID, boss.ID, disabled.ID, "已完成不算", nil)
	normal := mkTask(t, s, pj.ID, boss.ID, active.ID, "正常任务", nil)
	if _, err := s.UpdateTaskStatus(ctx, done.ID, TaskAccepted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskStatus(ctx, normal.ID, TaskInProgress); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE users SET status = 'disabled' WHERE id = $1`, disabled.ID)

	got, err := s.OrphanedTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("OrphanedTasks = %#v, want only task %d", got, want.ID)
	}
}

func TestKnowledge(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)

	k1, err := s.CreateKnowledge(ctx, "部署流程", "先跑迁移再起进程", []string{"运维"}, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateKnowledge(ctx, "100%返点规则", "仅限年度框架客户", nil, boss.ID); err != nil {
		t.Fatal(err)
	}

	// 标题/正文/标签三路检索。
	for query, wantTitle := range map[string]string{
		"部署":   "部署流程",
		"迁移":   "部署流程",
		"运维":   "部署流程",
		"100%": "100%返点规则", // LIKE 通配符需按字面匹配
	} {
		ks, err := s.SearchKnowledge(ctx, query, 10)
		if err != nil || len(ks) != 1 || ks[0].Title != wantTitle {
			t.Errorf("搜 %q = %d 条 err=%v", query, len(ks), err)
		}
	}
	// 更新与删除。
	newContent := "先备份，再跑迁移，最后起进程"
	if _, err := s.UpdateKnowledge(ctx, k1.ID, nil, &newContent, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.KnowledgeByID(ctx, k1.ID)
	if err != nil || got.Content != newContent || len(got.Tags) != 1 {
		t.Fatalf("更新后 = %+v err=%v", got, err)
	}
	if err := s.DeleteKnowledge(ctx, k1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.KnowledgeByID(ctx, k1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后应 ErrNotFound, got %v", err)
	}
}

func TestKnowledgeVersionsRollbackAndLearningGovernance(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)

	k, err := s.CreateKnowledge(ctx, "Token 规则", "v1", []string{"scope:worker"}, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	v2 := "v2"
	if _, err := s.UpdateKnowledge(ctx, k.ID, nil, &v2, nil); err != nil {
		t.Fatal(err)
	}
	v3 := "v3"
	if _, err := s.UpdateKnowledge(ctx, k.ID, nil, &v3, nil); err != nil {
		t.Fatal(err)
	}
	versions, err := s.KnowledgeVersions(ctx, k.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Content != "v2" || versions[1].Content != "v1" {
		t.Fatalf("更新应产生 v1/v2 两个历史快照: %+v", versions)
	}
	rolled, err := s.RollbackKnowledge(ctx, k.ID, 1, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Content != "v1" {
		t.Fatalf("rollback content = %q", rolled.Content)
	}
	versions, err = s.KnowledgeVersions(ctx, k.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 || versions[0].Content != "v3" {
		t.Fatalf("rollback 应只追加当前版本快照一次，got %+v", versions)
	}
	rule, err := s.CreateRule(ctx, "常驻规则", "默认不展示思考过程", []string{"scope:global"}, boss.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRulePinned(ctx, rule.ID, true); err != nil {
		t.Fatal(err)
	}
	ruleVersions, err := s.KnowledgeVersions(ctx, rule.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ruleVersions) != 1 || ruleVersions[0].Pinned {
		t.Fatalf("set_rule_pinned 应快照修改前 pinned=false，got %+v", ruleVersions)
	}

	old, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Title: "Worker Token 不外发", Content: "不要把 worker token 发给用户。", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	dupe, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Title: " worker token 不外发 ", Content: "不要把 worker token 发到群里。", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ScoreLearningCandidates(ctx, 1); err != nil || n != 1 {
		t.Fatalf("ScoreLearningCandidates = %d, %v", n, err)
	}
	got, err := s.LearningCandidateByID(ctx, dupe.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DuplicateOf == nil || *got.DuplicateOf != old.ID || got.ValueScore <= 0 {
		t.Fatalf("应跨历史识别重复候选: %+v old=%d", got, old.ID)
	}
}

func TestWorkerCapabilities(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertWorkerCapability(ctx, WorkerCapabilityInput{
		WorkerID: worker.ID, Engine: "codex", CLIName: "codex", OS: "linux", Arch: "amd64",
		Capabilities: []string{"Go", "go", " xlsx "},
	}); err != nil {
		t.Fatal(err)
	}
	caps, err := s.WorkerCapabilities(ctx, []int64{worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := caps[worker.ID]
	if got == nil || strings.Join(got.Capabilities, ",") != "go,xlsx" || got.OS != "linux" {
		t.Fatalf("capability normalization failed: %+v", got)
	}
}

func TestDeletePublishedKnowledgeUnlinksLearningCandidate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	k, err := s.CreateKnowledge(ctx, "资料规则", "先审核再入库", nil, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindKnowledge, Title: "资料规则", Content: "先审核再入库", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLearningCandidatePublished(ctx, c.ID, boss.ID, &k.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKnowledge(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.LearningCandidateByID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PublishedKnowledgeID != nil {
		t.Fatalf("删除知识后候选引用应清空: %+v", got.PublishedKnowledgeID)
	}
}

func TestSuperadminBootstrap(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	has, err := s.HasSuperadmin(ctx)
	if err != nil || has {
		t.Fatalf("空库不应有超管: %v err=%v", has, err)
	}
	// 首任 HTTP 引导成功，且同事务签发首个 API token。
	u, token, err := s.BootstrapSuperadminWithAPIToken(ctx, "老板", Identity{Provider: "test", ExternalID: "boss"})
	if err != nil || !u.IsSuperadmin {
		t.Fatalf("引导 = %+v err=%v", u, err)
	}
	authed, err := s.UserByAPIToken(ctx, token)
	if err != nil || authed.ID != u.ID {
		t.Fatalf("引导 token 不可用: user=%+v err=%v", authed, err)
	}
	// 第二个人抢注被拒。
	if _, err := s.BootstrapSuperadmin(ctx, "路人", Identity{Provider: "test", ExternalID: "someone"}); !errors.Is(err, ErrConflict) {
		t.Errorf("已有超管应 ErrConflict, got %v", err)
	}
	// 已绑定用户的晋升同样被拒。
	alice := mkUser(t, s, "alice", false)
	if err := s.PromoteFirstSuperadmin(ctx, alice.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("已有超管晋升应 ErrConflict, got %v", err)
	}
	// 超管被停用后（系统无活跃超管），晋升通道重新打开。
	if err := s.SetUserStatus(ctx, u.ID, UserDisabled); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteFirstSuperadmin(ctx, alice.ID); err != nil {
		t.Errorf("无活跃超管时晋升应成功: %v", err)
	}
}

func TestSeedRoles(t *testing.T) {
	s := openTestStore(t)
	roles, err := s.ListRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range roles {
		names[r.Name] = true
		if r.TriggerDesc == "" || r.Prompt == "" {
			t.Errorf("内置角色 %s 的触发描述/提示词不应为空", r.Name)
		}
	}
	for _, want := range []string{
		"CEO参谋", "产品经理", "开发工程师", "测试工程师", "前端工程师",
		"运营经理", "市场营销", "销售顾问", "财务顾问", "HR招聘", "UI设计师",
	} {
		if !names[want] {
			t.Errorf("缺内置角色 %s", want)
		}
	}
	// v2 升级应已生效（v1 没有"关键规则"小节）。
	for _, r := range roles {
		if r.Name == "CEO参谋" && !strings.Contains(r.Prompt, "关键规则") {
			t.Error("CEO参谋 应为 v2 提示词")
		}
	}
}

func TestAPITokenRoundtrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)

	st, err := s.APITokenStatus(ctx, boss.ID)
	if err != nil || st.Exists {
		t.Fatalf("初始 token 状态 = %+v err=%v", st, err)
	}
	plain, err := s.IssueAPIToken(ctx, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err = s.APITokenStatus(ctx, boss.ID)
	if err != nil || !st.Exists || st.CreatedAt.IsZero() {
		t.Fatalf("签发后 token 状态 = %+v err=%v", st, err)
	}
	u, err := s.UserByAPIToken(ctx, plain)
	if err != nil || u.ID != boss.ID {
		t.Fatalf("token 换用户 = %+v err=%v", u, err) // 曾因 JOIN 列歧义全线失败
	}
	// 换发替换旧 token。
	plain2, err := s.IssueAPIToken(ctx, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByAPIToken(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Errorf("旧 token 应失效, got %v", err)
	}
	if _, err := s.UserByAPIToken(ctx, plain2); err != nil {
		t.Errorf("新 token 应有效: %v", err)
	}
	if err := s.RevokeAPIToken(ctx, boss.ID); err != nil {
		t.Fatal(err)
	}
	st, err = s.APITokenStatus(ctx, boss.ID)
	if err != nil || st.Exists {
		t.Fatalf("撤销后 token 状态 = %+v err=%v", st, err)
	}
	if _, err := s.UserByAPIToken(ctx, plain2); !errors.Is(err, ErrNotFound) {
		t.Errorf("撤销后应失效, got %v", err)
	}
}

func TestPermsCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)

	g := Grant{Kind: KindActive, UserID: alice.ID, Action: "send_msg", Target: TargetAll, GrantedBy: boss.ID}
	if err := s.GrantPerm(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantPerm(ctx, g); !errors.Is(err, ErrConflict) {
		t.Errorf("重复授权应 ErrConflict, got %v", err)
	}
	grants, err := s.PermsOf(ctx, alice.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("授权列表 = %d err=%v", len(grants), err)
	}
	if err := s.RevokePerm(ctx, KindActive, alice.ID, "send_msg", TargetAll); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePerm(ctx, KindActive, alice.ID, "send_msg", TargetAll); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复撤销应 ErrNotFound, got %v", err)
	}
}

// 定向 daily 定时任务：新字段落库回读 + 到期认领时 daily 前滚 24h 不重触发。
func TestDirectedDailySchedule(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "老板", true)

	past := time.Now().UTC().Add(-time.Minute)
	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: boss.ID, Kind: ScheduleDaily, Message: "给每位员工写早安问候，结合其今日待办",
		FireAt: past, Target: ScheduleTargetAll, Mode: ScheduleModeAI,
		DailyAt: "10:00", Weekdays: "1,2,3,4,5", CreatedBy: boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Target != ScheduleTargetAll || sc.Mode != ScheduleModeAI || sc.DailyAt != "10:00" ||
		sc.Weekdays != "1,2,3,4,5" || sc.CreatedBy != boss.ID {
		t.Fatalf("字段回读不符: %+v", sc)
	}

	due, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != sc.ID {
		t.Fatalf("应认领到 1 条, got %d", len(due))
	}
	// 租约内不再到期（不重复触发）；租约过期可重试；ack 后推进 fire_at。
	due2, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(due2) != 0 {
		t.Fatalf("daily 租约内不应重复触发, got %d", len(due2))
	}
	mustExec(t, s, `UPDATE schedules SET delivery_claimed_at = now() - interval '20 minutes' WHERE id = $1`, sc.ID)
	due2, err = s.DueSchedules(ctx, time.Now().UTC())
	if err != nil || len(due2) != 1 {
		t.Fatalf("daily 租约过期应可重试, got %d err=%v", len(due2), err)
	}
	next := time.Now().UTC().Add(24 * time.Hour)
	if err := s.MarkScheduleDelivered(ctx, sc.ID, *due[0].DeliveryClaimedAt, time.Now().UTC(), &next, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧租约不得确认新认领, got %v", err)
	}
	if err := s.MarkScheduleDelivered(ctx, sc.ID, *due2[0].DeliveryClaimedAt, time.Now().UTC(), &next, false); err != nil {
		t.Fatal(err)
	}
	if due2, err = s.DueSchedules(ctx, time.Now().UTC()); err != nil || len(due2) != 0 {
		t.Fatalf("daily ack 推进后不应重复触发, got %d err=%v", len(due2), err)
	}
	// 创建者可见、创建者可取消。
	list, err := s.SchedulesOf(ctx, boss.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("创建者应看到定向任务: %v %d", err, len(list))
	}
	visible, err := s.SchedulesVisible(ctx, boss.ID, true, "all", 50)
	if err != nil || len(visible) != 1 || visible[0].CreatorName != "老板" {
		t.Fatalf("超管应看到全局定时任务及创建人: %+v err=%v", visible, err)
	}
	if err := s.CancelSchedule(ctx, sc.ID, boss.ID); err != nil {
		t.Fatal(err)
	}
	active, err := s.SchedulesVisible(ctx, boss.ID, true, ScheduleActive, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("已取消任务不应出现在 active 列表: %+v", active)
	}
	all, err := s.SchedulesVisible(ctx, boss.ID, true, "all", 50)
	if err != nil || len(all) != 1 || all[0].Status != ScheduleCancelled {
		t.Fatalf("all 应看到已取消任务: %+v err=%v", all, err)
	}
}

// 群共享会话 + 滚动摘要：按渠道取会话、按渠道重置、摘要位点只前进、
// MessagesAfter 只取位点之后。
func TestGroupSessionAndSummary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u1 := mkUser(t, s, "甲", true)
	u2 := mkUser(t, s, "乙", false)
	ch := "telegram:group:-42"

	sess, err := s.StartGroupSession(ctx, u1.ID, ch, "eino")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ActiveSessionByChannel(ctx, ch)
	if err != nil || got.ID != sess.ID {
		t.Fatalf("按渠道取会话失败: %v", err)
	}
	// 其他人重置（按渠道关旧）：旧会话应失活。
	sess2, err := s.StartGroupSession(ctx, u2.ID, ch, "eino")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ActiveSessionByChannel(ctx, ch); got.ID != sess2.ID {
		t.Fatal("重置后活跃会话应是新会话")
	}

	for i := 0; i < 5; i++ {
		if _, err := s.AppendMessage(ctx, sess2.ID, "user", "m"); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.MessagesAfter(ctx, sess2.ID, 0, 0)
	if len(all) != 5 {
		t.Fatalf("消息数 = %d", len(all))
	}
	mid := all[2].ID
	if err := s.UpdateSessionSummary(ctx, sess2.ID, "要点摘要", mid); err != nil {
		t.Fatal(err)
	}
	fresh, _ := s.SessionByID(ctx, sess2.ID)
	if fresh.Summary != "要点摘要" || fresh.SummaryUpto != mid {
		t.Fatalf("摘要回读不符: %+v", fresh)
	}
	after, _ := s.MessagesAfter(ctx, sess2.ID, fresh.SummaryUpto, 0)
	if len(after) != 2 {
		t.Fatalf("位点后消息数 = %d", len(after))
	}
	// 位点只前进：回退应 ErrNotFound（execOne 无行）。
	if err := s.UpdateSessionSummary(ctx, sess2.ID, "旧摘要", mid-1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("位点回退应被拒: %v", err)
	}
}

// SubmitWorkerTask 只在任务仍是该 worker 手上的 in_progress 时提交，
// 覆盖「分配者同时改需求把任务重置为 pending」的竞态。
func TestSubmitWorkerTaskAtomic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "写脚本", nil)
	claimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.WorkerClaimID == "" {
		t.Fatal("认领应返回 worker claim id")
	}
	// 分配者此刻把任务重置为 pending（模拟改需求）。
	if _, err := s.UpdateTaskStatus(ctx, tk.ID, TaskPending); err != nil {
		t.Fatal(err)
	}
	// worker 的提交应落空（任务已非 in_progress），不把旧交付当完成。
	if _, _, err := s.SubmitWorkerTask(ctx, tk.ID, worker.ID, claimed.WorkerClaimID, "旧结果"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重置为 pending 后提交应被拒: %v", err)
	}
	if ps, err := s.ProgressOf(ctx, tk.ID); err != nil || len(ps) != 0 {
		t.Fatalf("提交失败不应写完成汇报: progress=%+v err=%v", ps, err)
	}
	got, _ := s.TaskByID(ctx, tk.ID)
	if got.Status != TaskPending {
		t.Fatalf("任务应仍为 pending 待重做, got %s", got.Status)
	}
	// 重新认领后提交成功。
	reclaimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.WorkerClaimID == "" || reclaimed.WorkerClaimID == claimed.WorkerClaimID {
		t.Fatalf("重新认领应换 claim id: old=%q new=%q", claimed.WorkerClaimID, reclaimed.WorkerClaimID)
	}
	if err := s.AddWorkerProgress(ctx, tk.ID, worker.ID, claimed.WorkerClaimID, "旧进度"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 claim 进度应被拒: %v", err)
	}
	if _, _, err := s.SubmitWorkerTask(ctx, tk.ID, worker.ID, claimed.WorkerClaimID, "旧结果"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 claim 提交应被拒: %v", err)
	}
	if _, _, err := s.SubmitWorkerTask(ctx, tk.ID, worker.ID, reclaimed.WorkerClaimID, "新结果"); err != nil {
		t.Fatalf("正常提交应成功: %v", err)
	}
	got, _ = s.TaskByID(ctx, tk.ID)
	if got.Status != TaskDone {
		t.Fatalf("提交后应为 done 待验收, got %s", got.Status)
	}
	ps, err := s.ProgressOf(ctx, tk.ID)
	if err != nil || len(ps) != 1 || !strings.Contains(ps[0].Content, "新结果") {
		t.Fatalf("成功提交才应写完成汇报: progress=%+v err=%v", ps, err)
	}
}

// worker 产物落盘前的 claim 预校验 + 孤儿清理。
func TestWorkerArtifactGating(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "产物任务", nil)
	claimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := claimed.WorkerClaimID
	if claim == "" {
		t.Fatal("认领应生成 claim_id")
	}

	// 预校验：正确 claim 通过；空/错 claim、非 in_progress 拒绝。
	if ok, _ := s.WorkerCanSubmitArtifact(ctx, tk.ID, worker.ID, claim); !ok {
		t.Fatal("有效 claim 应通过预校验")
	}
	if ok, _ := s.WorkerCanSubmitArtifact(ctx, tk.ID, worker.ID, "wrong"); ok {
		t.Fatal("错 claim 不应通过")
	}
	if ok, _ := s.WorkerCanSubmitArtifact(ctx, tk.ID, worker.ID, ""); ok {
		t.Fatal("空 claim 不应通过")
	}

	// 建一个文件、挂成产物：有引用时 DeleteOrphanFileRow 不动它。
	f, err := s.CreateFile(ctx, &File{Source: "worker", OriginalName: "a.txt", SHA256: "abc", StoragePath: "ab/abc", CreatedBy: &worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkerArtifact(ctx, tk.ID, worker.ID, claim, f.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOrphanFileRow(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileByID(ctx, f.ID); err != nil {
		t.Fatal("有产物引用的文件行不应被删除")
	}

	// 无引用的孤儿文件行：删除（不碰 blob）。
	orphan, err := s.CreateFile(ctx, &File{Source: "worker", OriginalName: "o.txt", SHA256: "def", StoragePath: "de/def", CreatedBy: &worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOrphanFileRow(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileByID(ctx, orphan.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("孤儿 files 行应已删除")
	}
}

func TestKnowledgeEmbeddingAndSearch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	author := mkUser(t, s, "作者", true)

	k1, _ := s.CreateKnowledge(ctx, "报销流程", "发票拍照上传财务系统，三天到账", []string{"财务"}, author.ID)
	k2, _ := s.CreateKnowledge(ctx, "请假规定", "提前一天在钉钉提交", nil, author.ID)

	// 多词词法：命中词数多的排前。
	res, err := s.SearchKnowledge(ctx, "报销 发票 财务", 5)
	if err != nil || len(res) == 0 || res[0].ID != k1.ID {
		t.Fatalf("多词检索应把报销排首位: %+v err=%v", res, err)
	}
	// tag 精确命中优先级最高。
	if res, _ := s.SearchKnowledge(ctx, "财务", 5); len(res) == 0 || res[0].ID != k1.ID {
		t.Fatalf("tag 命中应排首位: %+v", res)
	}

	// embedding 存取往返（同时验证 pgx []float32 ↔ real[] 映射）。
	vec := []float32{0.1, 0.2, 0.3, 0.4}
	if err := s.SetKnowledgeEmbedding(ctx, k1.ID, "test-model", vec); err != nil {
		t.Fatal(err)
	}
	cands, err := s.EmbeddedKnowledge(ctx, "test-model")
	if err != nil || len(cands) != 1 || cands[0].ID != k1.ID || len(cands[0].Embedding) != 4 {
		t.Fatalf("EmbeddedKnowledge = %+v err=%v", cands, err)
	}
	if cands[0].Embedding[2] != 0.3 {
		t.Fatalf("向量往返值不对: %v", cands[0].Embedding)
	}
	authorScoped, err := s.EmbeddedKnowledgeByAuthor(ctx, "test-model", author.ID)
	if err != nil || len(authorScoped) != 1 || authorScoped[0].ID != k1.ID {
		t.Fatalf("按作者取向量候选 = %+v err=%v", authorScoped, err)
	}
	// 只 k1 嵌入了 test-model；k2 应在待回填列表里。
	need, err := s.KnowledgeNeedingEmbedding(ctx, "test-model", 10)
	if err != nil {
		t.Fatal(err)
	}
	var hasK2, hasK1 bool
	for _, k := range need {
		if k.ID == k2.ID {
			hasK2 = true
		}
		if k.ID == k1.ID {
			hasK1 = true
		}
	}
	if !hasK2 || hasK1 {
		t.Fatalf("待回填应含 k2 不含 k1: need=%+v", need)
	}
	k3, err := s.CreateKnowledge(ctx, "前端踩坑", "按钮态要覆盖 loading", []string{"project:9", "worker:7"}, author.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKnowledgeEmbedding(ctx, k3.ID, "test-model", []float32{0.9, 0.8, 0.7, 0.6}); err != nil {
		t.Fatal(err)
	}
	tagScoped, err := s.EmbeddedKnowledgeByTag(ctx, "test-model", "project:9")
	if err != nil || len(tagScoped) != 1 || tagScoped[0].ID != k3.ID {
		t.Fatalf("按标签取向量候选 = %+v err=%v", tagScoped, err)
	}
	byAuthor, err := s.SearchKnowledgeByAuthor(ctx, author.ID, "按钮", 5)
	if err != nil || len(byAuthor) == 0 || byAuthor[0].Title != "前端踩坑" {
		t.Fatalf("按作者检索知识 = %+v err=%v", byAuthor, err)
	}
	byTag, err := s.SearchKnowledgeByTag(ctx, "project:9", "按钮", 5)
	if err != nil || len(byTag) == 0 || byTag[0].Title != "前端踩坑" {
		t.Fatalf("按项目标签检索知识 = %+v err=%v", byTag, err)
	}
	// KnowledgeByIDs 保序。
	byIDs, _ := s.KnowledgeByIDs(ctx, []int64{k2.ID, k1.ID})
	if len(byIDs) != 2 || byIDs[0].ID != k2.ID || byIDs[1].ID != k1.ID {
		t.Fatalf("KnowledgeByIDs 未保序: %+v", byIDs)
	}
}

func TestWorkerBindCodes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)

	worker, code, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, WorkerBindCodePrefix) {
		t.Fatalf("create_worker 应返回绑定码而非 token: %q", code)
	}
	// 绑定码不是 access token，不能直接认证。
	if _, err := s.UserByAPIToken(ctx, code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("绑定码不应可直接认证, got %v", err)
	}
	// 兑换：换出真 token，可认证。
	u, token, err := s.RedeemWorkerBindCode(ctx, code)
	if err != nil || u.ID != worker.ID {
		t.Fatalf("兑换 = %+v err=%v", u, err)
	}
	authed, err := s.UserByAPIToken(ctx, token)
	if err != nil || authed.ID != worker.ID {
		t.Fatalf("兑换出的 token 应可认证: %+v err=%v", authed, err)
	}
	// 一次一用。
	if _, _, err := s.RedeemWorkerBindCode(ctx, code); !errors.Is(err, ErrNotFound) {
		t.Fatalf("绑定码应一次一用, got %v", err)
	}
	// 补发：旧 token 在新码兑换前仍有效；兑换后被替换。
	code2, err := s.NewWorkerBindCode(ctx, worker.ID, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByAPIToken(ctx, token); err != nil {
		t.Fatalf("补发绑定码不应立即作废旧 token: %v", err)
	}
	_, token2, err := s.RedeemWorkerBindCode(ctx, code2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByAPIToken(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("新码兑换后旧 token 应作废, got %v", err)
	}
	if _, err := s.UserByAPIToken(ctx, token2); err != nil {
		t.Fatalf("新 token 应可认证: %v", err)
	}
	// 过期码不可兑换。
	code3, err := s.NewWorkerBindCode(ctx, worker.ID, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE worker_bind_codes SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RedeemWorkerBindCode(ctx, code3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期码应拒绝, got %v", err)
	}
	// 非 worker 不能补发；停用 worker 后绑定码清空。
	if _, err := s.NewWorkerBindCode(ctx, boss.ID, boss.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("非 worker 补发应拒绝, got %v", err)
	}
	code4, err := s.NewWorkerBindCode(ctx, worker.ID, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RedeemWorkerBindCode(ctx, code4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("停用 worker 的绑定码应作废, got %v", err)
	}
}

func TestWorkerSessionClaimAndUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := mkProject(t, s, boss.ID)
	task := mkTask(t, s, p.ID, boss.ID, worker.ID, "nbco 功能开发", nil)

	ws, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "repo:nbco", "NBCO", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID == 0 || ws.ScopeKey != "repo:nbco" || ws.UseCount != 1 {
		t.Fatalf("unexpected session: %+v", ws)
	}
	ws2, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "repo:nbco", "NBCO", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ws2.ID != ws.ID || ws2.UseCount != 2 {
		t.Fatalf("session should be reused and counted: first=%+v second=%+v", ws, ws2)
	}
	if err := s.UpdateWorkerSession(ctx, ws.ID, worker.ID, task.ID, "完成了路由", "native-ref", "/root/src/nbco"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "repo:nbco", "NBCO", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "完成了路由" || got.EngineSessionRef != "native-ref" || got.Workdir != "/root/src/nbco" {
		t.Fatalf("session update not persisted: %+v", got)
	}
}

func TestKnowledgeRules(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)

	fact, err := s.CreateKnowledge(ctx, "报销流程", "报销先走 OA 审批", nil, boss.ID)
	if err != nil || fact.Kind != KnowledgeKindFact || fact.Pinned {
		t.Fatalf("普通知识应 kind=fact 不常驻: %+v err=%v", fact, err)
	}
	skill, err := s.CreateSkill(ctx, "群入职处理", "触发条件：群里有人要求加入\n摘要：先判断真人还是 worker\n执行方法：先查群身份再邀请", []string{"scope:telegram"}, boss.ID)
	if err != nil || skill.Kind != KnowledgeKindSkill {
		t.Fatalf("skill 应 kind=skill: %+v err=%v", skill, err)
	}
	hard, err := s.CreateRule(ctx, "凭据保密", "不得在对话中展示 token、key 等凭据",
		[]string{"scope:global"}, boss.ID, true)
	if err != nil || hard.Kind != KnowledgeKindPolicy || !hard.Pinned {
		t.Fatalf("常驻规则 = %+v err=%v", hard, err)
	}
	soft, err := s.CreateRule(ctx, "周报格式", "周报默认用列表格式，不写长段落",
		[]string{"scope:telegram"}, boss.ID, false)
	if err != nil || soft.Pinned {
		t.Fatalf("动态规则 = %+v err=%v", soft, err)
	}
	if hits, err := s.SearchKnowledge(ctx, "周报", 10); err != nil || len(hits) != 0 {
		t.Fatalf("普通知识检索不应混入规则: %+v err=%v", hits, err)
	}
	if hits, err := s.SearchSkills(ctx, "加入", 10); err != nil || len(hits) != 1 || hits[0].ID != skill.ID {
		t.Fatalf("SearchSkills 应只召回 skill: %+v err=%v", hits, err)
	}
	if recent, err := s.RecentKnowledge(ctx, 10); err != nil || len(recent) != 1 || recent[0].ID != fact.ID {
		t.Fatalf("最近知识不应混入规则/skill: %+v err=%v", recent, err)
	}

	pinned, err := s.PinnedRules(ctx)
	if err != nil || len(pinned) != 1 || pinned[0].ID != hard.ID {
		t.Fatalf("PinnedRules = %+v err=%v", pinned, err)
	}
	all, err := s.ListRules(ctx, 10)
	if err != nil || len(all) != 2 || all[0].ID != hard.ID {
		t.Fatalf("ListRules 应常驻在前且不含普通知识: %+v err=%v", all, err)
	}
	// 词法检索：只召回非常驻规则，不混普通知识。
	hits, err := s.SearchRules(ctx, "周报", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != soft.ID {
		t.Fatalf("SearchRules = %+v err=%v", hits, err)
	}
	if hits, _ := s.SearchRules(ctx, "凭据", 10); len(hits) != 0 {
		t.Fatalf("常驻规则不应参与动态召回: %+v", hits)
	}
	// 语义候选：同样只有非常驻规则。
	if err := s.SetKnowledgeEmbedding(ctx, hard.ID, "m:2", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKnowledgeEmbedding(ctx, soft.ID, "m:2", []float32{0, 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKnowledgeEmbedding(ctx, fact.ID, "m:2", []float32{1, 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKnowledgeEmbedding(ctx, skill.ID, "m:2", []float32{1, -1}); err != nil {
		t.Fatal(err)
	}
	factVecs, err := s.EmbeddedKnowledge(ctx, "m:2")
	if err != nil || len(factVecs) != 1 || factVecs[0].ID != fact.ID {
		t.Fatalf("普通语义候选不应混入规则: %+v err=%v", factVecs, err)
	}
	vecs, err := s.EmbeddedRules(ctx, "m:2")
	if err != nil || len(vecs) != 1 || vecs[0].ID != soft.ID {
		t.Fatalf("EmbeddedRules = %+v err=%v", vecs, err)
	}
	skillVecs, err := s.EmbeddedSkills(ctx, "m:2")
	if err != nil || len(skillVecs) != 1 || skillVecs[0].ID != skill.ID {
		t.Fatalf("EmbeddedSkills = %+v err=%v", skillVecs, err)
	}
	// 常驻开关。
	if err := s.SetRulePinned(ctx, soft.ID, true); err != nil {
		t.Fatal(err)
	}
	if pinned, _ := s.PinnedRules(ctx); len(pinned) != 2 {
		t.Fatalf("置顶后 PinnedRules = %+v", pinned)
	}
	if err := s.SetRulePinned(ctx, fact.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("普通知识不应可置顶为规则, got %v", err)
	}
}

func TestLearningCandidateExists(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	createdBy := boss.ID
	if _, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindSkill, Scope: "telegram", Title: "群邀请流程",
		Content: "先判断真人还是 worker", Status: LearningStatusPending,
		CreatedBy: &createdBy,
	}); err != nil {
		t.Fatal(err)
	}
	ok, err := s.LearningCandidateExists(ctx, LearningKindSkill, " 群邀请流程 ", LearningStatusPending)
	if err != nil || !ok {
		t.Fatalf("pending 候选应可查重: ok=%v err=%v", ok, err)
	}
	ok, err = s.LearningCandidateExists(ctx, LearningKindSkill, "群邀请流程", LearningStatusPublished)
	if err != nil || ok {
		t.Fatalf("限定 published 不应命中 pending: ok=%v err=%v", ok, err)
	}
	ok, err = s.LearningCandidateExists(ctx, LearningKindRule, "群邀请流程", LearningStatusPending)
	if err != nil || ok {
		t.Fatalf("kind 不同不应命中: ok=%v err=%v", ok, err)
	}
}

func TestTelegramGroupState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	newer := time.Now().Truncate(time.Second)
	if err := s.SaveTelegramGroupState(ctx, TelegramGroupState{
		ChatID:    -1001,
		Title:     "公司群",
		Type:      "supergroup",
		Status:    "member",
		Listen:    true,
		UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTelegramGroupState(ctx, TelegramGroupState{
		ChatID:    -1002,
		Title:     "项目群",
		Type:      "supergroup",
		Status:    "left",
		Listen:    false,
		UpdatedAt: newer,
	}); err != nil {
		t.Fatal(err)
	}
	g, err := s.TelegramGroupState(ctx, -1001)
	if err != nil {
		t.Fatal(err)
	}
	if g.Title != "公司群" || !g.Listen || g.Status != "member" {
		t.Fatalf("TelegramGroupState = %+v", g)
	}
	groups, err := s.ListTelegramGroupStates(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].ChatID != -1002 || groups[1].ChatID != -1001 {
		t.Fatalf("ListTelegramGroupStates order = %+v", groups)
	}
}

func TestTelegramGroupMonitor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mon := TelegramGroupMonitor{
		ChatID:       -1001,
		Enabled:      true,
		GroupTitle:   "视频项目群",
		Instruction:  "遇到问题总结给我",
		NotifyUserID: 1,
		CreatedBy:    1,
		PendingCount: 40,
	}
	for i := 0; i < 35; i++ {
		mon.Buffer = append(mon.Buffer, fmt.Sprintf("第 %d 条讨论", i))
	}
	if err := s.SaveTelegramGroupMonitor(ctx, mon); err != nil {
		t.Fatal(err)
	}
	got, err := s.TelegramGroupMonitor(ctx, -1001)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.GroupTitle != "视频项目群" || got.NotifyUserID != 1 {
		t.Fatalf("TelegramGroupMonitor = %+v", got)
	}
	if len(got.Buffer) != 30 || got.Buffer[0] != "第 5 条讨论" {
		t.Fatalf("TelegramGroupMonitor buffer cap = %+v", got.Buffer)
	}
	if _, err := s.TelegramGroupMonitor(ctx, -404); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing monitor err = %v", err)
	}
}

func TestTelegramPendingEmployeeInvite(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	inv := TelegramPendingEmployeeInvite{
		TelegramUserID: 42,
		GroupChatID:    -1001,
		Key:            "abc",
		Name:           "新人",
		CreatedBy:      1,
		ExpiresAt:      time.Now().Add(time.Hour),
	}
	if err := s.SaveTelegramPendingEmployeeInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	got, err := s.TelegramPendingEmployeeInvite(ctx, 42)
	if err != nil || got.Key != "abc" || got.Name != "新人" {
		t.Fatalf("TelegramPendingEmployeeInvite = %+v err=%v", got, err)
	}
	if err := s.SaveTelegramPendingEmployeeInvite(ctx, TelegramPendingEmployeeInvite{
		TelegramUserID: 43,
		Key:            "expired",
		ExpiresAt:      time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TelegramPendingEmployeeInvite(ctx, 43); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired pending invite should be ErrNotFound, got %v", err)
	}
	if err := s.ClearTelegramPendingEmployeeInvite(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TelegramPendingEmployeeInvite(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared pending invite should be ErrNotFound, got %v", err)
	}
}

func TestScriptTools(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "脚本管理员", true, Identity{Provider: "test", ExternalID: "script-admin"})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := s.CreateScriptTool(ctx, ScriptTool{
		Name:           "format_roster",
		Description:    "格式化值日表",
		Runtime:        "starlark",
		InputSchema:    []byte(`{"type":"object","properties":{"name":{"type":"string"}}}`),
		Source:         `def run(args): return args["name"]`,
		RequiredAction: "create_project",
		CreatedBy:      u.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tool.Enabled {
		t.Fatal("新脚本工具默认应未启用")
	}
	if err := s.SetScriptToolEnabled(ctx, tool.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScriptToolTestResult(ctx, tool.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ScriptToolByName(ctx, "format_roster")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.LastTestResult != "ok" || got.RequiredAction != "create_project" {
		t.Fatalf("ScriptToolByName = %+v", got)
	}
	list, err := s.ListScriptTools(ctx, true, 10)
	if err != nil || len(list) != 1 || list[0].ID != tool.ID {
		t.Fatalf("ListScriptTools = %+v err=%v", list, err)
	}
	updated, err := s.UpdateScriptTool(ctx, tool.ID, ScriptTool{
		Name:           "format_roster_v2",
		Description:    "格式化值日表 v2",
		Runtime:        "starlark",
		InputSchema:    []byte(`{"type":"object"}`),
		Source:         `def run(args): return "ok"`,
		RequiredAction: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "format_roster_v2" || updated.RequiredAction != "" {
		t.Fatalf("UpdateScriptTool = %+v", updated)
	}
}

func TestEpisodicMessageSearch(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	other := mkUser(t, s, "other", false)

	sess, err := s.StartSession(ctx, boss.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	id1, err := s.AppendMessage(ctx, sess.ID, "user", "我们决定用 PostgreSQL 存所有状态")
	if err != nil || id1 == 0 {
		t.Fatalf("AppendMessage 应返回 ID: %d err=%v", id1, err)
	}
	osess, _ := s.StartSession(ctx, other.ID, "telegram", "eino")
	if _, err := s.AppendMessage(ctx, osess.ID, "user", "别人的 PostgreSQL 秘密讨论"); err != nil {
		t.Fatal(err)
	}
	// 词法检索只搜自己名下会话。
	ms, err := s.SearchMessagesOfUser(ctx, boss.ID, "PostgreSQL", 10)
	if err != nil || len(ms) != 1 || ms[0].ID != id1 {
		t.Fatalf("应只命中自己的消息: %+v err=%v", ms, err)
	}
	// 向量：落库+按用户取候选，不含他人消息。
	if err := s.SetMessageEmbedding(ctx, id1, "m:2", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	vecs, err := s.EmbeddedMessagesOfUser(ctx, "m:2", boss.ID)
	if err != nil || len(vecs) != 1 || vecs[0].ID != id1 {
		t.Fatalf("向量候选 = %+v err=%v", vecs, err)
	}
	if vecs, _ := s.EmbeddedMessagesOfUser(ctx, "m:2", other.ID); len(vecs) != 0 {
		t.Fatalf("他人不应见到该向量: %+v", vecs)
	}
	// 回填游标：短消息（<8字节）不进队列。
	if _, err := s.AppendMessage(ctx, sess.ID, "user", "在"); err != nil {
		t.Fatal(err)
	}
	need, err := s.MessagesNeedingEmbeddingAfter(ctx, "m:2", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range need {
		if m.Content == "在" {
			t.Fatal("寒暄短消息不应进嵌入队列")
		}
		if m.ID == id1 {
			t.Fatal("已嵌入消息不应再进队列")
		}
	}
}

func TestAIUsageStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	if err := s.RecordAIUsage(ctx, AIUsage{UserID: boss.ID, Kind: "telegram", Model: "m", InputTokens: 100, OutputTokens: 50}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAIUsage(ctx, AIUsage{UserID: boss.ID, Kind: "worker_llm", InputTokens: 30, OutputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	tot, err := s.AIUsageSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || tot.Calls != 2 || tot.InputTokens != 130 || tot.OutputTokens != 70 {
		t.Fatalf("总量 = %+v err=%v", tot, err)
	}
	rows, err := s.AIUsageByUserSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(rows) != 1 || rows[0].UserID != boss.ID || rows[0].Name != "boss" {
		t.Fatalf("按人 = %+v err=%v", rows, err)
	}
}

func TestTaskDependencyGating(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	dev := mkTask(t, s, pj.ID, boss.ID, worker.ID, "开发", nil)
	test, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "测试", Description: "跑测试", DependsOn: []int64{dev.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(test.DependsOn) != 1 || test.DependsOn[0] != dev.ID {
		t.Fatalf("依赖应落库: %+v", test.DependsOn)
	}
	// 前置未验收：只能领到 dev。
	claimed, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil || claimed.ID != dev.ID {
		t.Fatalf("应先领到开发任务: %+v err=%v", claimed, err)
	}
	if _, err := s.ClaimNextTask(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("测试任务被前置挡住，不应可领: %v", err)
	}
	// 提交+验收 dev 后，test 就绪。
	if _, _, err := s.SubmitWorkerTask(ctx, dev.ID, worker.ID, claimed.WorkerClaimID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptTask(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := s.ReadyDependents(ctx, dev.ID)
	if err != nil || len(ready) != 1 || ready[0].ID != test.ID {
		t.Fatalf("ReadyDependents = %+v err=%v", ready, err)
	}
	claimed2, err := s.ClaimNextTask(ctx, worker.ID)
	if err != nil || claimed2.ID != test.ID {
		t.Fatalf("前置验收后应可领测试任务: %+v err=%v", claimed2, err)
	}
	if _, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "坏依赖", DependsOn: []int64{999999},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的前置任务应被 Store 拒绝，got %v", err)
	}
	other := mkProject(t, s, boss.ID)
	foreign := mkTask(t, s, other.ID, boss.ID, worker.ID, "别的项目", nil)
	if _, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "跨项目依赖", DependsOn: []int64{foreign.ID},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨项目前置任务应被 Store 拒绝，got %v", err)
	}
	dup, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "去重依赖", DependsOn: []int64{dev.ID, dev.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dup.DependsOn) != 1 || dup.DependsOn[0] != dev.ID {
		t.Fatalf("重复依赖应规范化: %+v", dup.DependsOn)
	}
}

func TestPendingApprovals(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)

	id, err := s.CreatePendingApproval(ctx, boss.ID, "disable_user", "hash1", 10, 100)
	if err != nil || id == 0 {
		t.Fatalf("登记 = %d err=%v", id, err)
	}
	// 参数/用户/会话不同不核销；同一条用户消息内也不能核销。
	if ok, _ := s.ConsumePendingApproval(ctx, boss.ID, "disable_user", "hash2", 10, 101); ok {
		t.Fatal("不同参数不应核销")
	}
	if ok, _ := s.ConsumePendingApproval(ctx, boss.ID+1, "disable_user", "hash1", 10, 101); ok {
		t.Fatal("他人不应核销")
	}
	if ok, _ := s.ConsumePendingApproval(ctx, boss.ID, "disable_user", "hash1", 11, 101); ok {
		t.Fatal("不同会话不应核销")
	}
	if ok, _ := s.ConsumePendingApproval(ctx, boss.ID, "disable_user", "hash1", 10, 100); ok {
		t.Fatal("同一条用户消息内不应核销")
	}
	ok, err := s.ConsumePendingApproval(ctx, boss.ID, "disable_user", "hash1", 10, 101)
	if err != nil || !ok {
		t.Fatalf("同参应核销: %v err=%v", ok, err)
	}
	if ok, _ := s.ConsumePendingApproval(ctx, boss.ID, "disable_user", "hash1", 10, 102); ok {
		t.Fatal("一次一用，二次核销应失败")
	}
	// 过期不核销。
	if _, err := s.CreatePendingApproval(ctx, boss.ID, "delete_role", "h", 10, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE pending_approvals SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ConsumePendingApproval(ctx, boss.ID, "delete_role", "h", 10, 201); ok {
		t.Fatal("过期不应核销")
	}
	items, err := s.ListPendingApprovals(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("过期审批不应出现在列表中: %+v", items)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pending_approvals`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("列出审批应清理过期残留，剩余 %d", n)
	}
}

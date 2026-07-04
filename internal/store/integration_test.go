package store

// 集成测试：需要真实 PostgreSQL，设置 NBCO_TEST_PG_DSN 后运行（CI 提供服务容器）：
//
//	NBCO_TEST_PG_DSN='postgres://nbco:nbco@127.0.0.1:5432/nbco_test?sslmode=disable' go test ./internal/store/
//
// 未设置时自动跳过。每个测试开始时清空全库。

import (
	"context"
	"errors"
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
	if _, err := s.pool.Exec(ctx,
		`TRUNCATE users, projects, roles, bind_keys, audit_log, knowledge, kv_state, info_fields RESTART IDENTITY CASCADE`); err != nil {
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
	u, err := s.BindUserWithKey(ctx, bk.Key, "新人", Identity{Provider: "test", ExternalID: "newbie"})
	if err != nil {
		t.Fatal(err)
	}
	if u.IsSuperadmin {
		t.Error("绑定用户不应是超管")
	}
	// Key 一次性。
	if _, err := s.BindUserWithKey(ctx, bk.Key, "又来", Identity{Provider: "test", ExternalID: "again"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("已用 Key 复用应 ErrNotFound, got %v", err)
	}
	// 过期 Key 无效。
	expired, err := s.CreateBindKey(ctx, admin.ID, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindUserWithKey(ctx, expired.Key, "迟到", Identity{Provider: "test", ExternalID: "late"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("过期 Key 应 ErrNotFound, got %v", err)
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

	// 临近提醒：认领一次，重复调用为空。
	warn, err := s.DueDeadlineReminders(ctx, now, 24*time.Hour)
	if err != nil || len(warn) != 1 || warn[0].ID != tk.ID {
		t.Fatalf("首次认领 = %d err=%v", len(warn), err)
	}
	if warn, _ = s.DueDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("重复认领应为空, got %d", len(warn))
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
		t.Fatalf("过期通知重复认领应为空, got %d", len(over))
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
	// 刚催过 → 不再催。
	if due, _ = s.DueNudges(ctx, now, 48*time.Hour); len(due) != 0 {
		t.Fatalf("催办重复认领应为空, got %d", len(due))
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

func TestSuperadminBootstrap(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	has, err := s.HasSuperadmin(ctx)
	if err != nil || has {
		t.Fatalf("空库不应有超管: %v err=%v", has, err)
	}
	// 首任引导成功。
	u, err := s.BootstrapSuperadmin(ctx, "老板", Identity{Provider: "test", ExternalID: "boss"})
	if err != nil || !u.IsSuperadmin {
		t.Fatalf("引导 = %+v err=%v", u, err)
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

	plain, err := s.IssueAPIToken(ctx, boss.ID)
	if err != nil {
		t.Fatal(err)
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
	// 前滚后不再到期（不重复触发）。
	due2, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(due2) != 0 {
		t.Fatalf("daily 前滚后不应重复触发, got %d", len(due2))
	}
	// 创建者可见、创建者可取消。
	list, err := s.SchedulesOf(ctx, boss.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("创建者应看到定向任务: %v %d", err, len(list))
	}
	if err := s.CancelSchedule(ctx, sc.ID, boss.ID); err != nil {
		t.Fatal(err)
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
		if err := s.AppendMessage(ctx, sess2.ID, "user", "m"); err != nil {
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

package store

// 集成测试：需要真实 PostgreSQL，设置 NBCO_TEST_PG_DSN 后运行（CI 提供服务容器）：
//
//	NBCO_TEST_PG_DSN='postgres://nbco:nbco@127.0.0.1:5432/nbco_test?sslmode=disable' go test ./store/
//
// 未设置时自动跳过。每个测试开始时清空全库。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zdypro888/nbco/interaction"
	"github.com/zdypro888/nbco/workerproto"
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
		`TRUNCATE users, projects, roles, bind_keys, audit_log, knowledge, kv_state, info_fields,
		 ai_usage, pending_approvals, goals, automation_runs, automation_snapshots,
		 notification_deliveries, external_action_receipts, telegram_inbound_updates,
		 telegram_delivery_parts, domain_outbox_events RESTART IDENTITY CASCADE`); err != nil {
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

func testWorkerFinalization(claimID, payload string) WorkerRunFinalization {
	return WorkerRunFinalization{ID: "test-" + claimID, Hash: "test-" + payload}
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

	recoverable, err := s.CreateBindInviteForRequest(ctx, admin.ID, time.Hour, "李四", "产品", "定向邀请", "invite-request-1")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.CreateBindInviteForRequest(ctx, admin.ID, 2*time.Hour, "李四", "产品", "定向邀请", "invite-request-1")
	if err != nil || replayed.Key != recoverable.Key || !replayed.ExpiresAt.Equal(recoverable.ExpiresAt) {
		t.Fatalf("邀请结果恢复=%+v first=%+v err=%v", replayed, recoverable, err)
	}
	byRequest, err := s.BindInviteByRequest(ctx, admin.ID, "invite-request-1")
	if err != nil || byRequest.Key != recoverable.Key {
		t.Fatalf("按调用恢复邀请=%+v err=%v", byRequest, err)
	}
	if _, err := s.CreateBindInviteForRequest(ctx, admin.ID, time.Hour, "不同的人", "产品", "定向邀请", "invite-request-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("复用请求身份改变参数应冲突: %v", err)
	}
}

func TestTaskReviewLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, alice.ID, "写方案", nil)
	evidence, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: "worker_run", SourceKey: "review-lifecycle", Kind: WorkEvidenceDeliverable,
		Status: WorkEvidenceActive, Title: tk.Title, Content: "已提交方案", TaskID: &tk.ID,
		ProjectID: &pj.ID, ActorUserID: &alice.ID, EventAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}

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
	evidence, err = scanWorkEvidence(s.pool.QueryRow(ctx,
		`SELECT `+workEvidenceCols+` FROM work_evidence e
		 LEFT JOIN users u ON u.id = e.actor_user_id
		 LEFT JOIN projects p ON p.id = e.project_id WHERE e.id = $1`, evidence.ID))
	if err != nil || evidence.Status != WorkEvidenceResolved {
		t.Fatalf("验收后的工作证据 = %+v err=%v", evidence, err)
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

func TestWorkEvidenceUpsertPreservesHigherConfidenceProjection(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	user := mkUser(t, s, "evidence-confidence", false)
	other := mkUser(t, s, "evidence-confidence-other", false)
	strongProject := mkProject(t, s, user.ID)
	weakProject := mkProject(t, s, other.ID)
	strongTask := mkTask(t, s, strongProject.ID, user.ID, user.ID, "strong evidence task", nil)
	weakTask := mkTask(t, s, weakProject.ID, other.ID, other.ID, "weak evidence task", nil)
	now := time.Now().UTC()
	strong, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: WorkEvidenceSourceConversationFact, SourceKey: "confidence-order",
		Kind: WorkEvidenceDecision, Status: WorkEvidenceActive,
		Title: "用户确认的决定", Content: "已经决定按正式方案执行", ActorUserID: &user.ID,
		ProjectID: &strongProject.ID, TaskID: &strongTask.ID,
		Confidence: 1, EventAt: now, Metadata: json.RawMessage(`{"origin":"agent_tool"}`), CreatedBy: &user.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	weaker, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: WorkEvidenceSourceConversationFact, SourceKey: "confidence-order",
		Kind: WorkEvidenceRisk, Status: WorkEvidenceObserved,
		Title: "模型猜测的风险", Content: "模型对同一句话的较弱解释", ActorUserID: &other.ID,
		ProjectID: &weakProject.ID, TaskID: &weakTask.ID,
		Confidence: 0.6, EventAt: now.Add(time.Minute), Metadata: json.RawMessage(`{"origin":"memory_miner"}`), CreatedBy: &other.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if weaker.ID != strong.ID || weaker.Kind != WorkEvidenceDecision || weaker.Status != WorkEvidenceActive ||
		weaker.Title != strong.Title || weaker.Content != strong.Content || weaker.Confidence != 1 ||
		weaker.ActorUserID == nil || *weaker.ActorUserID != user.ID || weaker.ProjectID == nil || *weaker.ProjectID != strongProject.ID ||
		weaker.TaskID == nil || *weaker.TaskID != strongTask.ID || weaker.CreatedBy == nil || *weaker.CreatedBy != user.ID ||
		!weaker.EventAt.Equal(now) || !strings.Contains(string(weaker.Metadata), "agent_tool") {
		t.Fatalf("weaker projection overwrote stronger evidence: strong=%+v weaker=%+v", strong, weaker)
	}

	equal, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: WorkEvidenceSourceConversationFact, SourceKey: "confidence-order",
		Kind: WorkEvidenceDecision, Status: WorkEvidenceResolved,
		Title: "同级来源更新", Content: "同等强度来源确认了最终状态", ActorUserID: &user.ID,
		Confidence: 1, EventAt: now.Add(2 * time.Minute), Metadata: json.RawMessage(`{"origin":"verified_update"}`), CreatedBy: &user.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if equal.Status != WorkEvidenceResolved || equal.Title != "同级来源更新" ||
		!equal.EventAt.Equal(now.Add(2*time.Minute)) || !strings.Contains(string(equal.Metadata), "verified_update") {
		t.Fatalf("equal-confidence update was not applied: %+v", equal)
	}
}

func TestRecentStructuredWorkEvidenceExcludesRawCommunication(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	user := mkUser(t, s, "structured-evidence", false)
	now := time.Now().UTC()
	for _, in := range []WorkEvidenceInput{
		{
			SourceType: "telegram", SourceKey: "raw-message", Kind: WorkEvidenceCommunication,
			Status: WorkEvidenceObserved, Title: "成员", Content: "原始聊天内容",
			ActorUserID: &user.ID, EventAt: now, CreatedBy: &user.ID,
		},
		{
			SourceType: "telegram_group_digest", SourceKey: "summary", Kind: WorkEvidenceSummary,
			Status: WorkEvidenceActive, Title: "项目摘要", Content: "已提炼的关键进展",
			ActorUserID: &user.ID, EventAt: now.Add(time.Second), CreatedBy: &user.ID,
		},
		{
			SourceType: "telegram_group_digest", SourceKey: "resolved", Kind: WorkEvidenceRisk,
			Status: WorkEvidenceResolved, Title: "已关闭风险", Content: "不属于当前主动摘要",
			ActorUserID: &user.ID, EventAt: now.Add(2 * time.Second), CreatedBy: &user.ID,
		},
	} {
		if _, err := s.UpsertWorkEvidence(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.RecentStructuredWorkEvidence(ctx, now.Add(-time.Minute), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != WorkEvidenceSummary || items[0].Content != "已提炼的关键进展" {
		t.Fatalf("structured evidence = %+v", items)
	}
}

func TestUserScopedRulesExcludeOtherUsersAndPinnedRules(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "scoped-rule-owner", true)
	other := mkUser(t, s, "scoped-rule-other", false)
	personal, err := s.CreateRule(ctx, "个人自动化偏好", "主动报告只发送摘要",
		[]string{fmt.Sprintf("scope:user:%d", owner.ID)}, owner.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRule(ctx, "其他人的偏好", "只属于其他人",
		[]string{fmt.Sprintf("scope:user:%d", other.ID)}, other.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRule(ctx, "公司常驻规则", "通过常驻规则通道加载",
		[]string{"scope:global"}, owner.ID, true); err != nil {
		t.Fatal(err)
	}
	rules, err := s.UserScopedRules(ctx, owner.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].ID != personal.ID {
		t.Fatalf("user scoped rules = %+v", rules)
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
	if got[pending.ID] != TaskPending {
		t.Fatalf("queue 应包含 pending: %+v", got)
	}
	if _, ok := got[done.ID]; ok {
		t.Fatalf("queue 不应包含待验收任务: %+v", got)
	}
	if _, ok := got[accepted.ID]; ok {
		t.Fatalf("queue 不应包含 accepted: %+v", got)
	}
	review, err := s.TaskQueue(ctx, "review", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(review) != 1 || review[0].ID != done.ID || review[0].Status != TaskDone {
		t.Fatalf("review 应只包含 done: %+v", review)
	}
	if err := s.AddProgress(ctx, done.ID, alice.ID, "first result"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProgress(ctx, done.ID, alice.ID, "latest result"); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestProgressForTasks(ctx, []int64{pending.ID, done.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 || latest[done.ID].Content != "latest result" {
		t.Fatalf("latest progress = %+v", latest)
	}
	all, err := s.TaskQueue(ctx, "all", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("all 应包含终态任务, got %d", len(all))
	}
}

func TestTaskCollaborationDeduplicatesOneDeliverable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bob := mkUser(t, s, "bob", false)
	reviewer := mkUser(t, s, "reviewer", false)
	watcher := mkUser(t, s, "watcher", false)
	outsider := mkUser(t, s, "outsider", false)
	pj := mkProject(t, s, boss.ID)

	type result struct {
		value *TaskCreateResult
		err   error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, ownerID := range []int64{alice.ID, bob.ID} {
		wg.Add(1)
		go func(ownerID int64) {
			defer wg.Done()
			value, err := s.CreateOrMergeTask(ctx, &Task{
				ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: ownerID,
				Title: "整理同一份值日表", Description: "核对并发布最终版本", Acceptance: "一份可发布文件",
			}, []TaskParticipantInput{
				{UserID: reviewer.ID, Role: TaskParticipantReviewer},
				{UserID: watcher.ID, Role: TaskParticipantWatcher},
			}, boss.ID, false)
			results <- result{value: value, err: err}
		}(ownerID)
	}
	wg.Wait()
	close(results)
	var taskID int64
	created := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if taskID == 0 {
			taskID = result.value.Task.ID
		} else if result.value.Task.ID != taskID {
			t.Fatalf("并发派发同一交付物应返回同一任务: %d != %d", result.value.Task.ID, taskID)
		}
		if result.value.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("应且只应创建一次，实际 created=%d", created)
	}
	projectTasks, err := s.TasksOfProject(ctx, pj.ID)
	if err != nil || len(projectTasks) != 1 {
		t.Fatalf("项目内应只有一条任务: tasks=%+v err=%v", projectTasks, err)
	}
	task := projectTasks[0]
	collaboratorID := alice.ID
	if task.AssigneeID == alice.ID {
		collaboratorID = bob.ID
	}
	participants, err := s.TaskParticipants(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[int64]string{}
	for _, participant := range participants {
		roles[participant.UserID] = participant.Role
	}
	if roles[collaboratorID] != TaskParticipantCollaborator || roles[reviewer.ID] != TaskParticipantReviewer || roles[watcher.ID] != TaskParticipantWatcher {
		t.Fatalf("第二责任人应合并为协作者，并保留验收人: %+v", participants)
	}
	watcherOpen, err := s.TasksOfAssignee(ctx, watcher.ID, true)
	if err != nil || len(watcherOpen) != 0 {
		t.Fatalf("关注任务不应变成关注人的待办: %+v err=%v", watcherOpen, err)
	}
	watcherAll, err := s.TasksOfAssignee(ctx, watcher.ID, false)
	if err != nil || len(watcherAll) != 1 || watcherAll[0].ID != task.ID {
		t.Fatalf("关注人应能重新找到相关任务: %+v err=%v", watcherAll, err)
	}
	watcherProjects, err := s.ProjectsOfAssignee(ctx, watcher.ID)
	if err != nil || len(watcherProjects) != 1 || watcherProjects[0].ID != pj.ID {
		t.Fatalf("关注人应能找到相关项目: %+v err=%v", watcherProjects, err)
	}
	access, err := s.TaskAccessForUser(ctx, task, collaboratorID, false)
	if err != nil || !access.CanView || !access.CanContribute || access.CanManage {
		t.Fatalf("协作者权限错误: %+v err=%v", access, err)
	}
	fileOwner := boss.ID
	file, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "shared.txt", MIMEType: "text/plain", SizeBytes: 1,
		SHA256: strings.Repeat("d", 64), StoragePath: "dd/shared", CreatedBy: &fileOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted, err := s.AddTaskAttachmentFileOnce(ctx, task.ID, file.ID, "共享输入"); err != nil || !inserted {
		t.Fatalf("共享附件写入失败: inserted=%v err=%v", inserted, err)
	}
	for _, userID := range []int64{collaboratorID, reviewer.ID, watcher.ID} {
		if ok, err := s.UserCanAccessFile(ctx, userID, false, file.ID); err != nil || !ok {
			t.Fatalf("任务参与者 %d 应可读取附件: ok=%v err=%v", userID, ok, err)
		}
	}
	for _, source := range []string{"projects", "tasks", "files"} {
		field := map[string]string{"projects": "project_id", "tasks": "task_id", "files": "file_id"}[source]
		id := map[string]int64{"projects": pj.ID, "tasks": task.ID, "files": file.ID}[source]
		rows, err := s.ReadData(ctx, watcher.ID, false, DataReadQuery{
			Source: source, Filters: map[string]string{field: strconv.FormatInt(id, 10)}, Limit: 5,
		})
		if err != nil || len(rows) != 1 {
			t.Fatalf("participant ReadData(%s) = %s err=%v", source, rows, err)
		}
	}
	for _, tc := range []struct {
		kind string
		term string
		id   int64
	}{{"task", task.Title, task.ID}, {"file", file.OriginalName, file.ID}, {"project", pj.Name, pj.ID}} {
		matches, err := s.WorkspaceCandidates(ctx, watcher.ID, false, WorkspaceCandidateFilter{
			Kinds: []string{tc.kind}, Terms: []string{tc.term}, Limit: 5,
		})
		if err != nil || len(matches) != 1 || matches[0].ID != tc.id {
			t.Fatalf("participant workspace %s = %+v err=%v", tc.kind, matches, err)
		}
	}
	if ok, err := s.UserCanAccessFile(ctx, outsider.ID, false, file.ID); err != nil || ok {
		t.Fatalf("任务外用户不应读取附件: ok=%v err=%v", ok, err)
	}

	submitted, _, err := s.SubmitTaskBy(ctx, task.ID, collaboratorID)
	if err != nil || submitted.Status != TaskDone || submitted.SubmittedBy == nil || *submitted.SubmittedBy != collaboratorID || submitted.SubmittedAt == nil {
		t.Fatalf("协作者提交归属错误: task=%+v err=%v", submitted, err)
	}
	reviewQueue, err := s.TasksAwaitingReview(ctx, reviewer.ID)
	if err != nil || len(reviewQueue) != 1 || reviewQueue[0].ID != task.ID {
		t.Fatalf("指定验收人应看到待验收任务: %+v err=%v", reviewQueue, err)
	}
	if _, _, err := s.AcceptTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	parallel, err := s.CreateOrMergeTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID,
		Title: "整理同一份值日表", Description: "核对并发布最终版本", Acceptance: "一份可发布文件",
	}, nil, boss.ID, true)
	if err != nil || !parallel.Created || parallel.Task.ID == task.ID {
		t.Fatalf("allow_parallel 应显式创建独立交付: %+v err=%v", parallel, err)
	}
	otherManager := mkUser(t, s, "other-manager", true)
	other, err := s.CreateOrMergeTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: otherManager.ID, AssigneeID: bob.ID,
		Title: "整理同一份值日表", Description: "核对并发布最终版本", Acceptance: "一份可发布文件",
	}, nil, otherManager.ID, false)
	if err != nil || !other.Created {
		t.Fatalf("不同分配者的业务责任不能被静默吞并: %+v err=%v", other, err)
	}

	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrMergeTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "Worker 独立产出", Description: "生成文件",
	}, []TaskParticipantInput{{UserID: alice.ID, Role: TaskParticipantCollaborator}}, boss.ID, false); !errors.Is(err, ErrWorkerTaskParticipant) {
		t.Fatalf("worker 任务不应被转成人机共享执行: %v", err)
	}
}

func TestTaskReviewersArePreservedAcrossWorkerAndCascadeSubmission(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	reviewer := mkUser(t, s, "reviewer", false)
	alice := mkUser(t, s, "alice", false)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)

	workerTask := mkTask(t, s, pj.ID, worker.ID, worker.ID, "worker self task", nil)
	if _, err := s.ReplaceTaskParticipants(ctx, workerTask.ID, boss.ID,
		[]TaskParticipantInput{{UserID: reviewer.ID, Role: TaskParticipantReviewer}}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.TaskID == nil || *claimed.TaskID != workerTask.ID {
		t.Fatalf("worker claim = %+v err=%v", claimed, err)
	}
	_, submitted, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "done", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "done"))
	if err != nil || submitted.Status != TaskDone {
		t.Fatalf("explicit reviewer must keep worker task awaiting review: %+v err=%v", submitted, err)
	}

	parent := mkTask(t, s, pj.ID, boss.ID, boss.ID, "self-assigned parent", nil)
	if _, err := s.ReplaceTaskParticipants(ctx, parent.ID, boss.ID,
		[]TaskParticipantInput{{UserID: reviewer.ID, Role: TaskParticipantReviewer}}); err != nil {
		t.Fatal(err)
	}
	children, err := s.SplitTask(ctx, parent.ID, []*Task{{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "child",
	}})
	if err != nil || len(children) != 1 {
		t.Fatalf("SplitTask = %+v err=%v", children, err)
	}
	if _, _, err := s.SubmitTaskBy(ctx, children[0].ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	_, chain, err := s.AcceptTask(ctx, children[0].ID)
	if err != nil || len(chain) != 1 || chain[0].ID != parent.ID || chain[0].Status != TaskDone {
		t.Fatalf("reviewed parent must stop at done: chain=%+v err=%v", chain, err)
	}
	queue, err := s.TasksAwaitingReview(ctx, reviewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundParent := false
	for _, task := range queue {
		foundParent = foundParent || task.ID == parent.ID
	}
	if !foundParent {
		t.Fatalf("explicit reviewer queue missing cascaded parent: %+v", queue)
	}
}

func TestReviewerChangeAndSubmissionStayConsistent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	reviewer := mkUser(t, s, "reviewer", false)
	task := mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, boss.ID, "并发验收策略", nil)
	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, err := s.SubmitTaskBy(ctx, task.ID, boss.ID)
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := s.ReplaceTaskParticipants(ctx, task.ID, boss.ID,
			[]TaskParticipantInput{{UserID: reviewer.ID, Role: TaskParticipantReviewer}})
		errCh <- err
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent reviewer/submission error: %v", err)
		}
	}
	fresh, err := s.TaskByID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	participants, err := s.TaskParticipants(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	hasReviewer := len(participants) == 1 && participants[0].UserID == reviewer.ID && participants[0].Role == TaskParticipantReviewer
	if fresh.Status == TaskAccepted && hasReviewer || fresh.Status == TaskDone && !hasReviewer {
		t.Fatalf("submission/reviewer invariant broken: task=%+v participants=%+v", fresh, participants)
	}
}

func TestWorkerRunIsSeparateFromTaskAndReviewUsesResponsibility(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	delegated, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "delegated work",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.TaskID == nil || *claimed.TaskID != delegated.ID {
		t.Fatalf("claim delegated task = %+v err=%v", claimed, err)
	}
	if claimed.ScopeKey != fmt.Sprintf("project:%d", pj.ID) || claimed.ScopeTitle != pj.Name {
		t.Fatalf("default project scope = %q/%q, want project identity/name", claimed.ScopeKey, claimed.ScopeTitle)
	}
	completedRun, completedTask, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "completed", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "completed"))
	if err != nil || completedRun.Status != WorkerRunCompleted || completedTask.Status != TaskDone {
		t.Fatalf("delegated work must await business review: run=%+v task=%+v err=%v", completedRun, completedTask, err)
	}

	self, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: worker.ID, AssigneeID: worker.ID,
		Title: "self-owned worker maintenance",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.TaskID == nil || *claimed.TaskID != self.ID {
		t.Fatalf("claim self task = %+v err=%v", claimed, err)
	}
	_, completedTask, _, _, err = s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "completed", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "completed"))
	if err != nil || completedTask.Status != TaskAccepted {
		t.Fatalf("self-owned work without reviewer should close: %+v err=%v", completedTask, err)
	}
	if _, err := s.CreateWorkerRun(ctx, WorkerRunSpec{
		WorkerID: worker.ID, RequestedBy: boss.ID, Executor: workerproto.ExecutorCommand,
		Input: json.RawMessage(`{"command":"   "}`), Title: "invalid command",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty command execution must be rejected at the store boundary: %v", err)
	}

	commandInput, _ := json.Marshal(WorkerCommandInput{Command: "true"})
	direct, err := s.CreateWorkerRun(ctx, WorkerRunSpec{
		WorkerID: worker.ID, RequestedBy: boss.ID, Executor: workerproto.ExecutorCommand,
		Input: commandInput, Title: "direct command", ScopeType: "ops", ScopeKey: "ops:test",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.ID != direct.ID || claimed.TaskID != nil {
		t.Fatalf("claim direct run = %+v err=%v", claimed, err)
	}
	completedRun, completedTask, _, _, err = s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "exit 0", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "exit-0"))
	if err != nil || completedTask != nil || completedRun.Status != WorkerRunCompleted {
		t.Fatalf("direct execution must finish without a task: run=%+v task=%+v err=%v", completedRun, completedTask, err)
	}
	queue, err := s.TaskQueue(ctx, "all", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range queue {
		if item.Title == direct.Title {
			t.Fatalf("direct execution leaked into business task queue: %+v", item)
		}
	}
}

func TestWorkerFinalizationReplayReconstructsCascadeResult(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "cascade-worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	project := mkProject(t, s, boss.ID)
	parent := mkTask(t, s, project.ID, worker.ID, worker.ID, "worker parent", nil)
	children, err := s.SplitTask(ctx, parent.ID, []*Task{{
		ProjectID: project.ID, AssignerID: worker.ID, AssigneeID: worker.ID, Title: "worker child",
	}})
	if err != nil || len(children) != 1 {
		t.Fatalf("split worker task = %+v err=%v", children, err)
	}
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.TaskID == nil || *claimed.TaskID != children[0].ID {
		t.Fatalf("claim child = %+v err=%v", claimed, err)
	}
	finalization := testWorkerFinalization(claimed.ClaimID, "cascade-replay")
	_, completed, chain, replayed, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID,
		"done", "", workerproto.OutcomeSucceeded, nil, finalization)
	if err != nil || replayed || completed.Status != TaskAccepted || len(chain) != 1 ||
		chain[0].ID != parent.ID || chain[0].Status != TaskAccepted {
		t.Fatalf("first completion task=%+v chain=%+v replayed=%v err=%v", completed, chain, replayed, err)
	}
	_, completed, chain, replayed, err = s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID,
		"done", "", workerproto.OutcomeSucceeded, nil, finalization)
	if err != nil || !replayed || completed.Status != TaskAccepted || len(chain) != 1 ||
		chain[0].ID != parent.ID || chain[0].Status != TaskAccepted {
		t.Fatalf("replayed completion task=%+v chain=%+v replayed=%v err=%v", completed, chain, replayed, err)
	}
}

func TestReassignTaskRemovesConflictingRoleAndWritesAuditAtomically(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bob := mkUser(t, s, "bob", false)
	pj := mkProject(t, s, boss.ID)
	task := mkTask(t, s, pj.ID, boss.ID, alice.ID, "handoff", nil)
	if _, err := s.ReplaceTaskParticipants(ctx, task.ID, boss.ID,
		[]TaskParticipantInput{{UserID: bob.ID, Role: TaskParticipantReviewer}}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReassignTaskWithProgress(ctx, task.ID, bob.ID, 999999, "must roll back"); err == nil {
		t.Fatal("invalid audit author must roll back the whole reassignment")
	}
	unchanged, err := s.TaskByID(ctx, task.ID)
	if err != nil || unchanged.AssigneeID != alice.ID {
		t.Fatalf("failed reassignment leaked ownership change: %+v err=%v", unchanged, err)
	}

	reassigned, err := s.ReassignTaskWithProgress(ctx, task.ID, bob.ID, boss.ID, "ownership changed")
	if err != nil || reassigned.AssigneeID != bob.ID {
		t.Fatalf("ReassignTaskWithProgress = %+v err=%v", reassigned, err)
	}
	participants, err := s.TaskParticipants(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, participant := range participants {
		if participant.UserID == bob.ID {
			t.Fatalf("new primary assignee retained participant role: %+v", participants)
		}
	}
	access, err := s.TaskAccessForUser(ctx, reassigned, bob.ID, false)
	if err != nil || !access.CanContribute || access.CanReview {
		t.Fatalf("new assignee access = %+v err=%v", access, err)
	}
	progress, err := s.ProgressOf(ctx, task.ID)
	if err != nil || len(progress) != 1 || progress[0].Content != "ownership changed" {
		t.Fatalf("atomic reassignment audit = %+v err=%v", progress, err)
	}
}

func TestCreateOrMergeTaskMergesOperationalConstraints(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bob := mkUser(t, s, "bob", false)
	pj := mkProject(t, s, boss.ID)
	dependency := mkTask(t, s, pj.ID, boss.ID, alice.ID, "prerequisite", nil)
	later := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Microsecond)
	earlier := later.Add(-24 * time.Hour)
	first, err := s.CreateOrMergeTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID,
		Title: "same deliverable", Description: "one output", Priority: "normal", Deadline: &later,
	}, nil, boss.ID, false)
	if err != nil || !first.Created {
		t.Fatalf("first task = %+v err=%v", first, err)
	}
	merged, err := s.CreateOrMergeTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: bob.ID,
		Title: "same deliverable", Description: "one output", Priority: "high", Deadline: &earlier,
		DependsOn: []int64{dependency.ID},
	}, nil, boss.ID, false)
	if err != nil || merged.Created || merged.Task.ID != first.Task.ID {
		t.Fatalf("merged task = %+v err=%v", merged, err)
	}
	if merged.Task.Priority != "high" || merged.Task.Deadline == nil || !merged.Task.Deadline.Equal(earlier) ||
		len(merged.Task.DependsOn) != 1 || merged.Task.DependsOn[0] != dependency.ID {
		t.Fatalf("merged constraints were lost: %+v", merged.Task)
	}
	updated := map[string]bool{}
	for _, field := range merged.UpdatedFields {
		updated[field] = true
	}
	for _, field := range []string{"priority", "deadline", "depends_on"} {
		if !updated[field] {
			t.Fatalf("updated fields missing %s: %+v", field, merged.UpdatedFields)
		}
	}

	newer := mkTask(t, s, pj.ID, boss.ID, alice.ID, "newer dependency", nil)
	if _, err := s.CreateOrMergeTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID,
		Title: "same deliverable", Description: "one output", Priority: "high", Deadline: &earlier,
		DependsOn: []int64{newer.ID},
	}, nil, boss.ID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("newer dependency must not create a merge cycle risk: %v", err)
	}
}

func TestCancelTaskMergesPeopleAttachmentsAndDependencies(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	bob := mkUser(t, s, "bob", false)
	reviewer := mkUser(t, s, "reviewer", false)
	pj := mkProject(t, s, boss.ID)
	replacement := mkTask(t, s, pj.ID, boss.ID, alice.ID, "保留任务", nil)
	obsolete := mkTask(t, s, pj.ID, boss.ID, bob.ID, "重复任务", nil)
	if _, err := s.ReplaceTaskParticipants(ctx, obsolete.ID, boss.ID,
		[]TaskParticipantInput{{UserID: reviewer.ID, Role: TaskParticipantReviewer}}); err != nil {
		t.Fatal(err)
	}
	dependent, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID,
		Title: "后续任务", DependsOn: []int64{obsolete.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	fileOwner := boss.ID
	file, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "input.txt", MIMEType: "text/plain", SizeBytes: 1,
		SHA256: strings.Repeat("c", 64), StoragePath: "cc/input", CreatedBy: &fileOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := s.AddTaskAttachmentFileOnce(ctx, obsolete.ID, file.ID, "输入")
	if err != nil || !inserted {
		t.Fatalf("首次附件写入失败: inserted=%v err=%v", inserted, err)
	}
	if inserted, err = s.AddTaskAttachmentFileOnce(ctx, obsolete.ID, file.ID, "重复说明"); err != nil || inserted {
		t.Fatalf("重复附件应幂等: inserted=%v err=%v", inserted, err)
	}

	cancelled, _, err := s.CancelTask(ctx, obsolete.ID, "与保留任务重复", &replacement.ID)
	if err != nil || cancelled.Status != TaskCancelled || cancelled.SupersededBy == nil || *cancelled.SupersededBy != replacement.ID {
		t.Fatalf("取消合并失败: task=%+v err=%v", cancelled, err)
	}
	participants, err := s.TaskParticipants(ctx, replacement.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[int64]string{}
	for _, participant := range participants {
		roles[participant.UserID] = participant.Role
	}
	if roles[bob.ID] != TaskParticipantCollaborator || roles[reviewer.ID] != TaskParticipantReviewer {
		t.Fatalf("旧责任人与参与者应迁移到保留任务: %+v", participants)
	}
	files, err := s.TaskFileAttachments(ctx, replacement.ID)
	if err != nil || len(files) != 1 || files[0].ID != file.ID {
		t.Fatalf("旧任务附件应迁移且不重复: %+v err=%v", files, err)
	}
	dependent, err = s.TaskByID(ctx, dependent.ID)
	if err != nil || len(dependent.DependsOn) != 1 || dependent.DependsOn[0] != replacement.ID {
		t.Fatalf("下游依赖应重连到保留任务: %+v err=%v", dependent, err)
	}
	queue, err := s.TaskQueue(ctx, "queue", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range queue {
		if task.ID == obsolete.ID {
			t.Fatalf("已取消重复任务不应留在活跃队列: %+v", queue)
		}
	}
	history, err := s.TaskQueue(ctx, "history", 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range history {
		found = found || task.ID == obsolete.ID
	}
	if !found {
		t.Fatalf("已取消任务必须留在历史队列: %+v", history)
	}
	parent := mkTask(t, s, pj.ID, boss.ID, alice.ID, "父任务", nil)
	childParentID := parent.ID
	child, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, ParentID: &childParentID, AssignerID: boss.ID, AssigneeID: bob.ID, Title: "子任务",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CancelTask(ctx, child.ID, "错误层级合并", &replacement.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("子任务不能合并到顶层任务: %v", err)
	}
	stillOpen, err := s.TaskByID(ctx, child.ID)
	if err != nil || stillOpen.Status != TaskPending {
		t.Fatalf("失败的合并必须完整回滚: %+v err=%v", stillOpen, err)
	}
}

func TestTaskOutcomeStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	tk, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID,
		Title: "整理员工资料", Kind: TaskKindMaterials,
	})
	if err != nil || tk.Kind != TaskKindMaterials {
		t.Fatalf("task kind = %+v err=%v", tk, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET kind = 'unsupported' WHERE id = $1`, tk.ID); err == nil {
		t.Fatal("database must reject unsupported task kinds")
	}
	persisted, err := s.TaskByID(ctx, tk.ID)
	if err != nil || persisted.Kind != TaskKindMaterials {
		t.Fatalf("rejected task kind update changed task = %+v err=%v", persisted, err)
	}

	if err := s.RecordTaskOutcome(ctx, TaskOutcomeInput{
		TaskID: tk.ID, AssigneeID: alice.ID, ReviewerID: boss.ID,
		Outcome: TaskOutcomeAccepted, TaskKind: TaskKindMaterials, Reason: "结构清晰",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskOutcome(ctx, TaskOutcomeInput{
		TaskID: tk.ID, AssigneeID: alice.ID, ReviewerID: boss.ID,
		Outcome: TaskOutcomeRejected, TaskKind: "engineering", Reason: "代码任务不相关",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE task_outcomes SET task_kind = 'unsupported' WHERE task_id = $1`, tk.ID); err == nil {
		t.Fatal("database must reject unsupported task outcome kinds")
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
		ExpectedTools:    []string{"schedule_once_push"},
		Evidence:         map[string]any{"tool_evidence": []map[string]any{{"tool": "schedule_once_push", "ok": true}}},
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
	if outcome != "evidence_ok" || len(expected) != 1 || expected[0] != "schedule_once_push" {
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
	sessionItems, err := s.ListActionTurnsBySession(ctx, u.ID, sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionItems) != 1 || sessionItems[0].UserTextHash != "abc123" {
		t.Fatalf("ListActionTurnsBySession leaked another scope: %+v", sessionItems)
	}
}

func TestConversationTurnIsIdempotentAndPublishesAtomically(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "turn-owner", true)
	sess, err := s.StartSession(ctx, u.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.CreateRule(ctx, "turn attribution", "record selected context", []string{"scope:global"}, u.ID, true)
	if err != nil {
		t.Fatal(err)
	}

	turn, created, err := s.BeginConversationTurn(
		ctx, "telegram:message:7:11", u.ID, sess.ID, "telegram", "执行并记录")
	if err != nil || !created || turn.UserMessageID == nil {
		t.Fatalf("BeginConversationTurn = %+v created=%t err=%v", turn, created, err)
	}
	replayed, created, err := s.BeginConversationTurn(
		ctx, "telegram:message:7:11", u.ID, sess.ID, "telegram", "执行并记录")
	if err != nil || created || replayed.ID != turn.ID || replayed.UserMessageID == nil ||
		*replayed.UserMessageID != *turn.UserMessageID {
		t.Fatalf("idempotent begin = %+v created=%t err=%v", replayed, created, err)
	}
	if _, _, err := s.BeginConversationTurn(
		ctx, "telegram:message:7:11", u.ID, sess.ID, "telegram", "相同键却是不同输入"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched idempotency payload should conflict: %v", err)
	}
	var userMessages int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_messages WHERE session_id = $1 AND role = 'user'`, sess.ID).
		Scan(&userMessages); err != nil || userMessages != 1 {
		t.Fatalf("user messages = %d err=%v", userMessages, err)
	}

	sid := sess.ID
	badAction := &ActionTurnInput{
		UserID: u.ID, SessionID: &sid, Channel: "telegram",
		UserTextHash: "bad", Outcome: "should_rollback",
		Evidence: map[string]any{"not_json": func() {}},
	}
	if _, err := s.CompleteConversationTurn(ctx, ConversationTurnCompletion{
		TurnID: turn.ID, AssistantText: "不能部分发布", ResultText: "不能部分发布",
		Action: badAction,
	}); err == nil {
		t.Fatal("invalid audit row should roll back the entire completion")
	}
	fresh, err := s.ConversationTurnByID(ctx, turn.ID)
	if err != nil || fresh.Status != "running" || fresh.AssistantMessageID != nil {
		t.Fatalf("rolled-back turn = %+v err=%v", fresh, err)
	}
	var assistantMessages int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_messages WHERE session_id = $1 AND role = 'assistant'`, sess.ID).
		Scan(&assistantMessages); err != nil || assistantMessages != 0 {
		t.Fatalf("assistant messages after rollback = %d err=%v", assistantMessages, err)
	}

	action := &ActionTurnInput{
		UserID: u.ID, SessionID: &sid, Channel: "telegram", UserTextHash: "ok",
		UserTextExcerpt: "执行并记录", ReplyExcerpt: "已完成", RequiresAction: true,
		ExpectedTools: []string{"test_tool"}, Outcome: "action_completed",
	}
	usage := &AIUsage{
		UserID: u.ID, SessionID: &sid, Kind: "telegram", Model: "test-model",
		InputTokens: 12, OutputTokens: 4,
	}
	memorySource := "执行并记录"
	assistantID, err := s.CompleteConversationTurn(ctx, ConversationTurnCompletion{
		TurnID: turn.ID, AssistantText: "已完成", ResultText: "已完成",
		ResultActions: []interaction.Action{{Kind: interaction.ActionOpenWebApp, Label: "打开报表", URL: "https://nbco.example/report"}},
		EngineSession: "eino:turn", Action: action, Usage: usage,
		EnqueueMemory: true, MemoryEvidence: "[test_tool] ok", MemorySourceText: &memorySource,
		AssetUsages: []ConversationAssetUsage{
			{KnowledgeID: rule.ID, Phase: AssetPhaseInjected, TurnOutcome: AssetOutcomeActionSucceeded},
			{KnowledgeID: rule.ID, Phase: AssetPhaseCandidate, TurnOutcome: AssetOutcomeActionSucceeded},
			{KnowledgeID: rule.ID, Phase: AssetPhaseCandidate, TurnOutcome: AssetOutcomeActionSucceeded},
		},
	})
	if err != nil || assistantID == 0 {
		t.Fatalf("CompleteConversationTurn = %d, %v", assistantID, err)
	}
	secondID, err := s.CompleteConversationTurn(ctx, ConversationTurnCompletion{
		TurnID: turn.ID, AssistantText: "不应重复", ResultText: "不应重复",
	})
	if err != nil || secondID != assistantID {
		t.Fatalf("idempotent completion = %d, %v", secondID, err)
	}
	for table, want := range map[string]int{"action_turns": 1, "ai_usage": 1, "conversation_asset_usages": 2} {
		var count int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE conversation_turn_id = $1`, turn.ID).
			Scan(&count); err != nil || count != want {
			t.Fatalf("%s count = %d err=%v", table, count, err)
		}
	}
	var assetOutcome string
	if err := s.pool.QueryRow(ctx,
		`SELECT turn_outcome FROM conversation_asset_usages
		  WHERE conversation_turn_id = $1 AND knowledge_id = $2 AND phase = $3`,
		turn.ID, rule.ID, AssetPhaseInjected).Scan(&assetOutcome); err != nil || assetOutcome != AssetOutcomeActionSucceeded {
		t.Fatalf("asset outcome = %q err=%v", assetOutcome, err)
	}
	assetStats, err := s.AssetUsageStatsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || assetStats.Injected != 1 || assetStats.Candidates != 1 || assetStats.ActionSucceeded != 1 {
		t.Fatalf("asset usage stats = %+v err=%v", assetStats, err)
	}
	effectiveness, err := s.ListAssetEffectivenessSince(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(effectiveness) != 1 || effectiveness[0].KnowledgeID != rule.ID ||
		effectiveness[0].Injected != 1 || effectiveness[0].Candidates != 1 || effectiveness[0].ActionSucceeded != 1 {
		t.Fatalf("asset effectiveness = %+v err=%v", effectiveness, err)
	}
	var memoryJobs int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM memory_mining_jobs WHERE user_message_id = $1`, *turn.UserMessageID).
		Scan(&memoryJobs); err != nil || memoryJobs != 1 {
		t.Fatalf("memory jobs = %d err=%v", memoryJobs, err)
	}
	var storedMemorySource *string
	if err := s.pool.QueryRow(ctx,
		`SELECT user_evidence_text FROM memory_mining_jobs WHERE user_message_id = $1`, *turn.UserMessageID).
		Scan(&storedMemorySource); err != nil || storedMemorySource == nil || *storedMemorySource != memorySource {
		t.Fatalf("memory source = %v err=%v", storedMemorySource, err)
	}
	if err := s.MarkConversationTurnDeliveryFailed(ctx, turn.ID, "network"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkConversationTurnDelivered(ctx, turn.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.ConversationTurnByID(ctx, turn.ID)
	if err != nil || fresh.Status != "completed" || fresh.DeliveryStatus != "delivered" ||
		fresh.DeliveryAttempts != 2 || fresh.ResultText != "已完成" || len(fresh.ResultActions) != 1 ||
		fresh.ResultActions[0].URL != "https://nbco.example/report" {
		t.Fatalf("delivered turn = %+v err=%v", fresh, err)
	}

	stale, created, err := s.BeginConversationTurn(
		ctx, "telegram:message:7:12", u.ID, sess.ID, "telegram", "执行到一半进程退出")
	if err != nil || !created {
		t.Fatalf("stale begin = %+v created=%t err=%v", stale, created, err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversation_turns SET started_at = now() - interval '1 hour' WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := s.FailStaleConversationTurns(ctx, 30*time.Minute); err != nil || count != 1 {
		t.Fatalf("FailStaleConversationTurns = %d, %v", count, err)
	}
	stale, err = s.ConversationTurnByID(ctx, stale.ID)
	if err != nil || stale.Status != "failed" {
		t.Fatalf("stale turn = %+v err=%v", stale, err)
	}
}

func TestProductOperatingLoopPreservesSourceIdentityAndMaterialLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "operating-loop-owner", true)
	sess, err := s.StartGroupSession(ctx, u.ID, "telegram:group:-10088", "eino")
	if err != nil {
		t.Fatal(err)
	}
	sourceAt := time.Date(2026, 8, 19, 9, 30, 0, 0, time.UTC)
	envelope := MessageEnvelope{
		Provider: "telegram", ExternalChatRef: "-10088", ExternalMessageRef: "42",
		ActorUserID: &u.ID, ExternalActorRef: "991", ActorDisplayName: "旧显示名",
		ReplyToExternalRef: "41", ThreadRef: "7", SourceCreatedAt: &sourceAt,
		Metadata: json.RawMessage(`{"media_group_id":"batch-a"}`),
	}
	messageID, err := s.AppendMessageWithEnvelope(ctx, sess.ID, "user", "第一版", envelope)
	if err != nil {
		t.Fatal(err)
	}
	envelope.ActorDisplayName = "新显示名"
	replayedID, err := s.AppendMessageWithEnvelope(ctx, sess.ID, "user", "编辑后的正文", envelope)
	if err != nil || replayedID != messageID {
		t.Fatalf("message replay id=%d want=%d err=%v", replayedID, messageID, err)
	}
	message, err := s.ChatMessageByID(ctx, messageID)
	if err != nil || message.Content != "编辑后的正文" || message.ActorUserID == nil || *message.ActorUserID != u.ID ||
		message.ActorDisplayName != "新显示名" || message.ThreadRef != "7" {
		t.Fatalf("message envelope = %+v err=%v", message, err)
	}

	project, err := s.CreateProject(ctx, "证据项目", "", u.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: "telegram_group_message", SourceKey: "-10088:42",
		Kind: WorkEvidenceCommunication, Status: WorkEvidenceObserved,
		Title: "新显示名", Content: "编辑后的正文", ActorUserID: &u.ID,
		ProjectID: &project.ID, SourceMessageID: &messageID, EventAt: sourceAt,
	})
	if err != nil || evidence.ActorName != u.Name || evidence.ProjectName != project.Name {
		t.Fatalf("work evidence = %+v err=%v", evidence, err)
	}
	if _, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: "telegram_group_message", SourceKey: "-10088:42",
		Kind: WorkEvidenceCommunication, Status: WorkEvidenceObserved,
		Title: "新显示名", Content: "最终正文", ActorUserID: &u.ID,
		ProjectID: &project.ID, SourceMessageID: &messageID, EventAt: sourceAt,
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := s.WorkEvidenceStatsSince(ctx, sourceAt.Add(-time.Minute))
	if err != nil || stats.ObservedMessages != 1 || stats.Actors != 1 || stats.Projects != 1 {
		t.Fatalf("work evidence stats = %+v err=%v", stats, err)
	}

	files := make([]*File, 0, 2)
	intakes := make([]*FileIntake, 0, 2)
	for index, name := range []string{"people.pdf", "roles.xlsx"} {
		file, err := s.CreateFile(ctx, &File{
			Source: "telegram", OriginalName: name, MIMEType: "application/octet-stream",
			SizeBytes: int64(index + 1), SHA256: fmt.Sprintf("material-sha-%d", index),
			StoragePath: fmt.Sprintf("material/%d", index), CreatedBy: &u.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		intake, err := s.CreateFileIntake(ctx, FileIntake{
			UserID: u.ID, Source: "telegram", ExternalRef: fmt.Sprintf("%d:%d", 77+index, index),
			OriginalName: name, MIMEType: file.MIMEType, SizeBytes: file.SizeBytes,
		})
		if err != nil {
			t.Fatal(err)
		}
		if canonicalID, err := s.CompleteFileIntake(ctx, intake.ID, file.ID, "media-group:album-77"); err != nil || canonicalID != file.ID {
			t.Fatal(err)
		}
		files = append(files, file)
		intakes = append(intakes, intake)
	}
	replayedIntake, err := s.CreateFileIntake(ctx, FileIntake{
		UserID: u.ID, Source: "telegram", ExternalRef: "77:0", OriginalName: "people.pdf",
	})
	if err != nil || replayedIntake.ID != intakes[0].ID || replayedIntake.Status != "saved" ||
		replayedIntake.FileID == nil || *replayedIntake.FileID != files[0].ID {
		t.Fatalf("replayed intake = %+v err=%v", replayedIntake, err)
	}
	cases, err := s.MaterialCases(ctx, u.ID, false, 10)
	if err != nil || len(cases) != 1 || cases[0].SourceRef != "media-group:album-77" || cases[0].Status != MaterialReceived || len(cases[0].Files) != 2 {
		t.Fatalf("material cases = %+v err=%v", cases, err)
	}
	if err := s.DeleteUnreferencedFile(ctx, files[0].ID); err != nil {
		t.Fatal(err)
	}
	cases, err = s.MaterialCases(ctx, u.ID, false, 10)
	if err != nil || len(cases) != 1 || len(cases[0].Files) != 1 {
		t.Fatalf("partially deleted material case = %+v err=%v", cases, err)
	}
	if err := s.DeleteUnreferencedFile(ctx, files[1].ID); err != nil {
		t.Fatal(err)
	}
	cases, err = s.MaterialCases(ctx, u.ID, false, 10)
	if err != nil || len(cases) != 0 {
		t.Fatalf("empty material case should be removed = %+v err=%v", cases, err)
	}
}

func TestQueueMaterialFilesKeepsUnselectedAlbumFilesPending(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "partial-material-owner", true)
	project, err := s.CreateProject(ctx, "partial-material-project", "", u.ID)
	if err != nil {
		t.Fatal(err)
	}

	files := make([]*File, 0, 2)
	for index, name := range []string{"selected.pdf", "pending.pdf"} {
		file, err := s.CreateFile(ctx, &File{
			Source: "telegram", OriginalName: name, MIMEType: "application/pdf",
			SizeBytes: int64(index + 1), SHA256: fmt.Sprintf("partial-material-sha-%d", index),
			StoragePath: fmt.Sprintf("partial-material/%d", index), CreatedBy: &u.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		intake, err := s.CreateFileIntake(ctx, FileIntake{
			UserID: u.ID, Source: "telegram", ExternalRef: fmt.Sprintf("88:%d", index),
			OriginalName: name, MIMEType: file.MIMEType, SizeBytes: file.SizeBytes,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteFileIntake(ctx, intake.ID, file.ID, "media-group:partial-album"); err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	task, err := s.CreateTask(ctx, &Task{
		ProjectID: project.ID, AssignerID: u.ID, AssigneeID: u.ID,
		Title: "分析部分材料", Description: "只分析选中的文件", Priority: "normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := queueMaterialFilesTx(ctx, tx, u.ID, []int64{files[0].ID}, task.ID, task.Title, task.Description); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	cases, err := s.MaterialCases(ctx, u.ID, false, 10)
	if err != nil || len(cases) != 2 {
		t.Fatalf("active material cases = %+v err=%v", cases, err)
	}
	byRef := make(map[string]*MaterialCase, len(cases))
	for _, materialCase := range cases {
		byRef[materialCase.SourceRef] = materialCase
	}
	pending := byRef["media-group:partial-album"]
	queued := byRef[fmt.Sprintf("task:%d", task.ID)]
	if pending == nil || pending.Status != MaterialReceived || len(pending.Files) != 1 || pending.Files[0].ID != files[1].ID {
		t.Fatalf("unselected album remainder = %+v", pending)
	}
	if queued == nil || queued.Status != MaterialQueued || len(queued.Files) != 1 || queued.Files[0].ID != files[0].ID {
		t.Fatalf("queued selection = %+v", queued)
	}

	secondTask, err := s.CreateTask(ctx, &Task{
		ProjectID: project.ID, AssignerID: u.ID, AssigneeID: u.ID,
		Title: "分析剩余材料", Description: "处理相册剩余文件", Priority: "normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err = s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := queueMaterialFilesTx(ctx, tx, u.ID, []int64{files[1].ID}, secondTask.ID, secondTask.Title, secondTask.Description); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	lateFile, err := s.CreateFile(ctx, &File{
		Source: "telegram", OriginalName: "late.pdf", MIMEType: "application/pdf",
		SizeBytes: 3, SHA256: "partial-material-sha-late", StoragePath: "partial-material/late", CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	lateIntake, err := s.CreateFileIntake(ctx, FileIntake{
		UserID: u.ID, Source: "telegram", ExternalRef: "88:late",
		OriginalName: lateFile.OriginalName, MIMEType: lateFile.MIMEType, SizeBytes: lateFile.SizeBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteFileIntake(ctx, lateIntake.ID, lateFile.ID, "media-group:partial-album"); err != nil {
		t.Fatal(err)
	}
	cases, err = s.MaterialCases(ctx, u.ID, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	byRef = make(map[string]*MaterialCase, len(cases))
	for _, materialCase := range cases {
		byRef[materialCase.SourceRef] = materialCase
	}
	reopened := byRef["media-group:partial-album"]
	if reopened == nil || reopened.Status != MaterialReceived || len(reopened.Files) != 1 || reopened.Files[0].ID != lateFile.ID {
		t.Fatalf("late album file should reopen source case = %+v", reopened)
	}
}

func TestMaterialWorkerLifecycleFollowsTaskTransaction(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "material-lifecycle-boss", true)
	worker, _, err := s.CreateWorker(ctx, "material-lifecycle-worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	project := mkProject(t, s, boss.ID)
	resultSchema := json.RawMessage(`{"type":"object","required":["facts"],"additionalProperties":false,"properties":{"facts":{"type":"array"}}}`)
	file, err := s.CreateFileWithMaterialCase(ctx, &File{
		Source: "api", OriginalName: "company.pdf", MIMEType: "application/pdf",
		SizeBytes: 42, SHA256: "material-lifecycle-sha", StoragePath: "material-lifecycle/file", CreatedBy: &boss.ID,
	}, "material-lifecycle-batch")
	if err != nil {
		t.Fatal(err)
	}
	entityInput := MaterialEntity{
		FileID: &file.ID, EntityType: "system", Name: "Company source", Content: "verified",
		SourceType: "worker_result", SourceRef: "run:test", SourceItemKey: "entity:0", CreatedBy: &worker.ID,
	}
	firstEntity, created, err := s.UpsertMaterialEntity(ctx, entityInput)
	if err != nil || !created {
		t.Fatalf("first sourced material entity = %+v created=%v err=%v", firstEntity, created, err)
	}
	replayedEntity, created, err := s.UpsertMaterialEntity(ctx, entityInput)
	if err != nil || created || replayedEntity.ID != firstEntity.ID {
		t.Fatalf("replayed sourced material entity = %+v created=%v err=%v", replayedEntity, created, err)
	}
	task, err := s.CreateMaterialTaskWithWorkerRun(ctx, &Task{
		ProjectID: project.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "整理公司资料", Description: "提炼结构化事实", Priority: "normal",
	}, []int64{file.ID}, "材料输入", WorkerRunSpec{
		Executor: workerproto.ExecutorAgent, ScopeType: "materials",
		ScopeKey: "materials:test", ScopeTitle: "Material lifecycle test",
		ResultRequired: true, ResultSchema: resultSchema, ResultHandler: "test.material.v1",
	}, MaterialTaskSpec{OwnerID: boss.ID, Title: "整理公司资料", Instruction: "提炼结构化事实"})
	if err != nil {
		t.Fatal(err)
	}
	materialForTask := func(includeClosed bool) *MaterialCase {
		t.Helper()
		cases, err := s.MaterialCases(ctx, boss.ID, includeClosed, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, materialCase := range cases {
			if materialCase.TaskID != nil && *materialCase.TaskID == task.ID {
				return materialCase
			}
		}
		return nil
	}
	if materialCase := materialForTask(false); materialCase == nil || materialCase.Status != MaterialQueued {
		t.Fatalf("created material task = %+v", materialCase)
	}
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if materialCase := materialForTask(false); materialCase == nil || materialCase.Status != MaterialProcessing {
		t.Fatalf("claimed material task = %+v", materialCase)
	}
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID,
		"缺少机器结果", "", workerproto.OutcomeSucceeded, nil,
		testWorkerFinalization(claimed.ClaimID, "material-missing-result")); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing structured result error = %v", err)
	}
	structuredResult := json.RawMessage(`{"facts":[{"name":"company"}]}`)
	finalization := testWorkerFinalization(claimed.ClaimID, "material-complete")
	completedRun, completedTask, _, replayed, err := s.CompleteWorkerRunWithStructuredResult(ctx, claimed.ID, worker.ID, claimed.ClaimID,
		"已提炼公司资料", "", structuredResult, workerproto.OutcomeSucceeded, nil, finalization)
	if err != nil || replayed || completedTask == nil || completedTask.Status != TaskDone {
		t.Fatalf("complete material task = %+v replayed=%v err=%v", completedTask, replayed, err)
	}
	var completedResultCompact, expectedResultCompact bytes.Buffer
	_ = json.Compact(&completedResultCompact, completedRun.StructuredResult)
	_ = json.Compact(&expectedResultCompact, structuredResult)
	if !bytes.Equal(completedResultCompact.Bytes(), expectedResultCompact.Bytes()) || !completedRun.ResultRequired || completedRun.ResultHandler != "test.material.v1" {
		t.Fatalf("completed structured contract/result = %+v", completedRun)
	}
	var attemptResult json.RawMessage
	var attemptResultCompact bytes.Buffer
	attemptErr := s.pool.QueryRow(ctx, `SELECT structured_result FROM worker_run_attempts WHERE run_id=$1 AND claim_id=$2`, claimed.ID, claimed.ClaimID).Scan(&attemptResult)
	if attemptErr == nil {
		_ = json.Compact(&attemptResultCompact, attemptResult)
	}
	if attemptErr != nil || !bytes.Equal(attemptResultCompact.Bytes(), expectedResultCompact.Bytes()) {
		t.Fatalf("attempt structured result = %s err=%v", attemptResult, attemptErr)
	}
	if _, _, _, replayed, err := s.CompleteWorkerRunWithStructuredResult(ctx, claimed.ID, worker.ID, claimed.ClaimID,
		"已提炼公司资料", "", structuredResult, workerproto.OutcomeSucceeded, nil, finalization); err != nil || !replayed {
		t.Fatalf("structured finalization replay = %v err=%v", replayed, err)
	}
	if materialCase := materialForTask(true); materialCase == nil || materialCase.Status != MaterialCompleted {
		t.Fatalf("completed material task = %+v", materialCase)
	}
	evidenceRows, err := s.RecentWorkEvidence(ctx, time.Now().Add(-time.Hour), 20)
	if err != nil {
		t.Fatal(err)
	}
	var evidence *WorkEvidence
	for _, item := range evidenceRows {
		if item.TaskID != nil && *item.TaskID == task.ID {
			evidence = item
			break
		}
	}
	if evidence == nil || evidence.Status != WorkEvidenceActive || evidence.WorkerRunID == nil || *evidence.WorkerRunID != claimed.ID {
		t.Fatalf("atomic worker evidence = %+v", evidence)
	}

	if _, err := s.RejectTask(ctx, task.ID, boss.ID, "需要补充来源页码"); err != nil {
		t.Fatal(err)
	}
	reopened := materialForTask(false)
	if reopened == nil || reopened.Status != MaterialQueued || reopened.CompletedAt != nil || reopened.WorkerRunID == nil || *reopened.WorkerRunID == claimed.ID || reopened.LastError != "" {
		t.Fatalf("rejected material task = %+v", reopened)
	}
	reworkRun, err := s.ActiveWorkerRunForTask(ctx, task.ID)
	if err != nil || reworkRun == nil {
		t.Fatalf("rework run unavailable: %+v err=%v", reworkRun, err)
	}
	_, schemaErr := workerproto.ValidateStructuredResult(true, reworkRun.ResultSchema, structuredResult)
	if schemaErr != nil || !reworkRun.ResultRequired || reworkRun.ResultHandler != "test.material.v1" {
		t.Fatalf("rework lost structured result contract: %+v schema_err=%v", reworkRun, schemaErr)
	}
	if _, err := s.CreateMaterialTaskWithWorkerRun(ctx, &Task{
		ProjectID: project.ID, AssignerID: boss.ID, AssigneeID: worker.ID,
		Title: "重复分析", Description: "不应重复创建", Priority: "normal",
	}, []int64{file.ID}, "重复输入", WorkerRunSpec{
		Executor: workerproto.ExecutorAgent, ScopeType: "materials", ScopeKey: "materials:duplicate",
	}, MaterialTaskSpec{OwnerID: boss.ID, Title: "重复分析", Instruction: "不应重复创建"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("active material workflow duplicate = %v, want ErrConflict", err)
	}
	if _, _, err := s.CancelTask(ctx, task.ID, "本轮不再处理", nil); err != nil {
		t.Fatal(err)
	}
	if materialCase := materialForTask(true); materialCase == nil || materialCase.Status != MaterialIgnored {
		t.Fatalf("cancelled material task = %+v", materialCase)
	}
	evidence, err = scanWorkEvidence(s.pool.QueryRow(ctx,
		`SELECT `+workEvidenceCols+` FROM work_evidence e
		 LEFT JOIN users u ON u.id = e.actor_user_id
		 LEFT JOIN projects p ON p.id = e.project_id WHERE e.id = $1`, evidence.ID))
	if err != nil || evidence.Status != WorkEvidenceIgnored {
		t.Fatalf("cancelled worker evidence = %+v err=%v", evidence, err)
	}
}

func TestConversationEvalRunsExposeLatestCaseHealth(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "eval-owner", true)
	createdBy := u.ID
	evalCase, err := s.CreateConversationEvalCase(ctx, ConversationEvalCase{
		Name: "tool selection", Channel: "telegram", UserInput: "列出员工",
		Assertions: json.RawMessage(`{"required_tools":["list_users"]}`), Enabled: true, CreatedBy: &createdBy,
	})
	if err != nil {
		t.Fatal(err)
	}
	caseID := evalCase.ID
	if _, err := s.CreateConversationEvalRun(ctx, ConversationEvalRun{
		CaseID: &caseID, Status: "failed", Output: "old", Details: json.RawMessage(`{"passed":false}`), RanBy: &createdBy,
	}); err != nil {
		t.Fatal(err)
	}
	latest, err := s.CreateConversationEvalRun(ctx, ConversationEvalRun{
		CaseID: &caseID, Status: "passed", Output: "ok", Details: json.RawMessage(`{"passed":true}`), RanBy: &createdBy,
	})
	if err != nil || latest.CaseName != evalCase.Name {
		t.Fatalf("latest eval run = %+v err=%v", latest, err)
	}
	stats, err := s.ConversationEvalStats(ctx)
	if err != nil || stats.EnabledCases != 1 || stats.TestedCases != 1 || stats.PassingCases != 1 || stats.FailingCases != 0 {
		t.Fatalf("eval stats = %+v err=%v", stats, err)
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
	if err := s.MarkDataCollectionCampaignTargetsNotified(ctx, c.ID, []int64{alice.ID}); err != nil {
		t.Fatal(err)
	}
	views, err := s.ListDataCollectionCampaigns(ctx, boss.ID, true, "all", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Notified != 1 || views[0].Pending != 1 {
		t.Fatalf("活动汇总应独立统计通知与完成状态: %+v", views)
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

func TestChannelMessagesCrossSessionBoundaries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	channel := "telegram:group:-100123"
	first, err := s.StartGroupSession(ctx, boss.ID, channel, "eino")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, first.ID, "user", "【Alice】第一条"); err != nil {
		t.Fatal(err)
	}
	second, err := s.StartGroupSession(ctx, boss.ID, channel, "eino")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, second.ID, "user", "【Bob】第二条"); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListChannelMessages(ctx, channel, time.Now().Add(-time.Minute), time.Now().Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Messages) != 1 || page.Messages[0].Content != "【Bob】第二条" || page.NextCursor == 0 {
		t.Fatalf("channel page should span reset sessions and keep latest limit: %+v", page)
	}
	older, err := s.ListChannelMessagesPage(ctx, channel, time.Now().Add(-time.Minute), time.Now().Add(time.Minute), page.NextCursor, 1)
	if err != nil || older.Total != 1 || len(older.Messages) != 1 || older.Messages[0].Content != "【Alice】第一条" || older.NextCursor != 0 {
		t.Fatalf("channel cursor should read the remaining older message: page=%+v err=%v", older, err)
	}
	page, err = s.ListChannelMessages(ctx, channel, time.Now().Add(-time.Minute), time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Content != "【Alice】第一条" || page.Messages[1].Content != "【Bob】第二条" {
		t.Fatalf("channel messages should be chronological: %+v", page.Messages)
	}
}

func TestChannelMessagesFilterBySourceEventTime(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "channel-source-time", true)
	channel := "telegram:group:-100124"
	session, err := s.StartGroupSession(ctx, boss.ID, channel, "eino")
	if err != nil {
		t.Fatal(err)
	}
	eventAt := time.Date(2025, 4, 2, 8, 30, 0, 0, time.UTC)
	messageID, err := s.AppendMessageWithEnvelope(ctx, session.ID, "user", "延迟送达的历史消息", MessageEnvelope{
		Provider: "telegram", ExternalChatRef: "-100124", ExternalMessageRef: "51", SourceCreatedAt: &eventAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	historical, err := s.ListChannelMessages(ctx, channel, eventAt.Add(-time.Minute), eventAt.Add(time.Minute), 10)
	if err != nil || len(historical.Messages) != 1 || historical.Messages[0].ID != messageID {
		t.Fatalf("source-time range = %+v err=%v", historical, err)
	}
	now := time.Now().UTC()
	current, err := s.ListChannelMessages(ctx, channel, now.Add(-time.Minute), now.Add(time.Minute), 10)
	if err != nil || len(current.Messages) != 0 {
		t.Fatalf("ingestion time must not move event into current range: %+v err=%v", current, err)
	}
}

func TestAutomationScheduleUpsertIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	base := &Schedule{
		UserID: boss.ID, Kind: ScheduleDaily, Message: "daily group digest",
		FireAt: time.Now().Add(time.Hour), Target: ScheduleTargetSelf, Mode: ScheduleModeAI,
		DailyAt: "18:30", CreatedBy: boss.ID, SourceKind: ScheduleSourceTelegramGroupDigest, SourceKey: "-100123",
		Title: "项目群 每日摘要",
	}
	first, err := s.UpsertAutomationSchedule(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE schedules SET updated_at = now() - interval '1 hour' WHERE id = $1`, first.ID)
	var staleUpdatedAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT updated_at FROM schedules WHERE id = $1`, first.ID).Scan(&staleUpdatedAt); err != nil {
		t.Fatal(err)
	}
	mandatory := ScheduleRecipientMandatory
	if _, err := s.UpdateScheduleVisible(ctx, first.ID, boss.ID, true, first.FireAt, first.IntervalS, first.DailyAt, first.Weekdays, nil, &mandatory); err != nil {
		t.Fatal(err)
	}
	base.DailyAt = "19:00"
	base.Title = "项目群 晚间摘要"
	base.FireAt = time.Now().Add(2 * time.Hour)
	second, err := s.UpsertAutomationSchedule(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.DailyAt != "19:00" || second.Title != "项目群 晚间摘要" || second.RecipientPolicy != ScheduleRecipientMandatory {
		t.Fatalf("automation upsert should update one row: first=%+v second=%+v", first, second)
	}
	var refreshedUpdatedAt time.Time
	if err := s.pool.QueryRow(ctx, `SELECT updated_at FROM schedules WHERE id = $1`, first.ID).Scan(&refreshedUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if !refreshedUpdatedAt.After(staleUpdatedAt) {
		t.Fatalf("automation upsert must refresh semantic timestamp: stale=%s refreshed=%s", staleUpdatedAt, refreshedUpdatedAt)
	}
	active, err := s.HasActiveAutomationSchedule(ctx, base.SourceKind, base.SourceKey)
	if err != nil || !active {
		t.Fatalf("HasActiveAutomationSchedule active=%v err=%v", active, err)
	}
	loaded, err := s.AutomationSchedule(ctx, boss.ID, base.SourceKind, base.SourceKey)
	if err != nil || loaded.ID != first.ID {
		t.Fatalf("AutomationSchedule = %+v err=%v", loaded, err)
	}
	if err := s.CancelAutomationSchedule(ctx, boss.ID, base.SourceKind, base.SourceKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AutomationSchedule(ctx, boss.ID, base.SourceKind, base.SourceKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled automation should not be active: %v", err)
	}
	active, err = s.HasActiveAutomationSchedule(ctx, base.SourceKind, base.SourceKey)
	if err != nil || active {
		t.Fatalf("cancelled automation active=%v err=%v", active, err)
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

	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.TaskID == nil || *claimed.TaskID != tk.ID || claimed.Status != WorkerRunClaimed {
		t.Fatalf("首次认领 = %+v err=%v", claimed, err)
	}
	if claimed.ClaimID == "" {
		t.Fatal("首次认领应返回 claim id")
	}
	if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未超时不应重复认领, got %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE worker_runs SET claimed_at = now() - interval '4 hours' WHERE id = $1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || reclaimed.ID != claimed.ID || reclaimed.Status != WorkerRunClaimed {
		t.Fatalf("超时任务应可重新认领 = %+v err=%v", reclaimed, err)
	}
	if reclaimed.ClaimID == "" || reclaimed.ClaimID == claimed.ClaimID {
		t.Fatalf("超时重领应刷新 claim id: old=%q new=%q", claimed.ClaimID, reclaimed.ClaimID)
	}
}

func TestWorkerClaimRecoversUndeliveredRunQuickly(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, worker.ID, "交付确认", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	var heartbeat *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT heartbeat_at FROM worker_run_attempts WHERE run_id = $1 AND claim_id = $2`, claimed.ID, claimed.ClaimID).Scan(&heartbeat); err != nil {
		t.Fatal(err)
	}
	if heartbeat != nil {
		t.Fatalf("new claim was acknowledged before worker receipt: %s", *heartbeat)
	}
	mustExec(t, s, `UPDATE worker_runs SET claimed_at = now() - interval '2 minutes' WHERE id = $1`, claimed.ID)
	reclaimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || reclaimed.ID != claimed.ID || reclaimed.ClaimID == claimed.ClaimID {
		t.Fatalf("undelivered claim was not recovered: old=%+v new=%+v err=%v", claimed, reclaimed, err)
	}
}

func TestAcknowledgedWorkerClaimKeepsLongLease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, worker.ID, "已交付长任务", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.HeartbeatWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE worker_runs SET claimed_at = now() - interval '2 minutes' WHERE id = $1`, claimed.ID)
	if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("acknowledged claim must use the full lease timeout: %v", err)
	}
}

func TestWorkerHeartbeatRenewsLease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, worker.ID, "长任务", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE worker_runs SET claimed_at = now() - interval '4 hours' WHERE id = $1`, claimed.ID)
	if err := s.HeartbeatWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat-renewed lease must not be reclaimed: %v", err)
	}
	var heartbeat time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT heartbeat_at FROM worker_run_attempts WHERE run_id = $1 AND claim_id = $2`, claimed.ID, claimed.ClaimID).Scan(&heartbeat); err != nil {
		t.Fatal(err)
	}
	if time.Since(heartbeat) > time.Minute {
		t.Fatalf("attempt heartbeat was not refreshed: %s", heartbeat)
	}
}

func TestWorkerConcurrentPollersHoldOnlyOneClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	mkTask(t, s, pj.ID, boss.ID, worker.ID, "任务一", nil)
	mkTask(t, s, pj.ID, boss.ID, worker.ID, "任务二", nil)

	start := make(chan struct{})
	type result struct {
		run *WorkerRun
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			run, err := s.ClaimNextWorkerRun(ctx, worker.ID)
			results <- result{run: run, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes, empty := 0, 0
	var claimed *WorkerRun
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			claimed = result.run
		case errors.Is(result.err, ErrNotFound):
			empty++
		default:
			t.Fatalf("concurrent claim error: %v", result.err)
		}
	}
	if successes != 1 || empty != 1 || claimed == nil {
		t.Fatalf("concurrent claims success=%d empty=%d claimed=%+v", successes, empty, claimed)
	}
	if err := s.ReleaseWorkerRunClaim(ctx, claimed.ID, worker.ID, claimed.ClaimID); err != nil {
		t.Fatal(err)
	}
	next, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatalf("release should make worker queue immediately claimable: next=%+v err=%v", next, err)
	}
	if next.ClaimID == "" || next.ClaimID == claimed.ClaimID {
		t.Fatalf("reclaim should issue a fresh claim id: old=%q new=%q", claimed.ClaimID, next.ClaimID)
	}
}

func TestWorkerRequestInputPausesUntilTaskUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk, err := s.CreateTaskWithWorkerRun(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID, Title: "分析仓库",
	}, WorkerRunSpec{
		Executor:  workerproto.ExecutorAgent,
		ScopeType: "repo", ScopeKey: "repo:example", ScopeTitle: "Example repository",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitingRun, waitingTask, _, err := s.RequestWorkerRunInput(ctx, claimed.ID, worker.ID, claimed.ClaimID, "请提供仓库地址", testWorkerFinalization(claimed.ClaimID, "input"))
	if err != nil {
		t.Fatal(err)
	}
	if waitingRun.Status != WorkerRunAwaitingInput || waitingRun.ClaimID != "" || waitingTask == nil || waitingTask.Status != TaskAwaitingInput {
		t.Fatalf("request input result: run=%+v task=%+v", waitingRun, waitingTask)
	}
	if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("awaiting_input task must not be reclaimed: %v", err)
	}
	// Even an old timestamp cannot turn a paused task into a stale execution claim.
	if _, err := s.pool.Exec(ctx,
		`UPDATE tasks SET updated_at = now() - interval '12 hours' WHERE id = $1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old awaiting_input task must stay paused: %v", err)
	}
	unchanged, err := s.UpdateTaskContent(ctx, tk.ID, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != TaskAwaitingInput {
		t.Fatalf("empty update must not resume waiting task: %q", unchanged.Status)
	}
	description := "仓库地址：https://example.invalid/repo.git"
	kind := TaskKindEngineering
	updated, err := s.UpdateTaskContentWithKind(ctx, tk.ID, nil, &description, nil, &kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Kind != TaskKindEngineering {
		t.Fatalf("updated task kind = %q", updated.Kind)
	}
	if updated.Status != TaskPending {
		t.Fatalf("updated waiting task status = %q, want pending", updated.Status)
	}
	reclaimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || reclaimed.TaskID == nil || *reclaimed.TaskID != tk.ID || reclaimed.ID == claimed.ID {
		t.Fatalf("updated task should be claimable: %+v err=%v", reclaimed, err)
	}
	if reclaimed.ScopeType != "repo" || reclaimed.ScopeKey != "repo:example" {
		t.Fatalf("worker scope was not persisted: %+v", reclaimed)
	}
}

func TestWorkerFailureBackoffAndPause(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "会失败的任务", nil)

	for attempt := 1; attempt <= workerMaxFailures; attempt++ {
		if attempt > 1 {
			if _, err := s.pool.Exec(ctx, `UPDATE worker_runs SET available_at=now()-interval '1 second' WHERE task_id=$1 AND status='retry_wait'`, tk.ID); err != nil {
				t.Fatal(err)
			}
		}
		claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
		if err != nil {
			t.Fatalf("attempt %d claim: %v", attempt, err)
		}
		failedRun, failedTask, _, err := s.FailWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "agent unavailable", testWorkerFinalization(claimed.ClaimID, fmt.Sprintf("fail-%d", attempt)))
		if err != nil {
			t.Fatalf("attempt %d fail: %v", attempt, err)
		}
		if failedRun.Attempts != attempt || failedRun.LastError != "agent unavailable" {
			t.Fatalf("attempt %d state = %+v", attempt, failedRun)
		}
		if attempt < workerMaxFailures {
			if failedRun.Status != WorkerRunRetryWait || !failedRun.AvailableAt.After(time.Now().UTC()) || failedTask == nil || failedTask.Status != TaskPending {
				t.Fatalf("attempt %d should back off: run=%+v task=%+v", attempt, failedRun, failedTask)
			}
			if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("attempt %d should not be immediately reclaimable: %v", attempt, err)
			}
		} else if failedRun.Status != WorkerRunAwaitingInput || failedRun.ClaimID != "" || failedTask == nil || failedTask.Status != TaskAwaitingInput {
			t.Fatalf("exhausted task should pause: run=%+v task=%+v", failedRun, failedTask)
		}
	}
	if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("paused failed task must not be reclaimable: %v", err)
	}
}

func TestUpdateWorkerTaskContentAtomicallyInvalidatesClaim(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "旧要求", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	description := "新要求"
	updated, err := s.UpdateTaskContent(ctx, tk.ID, nil, &description, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != TaskPending {
		t.Fatalf("worker content update must atomically reset claim: %+v", updated)
	}
	if updated.Revision != tk.Revision+1 {
		t.Fatalf("worker content update revision=%d, want %d", updated.Revision, tk.Revision+1)
	}
	activeBefore, err := s.ActiveWorkerRunForTask(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.UpdateTaskContent(ctx, tk.ID, nil, &description, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	activeAfter, err := s.ActiveWorkerRunForTask(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Revision != updated.Revision || activeAfter.ID != activeBefore.ID {
		t.Fatalf("same-value update must not create a new revision/run: task=%+v before=%+v after=%+v", unchanged, activeBefore, activeAfter)
	}
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "旧结果", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "old")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old claim must not submit after content update: %v", err)
	}
	reclaimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, reclaimed.ID, worker.ID, reclaimed.ClaimID, "新结果", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(reclaimed.ClaimID, "new")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTaskContent(ctx, tk.ID, nil, &description, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed task content must be immutable through active update path: %v", err)
	}
	got, err := s.TaskByID(ctx, tk.ID)
	if err != nil || got.Status != TaskDone {
		t.Fatalf("completed task status changed: %+v err=%v", got, err)
	}
}

func TestTaskStatusRejectsUnknownState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	task := mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, boss.ID, "状态约束", nil)
	if _, err := s.UpdateTaskStatus(ctx, task.ID, "almost_done"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown task status must be rejected: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET status = 'almost_done' WHERE id = $1`, task.ID); err == nil {
		t.Fatal("database status constraint must reject unknown task state")
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
	// worker 认领 → task in_progress + run claim。
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
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
	run, err := s.WorkerRunByID(ctx, claimed.ID)
	if err != nil || run.Status != WorkerRunCancelled || run.ClaimID != "" {
		t.Errorf("吊销后执行应取消且清空 claim: run=%+v err=%v", run, err)
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
	missingTaskID := int64(9_999_999)
	if _, err := s.UpsertDecisionItem(ctx, DecisionItem{
		OwnerID: boss.ID, Kind: "review", Title: "已不存在的任务", RefType: "task", RefID: &missingTaskID, Priority: "high",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildDecisionQueue(ctx, boss.ID); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListDecisionItems(ctx, boss.ID, "open", 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range items {
		if d.Kind == "review" && d.RefID != nil && *d.RefID == missingTaskID {
			t.Fatalf("刷新后不应保留失效验收项: %+v", d)
		}
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

func TestDeleteTaskClosesSubtreeDecisionsAndDependencies(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker := mkUser(t, s, "worker", false)
	pj := mkProject(t, s, boss.ID)
	parent := mkTask(t, s, pj.ID, boss.ID, worker.ID, "父任务", nil)
	parentID := parent.ID
	child, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, ParentID: &parentID, AssignerID: boss.ID, AssigneeID: worker.ID, Title: "子任务",
	})
	if err != nil {
		t.Fatal(err)
	}
	dependent, err := s.CreateTask(ctx, &Task{
		ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: worker.ID, Title: "依赖子任务",
		DependsOn: []int64{child.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []int64{parent.ID, child.ID} {
		id := taskID
		if _, err := s.UpsertDecisionItem(ctx, DecisionItem{
			OwnerID: boss.ID, Kind: "review", Title: "待验收", RefType: "task", RefID: &id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteTask(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []int64{parent.ID, child.ID} {
		if _, err := s.TaskByID(ctx, taskID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("任务 %d 删除后查询错误 = %v, want ErrNotFound", taskID, err)
		}
	}
	remaining, err := s.TaskByID(ctx, dependent.ID)
	if err != nil || len(remaining.DependsOn) != 0 {
		t.Fatalf("下游依赖未清理: task=%+v err=%v", remaining, err)
	}
	items, err := s.ListDecisionItems(ctx, boss.ID, "open", 30)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.RefType == "task" && item.RefID != nil && (*item.RefID == parent.ID || *item.RefID == child.ID) {
			t.Fatalf("删除任务树后仍有开放决策: %+v", item)
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

func TestReleaseWorkerRunClaimMakesTaskImmediatelyClaimable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "跑测试", nil)

	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.TaskID == nil || *claimed.TaskID != tk.ID || claimed.ClaimID == "" {
		t.Fatalf("认领 = %+v err=%v", claimed, err)
	}
	if err := s.ReleaseWorkerRunClaim(ctx, claimed.ID, worker.ID, claimed.ClaimID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || reclaimed.ID != claimed.ID {
		t.Fatalf("释放后应立即重领 = %+v err=%v", reclaimed, err)
	}
	if reclaimed.ClaimID == "" || reclaimed.ClaimID == claimed.ClaimID {
		t.Fatalf("重领应刷新 claim id: old=%q new=%q", claimed.ClaimID, reclaimed.ClaimID)
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

	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "先交一版", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "first")); err != nil {
		t.Fatal(err)
	}
	rejected, err := s.RejectTask(ctx, tk.ID, boss.ID, "还要补文件")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != TaskPending {
		t.Fatalf("worker rejection must atomically return to pending: %+v", rejected)
	}
	reclaimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.TaskID == nil || *reclaimed.TaskID != tk.ID || reclaimed.ID == claimed.ID || reclaimed.ClaimID == "" || reclaimed.ClaimID == claimed.ClaimID {
		t.Fatalf("打回任务应刷新 claim 后重新认领: first=%q second=%+v", claimed.ClaimID, reclaimed)
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
	initialRun, err := s.LatestWorkerRunForTask(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}

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
	updatedTask, err := s.TaskByID(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	inputRun, err := s.LatestWorkerRunForTask(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedTask.Revision != tk.Revision+1 || inputRun.ID == initialRun.ID || inputRun.TaskRevision == nil || *inputRun.TaskRevision != updatedTask.Revision {
		t.Fatalf("新增任务输入必须推进版本并替换执行: task=%+v initial=%+v current=%+v", updatedTask, initialRun, inputRun)
	}
	inputSnapshot, err := s.WorkerRunFiles(ctx, inputRun.ID, "input")
	if err != nil || len(inputSnapshot) != 1 || inputSnapshot[0].ID != in.ID {
		t.Fatalf("新增附件执行快照 = %+v err=%v", inputSnapshot, err)
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
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, claimed.ID, worker.ID, "stale", in.ID); err != nil || ok {
		t.Fatalf("旧 claim 不应可下载附件 ok=%v err=%v", ok, err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, claimed.ID, worker.ID, claimed.ClaimID, in.ID); err != nil || !ok {
		t.Fatalf("worker 应可用当前 claim 下载自己的任务附件 ok=%v err=%v", ok, err)
	}
	out, err := s.CreateFile(ctx, &File{
		Source: "worker", OriginalName: "result.txt", MIMEType: "text/plain",
		SizeBytes: 6, SHA256: strings.Repeat("b", 64), StoragePath: "bb/result", CreatedBy: &worker.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddWorkerArtifact(ctx, claimed.ID, worker.ID, "stale", out.ID, "", "旧 claim"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 claim 不应可登记产物: %v", err)
	}
	if _, _, err := s.AddWorkerArtifact(ctx, claimed.ID, worker.ID, claimed.ClaimID, out.ID, "", "结果"); err != nil {
		t.Fatal(err)
	}
	arts, err := s.TaskArtifacts(ctx, tk.ID)
	if err != nil || len(arts) != 1 || arts[0].File.ID != out.ID {
		t.Fatalf("产物 = %+v err=%v", arts, err)
	}
	if ok, err := s.UserCanAccessFile(ctx, boss.ID, false, out.ID); err != nil || !ok {
		t.Fatalf("分配者应可访问产物 ok=%v err=%v", ok, err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, claimed.ID, worker.ID, "stale", out.ID); err != nil || ok {
		t.Fatalf("旧 claim 不应可下载历史产物 ok=%v err=%v", ok, err)
	}
	if ok, err := s.WorkerCanDownloadFile(ctx, claimed.ID, worker.ID, claimed.ClaimID, out.ID); err != nil || !ok {
		t.Fatalf("worker 应可用当前 claim 下载历史产物 ok=%v err=%v", ok, err)
	}

	atomicTask, err := s.CreateTaskWithFileAttachments(ctx, &Task{
		ProjectID:  pj.ID,
		AssignerID: boss.ID,
		AssigneeID: worker.ID,
		Title:      "原子发布附件任务",
		Goal:       "worker 认领时附件必须已经可见",
		Priority:   "normal",
	}, []int64{in.ID}, "输入资料")
	if err != nil {
		t.Fatal(err)
	}
	atomicAttachments, err := s.TaskFileAttachments(ctx, atomicTask.ID)
	if err != nil || len(atomicAttachments) != 1 || atomicAttachments[0].ID != in.ID {
		t.Fatalf("原子任务附件 = %+v err=%v", atomicAttachments, err)
	}
	atomicRun, err := s.LatestWorkerRunForTask(ctx, atomicTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	runInputs, err := s.WorkerRunFiles(ctx, atomicRun.ID, "input")
	if err != nil || len(runInputs) != 1 || runInputs[0].ID != in.ID {
		t.Fatalf("执行输入快照 = %+v err=%v", runInputs, err)
	}
	updatedDescription := "补充要求后仍需携带原始文件"
	if _, err := s.UpdateTaskContent(ctx, atomicTask.ID, nil, &updatedDescription, nil, nil); err != nil {
		t.Fatal(err)
	}
	restartedRun, err := s.LatestWorkerRunForTask(ctx, atomicTask.ID)
	if err != nil || restartedRun.ID == atomicRun.ID {
		t.Fatalf("任务要求更新后应创建新执行: old=%d new=%+v err=%v", atomicRun.ID, restartedRun, err)
	}
	restartedInputs, err := s.WorkerRunFiles(ctx, restartedRun.ID, "input")
	if err != nil || len(restartedInputs) != 1 || restartedInputs[0].ID != in.ID {
		t.Fatalf("重启执行必须保留输入快照 = %+v err=%v", restartedInputs, err)
	}

	var before, after int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, pj.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateTaskWithFileAttachments(ctx, &Task{
		ProjectID:  pj.ID,
		AssignerID: boss.ID,
		AssigneeID: worker.ID,
		Title:      "应整体回滚的任务",
	}, []int64{in.ID, 1 << 62}, "输入资料")
	if err == nil {
		t.Fatal("不存在的附件必须使任务与全部附件关系一起回滚")
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE project_id=$1`, pj.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("附件写入失败后留下了部分任务：before=%d after=%d", before, after)
	}
}

func TestFileIntakeLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "file-owner", false)

	pending, err := s.CreateFileIntake(ctx, FileIntake{
		UserID: u.ID, Source: "telegram", ExternalRef: "10:unique",
		OriginalName: "report.pdf", MIMEType: "application/pdf", SizeBytes: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != FileIntakePending || pending.FileID != nil {
		t.Fatalf("new intake = %+v", pending)
	}
	f, err := s.CreateFile(ctx, &File{
		Source: "telegram", OriginalName: "report.pdf", MIMEType: "application/pdf",
		SizeBytes: 128, SHA256: strings.Repeat("a", 64), StoragePath: "aa/report", CreatedBy: &u.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if canonicalID, err := s.CompleteFileIntake(ctx, pending.ID, f.ID, ""); err != nil || canonicalID != f.ID {
		t.Fatal(err)
	}

	failed, err := s.CreateFileIntake(ctx, FileIntake{
		UserID: u.ID, Source: "telegram", ExternalRef: "11:unique",
		OriginalName: "large.zip", SizeBytes: 25 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FailFileIntake(ctx, failed.ID, "telegram_cloud_limit", "文件没有进入 nbco"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO file_intakes (user_id, source, external_ref, original_name, status, error_code, error_message, canonical)
		 VALUES ($1,'telegram','10:unique','旧重放失败','failed','legacy_duplicate','仅保留审计',false)`, u.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentFileIntakesByUser(ctx, u.ID, 10, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("intakes = %+v", got)
	}
	if got[0].Status != FileIntakeFailed || got[0].ErrorCode != "telegram_cloud_limit" || got[0].FileID != nil {
		t.Fatalf("failed intake = %+v", got[0])
	}
	if got[1].Status != FileIntakeSaved || got[1].FileID == nil || *got[1].FileID != f.ID {
		t.Fatalf("saved intake = %+v", got[1])
	}
}

func TestFileContentIndexLifecycleAndVisibility(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "file-index-owner", false)
	other := mkUser(t, s, "file-index-other", false)
	f, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "员工资料.txt", MIMEType: "text/plain",
		SizeBytes: 20, SHA256: strings.Repeat("e", 64), StoragePath: "ee/file", CreatedBy: &owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ClaimFilesForContentIndex(ctx, 2, "test-v1")
	if err != nil || len(jobs) != 1 || jobs[0].ID != f.ID || jobs[0].ClaimToken == "" || jobs[0].Attempts != 1 {
		t.Fatalf("claims = %+v, %v", jobs, err)
	}
	stale := jobs[0]
	stale.ClaimToken += "-stale"
	if _, err := s.CompleteFileContentIndex(ctx, stale, "test", []string{"不应写入"}, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale claim = %v", err)
	}
	chunks, err := s.CompleteFileContentIndex(ctx, jobs[0], "test", []string{"黄桑是产品经理", "手机号由本人维护"}, false)
	if err != nil || len(chunks) != 2 || chunks[0].ChunkIndex != 0 || chunks[1].FileID != f.ID {
		t.Fatalf("chunks = %+v, %v", chunks, err)
	}
	stats, err := s.FileContentIndexStats(ctx)
	if err != nil || stats.Total != 1 || stats.Indexed != 1 || stats.Chunks != 2 || stats.Pending != 0 || stats.VectorPending != 1 {
		t.Fatalf("index stats = %+v, %v", stats, err)
	}
	if jobs, err := s.ClaimFilesForContentIndex(ctx, 2, "test-v1"); err != nil || len(jobs) != 0 {
		t.Fatalf("completed file reclaimed: %+v, %v", jobs, err)
	}
	vectorJobs, err := s.ClaimFilesForVectorIndex(ctx, 2, "embedding-v1")
	if err != nil || len(vectorJobs) != 1 || len(vectorJobs[0].Chunks) != 2 || vectorJobs[0].ClaimToken == "" {
		t.Fatalf("vector claims = %+v, %v", vectorJobs, err)
	}
	staleVector := vectorJobs[0]
	staleVector.ClaimToken += "-stale"
	if err := s.CompleteFileVectorIndex(ctx, staleVector); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale vector claim = %v", err)
	}
	if err := s.CompleteFileVectorIndex(ctx, vectorJobs[0]); err != nil {
		t.Fatal(err)
	}
	if jobs, err := s.ClaimFilesForVectorIndex(ctx, 2, "embedding-v1"); err != nil || len(jobs) != 0 {
		t.Fatalf("same model reclaimed: %+v, %v", jobs, err)
	}
	// A model/fingerprint change maps to a new Qdrant collection, so every
	// completed file must be durably reclaimed for the new model.
	vectorJobs, err = s.ClaimFilesForVectorIndex(ctx, 2, "embedding-v2")
	if err != nil || len(vectorJobs) != 1 || vectorJobs[0].Attempts != 1 || vectorJobs[0].VectorModel != "embedding-v2" {
		t.Fatalf("new-model claims = %+v, %v", vectorJobs, err)
	}
	if err := s.CompleteFileVectorIndex(ctx, vectorJobs[0]); err != nil {
		t.Fatal(err)
	}
	chunkIDs, err := s.FileTextChunkIDs(ctx)
	if err != nil || len(chunkIDs) != 2 || chunkIDs[0] != chunks[0].ID || chunkIDs[1] != chunks[1].ID {
		t.Fatalf("chunk IDs = %v, %v", chunkIDs, err)
	}
	ownerRows, err := s.ReadData(ctx, owner.ID, false, DataReadQuery{Source: "file_chunks", Terms: []string{"产品经理"}, Limit: 10})
	if err != nil || len(ownerRows) != 1 || !strings.Contains(string(ownerRows[0]), "黄桑") {
		t.Fatalf("owner rows = %s, %v", ownerRows, err)
	}
	otherRows, err := s.ReadData(ctx, other.ID, false, DataReadQuery{Source: "file_chunks", Limit: 10})
	if err != nil || len(otherRows) != 0 {
		t.Fatalf("other rows leaked = %s, %v", otherRows, err)
	}
}

func TestUnsupportedFileRetriesOnlyWhenExtractorCapabilitiesChange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "extractor-revision-owner", false)
	if _, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "scan.bin", MIMEType: "application/octet-stream",
		SizeBytes: 1, SHA256: strings.Repeat("a", 64), StoragePath: "aa/scan", CreatedBy: &owner.ID,
	}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ClaimFilesForContentIndex(ctx, 1, "cap-v1")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("first claim = %+v, %v", jobs, err)
	}
	if err := s.FailFileContentIndex(ctx, jobs[0], errors.New("unsupported"), false); err != nil {
		t.Fatal(err)
	}
	if jobs, err := s.ClaimFilesForContentIndex(ctx, 1, "cap-v1"); err != nil || len(jobs) != 0 {
		t.Fatalf("same capabilities reclaimed terminal file: %+v, %v", jobs, err)
	}
	jobs, err = s.ClaimFilesForContentIndex(ctx, 1, "cap-v2")
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 || jobs[0].ExtractorRevision != "cap-v2" {
		t.Fatalf("changed capabilities claim = %+v, %v", jobs, err)
	}
}

func TestFileIndexAbandonedClaimsStopAndRecoverAfterRuntimeChange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "abandoned-file-index-owner", false)
	file, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "abandoned.txt", MIMEType: "text/plain",
		SizeBytes: 1, SHA256: strings.Repeat("b", 64), StoragePath: "bb/abandoned", CreatedBy: &owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE file_content_indexes
		SET status='processing', attempts=$2, extractor_revision='extract-v1',
		    claim_token='dead', claimed_at=now()-interval '1 hour'
		WHERE file_id=$1`, file.ID, fileIndexMaxAttempts)
	if jobs, err := s.ClaimFilesForContentIndex(ctx, 1, "extract-v1"); err != nil || len(jobs) != 0 {
		t.Fatalf("exhausted extraction claim = %+v, %v", jobs, err)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM file_content_indexes WHERE file_id=$1`, file.ID).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("exhausted extraction status = %q, %v", status, err)
	}
	jobs, err := s.ClaimFilesForContentIndex(ctx, 1, "extract-v2")
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 {
		t.Fatalf("changed extractor recovery = %+v, %v", jobs, err)
	}
	if _, err := s.CompleteFileContentIndex(ctx, jobs[0], "test", []string{"recoverable"}, false); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE file_content_indexes
		SET vector_status='processing', vector_attempts=$2, vector_model='embed-v1',
		    vector_claim_token='dead', vector_claimed_at=now()-interval '1 hour'
		WHERE file_id=$1`, file.ID, fileIndexMaxAttempts)
	if jobs, err := s.ClaimFilesForVectorIndex(ctx, 1, "embed-v1"); err != nil || len(jobs) != 0 {
		t.Fatalf("exhausted vector claim = %+v, %v", jobs, err)
	}
	var vectorStatus string
	if err := s.pool.QueryRow(ctx, `SELECT vector_status FROM file_content_indexes WHERE file_id=$1`, file.ID).Scan(&vectorStatus); err != nil || vectorStatus != "failed" {
		t.Fatalf("exhausted vector status = %q, %v", vectorStatus, err)
	}
	vectorJobs, err := s.ClaimFilesForVectorIndex(ctx, 1, "embed-v2")
	if err != nil || len(vectorJobs) != 1 || vectorJobs[0].Attempts != 1 {
		t.Fatalf("changed model recovery = %+v, %v", vectorJobs, err)
	}
}

func TestWorkspaceCandidatesAndDeleteFile(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "workspace-owner", false)
	other := mkUser(t, s, "workspace-other", false)
	admin := mkUser(t, s, "workspace-admin", true)
	pj, err := s.CreateProject(ctx, "无成人陪伴资料项目", "", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := mkTask(t, s, pj.ID, owner.ID, owner.ID, "无成人陪伴申请表分析", nil)
	owned, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "无成人陪伴乘机申请表.pdf", MIMEType: "application/pdf",
		SizeBytes: 10, SHA256: strings.Repeat("c", 64), StoragePath: "cc/owned", CreatedBy: &owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherFile, err := s.CreateFile(ctx, &File{
		Source: "test", OriginalName: "无成人陪伴内部附件.pdf", MIMEType: "application/pdf",
		SizeBytes: 11, SHA256: strings.Repeat("d", 64), StoragePath: "dd/other", CreatedBy: &other.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	matches, err := s.WorkspaceCandidates(ctx, owner.ID, false, WorkspaceCandidateFilter{
		Terms: []string{"无成人陪伴"}, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range matches {
		seen[fmt.Sprintf("%s:%d", item.Kind, item.ID)] = true
	}
	for _, want := range []string{fmt.Sprintf("task:%d", task.ID), fmt.Sprintf("file:%d", owned.ID), fmt.Sprintf("project:%d", pj.ID)} {
		if !seen[want] {
			t.Fatalf("owner search missing %s: %+v", want, matches)
		}
	}
	if seen[fmt.Sprintf("file:%d", otherFile.ID)] {
		t.Fatalf("owner search leaked another user's file: %+v", matches)
	}
	adminMatches, err := s.WorkspaceCandidates(ctx, admin.ID, true, WorkspaceCandidateFilter{
		Terms: []string{"无成人陪伴"}, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundOther := false
	for _, item := range adminMatches {
		foundOther = foundOther || item.Kind == "file" && item.ID == otherFile.ID
	}
	if !foundOther {
		t.Fatalf("superadmin search should include all matching files: %+v", adminMatches)
	}

	if err := s.AddTaskAttachmentFile(ctx, task.ID, owned.ID, "input"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUnreferencedFile(ctx, owned.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("referenced file delete = %v, want conflict", err)
	}
	if err := s.DeleteTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUnreferencedFile(ctx, owned.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FileByID(ctx, owned.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted file lookup = %v, want not found", err)
	}
}

func TestReadDataEnforcesRowAndFieldVisibility(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "data-owner", false)
	other := mkUser(t, s, "data-other", false)
	admin := mkUser(t, s, "data-admin", true)
	if err := s.UpdateUserInfo(ctx, other.ID, map[string]string{"phone": "13800000000"}); err != nil {
		t.Fatal(err)
	}
	ownerProject := mkProject(t, s, owner.ID)
	ownerTask := mkTask(t, s, ownerProject.ID, owner.ID, owner.ID, "owner visible task", nil)
	otherProject := mkProject(t, s, other.ID)
	otherTask := mkTask(t, s, otherProject.ID, other.ID, other.ID, "other hidden task", nil)

	ownerRows, err := s.ReadData(ctx, owner.ID, false, DataReadQuery{Source: "tasks", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	ownerIDs := dataReadIDs(t, ownerRows, "task_id")
	if !ownerIDs[ownerTask.ID] || ownerIDs[otherTask.ID] {
		t.Fatalf("ordinary task visibility = %v", ownerIDs)
	}
	adminRows, err := s.ReadData(ctx, admin.ID, true, DataReadQuery{Source: "tasks", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	adminIDs := dataReadIDs(t, adminRows, "task_id")
	if !adminIDs[ownerTask.ID] || !adminIDs[otherTask.ID] {
		t.Fatalf("admin task visibility = %v", adminIDs)
	}

	userRows, err := s.ReadData(ctx, owner.ID, false, DataReadQuery{
		Source: "users", Filters: map[string]string{"user_id": fmt.Sprint(other.ID)}, Limit: 5,
	})
	if err != nil || len(userRows) != 1 {
		t.Fatalf("user read = %s, %v", userRows, err)
	}
	var hidden struct {
		Info map[string]string `json:"info"`
	}
	if err := json.Unmarshal(userRows[0], &hidden); err != nil {
		t.Fatal(err)
	}
	if len(hidden.Info) != 0 {
		t.Fatalf("hidden info leaked: %#v", hidden.Info)
	}
	ghost := mkUser(t, s, "data-unrooted-viewer", false)
	mustExec(t, s,
		`INSERT INTO permissions (kind, user_id, action, target, granted_by)
		 VALUES ('active', $1, 'view_self_intro', $2, $1)`,
		ghost.ID, fmt.Sprint(other.ID))
	ghostRows, err := s.ReadData(ctx, ghost.ID, false, DataReadQuery{
		Source: "users", Filters: map[string]string{"user_id": fmt.Sprint(other.ID)}, Limit: 5,
	})
	if err != nil || len(ghostRows) != 1 {
		t.Fatalf("unrooted user read = %s, %v", ghostRows, err)
	}
	if err := json.Unmarshal(ghostRows[0], &hidden); err != nil {
		t.Fatal(err)
	}
	if len(hidden.Info) != 0 {
		t.Fatalf("unrooted permission leaked info: %#v", hidden.Info)
	}
	permissionRows, err := s.ReadData(ctx, ghost.ID, false, DataReadQuery{Source: "permissions", Limit: 5})
	if err != nil || len(permissionRows) != 0 {
		t.Fatalf("unrooted permission exposed as effective: %s, %v", permissionRows, err)
	}
	if err := s.GrantPerm(ctx, Grant{
		Kind: KindActive, UserID: owner.ID, Action: "view_self_intro",
		Target: fmt.Sprint(other.ID), GrantedBy: admin.ID,
	}); err != nil {
		t.Fatal(err)
	}
	userRows, err = s.ReadData(ctx, owner.ID, false, DataReadQuery{
		Source: "users", Filters: map[string]string{"user_id": fmt.Sprint(other.ID)}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	var visible struct {
		Info map[string]string `json:"info"`
	}
	if err := json.Unmarshal(userRows[0], &visible); err != nil {
		t.Fatal(err)
	}
	if visible.Info["phone"] != "13800000000" {
		t.Fatalf("granted info = %#v", visible.Info)
	}
	if _, err := s.ReadData(ctx, owner.ID, false, DataReadQuery{Source: "audit_activity"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary audit access = %v", err)
	}
	if _, err := s.ReadData(ctx, owner.ID, false, DataReadQuery{
		Source: "users", Filters: map[string]string{"unknown_field": "x"},
	}); !errors.Is(err, ErrInvalidDataQuery) {
		t.Fatalf("invalid data filter error = %v", err)
	}
}

func TestReadDataWorkEvidenceUsesStableIdentityAndRelationshipVisibility(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "evidence-owner", false)
	participant := mkUser(t, s, "evidence-participant", false)
	unrelated := mkUser(t, s, "evidence-unrelated", false)
	admin := mkUser(t, s, "evidence-admin", true)
	project := mkProject(t, s, owner.ID)
	task := mkTask(t, s, project.ID, owner.ID, owner.ID, "evidence task", nil)
	if _, err := s.ReplaceTaskParticipants(ctx, task.ID, owner.ID, []TaskParticipantInput{{
		UserID: participant.ID, Role: TaskParticipantWatcher,
	}}); err != nil {
		t.Fatal(err)
	}

	visible, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: "telegram_group_message", SourceKey: "evidence-visible",
		Kind: WorkEvidenceCommunication, Status: WorkEvidenceObserved,
		Title: "旧显示名", Content: "稳定身份关联的工作事实", ActorUserID: &owner.ID,
		ProjectID: &project.ID, TaskID: &task.ID, CreatedBy: &owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: "telegram_group_message", SourceKey: "evidence-hidden",
		Kind: WorkEvidenceCommunication, Status: WorkEvidenceObserved,
		Title: unrelated.Name, Content: "无关系工作事实", ActorUserID: &unrelated.ID,
		CreatedBy: &unrelated.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, viewer := range []*User{owner, participant} {
		rows, err := s.ReadData(ctx, viewer.ID, false, DataReadQuery{Source: "work_evidence", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		ids := dataReadIDs(t, rows, "evidence_id")
		if !ids[visible.ID] || ids[hidden.ID] {
			t.Fatalf("viewer %d evidence visibility = %v", viewer.ID, ids)
		}
		var item struct {
			ActorUserID int64  `json:"actor_user_id"`
			ActorName   string `json:"actor_name"`
		}
		if err := json.Unmarshal(rows[0], &item); err != nil {
			t.Fatal(err)
		}
		if item.ActorUserID != owner.ID || item.ActorName != owner.Name {
			t.Fatalf("stable actor identity = %+v, want %d/%q", item, owner.ID, owner.Name)
		}
	}

	rows, err := s.ReadData(ctx, unrelated.ID, false, DataReadQuery{Source: "work_evidence", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	ids := dataReadIDs(t, rows, "evidence_id")
	if ids[visible.ID] || !ids[hidden.ID] {
		t.Fatalf("unrelated evidence visibility = %v", ids)
	}
	rows, err = s.ReadData(ctx, admin.ID, true, DataReadQuery{Source: "work_evidence", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	ids = dataReadIDs(t, rows, "evidence_id")
	if !ids[visible.ID] || !ids[hidden.ID] {
		t.Fatalf("admin evidence visibility = %v", ids)
	}
}

func TestSemanticDocumentsDoNotDuplicateRawGroupCommunications(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "semantic-evidence-owner", true)
	communication, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: "telegram_group_message", SourceKey: "semantic-communication",
		Kind: WorkEvidenceCommunication, Content: "群消息原文", ActorUserID: &owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	deliverable, err := s.UpsertWorkEvidence(ctx, WorkEvidenceInput{
		SourceType: "worker_run", SourceKey: "semantic-deliverable",
		Kind: WorkEvidenceDeliverable, Content: "已验证交付物", ActorUserID: &owner.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	docs, err := s.SemanticDocuments(ctx, "work_evidence", nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].EntityID != fmt.Sprint(deliverable.ID) ||
		docs[0].EntityID == fmt.Sprint(communication.ID) || !strings.Contains(docs[0].Content, "已验证交付物") {
		t.Fatalf("work evidence semantic documents = %+v", docs)
	}
}

func TestGroupMessageProvenanceBackfillUsesOnlyUnambiguousExactActors(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "backfill-owner", true)
	alice := mkUser(t, s, "backfill-alice", false)
	ambiguousA := mkUser(t, s, "backfill-duplicate", false)
	ambiguousB, err := s.CreateUser(ctx, ambiguousA.Name, false, Identity{Provider: "test", ExternalID: "backfill-duplicate-2"})
	if err != nil || ambiguousB.ID == ambiguousA.ID {
		t.Fatalf("create duplicate display name = %+v, %v", ambiguousB, err)
	}
	session, err := s.StartGroupSession(ctx, owner.ID, "telegram:group:-9001", "test")
	if err != nil {
		t.Fatal(err)
	}
	aliceID, err := s.AppendMessage(ctx, session.ID, "user", "【"+alice.Name+"】完成历史任务")
	if err != nil {
		t.Fatal(err)
	}
	ambiguousID, err := s.AppendMessage(ctx, session.ID, "user", "【"+ambiguousA.Name+"】不能猜身份")
	if err != nil {
		t.Fatal(err)
	}
	migration, err := migrationsFS.ReadFile("migrations/0090_backfill_group_message_provenance.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.pool.Exec(ctx, string(migration)); err != nil {
			t.Fatal(err)
		}
	}

	aliceMessage, err := s.ChatMessageByID(ctx, aliceID)
	if err != nil || aliceMessage.ActorUserID == nil || *aliceMessage.ActorUserID != alice.ID ||
		aliceMessage.ActorDisplayName != alice.Name || aliceMessage.Provider != "telegram" ||
		aliceMessage.ExternalChatRef != "-9001" || aliceMessage.SourceCreatedAt == nil {
		t.Fatalf("backfilled exact actor = %+v, %v", aliceMessage, err)
	}
	ambiguousMessage, err := s.ChatMessageByID(ctx, ambiguousID)
	if err != nil || ambiguousMessage.ActorUserID != nil || ambiguousMessage.ActorDisplayName != ambiguousA.Name {
		t.Fatalf("ambiguous actor must stay unbound = %+v, %v", ambiguousMessage, err)
	}
	var evidenceCount int
	var evidenceActor *int64
	var evidenceContent string
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), max(actor_user_id), max(content)
		   FROM work_evidence WHERE source_message_id = $1`, aliceID).
		Scan(&evidenceCount, &evidenceActor, &evidenceContent); err != nil {
		t.Fatal(err)
	}
	if evidenceCount != 1 || evidenceActor == nil || *evidenceActor != alice.ID || evidenceContent != "完成历史任务" {
		t.Fatalf("backfilled evidence count=%d actor=%v content=%q", evidenceCount, evidenceActor, evidenceContent)
	}
}

func dataReadIDs(t *testing.T, rows []json.RawMessage, field string) map[int64]bool {
	t.Helper()
	out := map[int64]bool{}
	for _, row := range rows {
		var item map[string]any
		if err := json.Unmarshal(row, &item); err != nil {
			t.Fatal(err)
		}
		id, ok := item[field].(float64)
		if !ok {
			t.Fatalf("%s missing numeric %s", row, field)
		}
		out[int64(id)] = true
	}
	return out
}

func TestReadDataAllSourcesCompile(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	admin := mkUser(t, s, "catalog-admin", true)
	employee := mkUser(t, s, "catalog-employee", false)
	for _, tc := range []struct {
		user    *User
		sources []DataSource
	}{
		{admin, DataSources(true)},
		{employee, DataSources(false)},
	} {
		for _, source := range tc.sources {
			t.Run(fmt.Sprintf("user_%d/%s", tc.user.ID, source.Name), func(t *testing.T) {
				if _, err := s.ReadData(ctx, tc.user.ID, tc.user.IsSuperadmin, DataReadQuery{
					Source: source.Name, Limit: 1,
				}); err != nil {
					t.Fatalf("source %s: %v", source.Name, err)
				}
			})
		}
	}
	for _, source := range SemanticDataSources() {
		t.Run("semantic/"+source, func(t *testing.T) {
			if _, err := s.SemanticDocuments(ctx, source, nil, nil, 1); err != nil {
				t.Fatalf("semantic source %s: %v", source, err)
			}
		})
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
	deadlineGeneration := warn[0].DeadlineGeneration
	if warn, _ = s.DueDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("租约内重复认领应为空, got %d", len(warn))
	}
	mustExec(t, s, `UPDATE tasks SET deadline_reminder_claimed_at = now() - interval '20 minutes' WHERE id = $1`, tk.ID)
	if warn, err = s.DueDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 {
		t.Fatalf("租约过期应可重试, got %d err=%v", len(warn), err)
	}
	if err := s.MarkDeadlineReminderSent(ctx, tk.ID, deadlineGeneration, now); err != nil {
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
	if err := s.MarkOverdueNoticeSent(ctx, tk.ID, over[0].DeadlineGeneration, now); err != nil {
		t.Fatal(err)
	}
	if over, _ = s.DueOverdueNotices(ctx, now); len(over) != 0 {
		t.Fatalf("过期通知 ack 后不应再认领, got %d", len(over))
	}

	// 传输边界已结算但渠道未确认时，不重放，也不伪造 sent 事实。
	failedSoon := now.Add(3 * time.Hour)
	failed := mkTask(t, s, pj.ID, boss.ID, alice.ID, "投递未确认", &failedSoon)
	if warn, err = s.DueDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 || warn[0].ID != failed.ID {
		t.Fatalf("未确认提醒认领=%v err=%v", warn, err)
	}
	if err := s.MarkDeadlineReminderAttempt(ctx, failed.ID, warn[0].DeadlineGeneration, now, false); err != nil {
		t.Fatal(err)
	}
	var attemptedAt, sentAt *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT deadline_reminder_attempted_at, deadline_reminded_at FROM tasks WHERE id=$1`, failed.ID).
		Scan(&attemptedAt, &sentAt); err != nil || attemptedAt == nil || sentAt != nil {
		t.Fatalf("未确认提醒 attempted=%v sent=%v err=%v", attemptedAt, sentAt, err)
	}
	if warn, _ = s.DueDeadlineReminders(ctx, now, 24*time.Hour); len(warn) != 0 {
		t.Fatalf("已结算未确认提醒不应盲目重放, got %d", len(warn))
	}

	// 投递期间改期时，旧 claim 不能结算新一代；改走再改回原值也是新事件。
	replayedDeadline := now.Add(5 * time.Hour)
	replayed := mkTask(t, s, pj.ID, boss.ID, alice.ID, "改期回滚", &replayedDeadline)
	if warn, err = s.DueDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 || warn[0].ID != replayed.ID {
		t.Fatalf("改期任务首次认领=%v err=%v", warn, err)
	}
	oldGeneration := warn[0].DeadlineGeneration
	movedDeadline := now.Add(6 * time.Hour)
	if _, err := s.UpdateTaskContent(ctx, replayed.ID, nil, nil, nil, &movedDeadline); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDeadlineReminderSent(ctx, replayed.ID, oldGeneration, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧代次 ack 应被拒绝，got %v", err)
	}
	updated, err := s.UpdateTaskContent(ctx, replayed.ID, nil, nil, nil, &replayedDeadline)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DeadlineGeneration <= oldGeneration {
		t.Fatalf("截止时间回滚必须产生新代次: old=%d new=%d", oldGeneration, updated.DeadlineGeneration)
	}
	if warn, err = s.DueDeadlineReminders(ctx, now, 24*time.Hour); err != nil || len(warn) != 1 || warn[0].DeadlineGeneration != updated.DeadlineGeneration {
		t.Fatalf("回滚后的新一代应可认领: warn=%v err=%v", warn, err)
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
	if err := s.MarkNudgeSent(ctx, stale.ID, due[0].DeadlineGeneration, now); err != nil {
		t.Fatal(err)
	}
	if due, _ = s.DueNudges(ctx, now, 48*time.Hour); len(due) != 0 {
		t.Fatalf("催办 ack 后不应重复, got %d", len(due))
	}
}

func TestFailedNudgeAttemptDoesNotCountAsDelivered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	alice := mkUser(t, s, "alice", false)
	pj := mkProject(t, s, boss.ID)
	now := time.Now().UTC()
	past := now.Add(-72 * time.Hour)
	task := mkTask(t, s, pj.ID, boss.ID, alice.ID, "投递失败", &past)
	mustExec(t, s, `UPDATE tasks SET overdue_notified_at=$2 WHERE id=$1`, task.ID, now.Add(-49*time.Hour))

	due, err := s.DueNudges(ctx, now, 48*time.Hour)
	if err != nil || len(due) != 1 || due[0].NudgeAttemptCount != 0 || due[0].NudgeCount != 0 {
		t.Fatalf("first nudge claim = %+v err=%v", due, err)
	}
	if err := s.MarkNudgeAttempt(ctx, task.ID, due[0].DeadlineGeneration, now, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.TaskByID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NudgeAttemptCount != 1 || got.NudgeCount != 0 {
		t.Fatalf("failed attempt counts = attempts:%d delivered:%d", got.NudgeAttemptCount, got.NudgeCount)
	}
	mustExec(t, s, `UPDATE tasks SET nudged_at=$2 WHERE id=$1`, task.ID, now.Add(-49*time.Hour))
	due, err = s.DueNudges(ctx, now, 48*time.Hour)
	if err != nil || len(due) != 1 || due[0].NudgeAttemptCount != 1 {
		t.Fatalf("next nudge claim = %+v err=%v", due, err)
	}
	if err := s.MarkNudgeAttempt(ctx, task.ID, due[0].DeadlineGeneration, now, true); err != nil {
		t.Fatal(err)
	}
	got, err = s.TaskByID(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NudgeAttemptCount != 2 || got.NudgeCount != 1 {
		t.Fatalf("settled nudge counts = attempts:%d delivered:%d", got.NudgeAttemptCount, got.NudgeCount)
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
	if _, _, err := s.SubmitTask(ctx, done.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptTask(ctx, done.ID); err != nil {
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
	related, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Title: " worker token 不外发 ", Content: "不要把 worker token 发到群里。", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ScoreLearningCandidates(ctx, 1); err != nil || n != 1 {
		t.Fatalf("ScoreLearningCandidates = %d, %v", n, err)
	}
	got, err := s.LearningCandidateByID(ctx, related.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DuplicateOf != nil || got.ConflictWith != nil || got.Status != LearningStatusPending ||
		!strings.Contains(got.ReviewNote, "相关候选") || got.ValueScore <= 0 {
		t.Fatalf("相似文本只能作为 Agent 审核线索: %+v old=%d", got, old.ID)
	}
	// Simulate an exact duplicate carried by a pre-identity migration row. New
	// concurrent writes are blocked by the database unique identity below.
	var exactID int64
	err = s.pool.QueryRow(ctx,
		`INSERT INTO learning_candidates
		   (kind, scope, title, content, created_by, memory_class, content_identity)
		 VALUES ($1, 'global', $2, $3, $4, $5, $6) RETURNING id`,
		LearningKindRule, " worker token 不外发 ", "不要把 worker token 发给用户。", boss.ID,
		LearningMemoryDurable, "legacy:test-exact").Scan(&exactID)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := s.LearningCandidateByID(ctx, exactID)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ScoreLearningCandidates(ctx, 1); err != nil || n != 1 {
		t.Fatalf("ScoreLearningCandidates exact = %d, %v", n, err)
	}
	got, err = s.LearningCandidateByID(ctx, exact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DuplicateOf == nil || *got.DuplicateOf != old.ID || got.Status != LearningStatusRejected {
		t.Fatalf("精确重复应幂等归档: %+v old=%d", got, old.ID)
	}
	conflicting, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Title: "Worker Token 外发规则", Content: "默认允许把 worker token 发给用户。", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ScoreLearningCandidates(ctx, 1); err != nil || n != 1 {
		t.Fatalf("ScoreLearningCandidates conflict = %d, %v", n, err)
	}
	got, err = s.LearningCandidateByID(ctx, conflicting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConflictWith != nil || got.DuplicateOf != nil || got.Status != LearningStatusPending ||
		!strings.Contains(got.ReviewNote, "Agent") {
		t.Fatalf("自然语言方向不能由数据库词表猜测: %+v", got)
	}
}

func TestLearningCandidateIdentityIsConcurrent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	user := mkUser(t, s, "learning-concurrent", false)
	input := LearningCandidateInput{
		Kind: LearningKindRule, Scope: "user:1", Title: " 统一 通知 ", Content: "只发送一次。",
		CreatedBy: &user.ID, MemoryClass: LearningMemoryDurable,
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.CreateLearningCandidate(ctx, input)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	var created, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent insert error = %v", err)
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("concurrent identity results = created:%d conflicts:%d", created, conflicts)
	}
}

func TestLearningCandidateIdentityNormalizesScopeWhitespace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	user := mkUser(t, s, "learning-scope-normalization", false)
	input := LearningCandidateInput{
		Kind: LearningKindRule, Scope: " user:42 ", Title: "统一通知", Content: "只发送一次。",
		CreatedBy: &user.ID, MemoryClass: LearningMemoryDurable,
	}
	created, err := s.CreateLearningCandidate(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Scope != "user:42" {
		t.Fatalf("normalized scope = %q", created.Scope)
	}
	input.Scope = "user:42"
	if _, err := s.CreateLearningCandidate(ctx, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("scope whitespace bypassed identity: %v", err)
	}
}

func TestLearningCandidateDeduplicationRespectsScope(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "learning-scope-boss", true)

	first, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Scope: "scope:user:101", Title: "回复风格", Content: "回复保持简洁。", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Scope: "scope:user:202", Title: "回复风格", Content: "回复保持简洁。", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ScoreLearningCandidates(ctx, 10); err != nil || n != 2 {
		t.Fatalf("ScoreLearningCandidates = %d, %v", n, err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		candidate, err := s.LearningCandidateByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Status != LearningStatusPending || candidate.DuplicateOf != nil {
			t.Fatalf("cross-scope candidate %d was deduplicated: %+v", id, candidate)
		}
	}

	found, err := s.EquivalentLearningCandidateExistsInClass(
		ctx, LearningKindRule, LearningMemoryDurable, "scope:user:101", "回复风格", "回复保持简洁。", LearningStatusPending,
	)
	if err != nil || !found {
		t.Fatalf("same-scope equivalent = %v, %v", found, err)
	}
	found, err = s.EquivalentLearningCandidateExistsInClass(
		ctx, LearningKindRule, LearningMemoryDurable, "scope:user:303", "回复风格", "回复保持简洁。", LearningStatusPending,
	)
	if err != nil || found {
		t.Fatalf("cross-scope equivalent = %v, %v", found, err)
	}
}

func TestEquivalentLearningCandidateExistsBeyondRecentWindow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	user := mkUser(t, s, "learning-deep-dedupe", false)
	scope := fmt.Sprintf("user:%d", user.ID)
	original, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Scope: scope, Title: "长期沟通规则", Content: "非紧急通知统一在工作时间发送。",
		Status: LearningStatusPending, MemoryClass: LearningMemoryDurable, CreatedBy: &user.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		if _, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
			Kind: LearningKindRule, Scope: scope, Title: fmt.Sprintf("其他规则 %03d", i), Content: fmt.Sprintf("不同内容 %03d", i),
			Status: LearningStatusPending, MemoryClass: LearningMemoryDurable, CreatedBy: &user.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	found, err := s.EquivalentLearningCandidateExistsInClass(ctx, LearningKindRule, LearningMemoryDurable, scope,
		original.Title, original.Content, LearningStatusPending)
	if err != nil || !found {
		t.Fatalf("old exact candidate should remain discoverable: found=%t err=%v", found, err)
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

func TestSuperadminBootstrapResponseRecovery(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	token := strings.Repeat("ab", 24)
	ident := Identity{Provider: "api", ExternalID: "bootstrap:http:stable", ChatRef: "api:bootstrap:http:stable"}

	first, firstToken, err := s.BootstrapSuperadminWithAPITokenCandidate(ctx, "老板", ident, token)
	if err != nil {
		t.Fatal(err)
	}
	replayed, replayedToken, err := s.BootstrapSuperadminWithAPITokenCandidate(ctx, "老板", ident, token)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != replayed.ID || firstToken != token || replayedToken != token {
		t.Fatalf("bootstrap replay changed result: first=%+v replay=%+v tokens=%q/%q", first, replayed, firstToken, replayedToken)
	}
	if _, _, err := s.BootstrapSuperadminWithAPITokenCandidate(ctx, "另一个名字", ident, token); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed bootstrap payload = %v, want ErrConflict", err)
	}
	if _, _, err := s.BootstrapSuperadminWithAPITokenCandidate(ctx, "老板", ident, strings.Repeat("cd", 24)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed bootstrap token = %v, want ErrConflict", err)
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

func TestAPITokenCandidateAndRotationRecovery(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "token-rotation-boss", true)

	first := strings.Repeat("ab", 24)
	if got, err := s.IssueAPITokenCandidate(ctx, boss.ID, first); err != nil || got != first {
		t.Fatalf("issue candidate got=%q err=%v", got, err)
	}
	if got, err := s.IssueAPITokenCandidate(ctx, boss.ID, first); err != nil || got != first {
		t.Fatalf("replay candidate got=%q err=%v", got, err)
	}
	var tokenCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE user_id=$1`, boss.ID).Scan(&tokenCount); err != nil || tokenCount != 1 {
		t.Fatalf("candidate token count=%d err=%v", tokenCount, err)
	}

	pending, err := s.BeginAPITokenRotation(ctx, boss.ID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	replayedPending, err := s.BeginAPITokenRotation(ctx, boss.ID, 10*time.Minute)
	if err != nil || replayedPending.Candidate != pending.Candidate || !replayedPending.ExpiresAt.Equal(pending.ExpiresAt) {
		t.Fatalf("pending replay=%+v first=%+v err=%v", replayedPending, pending, err)
	}
	confirmed, err := s.ConfirmAPITokenRotation(ctx, boss.ID)
	if err != nil || confirmed.Candidate != pending.Candidate || confirmed.IssuedAt == nil {
		t.Fatalf("confirm=%+v err=%v", confirmed, err)
	}
	confirmedAgain, err := s.ConfirmAPITokenRotation(ctx, boss.ID)
	if err != nil || confirmedAgain.Candidate != confirmed.Candidate {
		t.Fatalf("confirm replay=%+v err=%v", confirmedAgain, err)
	}
	if _, err := s.UserByAPIToken(ctx, confirmed.Candidate); err != nil {
		t.Fatalf("confirmed token invalid: %v", err)
	}
	if _, err := s.UserByAPIToken(ctx, first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old candidate remains valid: %v", err)
	}
	if err := s.AcknowledgeAPITokenRotation(ctx, boss.ID, confirmed.Candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmAPITokenRotation(ctx, boss.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("acknowledged rotation confirm=%v, want ErrNotFound", err)
	}

	expired, err := s.BeginAPITokenRotation(ctx, boss.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if deleted, err := s.DeleteExpiredAPITokenRotations(ctx, time.Now().UTC()); err != nil || deleted != 1 {
		t.Fatalf("expired rotation=%+v deleted=%d err=%v", expired, deleted, err)
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
	if err := s.RevokePerm(ctx, boss.ID, KindActive, alice.ID, "send_msg", TargetAll); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePerm(ctx, boss.ID, KindActive, alice.ID, "send_msg", TargetAll); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复撤销应 ErrNotFound, got %v", err)
	}
}

func TestActivePermissionDelegationGraph(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	root := mkUser(t, s, "permission-root", true)
	managerA := mkUser(t, s, "permission-manager-a", false)
	managerB := mkUser(t, s, "permission-manager-b", false)
	lead := mkUser(t, s, "permission-lead", false)
	member := mkUser(t, s, "permission-member", false)
	target := mkUser(t, s, "permission-target", false)
	targetKey := strconv.FormatInt(target.ID, 10)
	if err := s.GrantPerm(ctx, Grant{
		Kind: KindActive, UserID: member.ID, Action: "send_msg", Target: targetKey, GrantedBy: managerA.ID,
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unrooted grant = %v, want ErrForbidden", err)
	}

	grant := func(userID, grantedBy int64) {
		t.Helper()
		if err := s.GrantPerm(ctx, Grant{
			Kind: KindActive, UserID: userID, Action: "send_msg", Target: targetKey, GrantedBy: grantedBy,
		}); err != nil {
			t.Fatal(err)
		}
	}
	grant(managerA.ID, root.ID)
	grant(managerB.ID, root.ID)
	if err := s.GrantPerm(ctx, Grant{
		Kind: KindActive, UserID: lead.ID, Action: "send_msg", Target: targetKey, GrantedBy: managerA.ID,
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("delegation without manage_perm = %v, want ErrForbidden", err)
	}
	for _, edge := range []Grant{
		{Kind: KindActive, UserID: managerA.ID, Action: "manage_perm", Target: strconv.FormatInt(lead.ID, 10), GrantedBy: root.ID},
		{Kind: KindActive, UserID: managerB.ID, Action: "manage_perm", Target: strconv.FormatInt(lead.ID, 10), GrantedBy: root.ID},
		{Kind: KindActive, UserID: lead.ID, Action: "manage_perm", Target: strconv.FormatInt(member.ID, 10), GrantedBy: root.ID},
	} {
		if err := s.GrantPerm(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	grant(lead.ID, managerA.ID)
	grant(lead.ID, managerB.ID)
	grant(member.ID, lead.ID)
	sendGrants := func(grants []Grant) []Grant {
		return slices.DeleteFunc(slices.Clone(grants), func(grant Grant) bool {
			return grant.Kind != KindActive || grant.Action != "send_msg" || grant.Target != targetKey
		})
	}

	leadGrants, err := s.PermsOf(ctx, lead.ID)
	leadGrants = sendGrants(leadGrants)
	if err != nil || len(leadGrants) != 2 {
		t.Fatalf("independent grant sources = %+v, %v", leadGrants, err)
	}
	if err := s.RevokePerm(ctx, managerA.ID, KindActive, lead.ID, "send_msg", targetKey); err != nil {
		t.Fatal(err)
	}
	leadGrants, err = s.PermsOf(ctx, lead.ID)
	leadGrants = sendGrants(leadGrants)
	if err != nil || len(leadGrants) != 1 || leadGrants[0].GrantedBy != managerB.ID {
		t.Fatalf("peer grant source should survive manager revoke: %+v, %v", leadGrants, err)
	}
	if err := s.RevokePerm(ctx, managerA.ID, KindActive, lead.ID, "send_msg", targetKey); !errors.Is(err, ErrForbidden) {
		t.Fatalf("peer-only grant revoke = %v, want ErrForbidden", err)
	}
	grant(lead.ID, managerA.ID)
	memberGrants, err := s.PermsOf(ctx, member.ID)
	memberGrants = sendGrants(memberGrants)
	if err != nil || len(memberGrants) != 1 {
		t.Fatalf("delegated member grant = %+v, %v", memberGrants, err)
	}
	if err := s.RevokePerm(ctx, member.ID, KindActive, managerA.ID, "send_msg", targetKey); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthorized revoke = %v, want ErrForbidden", err)
	}

	if err := s.RevokePerm(ctx, root.ID, KindActive, managerA.ID, "send_msg", targetKey); err != nil {
		t.Fatal(err)
	}
	leadGrants, err = s.PermsOf(ctx, lead.ID)
	leadGrants = sendGrants(leadGrants)
	if err != nil || len(leadGrants) != 1 || leadGrants[0].GrantedBy != managerB.ID {
		t.Fatalf("alternate authorization path not preserved: %+v, %v", leadGrants, err)
	}
	memberGrants, err = s.PermsOf(ctx, member.ID)
	memberGrants = sendGrants(memberGrants)
	if err != nil || len(memberGrants) != 1 {
		t.Fatalf("downstream grant should survive through alternate path: %+v, %v", memberGrants, err)
	}

	if err := s.RevokePerm(ctx, root.ID, KindActive, managerB.ID, "send_msg", targetKey); err != nil {
		t.Fatal(err)
	}
	leadGrants, err = s.PermsOf(ctx, lead.ID)
	leadGrants = sendGrants(leadGrants)
	if err != nil || len(leadGrants) != 0 {
		t.Fatalf("orphaned lead grant remained effective: %+v, %v", leadGrants, err)
	}
	memberGrants, err = s.PermsOf(ctx, member.ID)
	memberGrants = sendGrants(memberGrants)
	if err != nil || len(memberGrants) != 0 {
		t.Fatalf("orphaned downstream grant remained effective: %+v, %v", memberGrants, err)
	}
	var orphanEdges int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM permissions
		  WHERE kind = 'active' AND action = 'send_msg' AND target = $3 AND user_id IN ($1, $2)`,
		lead.ID, member.ID, targetKey).Scan(&orphanEdges); err != nil {
		t.Fatal(err)
	}
	if orphanEdges != 0 {
		t.Fatalf("revoked descendants must be pruned permanently, got %d rows", orphanEdges)
	}
}

func TestWorkerAdminDemotionPrunesRootedDelegations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	root := mkUser(t, s, "worker-admin-root", true)
	ordinary := mkUser(t, s, "worker-admin-ordinary", false)
	member := mkUser(t, s, "worker-admin-member", false)
	worker, _, err := s.CreateWorker(ctx, "delegating-worker", root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkerAdmin(ctx, ordinary.ID, worker.ID, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ordinary promotion = %v, want ErrForbidden", err)
	}
	if err := s.SetWorkerAdmin(ctx, root.ID, worker.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.GrantPerm(ctx, Grant{
		Kind: KindActive, UserID: member.ID, Action: "send_msg", Target: TargetAll, GrantedBy: worker.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if grants, err := s.PermsOf(ctx, member.ID); err != nil || len(grants) != 1 {
		t.Fatalf("worker-rooted permission = %+v, %v", grants, err)
	}
	if err := s.SetWorkerAdmin(ctx, root.ID, worker.ID, false); err != nil {
		t.Fatal(err)
	}
	if grants, err := s.PermsOf(ctx, member.ID); err != nil || len(grants) != 0 {
		t.Fatalf("demoted worker grant remained effective: %+v, %v", grants, err)
	}
	if err := s.SetWorkerAdmin(ctx, root.ID, worker.ID, true); err != nil {
		t.Fatal(err)
	}
	if grants, err := s.PermsOf(ctx, member.ID); err != nil || len(grants) != 0 {
		t.Fatalf("stale worker grant revived after promotion: %+v, %v", grants, err)
	}
	if err := s.GrantPerm(ctx, Grant{
		Kind: KindActive, UserID: member.ID, Action: "send_msg", Target: TargetAll, GrantedBy: worker.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeWorker(ctx, worker.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.UserByID(ctx, worker.ID)
	if err != nil || revoked.Status != UserDisabled || revoked.IsSuperadmin {
		t.Fatalf("revoked admin worker = %+v, %v", revoked, err)
	}
	if grants, err := s.PermsOf(ctx, member.ID); err != nil || len(grants) != 0 {
		t.Fatalf("revoked worker grant remained effective: %+v, %v", grants, err)
	}
}

func TestDisabledDelegationChainIsSuspendedNotPruned(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	root := mkUser(t, s, "suspension-root", true)
	manager := mkUser(t, s, "suspension-manager", false)
	member := mkUser(t, s, "suspension-member", false)
	target := mkUser(t, s, "suspension-target", false)
	scratch := mkUser(t, s, "suspension-scratch", false)
	targetKey := strconv.FormatInt(target.ID, 10)
	for _, grant := range []Grant{
		{Kind: KindActive, UserID: manager.ID, Action: "send_msg", Target: targetKey, GrantedBy: root.ID},
		{Kind: KindActive, UserID: manager.ID, Action: "manage_perm", Target: strconv.FormatInt(member.ID, 10), GrantedBy: root.ID},
		{Kind: KindActive, UserID: member.ID, Action: "send_msg", Target: targetKey, GrantedBy: manager.ID},
		{Kind: KindActive, UserID: scratch.ID, Action: "send_msg", Target: targetKey, GrantedBy: root.ID},
	} {
		if err := s.GrantPerm(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUserStatus(ctx, manager.ID, UserDisabled); err != nil {
		t.Fatal(err)
	}
	if grants, err := s.PermsOf(ctx, member.ID); err != nil || len(grants) != 0 {
		t.Fatalf("disabled issuer grant should be suspended: %+v, %v", grants, err)
	}
	if err := s.RevokePerm(ctx, root.ID, KindActive, scratch.ID, "send_msg", targetKey); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserStatus(ctx, manager.ID, UserActive); err != nil {
		t.Fatal(err)
	}
	if grants, err := s.PermsOf(ctx, member.ID); err != nil || len(grants) != 1 {
		t.Fatalf("reenabled issuer grant should recover: %+v, %v", grants, err)
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
	if err := s.MarkScheduleDelivered(ctx, sc.ID, due[0].OccurrenceGeneration, *due[0].DeliveryClaimedAt, time.Now().UTC(), &next, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧租约不得确认新认领, got %v", err)
	}
	if err := s.MarkScheduleDelivered(ctx, sc.ID, due2[0].OccurrenceGeneration, *due2[0].DeliveryClaimedAt, time.Now().UTC(), &next, false); err != nil {
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

	member := mkUser(t, s, "日程创建者", false)
	memberSchedule, err := s.CreateSchedule(ctx, &Schedule{
		UserID: member.ID, Kind: ScheduleOnce, Message: "成员提醒",
		FireAt: time.Now().UTC().Add(time.Hour), CreatedBy: member.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CancelScheduleVisible(ctx, memberSchedule.ID, boss.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("非所有者且未启用超管范围时不应取消: %v", err)
	}
	if err := s.CancelScheduleVisible(ctx, memberSchedule.ID, boss.ID, true); err != nil {
		t.Fatalf("超级管理员应能取消全局可见日程: %v", err)
	}
}

func TestReconcileScheduleTimezoneOnlyOnChange(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "老板", true)
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	oldFire := NextDailyFire(now, "18:00", "", shanghai)
	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: boss.ID, Kind: ScheduleDaily, Message: "日报", FireAt: oldFire,
		Target: ScheduleTargetSelf, Mode: ScheduleModeMessage, DailyAt: "18:00", CreatedBy: boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetKV(ctx, KVSchedulerTimezone, shanghai.String()); err != nil {
		t.Fatal(err)
	}

	changed, updated, err := s.ReconcileScheduleTimezone(ctx, now, tokyo)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || updated != 1 {
		t.Fatalf("timezone reconcile = changed %v, updated %d; want true, 1", changed, updated)
	}
	got, err := s.ScheduleByID(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := NextDailyFire(now, "18:00", "", tokyo)
	if !got.FireAt.Equal(want) {
		t.Fatalf("rebased fire_at = %s; want %s", got.FireAt, want)
	}

	changed, updated, err = s.ReconcileScheduleTimezone(ctx, now.Add(48*time.Hour), tokyo)
	if err != nil {
		t.Fatal(err)
	}
	if changed || updated != 0 {
		t.Fatalf("same timezone reconcile = changed %v, updated %d; want false, 0", changed, updated)
	}
	got, err = s.ScheduleByID(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.FireAt.Equal(want) {
		t.Fatalf("ordinary restart moved fire_at to %s; want unchanged %s", got.FireAt, want)
	}
}

func TestUpdateScheduleTimingVisibleKeepsStableFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "schedule-owner", false)
	other := mkUser(t, s, "schedule-other", false)
	sess, err := s.StartSession(ctx, owner.ID, "test:schedule-update", "eino")
	if err != nil {
		t.Fatal(err)
	}
	sourceMessageID, err := s.AppendMessage(ctx, sess.ID, "user", "创建日报日程")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: owner.ID, CreatedBy: owner.ID, Kind: ScheduleDaily,
		Message: "日报内容", Title: "日报",
		FireAt: time.Now().UTC().Add(time.Hour), DailyAt: "18:00",
		Target: ScheduleTargetSelf, Mode: ScheduleModeAI,
		SourceKind: ScheduleSourceTelegramGroupDigest, SourceKey: "group-1",
		SourceMessageID: &sourceMessageID,
	})
	if err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	if _, err := s.UpdateScheduleTimingVisible(ctx, sc.ID, other.ID, false, next, 0, "19:00", "1,2,3,4,5", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated user updated schedule: %v", err)
	}
	newSourceMessageID, err := s.AppendMessage(ctx, sess.ID, "user", "把日报改到十九点")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UpdateScheduleTimingVisible(ctx, sc.ID, owner.ID, false, next, 0, "19:00", "1,2,3,4,5", &newSourceMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sc.ID || got.UserID != sc.UserID || got.CreatedBy != sc.CreatedBy ||
		got.Target != sc.Target || got.Mode != sc.Mode || got.Message != sc.Message ||
		got.Title != sc.Title || got.SourceKind != sc.SourceKind || got.SourceKey != sc.SourceKey {
		t.Fatalf("stable schedule fields changed: before=%+v after=%+v", sc, got)
	}
	if !got.FireAt.Equal(next) || got.DailyAt != "19:00" || got.Weekdays != "1,2,3,4,5" ||
		got.SourceMessageID == nil || *got.SourceMessageID != newSourceMessageID {
		t.Fatalf("timing patch not applied: %+v", got)
	}
}

func TestQuarantineLegacyAutomationHistoryMigration(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "automation-history", true)
	sess, err := s.StartSession(ctx, u.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []struct{ role, content string }{
		{"user", "正常问题"},
		{"assistant", "正常回答"},
		{"user", "[系统定时触发·定制推送]内部指令"},
		{"assistant", "自动推送结果"},
		{"user", "后续问题"},
		{"assistant", "后续回答"},
	} {
		if _, err := s.AppendMessage(ctx, sess.ID, message.role, message.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateSessionSummary(ctx, sess.ID, "含有旧自动化内容", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionEngineRef(ctx, sess.ID, "stale-agent-trace"); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationsFS.ReadFile("migrations/0057_quarantine_automation_history.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}

	original, err := s.MessagesOf(ctx, sess.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 4 {
		t.Fatalf("interactive session should retain 4 human messages, got %+v", original)
	}
	for _, message := range original {
		if strings.HasPrefix(message.Content, "[系统") || message.Content == "自动推送结果" {
			t.Fatalf("automation content remained interactive: %+v", original)
		}
	}
	var moved int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_messages m JOIN chat_sessions cs ON cs.id=m.session_id
		  WHERE cs.channel=$1`, fmt.Sprintf("internal:legacy:automation:%d", sess.ID)).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("quarantined messages = %d, want 2", moved)
	}
	clean, err := s.SessionByID(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Summary != "" || clean.SummaryUpto != 0 || clean.EngineRef != "" {
		t.Fatalf("interactive session state was not reset: %+v", clean)
	}
}

func TestSchedulesVisibleOrdersRecentHistoryFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "schedule-history", true)

	older, err := s.CreateSchedule(ctx, &Schedule{
		UserID: u.ID, Kind: ScheduleOnce, Message: "older",
		FireAt: time.Now().UTC().Add(-2 * time.Hour), CreatedBy: u.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.CreateSchedule(ctx, &Schedule{
		UserID: u.ID, Kind: ScheduleOnce, Message: "newer",
		FireAt: time.Now().UTC().Add(-time.Hour), CreatedBy: u.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE schedules SET status='done', updated_at=now()-interval '2 hours' WHERE id=$1`, older.ID)
	mustExec(t, s, `UPDATE schedules SET status='done', updated_at=now()-interval '1 hour' WHERE id=$1`, newer.ID)

	items, err := s.SchedulesVisible(ctx, u.ID, true, ScheduleDone, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != newer.ID {
		t.Fatalf("recent history should be first: %+v", items)
	}
}

func TestScheduleOccurrenceFansOutPerRecipient(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "fanout-boss", true)
	a := mkUser(t, s, "fanout-a", false)
	b := mkUser(t, s, "fanout-b", false)
	c := mkUser(t, s, "fanout-c", false)
	fireAt := time.Now().UTC().Add(-time.Minute)
	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: boss.ID, CreatedBy: boss.ID, Kind: ScheduleOnce, FireAt: fireAt,
		Target: ScheduleTargetAll, Mode: ScheduleModeMessage, Message: "hello", Title: "broadcast",
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil || len(due) != 1 {
		t.Fatalf("DueSchedules = %+v, %v", due, err)
	}
	if err := s.FanOutScheduleOccurrence(ctx, due[0], []int64{a.ID, b.ID, c.ID, a.ID}, time.Now().UTC(), nil, true); err != nil {
		t.Fatal(err)
	}
	deliveries, err := s.DueScheduleDeliveries(ctx, time.Now().UTC())
	if err != nil || len(deliveries) != 3 {
		t.Fatalf("DueScheduleDeliveries = %+v, %v", deliveries, err)
	}
	if deliveries[0].ClaimedAt == nil || deliveries[1].ClaimedAt == nil || deliveries[2].ClaimedAt == nil {
		t.Fatal("recipient deliveries must carry independent claims")
	}
	if err := s.PrepareScheduleDeliveryResult(ctx, deliveries[1].ID, *deliveries[1].ClaimedAt, "durable generated message"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkScheduleDeliveryDelivered(ctx, deliveries[0].ID, *deliveries[0].ClaimedAt, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryScheduleDelivery(ctx, deliveries[1].ID, *deliveries[1].ClaimedAt, deliveries[1].Attempts, "temporary"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkScheduleDeliveryFailed(ctx, deliveries[2].ID, *deliveries[2].ClaimedAt, "recipient disabled"); err != nil {
		t.Fatal(err)
	}
	var delivered, pending, failed int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status='delivered'), count(*) FILTER (WHERE status='pending'),
		        count(*) FILTER (WHERE status='failed') FROM schedule_deliveries WHERE schedule_id=$1`, sc.ID).
		Scan(&delivered, &pending, &failed); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 || pending != 1 || failed != 1 {
		t.Fatalf("delivery states = delivered:%d pending:%d failed:%d", delivered, pending, failed)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE schedule_deliveries SET available_at=now() WHERE id=$1`, deliveries[1].ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := s.DueScheduleDeliveries(ctx, time.Now().UTC())
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != deliveries[1].ID {
		t.Fatalf("reclaimed delivery = %+v, %v", reclaimed, err)
	}
	if reclaimed[0].ResultText != "durable generated message" {
		t.Fatalf("schedule retry lost generated message: %+v", reclaimed[0])
	}
}

func TestScheduleOccurrenceIdentitySurvivesReturningToPriorFireTime(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "occurrence-owner", true)
	recipient := mkUser(t, s, "occurrence-recipient", false)
	firstFire := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: owner.ID, CreatedBy: owner.ID, Kind: ScheduleRepeat,
		FireAt: firstFire, IntervalS: 3600, Target: ScheduleTargetAll,
		Mode: ScheduleModeMessage, Message: "repeatable occurrence",
	})
	if err != nil {
		t.Fatal(err)
	}

	due, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil || len(due) != 1 {
		t.Fatalf("first DueSchedules = %+v, %v", due, err)
	}
	first := due[0]
	next := firstFire.Add(time.Hour)
	if err := s.FanOutScheduleOccurrence(ctx, first, []int64{recipient.ID}, time.Now().UTC(), &next, false); err != nil {
		t.Fatal(err)
	}
	afterFirst, err := s.ScheduleByID(ctx, sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFirst.OccurrenceGeneration <= first.OccurrenceGeneration {
		t.Fatalf("generation did not advance after fanout: first=%d next=%d", first.OccurrenceGeneration, afterFirst.OccurrenceGeneration)
	}

	away := firstFire.Add(2 * time.Hour)
	if _, err := s.UpdateScheduleTimingVisible(ctx, sc.ID, owner.ID, true, away, sc.IntervalS, "", "", nil); err != nil {
		t.Fatal(err)
	}
	returned, err := s.UpdateScheduleTimingVisible(ctx, sc.ID, owner.ID, true, firstFire, sc.IntervalS, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if returned.OccurrenceGeneration <= afterFirst.OccurrenceGeneration {
		t.Fatalf("generation did not advance across reschedule: prior=%d returned=%d", afterFirst.OccurrenceGeneration, returned.OccurrenceGeneration)
	}
	if err := s.FanOutScheduleOccurrence(ctx, first, []int64{recipient.ID}, time.Now().UTC(), &next, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale occurrence fanout = %v, want ErrNotFound", err)
	}

	due, err = s.DueSchedules(ctx, time.Now().UTC())
	if err != nil || len(due) != 1 || due[0].OccurrenceGeneration != returned.OccurrenceGeneration {
		t.Fatalf("returned DueSchedules = %+v, %v", due, err)
	}
	finalNext := firstFire.Add(3 * time.Hour)
	if err := s.FanOutScheduleOccurrence(ctx, due[0], []int64{recipient.ID}, time.Now().UTC(), &finalNext, false); err != nil {
		t.Fatal(err)
	}
	var deliveries, generations int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT occurrence_generation)
		   FROM schedule_deliveries
		  WHERE schedule_id=$1 AND user_id=$2 AND occurrence_at=$3`,
		sc.ID, recipient.ID, firstFire).Scan(&deliveries, &generations); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 || generations != 2 {
		t.Fatalf("same fire time should retain two occurrences: deliveries=%d generations=%d", deliveries, generations)
	}
}

func TestScheduleRecipientPreferencesControlIncomingBroadcasts(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "preference-boss", true)
	member := mkUser(t, s, "preference-member", false)
	other := mkUser(t, s, "preference-other", false)

	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: boss.ID, CreatedBy: boss.ID, Kind: ScheduleDaily,
		FireAt: time.Now().UTC().Add(-time.Minute), DailyAt: "09:30",
		Target: ScheduleTargetAll, Mode: ScheduleModeMessage, Message: "company notice", Title: "morning notice",
	})
	if err != nil {
		t.Fatal(err)
	}
	visible, err := s.SchedulesVisible(ctx, member.ID, false, ScheduleActive, 20)
	if err != nil || len(visible) != 1 || !visible[0].TargetsViewer || !visible[0].DeliveryEnabled {
		t.Fatalf("incoming broadcast visibility = %+v, %v", visible, err)
	}
	if err := s.SetScheduleDeliveryPreference(ctx, member.ID, SchedulePreferenceAll, false); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.ScheduleDeliveryAllowed(ctx, member.ID, sc.ID); err != nil || allowed {
		t.Fatalf("global opt-out allowed=%t err=%v", allowed, err)
	}
	if err := s.SetScheduleDeliveryPreference(ctx, member.ID, sc.ID, true); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.ScheduleDeliveryAllowed(ctx, member.ID, sc.ID); err != nil || !allowed {
		t.Fatalf("specific override allowed=%t err=%v", allowed, err)
	}

	due, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil || len(due) != 1 {
		t.Fatalf("DueSchedules = %+v, %v", due, err)
	}
	next := time.Now().UTC().Add(24 * time.Hour)
	if err := s.FanOutScheduleOccurrence(ctx, due[0], []int64{member.ID}, time.Now().UTC(), &next, false); err != nil {
		t.Fatal(err)
	}
	deliveries, err := s.DueScheduleDeliveries(ctx, time.Now().UTC())
	if err != nil || len(deliveries) != 1 || deliveries[0].ClaimedAt == nil {
		t.Fatalf("claimed delivery = %+v, %v", deliveries, err)
	}
	if err := s.SetScheduleDeliveryPreference(ctx, member.ID, sc.ID, false); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM schedule_deliveries WHERE id=$1`, deliveries[0].ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "suppressed" {
		t.Fatalf("queued delivery status = %q", status)
	}

	direct, err := s.CreateSchedule(ctx, &Schedule{
		UserID: other.ID, CreatedBy: boss.ID, Kind: ScheduleDaily,
		FireAt: time.Now().UTC().Add(time.Hour), DailyAt: "10:00",
		Target: strconv.FormatInt(other.ID, 10), Mode: ScheduleModeMessage, Message: "private",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetScheduleDeliveryPreference(ctx, member.ID, direct.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated schedule preference = %v", err)
	}

	historical, err := s.CreateSchedule(ctx, &Schedule{
		UserID: boss.ID, CreatedBy: boss.ID, Kind: ScheduleOnce,
		FireAt: time.Now().UTC().Add(-time.Minute), Target: ScheduleTargetAll,
		Mode: ScheduleModeMessage, Message: "old broadcast",
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err = s.DueSchedules(ctx, time.Now().UTC())
	if err != nil || len(due) != 1 || due[0].ID != historical.ID {
		t.Fatalf("historical due schedule = %+v, %v", due, err)
	}
	if err := s.MarkScheduleDelivered(ctx, historical.ID, due[0].OccurrenceGeneration, *due[0].DeliveryClaimedAt, time.Now().UTC(), nil, true); err != nil {
		t.Fatal(err)
	}
	history, err := s.SchedulesVisible(ctx, member.ID, false, "all", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range history {
		if item.ID == historical.ID {
			t.Fatalf("recipient without a delivery inherited historical broadcast: %+v", item)
		}
	}
}

func TestMandatoryScheduleOverridesRecipientPreferences(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "mandatory-owner", true)
	recipient := mkUser(t, s, "mandatory-recipient", false)

	if err := s.SetScheduleDeliveryPreference(ctx, recipient.ID, SchedulePreferenceAll, false); err != nil {
		t.Fatal(err)
	}
	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: recipient.ID, CreatedBy: owner.ID, Kind: ScheduleOnce,
		FireAt: time.Now().UTC().Add(-time.Minute), Target: strconv.FormatInt(recipient.ID, 10),
		Mode: ScheduleModeMessage, Message: "required notice", Title: "required notice",
		RecipientPolicy: ScheduleRecipientMandatory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.ScheduleDeliveryAllowed(ctx, recipient.ID, sc.ID); err != nil || !allowed {
		t.Fatalf("mandatory delivery allowed=%t err=%v", allowed, err)
	}
	if err := s.SetScheduleDeliveryPreference(ctx, recipient.ID, sc.ID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("mandatory schedule opt-out = %v, want ErrConflict", err)
	}
	visible, err := s.SchedulesVisible(ctx, recipient.ID, false, ScheduleActive, 20)
	if err != nil || len(visible) != 1 || visible[0].RecipientPolicy != ScheduleRecipientMandatory || !visible[0].DeliveryEnabled {
		t.Fatalf("mandatory schedule visibility = %+v, %v", visible, err)
	}
	if err := s.CancelScheduleVisible(ctx, sc.ID, recipient.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recipient cancelled mandatory schedule: %v", err)
	}

	due, err := s.DueSchedules(ctx, time.Now().UTC())
	if err != nil || len(due) != 1 || due[0].ID != sc.ID {
		t.Fatalf("mandatory due schedule = %+v, %v", due, err)
	}
	if err := s.FanOutScheduleOccurrence(ctx, due[0], []int64{recipient.ID}, time.Now().UTC(), nil, true); err != nil {
		t.Fatal(err)
	}
	deliveries, err := s.DueScheduleDeliveries(ctx, time.Now().UTC())
	if err != nil || len(deliveries) != 1 || deliveries[0].ClaimedAt == nil {
		t.Fatalf("mandatory delivery claim = %+v, %v", deliveries, err)
	}
	if err := s.SetScheduleDeliveryPreference(ctx, recipient.ID, SchedulePreferenceAll, false); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM schedule_deliveries WHERE id=$1`, deliveries[0].ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "processing" {
		t.Fatalf("global opt-out suppressed mandatory delivery: %q", status)
	}
}

func TestNotificationDeliveryBoundaryIsConcurrentAndImmutable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	recipient := mkUser(t, s, "notification-recipient", false)

	const workers = 16
	var wg sync.WaitGroup
	created := make(chan bool, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			delivery, won, err := s.BeginNotificationDelivery(ctx, "event:42", recipient.ID, "sha256:alpha")
			if err == nil && (delivery == nil || delivery.Key != "event:42") {
				err = fmt.Errorf("unexpected delivery: %+v", delivery)
			}
			created <- won
			errs <- err
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	winners := 0
	for won := range created {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("delivery boundary winners=%d, want 1", winners)
	}

	if err := s.MarkNotificationDeliveryDelivered(ctx, "event:42", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	delivery, won, err := s.BeginNotificationDelivery(ctx, "event:42", recipient.ID, "sha256:alpha")
	if err != nil || won || delivery == nil || delivery.Status != NotificationDeliveryDelivered {
		t.Fatalf("settled replay = %+v created=%v err=%v", delivery, won, err)
	}
	if delivery, won, err = s.BeginNotificationDelivery(ctx, "event:42", recipient.ID, "sha256:changed"); !errors.Is(err, ErrConflict) || won || delivery == nil {
		t.Fatalf("content mutation = %+v created=%v err=%v", delivery, won, err)
	}
	other := mkUser(t, s, "notification-other", false)
	if delivery, won, err = s.BeginNotificationDelivery(ctx, "event:42", other.ID, "sha256:alpha"); !errors.Is(err, ErrConflict) || won || delivery == nil {
		t.Fatalf("recipient mutation = %+v created=%v err=%v", delivery, won, err)
	}
}

func TestTelegramInboundUpdateInboxIsDurableAndOwnerFenced(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	payload := json.RawMessage(`{"update_id":9001,"message":{"message_id":1}}`)
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])

	const writers = 12
	var wg sync.WaitGroup
	created := make(chan bool, writers)
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wasCreated, err := s.EnqueueTelegramInboundUpdate(ctx, 9001, payload, hash)
			created <- wasCreated
			errs <- err
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	createdCount := 0
	for wasCreated := range created {
		if wasCreated {
			createdCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d, want 1", createdCount)
	}
	if _, err := s.EnqueueTelegramInboundUpdate(ctx, 9001, json.RawMessage(`{"update_id":9001}`), "different"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mutated update identity = %v, want ErrConflict", err)
	}

	claimed, err := s.ClaimTelegramInboundUpdates(ctx, "instance-a", 10)
	if err != nil || len(claimed) != 1 || claimed[0].ClaimOwner != "instance-a" {
		t.Fatalf("first claim = %+v, %v", claimed, err)
	}
	if err := s.HeartbeatTelegramInboundClaims(ctx, "instance-a", []int64{9001}); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimTelegramInboundUpdates(ctx, "instance-b", 10)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("live owner was stolen: %+v, %v", claimed, err)
	}
	mustExec(t, s, `UPDATE telegram_inbound_updates SET claimed_at=clock_timestamp() - $2::interval WHERE update_id=$1`,
		9001, telegramInboundLease+time.Second)
	claimed, err = s.ClaimTelegramInboundUpdates(ctx, "instance-b", 10)
	if err != nil || len(claimed) != 1 || claimed[0].ClaimOwner != "instance-b" {
		t.Fatalf("stale claim was not recovered: %+v, %v", claimed, err)
	}
	if err := s.CompleteTelegramInboundUpdates(ctx, "instance-a", []int64{9001}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var status, owner string
	if err := s.pool.QueryRow(ctx, `SELECT status, claim_owner FROM telegram_inbound_updates WHERE update_id=9001`).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != TelegramInboundProcessing || owner != "instance-b" {
		t.Fatalf("stale owner settled new claim: status=%q owner=%q", status, owner)
	}
	if err := s.CompleteTelegramInboundUpdates(ctx, "instance-b", []int64{9001}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT status, claim_owner FROM telegram_inbound_updates WHERE update_id=9001`).Scan(&status, &owner); err != nil {
		t.Fatal(err)
	}
	if status != TelegramInboundDone || owner != "" {
		t.Fatalf("completed inbound update = status %q owner %q", status, owner)
	}
}

func TestTelegramInboundHeartbeatOnlyRenewsActiveClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, updateID := range []int64{9011, 9012} {
		payload := json.RawMessage(fmt.Sprintf(`{"update_id":%d}`, updateID))
		sum := sha256.Sum256(payload)
		if _, err := s.EnqueueTelegramInboundUpdate(ctx, updateID, payload, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := s.ClaimTelegramInboundUpdates(ctx, "instance-a", 10)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("initial claims = %+v, %v", claimed, err)
	}
	mustExec(t, s,
		`UPDATE telegram_inbound_updates SET claimed_at=clock_timestamp() - $2::interval WHERE update_id=ANY($1)`,
		[]int64{9011, 9012}, telegramInboundLease+time.Second)
	if err := s.HeartbeatTelegramInboundClaims(ctx, "instance-a", []int64{9011}); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimTelegramInboundUpdates(ctx, "instance-b", 10)
	if err != nil || len(claimed) != 1 || claimed[0].UpdateID != 9012 {
		t.Fatalf("inactive claim was not selectively released: %+v, %v", claimed, err)
	}
}

func TestTelegramDeliveryPartsAreIndependentAndImmutable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	part, created, err := s.BeginTelegramDeliveryPart(ctx, "reply:42", 0, 2, -1001, "hash-a")
	if err != nil || !created || part.Status != TelegramDeliveryStarted {
		t.Fatalf("first part = %+v created=%t err=%v", part, created, err)
	}
	part, created, err = s.BeginTelegramDeliveryPart(ctx, "reply:42", 0, 2, -1001, "hash-a")
	if err != nil || created || part.Status != TelegramDeliveryStarted {
		t.Fatalf("replayed part = %+v created=%t err=%v", part, created, err)
	}
	if _, _, err := s.BeginTelegramDeliveryPart(ctx, "reply:42", 0, 2, -1001, "hash-b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed part payload = %v, want ErrConflict", err)
	}
	if err := s.MarkTelegramDeliveryPartDelivered(ctx, "reply:42", 0, 77, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	part, created, err = s.BeginTelegramDeliveryPart(ctx, "reply:42", 0, 2, -1001, "hash-a")
	if err != nil || created || part.Status != TelegramDeliveryDelivered || part.TelegramMessageID == nil || *part.TelegramMessageID != 77 {
		t.Fatalf("delivered replay = %+v created=%t err=%v", part, created, err)
	}
	second, created, err := s.BeginTelegramDeliveryPart(ctx, "reply:42", 1, 2, -1001, "hash-c")
	if err != nil || !created || second.PartIndex != 1 {
		t.Fatalf("independent second part = %+v created=%t err=%v", second, created, err)
	}
}

func TestExternalActionReceiptConcurrentBoundaryAndImmutableIdentity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const workers = 16
	var wg sync.WaitGroup
	created := make(chan bool, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, won, err := s.BeginExternalAction(ctx, "telegram:message:-7:42:direct:group-command:/listen", "group-command:/listen", "sha256:alpha")
			if err == nil && (receipt == nil || receipt.Key == "") {
				err = fmt.Errorf("unexpected receipt: %+v", receipt)
			}
			created <- won
			errs <- err
		}()
	}
	wg.Wait()
	close(created)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	winners := 0
	for won := range created {
		if won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("external action winners=%d, want 1", winners)
	}

	const key = "telegram:message:-7:42:direct:group-command:/listen"
	if err := s.CompleteExternalAction(ctx, key); err != nil {
		t.Fatal(err)
	}
	receipt, won, err := s.BeginExternalAction(ctx, key, "group-command:/listen", "sha256:alpha")
	if err != nil || won || receipt == nil || receipt.Status != ExternalActionCompleted {
		t.Fatalf("completed replay = %+v created=%v err=%v", receipt, won, err)
	}
	if receipt, won, err = s.BeginExternalAction(ctx, key, "group-command:/listen", "sha256:changed"); !errors.Is(err, ErrConflict) || won || receipt == nil {
		t.Fatalf("payload mutation = %+v created=%v err=%v", receipt, won, err)
	}
	if receipt, won, err = s.BeginExternalAction(ctx, key, "group-command:/new", "sha256:alpha"); !errors.Is(err, ErrConflict) || won || receipt == nil {
		t.Fatalf("kind mutation = %+v created=%v err=%v", receipt, won, err)
	}

	recoverableKey := "http:file-upload:recoverable"
	if _, won, err = s.BeginExternalAction(ctx, recoverableKey, "file-upload", "sha256:file"); err != nil || !won {
		t.Fatalf("begin recoverable action: won=%v err=%v", won, err)
	}
	if err := s.FailExternalAction(ctx, recoverableKey, "temporary database failure"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteRecoverableExternalAction(ctx, recoverableKey); err != nil {
		t.Fatal(err)
	}
	receipt, won, err = s.BeginExternalAction(ctx, recoverableKey, "file-upload", "sha256:file")
	if err != nil || won || receipt == nil || receipt.Status != ExternalActionCompleted || receipt.LastError != "" {
		t.Fatalf("recovered action = %+v created=%v err=%v", receipt, won, err)
	}
}

func TestDeliveryReceiptCleanupKeepsUncertainBoundaries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	recipient := mkUser(t, s, "receipt-cleanup-recipient", false)

	if _, created, err := s.BeginNotificationDelivery(ctx, "delivery:old-terminal", recipient.ID, "sha256:terminal"); err != nil || !created {
		t.Fatalf("begin terminal notification created=%v err=%v", created, err)
	}
	if err := s.MarkNotificationDeliveryDelivered(ctx, "delivery:old-terminal", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.BeginNotificationDelivery(ctx, "delivery:old-uncertain", recipient.ID, "sha256:uncertain"); err != nil || !created {
		t.Fatalf("begin uncertain notification created=%v err=%v", created, err)
	}
	if _, created, err := s.BeginExternalAction(ctx, "action:old-terminal", "test", "sha256:terminal"); err != nil || !created {
		t.Fatalf("begin terminal action created=%v err=%v", created, err)
	}
	if err := s.CompleteExternalAction(ctx, "action:old-terminal"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.BeginExternalAction(ctx, "action:old-uncertain", "test", "sha256:uncertain"); err != nil || !created {
		t.Fatalf("begin uncertain action created=%v err=%v", created, err)
	}
	if _, created, err := s.BeginExternalAction(ctx, "action:recent-expired-result", "test", "sha256:result"); err != nil || !created {
		t.Fatalf("begin result action created=%v err=%v", created, err)
	}
	if err := s.CompleteExternalActionWithResult(ctx, "action:recent-expired-result", "one-time-secret", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.BeginWorkerLLMCall(ctx, recipient.ID, "llm-old-terminal", "sha256:terminal"); err != nil || !created {
		t.Fatalf("begin terminal worker llm call created=%v err=%v", created, err)
	}
	if err := s.CompleteWorkerLLMCall(ctx, recipient.ID, "llm-old-terminal", 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.BeginWorkerLLMCall(ctx, recipient.ID, "llm-old-uncertain", "sha256:uncertain"); err != nil || !created {
		t.Fatalf("begin uncertain worker llm call created=%v err=%v", created, err)
	}

	old := time.Now().UTC().AddDate(0, 0, -100)
	mustExec(t, s, `UPDATE notification_deliveries SET updated_at=$1 WHERE delivery_key LIKE 'delivery:old-%'`, old)
	mustExec(t, s, `UPDATE external_action_receipts SET updated_at=$1 WHERE action_key LIKE 'action:old-%'`, old)
	mustExec(t, s, `UPDATE worker_llm_calls SET updated_at=$1 WHERE request_id LIKE 'llm-old-%'`, old)
	if cleared, err := s.ClearExpiredExternalActionResults(ctx, time.Now().UTC()); err != nil || cleared != 1 {
		t.Fatalf("expired action results cleared=%d err=%v", cleared, err)
	}
	notifications, actions, workerLLMCalls, err := s.DeleteExpiredDeliveryReceipts(ctx, time.Now().UTC().AddDate(0, 0, -90))
	if err != nil || notifications != 1 || actions != 1 || workerLLMCalls != 1 {
		t.Fatalf("cleanup notifications=%d actions=%d worker_llm_calls=%d err=%v", notifications, actions, workerLLMCalls, err)
	}
	for table, keys := range map[string][3]string{
		"notification_deliveries":  {"delivery_key", "delivery:old-terminal", "delivery:old-uncertain"},
		"external_action_receipts": {"action_key", "action:old-terminal", "action:old-uncertain"},
		"worker_llm_calls":         {"request_id", "llm-old-terminal", "llm-old-uncertain"},
	} {
		keyColumn, terminal, uncertain := keys[0], keys[1], keys[2]
		var terminalCount, uncertainCount int
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s=$1`, table, keyColumn), terminal).Scan(&terminalCount); err != nil {
			t.Fatal(err)
		}
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s=$1`, table, keyColumn), uncertain).Scan(&uncertainCount); err != nil {
			t.Fatal(err)
		}
		if terminalCount != 0 || uncertainCount != 1 {
			t.Fatalf("%s terminal=%d uncertain=%d, want 0/1", table, terminalCount, uncertainCount)
		}
	}
	receipt, created, err := s.BeginExternalAction(ctx, "action:recent-expired-result", "test", "sha256:result")
	if err != nil || created || receipt == nil || receipt.Status != ExternalActionCompleted || receipt.ResultText != "" || receipt.ResultUntil != nil {
		t.Fatalf("expired result cleanup = %+v created=%v err=%v", receipt, created, err)
	}
}

func TestWorkerLLMCallReceiptReplaysExactResponse(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	worker := mkUser(t, s, "llm-receipt-worker", false)

	call, created, err := s.BeginWorkerLLMCall(ctx, worker.ID, "request-1", "sha256:alpha")
	if err != nil || !created || call == nil || call.Status != WorkerLLMCallStarted {
		t.Fatalf("begin worker llm call = %+v created=%v err=%v", call, created, err)
	}
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	if err := s.CompleteWorkerLLMCall(ctx, worker.ID, "request-1", 200, body); err != nil {
		t.Fatal(err)
	}
	call, created, err = s.BeginWorkerLLMCall(ctx, worker.ID, "request-1", "sha256:alpha")
	if err != nil || created || call == nil || call.Status != WorkerLLMCallCompleted ||
		call.HTTPStatus == nil || *call.HTTPStatus != 200 || !bytes.Equal(call.Response, body) {
		t.Fatalf("replayed worker llm call = %+v created=%v err=%v", call, created, err)
	}
	if call, created, err = s.BeginWorkerLLMCall(ctx, worker.ID, "request-1", "sha256:changed"); !errors.Is(err, ErrConflict) || created || call == nil {
		t.Fatalf("mutated worker llm replay = %+v created=%v err=%v", call, created, err)
	}
}

func TestStableRequiredEventDoesNotReopenAfterTerminalFailure(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "stable-event-owner", true)
	id, wake, err := s.EnqueueEventOnceWithPolicy(ctx, "task:7:ready", "task_ready", u.ID, "task 7", true)
	if err != nil || !wake {
		t.Fatalf("enqueue stable event = %d wake=%v err=%v", id, wake, err)
	}
	events, err := s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(events) != 1 || events[0].SourceKey != "task:7:ready" {
		t.Fatalf("claim stable event = %+v err=%v", events, err)
	}
	if err := s.RetryEvent(ctx, id, *events[0].ClaimedAt, eventMaxAttempts, "ambiguous transport failure"); err != nil {
		t.Fatal(err)
	}

	duplicateID, wake, err := s.EnqueueEventOnceWithPolicy(ctx, "task:7:ready", "renamed_kind", u.ID, "changed generated detail", true)
	if !errors.Is(err, ErrConflict) || wake || duplicateID != id {
		t.Fatalf("terminal stable replay = %d wake=%v err=%v", duplicateID, wake, err)
	}
	if due, err := s.DueEvents(ctx, time.Now().UTC(), 4); err != nil || len(due) != 0 {
		t.Fatalf("terminal stable event reopened = %+v err=%v", due, err)
	}
}

func TestDurableEventAndAutomationRunClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "event-owner", true)
	id, created, err := s.EnqueueEvent(ctx, "task_ready", u.ID, "task 7", 5*time.Minute)
	if err != nil || !created {
		t.Fatalf("EnqueueEvent = %d %v %v", id, created, err)
	}
	id2, created, err := s.EnqueueEvent(ctx, "task_ready", u.ID, "task 7", 5*time.Minute)
	if err != nil || created || id2 != id {
		t.Fatalf("dedupe event = %d %v %v", id2, created, err)
	}
	upgradedID, wake, err := s.EnqueueEventWithPolicy(ctx, "task_ready", u.ID, "task 7", true, 5*time.Minute)
	if err != nil || !wake || upgradedID != id {
		t.Fatalf("required event upgrade = %d %v %v", upgradedID, wake, err)
	}
	events, err := s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(events) != 1 || events[0].ClaimedAt == nil || !events[0].NotificationRequired {
		t.Fatalf("DueEvents = %+v %v", events, err)
	}
	event := events[0]
	if err := s.BeginEventDecision(ctx, event.ID, *event.ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareEventDelivery(ctx, event.ID, *event.ClaimedAt, EventOutcomeHandled, "prepared"); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryEvent(ctx, event.ID, *event.ClaimedAt, event.Attempts, "temporary"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE events SET available_at=now() WHERE id=$1`, event.ID); err != nil {
		t.Fatal(err)
	}
	events, err = s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(events) != 1 || events[0].Reply != "prepared" {
		t.Fatalf("retried event = %+v %v", events, err)
	}
	if err := s.CompleteEvent(ctx, events[0].ID, *events[0].ClaimedAt, EventOutcomeHandled); err != nil {
		t.Fatal(err)
	}
	if duplicateID, wake, err := s.EnqueueEventWithPolicy(ctx, "task_ready", u.ID, "task 7", true, 5*time.Minute); err != nil || wake || duplicateID != id {
		t.Fatalf("delivered required event should dedupe without reopening: id=%d wake=%v err=%v", duplicateID, wake, err)
	}

	failedID, _, err := s.EnqueueEventWithPolicy(ctx, "critical", u.ID, "retry me", true, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	failedEvents, err := s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(failedEvents) != 1 || failedEvents[0].ID != failedID {
		t.Fatalf("claim critical event = %+v %v", failedEvents, err)
	}
	if err := s.RetryEvent(ctx, failedID, *failedEvents[0].ClaimedAt, eventMaxAttempts, "delivery exhausted"); err != nil {
		t.Fatal(err)
	}
	if duplicateID, wake, err := s.EnqueueEventWithPolicy(ctx, "critical", u.ID, "retry me", true, 5*time.Minute); err != nil || !wake || duplicateID != failedID {
		t.Fatalf("failed required event should reopen: id=%d wake=%v err=%v", duplicateID, wake, err)
	}
	reopened, err := s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(reopened) != 1 || reopened[0].ID != failedID || reopened[0].Attempts != 1 {
		t.Fatalf("reopened required event = %+v %v", reopened, err)
	}
	if err := s.CompleteEvent(ctx, reopened[0].ID, *reopened[0].ClaimedAt, EventOutcomeFallback); err != nil {
		t.Fatal(err)
	}

	optionalID, _, err := s.EnqueueEvent(ctx, "optional", u.ID, "may skip", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	optionalEvents, err := s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(optionalEvents) != 1 || optionalEvents[0].ID != optionalID {
		t.Fatalf("claim optional event = %+v %v", optionalEvents, err)
	}
	if _, _, err := s.EnqueueEventWithPolicy(ctx, "optional", u.ID, "may skip", true, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if skipped, err := s.FinalizeEventSkip(ctx, optionalID, *optionalEvents[0].ClaimedAt); err != nil || skipped {
		t.Fatalf("required upgrade must prevent stale skip: skipped=%v err=%v", skipped, err)
	}
	if err := s.PrepareEventDelivery(ctx, optionalID, *optionalEvents[0].ClaimedAt, EventOutcomeFallback, "required fallback"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteEvent(ctx, optionalID, *optionalEvents[0].ClaimedAt, EventOutcomeFallback); err != nil {
		t.Fatal(err)
	}

	interruptedID, _, err := s.EnqueueEvent(ctx, "worker_online", u.ID, "worker 9", 0)
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(interrupted) != 1 || interrupted[0].ID != interruptedID {
		t.Fatalf("interrupted claim = %+v %v", interrupted, err)
	}
	if err := s.BeginEventDecision(ctx, interruptedID, *interrupted[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryEvent(ctx, interruptedID, *interrupted[0].ClaimedAt, interrupted[0].Attempts, "process interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE events SET available_at=now() WHERE id=$1`, interruptedID); err != nil {
		t.Fatal(err)
	}
	interrupted, err = s.DueEvents(ctx, time.Now().UTC(), 4)
	if err != nil || len(interrupted) != 1 || interrupted[0].DeliveryMode != EventDeliveryModeGenerating || interrupted[0].Reply != "" {
		t.Fatalf("reclaimed event must preserve decision boundary = %+v %v", interrupted, err)
	}

	now := time.Now().UTC()
	run, err := s.ClaimAutomationRun(ctx, "daily", "2026-07-10", u.ID, now)
	if err != nil || run.ClaimedAt == nil {
		t.Fatalf("ClaimAutomationRun = %+v %v", run, err)
	}
	if _, err := s.ClaimAutomationRun(ctx, "daily", "2026-07-10", u.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active automation claim must be exclusive: %v", err)
	}
	if err := s.BeginAutomationAction(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareAutomationResult(ctx, run, "durable report", AutomationOutcomeSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryAutomationRun(ctx, run, "temporary"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE automation_runs SET available_at=now() WHERE automation_key='daily' AND occurrence_key='2026-07-10' AND subject_id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	run, err = s.ClaimAutomationRun(ctx, "daily", "2026-07-10", u.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !run.ActionStarted || run.ResultText != "durable report" || run.Outcome != AutomationOutcomeSucceeded {
		t.Fatalf("automation retry lost no-replay boundary or report: %+v", run)
	}
	if err := s.CompleteAutomationRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAutomationRun(ctx, "daily", "2026-07-10", u.ID, time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("completed automation must remain deduplicated: %v", err)
	}
}

func TestExhaustedInterruptedClaimsReachTerminalState(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "lease-owner", true)

	eventID, _, err := s.EnqueueEvent(ctx, "interrupted", u.ID, "event", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE events SET status='processing', attempts=$2, claimed_at=now()-interval '1 hour' WHERE id=$1`, eventID, eventMaxAttempts)
	if events, err := s.DueEvents(ctx, time.Now().UTC(), 4); err != nil || len(events) != 0 {
		t.Fatalf("exhausted event was reclaimed: %+v %v", events, err)
	}
	var eventStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM events WHERE id=$1`, eventID).Scan(&eventStatus); err != nil || eventStatus != "failed" {
		t.Fatalf("event status=%q err=%v", eventStatus, err)
	}

	sc, err := s.CreateSchedule(ctx, &Schedule{
		UserID: u.ID, CreatedBy: u.ID, Kind: ScheduleOnce, FireAt: time.Now().UTC(),
		Target: ScheduleTargetSelf, Mode: ScheduleModeMessage, Message: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	var deliveryID int64
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO schedule_deliveries (schedule_id, occurrence_at, occurrence_generation, user_id, mode, message, status, attempts, claimed_at)
		 VALUES ($1,now(),$2,$3,'message','x','processing',$4,now()-interval '1 hour') RETURNING id`,
		sc.ID, sc.OccurrenceGeneration, u.ID, scheduleRecipientMaxAttempts).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	if deliveries, err := s.DueScheduleDeliveries(ctx, time.Now().UTC()); err != nil || len(deliveries) != 0 {
		t.Fatalf("exhausted delivery was reclaimed: %+v %v", deliveries, err)
	}
	var deliveryStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM schedule_deliveries WHERE id=$1`, deliveryID).Scan(&deliveryStatus); err != nil || deliveryStatus != "failed" {
		t.Fatalf("delivery status=%q err=%v", deliveryStatus, err)
	}

	sess, err := s.StartSession(ctx, u.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	userMessageID, _ := s.AppendMessage(ctx, sess.ID, "user", "stable fact")
	assistantMessageID, _ := s.AppendMessage(ctx, sess.ID, "assistant", "ok")
	if err := s.EnqueueMemoryMiningJob(ctx, u.ID, "telegram", sess.ID, userMessageID, assistantMessageID, "", nil, false); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE memory_mining_jobs SET status='processing', attempts=$2, claimed_at=now()-interval '1 hour' WHERE session_id=$1`, sess.ID, memoryMiningMaxAttempts)
	if jobs, err := s.DueMemoryMiningJobs(ctx, 4); err != nil || len(jobs) != 0 {
		t.Fatalf("exhausted memory job was reclaimed: %+v %v", jobs, err)
	}
	var memoryStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM memory_mining_jobs WHERE session_id=$1`, sess.ID).Scan(&memoryStatus); err != nil || memoryStatus != "failed" {
		t.Fatalf("memory status=%q err=%v", memoryStatus, err)
	}

	if _, err := s.ClaimAutomationRun(ctx, "exhausted", "once", u.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE automation_runs SET status='processing', attempts=$4, claimed_at=now()-interval '1 hour'
		WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3`, "exhausted", "once", u.ID, automationRunMaxAttempts)
	if _, err := s.ClaimAutomationRun(ctx, "exhausted", "once", u.ID, time.Now().UTC()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exhausted automation claim = %v", err)
	}
	var automationStatus string
	if err := s.pool.QueryRow(ctx,
		`SELECT status FROM automation_runs WHERE automation_key='exhausted' AND occurrence_key='once' AND subject_id=$1`, u.ID).
		Scan(&automationStatus); err != nil || automationStatus != "failed" {
		t.Fatalf("automation status=%q err=%v", automationStatus, err)
	}
}

func TestAutomationOutcomeExpiryAndReplaySafety(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "automation-owner", true)
	now := time.Now().UTC()

	uncertain, err := s.ClaimAutomationRunUntil(ctx, "maintenance", "uncertain", u.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareAutomationResult(ctx, uncertain, "需要人工核对", AutomationOutcomeUncertain, "partial write"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAutomationRun(ctx, uncertain); err != nil {
		t.Fatal(err)
	}
	var status, outcome string
	if err := s.pool.QueryRow(ctx,
		`SELECT status, outcome FROM automation_runs WHERE automation_key='maintenance' AND occurrence_key='uncertain' AND subject_id=$1`, u.ID).
		Scan(&status, &outcome); err != nil || status != "failed" || outcome != AutomationOutcomeUncertain {
		t.Fatalf("uncertain automation status=%q outcome=%q err=%v", status, outcome, err)
	}
	if _, err := s.ClaimAutomationRunUntil(ctx, "maintenance", "uncertain", u.ID, now, now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal uncertain run was reclaimed: %v", err)
	}

	replaySafe, err := s.ClaimAutomationRunUntil(ctx, "maintenance", "safe-retry", u.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BeginAutomationAction(ctx, replaySafe); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryAutomationRunReplaySafe(ctx, replaySafe, "completed without effects"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE automation_runs SET available_at=now() WHERE automation_key='maintenance' AND occurrence_key='safe-retry' AND subject_id=$1`, u.ID)
	replaySafe, err = s.ClaimAutomationRunUntil(ctx, "maintenance", "safe-retry", u.ID, time.Now().UTC(), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if replaySafe.ActionStarted || replaySafe.ResultText != "" || replaySafe.Outcome != "" {
		t.Fatalf("safe retry retained action boundary: %+v", replaySafe)
	}

	delivered, err := s.ClaimAutomationRunUntil(ctx, "maintenance", "delivery-failed", u.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareAutomationResult(ctx, delivered, "业务已完成", AutomationOutcomeSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE automation_runs SET attempts=$4 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3`,
		"maintenance", "delivery-failed", u.ID, automationRunMaxAttempts)
	delivered.Attempts = automationRunMaxAttempts
	if err := s.RetryAutomationRun(ctx, delivered, "notification unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT status, outcome FROM automation_runs WHERE automation_key='maintenance' AND occurrence_key='delivery-failed' AND subject_id=$1`, u.ID).
		Scan(&status, &outcome); err != nil || status != "failed" || outcome != AutomationOutcomeSucceeded {
		t.Fatalf("delivery failure overwrote business outcome: status=%q outcome=%q err=%v", status, outcome, err)
	}

	ambiguous, err := s.ClaimAutomationRunUntil(ctx, "maintenance", "delivery-ambiguous", u.ID, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareAutomationResult(ctx, ambiguous, "业务结果已持久化", AutomationOutcomeSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FailAutomationRunDelivery(ctx, ambiguous, "response lost after send"); err != nil {
		t.Fatal(err)
	}
	var resultText, lastError string
	if err := s.pool.QueryRow(ctx,
		`SELECT status, outcome, result_text, last_error FROM automation_runs
		 WHERE automation_key='maintenance' AND occurrence_key='delivery-ambiguous' AND subject_id=$1`, u.ID).
		Scan(&status, &outcome, &resultText, &lastError); err != nil || status != "failed" ||
		outcome != AutomationOutcomeSucceeded || resultText != "业务结果已持久化" || !strings.Contains(lastError, "response lost") {
		t.Fatalf("ambiguous delivery status=%q outcome=%q result=%q error=%q err=%v", status, outcome, resultText, lastError, err)
	}

	expiringDelivery, err := s.ClaimAutomationRunUntil(ctx, "maintenance", "delivery-expired", u.ID, now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareAutomationResult(ctx, expiringDelivery, "没有需要修改的对象", AutomationOutcomeNoChange, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryAutomationRun(ctx, expiringDelivery, "notification unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpireAutomationRuns(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT status, outcome FROM automation_runs WHERE automation_key='maintenance' AND occurrence_key='delivery-expired' AND subject_id=$1`, u.ID).
		Scan(&status, &outcome); err != nil || status != "failed" || outcome != AutomationOutcomeNoChange {
		t.Fatalf("expiry overwrote business outcome: status=%q outcome=%q err=%v", status, outcome, err)
	}

	if _, err := s.ClaimAutomationRunUntil(ctx, "maintenance", "expired", u.ID, now, now.Add(-time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired occurrence was claimed: %v", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT status, outcome FROM automation_runs WHERE automation_key='maintenance' AND occurrence_key='expired' AND subject_id=$1`, u.ID).
		Scan(&status, &outcome); err != nil || status != "failed" || outcome != AutomationOutcomeFailed {
		t.Fatalf("expired automation status=%q outcome=%q err=%v", status, outcome, err)
	}
}

func TestHasActiveAutomationRunIgnoresExpiredLease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "automation-active", true)
	now := time.Now().UTC()
	run, err := s.ClaimAutomationRun(ctx, "serialized-maintenance", "first", u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.HasActiveAutomationRun(ctx, run.AutomationKey, now)
	if err != nil || !active {
		t.Fatalf("fresh lease active=%v err=%v", active, err)
	}
	mustExec(t, s, `UPDATE automation_runs SET claimed_at=$4 WHERE automation_key=$1 AND occurrence_key=$2 AND subject_id=$3`,
		run.AutomationKey, run.OccurrenceKey, run.SubjectID, now.Add(-automationRunLease-time.Second))
	active, err = s.HasActiveAutomationRun(ctx, run.AutomationKey, now)
	if err != nil || active {
		t.Fatalf("expired lease active=%v err=%v", active, err)
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

// CompleteWorkerRun 只在执行仍持有有效租约且业务任务未被修订时提交，
// 覆盖「分配者同时改需求把任务重置为 pending」的竞态。
func TestCompleteWorkerRunAtomic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	tk := mkTask(t, s, pj.ID, boss.ID, worker.ID, "写脚本", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimID == "" {
		t.Fatal("认领应返回 worker claim id")
	}
	// 分配者此刻把任务重置为 pending（模拟改需求）。
	if _, err := s.UpdateTaskStatus(ctx, tk.ID, TaskPending); err != nil {
		t.Fatal(err)
	}
	// worker 的提交应落空（任务已非 in_progress），不把旧交付当完成。
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "旧结果", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "old")); !errors.Is(err, ErrNotFound) {
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
	reclaimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.ClaimID == "" || reclaimed.ClaimID == claimed.ClaimID {
		t.Fatalf("重新认领应换 claim id: old=%q new=%q", claimed.ClaimID, reclaimed.ClaimID)
	}
	if _, err := s.AddWorkerRunProgress(ctx, claimed.ID, worker.ID, claimed.ClaimID, "stale-progress", "旧进度"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 claim 进度应被拒: %v", err)
	}
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "旧结果", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "old")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("旧 claim 提交应被拒: %v", err)
	}
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, reclaimed.ID, worker.ID, reclaimed.ClaimID, "新结果", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(reclaimed.ClaimID, "new")); err != nil {
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

func TestCompleteWorkerRunFinalizationIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	pj := mkProject(t, s, boss.ID)
	task := mkTask(t, s, pj.ID, boss.ID, worker.ID, "幂等提交", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalization := testWorkerFinalization(claimed.ClaimID, "same-payload")
	completedRun, completedTask, _, replayed, err := s.CompleteWorkerRun(ctx,
		claimed.ID, worker.ID, claimed.ClaimID, "唯一结果", "", workerproto.OutcomeSucceeded, nil, finalization)
	if err != nil || replayed || completedRun.Status != WorkerRunCompleted || completedTask == nil || completedTask.ID != task.ID {
		t.Fatalf("first finalization: run=%+v task=%+v replayed=%v err=%v", completedRun, completedTask, replayed, err)
	}
	if completedRun.TaskRevision == nil || *completedRun.TaskRevision != 1 || completedTask.Revision != 2 {
		t.Fatalf("task revision boundary lost: run=%+v task=%+v", completedRun, completedTask)
	}
	_, replayedTask, _, replayed, err := s.CompleteWorkerRun(ctx,
		claimed.ID, worker.ID, claimed.ClaimID, "唯一结果", "", workerproto.OutcomeSucceeded, nil, finalization)
	if err != nil || !replayed || replayedTask == nil || replayedTask.ID != task.ID {
		t.Fatalf("duplicate finalization: task=%+v replayed=%v err=%v", replayedTask, replayed, err)
	}
	progress, err := s.ProgressOf(ctx, task.ID)
	if err != nil || len(progress) != 1 || !strings.Contains(progress[0].Content, "唯一结果") {
		t.Fatalf("duplicate finalization wrote duplicate effects: progress=%+v err=%v", progress, err)
	}
	conflict := WorkerRunFinalization{ID: finalization.ID, Hash: "different-payload"}
	if _, _, _, _, err := s.CompleteWorkerRun(ctx,
		claimed.ID, worker.ID, claimed.ClaimID, "篡改结果", "", workerproto.OutcomeSucceeded, nil, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("same finalization id with another payload must conflict: %v", err)
	}
}

func TestConcurrentWorkerFinalizationCommitsOnce(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "boss", true)
	worker, _, err := s.CreateWorker(ctx, "worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, worker.ID, "并发最终化", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalization := testWorkerFinalization(claimed.ClaimID, "concurrent")
	start := make(chan struct{})
	type result struct {
		replayed bool
		err      error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, _, replayed, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID,
				"并发结果", "", workerproto.OutcomeSucceeded, nil, finalization)
			results <- result{replayed: replayed, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	fresh, replay := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent finalization failed: %v", result.err)
		}
		if result.replayed {
			replay++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replay != 1 {
		t.Fatalf("finalization results fresh=%d replay=%d", fresh, replay)
	}
	progress, err := s.ProgressOf(ctx, task.ID)
	if err != nil || len(progress) != 1 {
		t.Fatalf("concurrent finalization effects=%+v err=%v", progress, err)
	}
}

func TestWorkerProgressRequestIsIdempotentAndImmutable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "progress-boss", true)
	worker, _, err := s.CreateWorker(ctx, "progress-worker", boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := mkTask(t, s, mkProject(t, s, boss.ID).ID, boss.ID, worker.ID, "进度幂等", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := s.AddWorkerRunProgress(ctx, claimed.ID, worker.ID, claimed.ClaimID, "progress-1", "完成 50%")
	if err != nil || !inserted {
		t.Fatalf("first progress inserted=%v err=%v", inserted, err)
	}
	inserted, err = s.AddWorkerRunProgress(ctx, claimed.ID, worker.ID, claimed.ClaimID, "progress-1", "完成 50%")
	if err != nil || inserted {
		t.Fatalf("progress replay inserted=%v err=%v", inserted, err)
	}
	if _, err := s.AddWorkerRunProgress(ctx, claimed.ID, worker.ID, claimed.ClaimID, "progress-1", "篡改后的进度"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same request with another payload must conflict: %v", err)
	}
	var runCount, taskCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM worker_run_progress WHERE run_id=$1`, claimed.ID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM task_progress WHERE task_id=$1`, task.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 1 || taskCount != 1 {
		t.Fatalf("progress rows run=%d task=%d, want 1/1", runCount, taskCount)
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
	task := mkTask(t, s, pj.ID, boss.ID, worker.ID, "产物任务", nil)
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := claimed.ClaimID
	if claim == "" {
		t.Fatal("认领应生成 claim_id")
	}

	// 预校验：正确 claim 通过；空/错 claim、非 in_progress 拒绝。
	if ok, _ := s.WorkerCanSubmitArtifact(ctx, claimed.ID, worker.ID, claim); !ok {
		t.Fatal("有效 claim 应通过预校验")
	}
	if ok, _ := s.WorkerCanSubmitArtifact(ctx, claimed.ID, worker.ID, "wrong"); ok {
		t.Fatal("错 claim 不应通过")
	}
	if ok, _ := s.WorkerCanSubmitArtifact(ctx, claimed.ID, worker.ID, ""); ok {
		t.Fatal("空 claim 不应通过")
	}

	// 建一个文件、挂成产物：有引用时 DeleteOrphanFileRow 不动它。
	f, err := s.CreateFile(ctx, &File{Source: "worker", OriginalName: "a.txt", SizeBytes: 3, SHA256: "abc", StoragePath: "ab/abc", CreatedBy: &worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	canonicalID, inserted, err := s.AddWorkerArtifact(ctx, claimed.ID, worker.ID, claim, f.ID, "artifact-1", "report")
	if err != nil || !inserted || canonicalID != f.ID {
		t.Fatalf("first artifact canonical=%d inserted=%v err=%v", canonicalID, inserted, err)
	}
	replayFile, err := s.CreateFile(ctx, &File{Source: "worker", OriginalName: "replayed.txt", SizeBytes: 3, SHA256: "abc", StoragePath: "ab/abc", CreatedBy: &worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	canonicalID, inserted, err = s.AddWorkerArtifact(ctx, claimed.ID, worker.ID, claim, replayFile.ID, "artifact-1", " report ")
	if err != nil || inserted || canonicalID != f.ID {
		t.Fatalf("artifact replay canonical=%d inserted=%v err=%v", canonicalID, inserted, err)
	}
	if _, _, err := s.AddWorkerArtifact(ctx, claimed.ID, worker.ID, claim, replayFile.ID, "artifact-1", "changed caption"); !errors.Is(err, ErrConflict) {
		t.Fatalf("same artifact request with another payload must conflict: %v", err)
	}
	var runFiles, taskArtifacts int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM worker_run_files WHERE run_id=$1 AND role='artifact'`, claimed.ID).Scan(&runFiles); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM task_artifacts WHERE task_id=$1`, task.ID).Scan(&taskArtifacts); err != nil {
		t.Fatal(err)
	}
	if runFiles != 1 || taskArtifacts != 1 {
		t.Fatalf("artifact rows run=%d task=%d, want 1/1", runFiles, taskArtifacts)
	}

	concurrentFiles := make([]*File, 2)
	for i := range concurrentFiles {
		concurrentFiles[i], err = s.CreateFile(ctx, &File{
			Source: "worker", OriginalName: fmt.Sprintf("concurrent-%d.txt", i), SizeBytes: 4,
			SHA256: "same", StoragePath: fmt.Sprintf("sa/same-%d", i), CreatedBy: &worker.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	type artifactResult struct {
		canonicalID int64
		inserted    bool
		err         error
	}
	start := make(chan struct{})
	results := make(chan artifactResult, len(concurrentFiles))
	for _, candidate := range concurrentFiles {
		candidate := candidate
		go func() {
			<-start
			canonicalID, inserted, err := s.AddWorkerArtifact(ctx, claimed.ID, worker.ID, claim, candidate.ID, "artifact-concurrent", "same report")
			results <- artifactResult{canonicalID: canonicalID, inserted: inserted, err: err}
		}()
	}
	close(start)
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil || first.canonicalID == 0 || first.canonicalID != second.canonicalID {
		t.Fatalf("concurrent artifact results first=%+v second=%+v", first, second)
	}
	if first.inserted == second.inserted {
		t.Fatalf("concurrent artifact insert flags first=%v second=%v, want exactly one", first.inserted, second.inserted)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM worker_run_files WHERE run_id=$1 AND request_id='artifact-concurrent'`, claimed.ID).Scan(&runFiles); err != nil {
		t.Fatal(err)
	}
	if runFiles != 1 {
		t.Fatalf("concurrent artifact rows=%d, want 1", runFiles)
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
	// 外部索引只保留模型标签、不保留 PostgreSQL 向量；切回本地索引时
	// embedding IS NULL 必须让同模型记录重新进入回填队列。
	if err := s.MarkKnowledgeVectorIndexed(ctx, k1.ID, "test-model"); err != nil {
		t.Fatal(err)
	}
	need, err = s.KnowledgeNeedingEmbedding(ctx, "test-model", 10)
	if err != nil {
		t.Fatal(err)
	}
	hasK1 = false
	for _, k := range need {
		hasK1 = hasK1 || k.ID == k1.ID
	}
	if !hasK1 {
		t.Fatal("同模型但 PostgreSQL 向量为空的知识应重新回填")
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
	if err := s.ClearLegacyKnowledgeEmbeddings(ctx); err != nil {
		t.Fatal(err)
	}
	if cands, err := s.EmbeddedKnowledge(ctx, "test-model"); err != nil || len(cands) != 0 {
		t.Fatalf("旧知识向量应已清理: %+v err=%v", cands, err)
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
	// 客户端预持久化的候选 token 原样激活；响应丢失后客户端可直接用它认证恢复。
	codeCandidate, err := s.NewWorkerBindCode(ctx, worker.ID, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.Repeat("ab", 24)
	if _, got, err := s.RedeemWorkerBindCodeWithToken(ctx, codeCandidate, candidate); err != nil || got != candidate {
		t.Fatalf("候选 token 兑换 got=%q err=%v", got, err)
	}
	if authed, err := s.UserByAPIToken(ctx, candidate); err != nil || authed.ID != worker.ID {
		t.Fatalf("候选 token 未激活: user=%+v err=%v", authed, err)
	}
	if replayed, got, err := s.RedeemWorkerBindCodeWithToken(ctx, codeCandidate, candidate); err != nil ||
		replayed.ID != worker.ID || got != candidate {
		t.Fatalf("兑换响应丢失后的服务端恢复 user=%+v token=%q err=%v", replayed, got, err)
	}
	codeInvalid, err := s.NewWorkerBindCode(ctx, worker.ID, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RedeemWorkerBindCodeWithToken(ctx, codeInvalid, "weak"); !errors.Is(err, ErrConflict) {
		t.Fatalf("弱候选 token 应拒绝: %v", err)
	}
	if _, _, err := s.RedeemWorkerBindCode(ctx, codeInvalid); err != nil {
		t.Fatalf("格式错误不应消费绑定码: %v", err)
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

func TestWorkerBindCodeResultsRecoverByInvocation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "worker-recovery-boss", true)

	worker, code, err := s.CreateWorkerForRequest(ctx, "recoverable-worker", boss.ID, "create-request-1")
	if err != nil {
		t.Fatal(err)
	}
	replayedWorker, replayedCode, err := s.CreateWorkerForRequest(ctx, "recoverable-worker", boss.ID, "create-request-1")
	if err != nil || replayedWorker.ID != worker.ID || replayedCode != code {
		t.Fatalf("create recovery worker=%+v code_equal=%v err=%v", replayedWorker, replayedCode == code, err)
	}
	byRequest, recoveredCode, err := s.WorkerBindCodeByRequest(ctx, boss.ID, "create-request-1")
	if err != nil || byRequest.ID != worker.ID || recoveredCode != code {
		t.Fatalf("lookup recovery worker=%+v code_equal=%v err=%v", byRequest, recoveredCode == code, err)
	}

	issued, err := s.NewWorkerBindCodeForRequest(ctx, worker.ID, boss.ID, "issue-request-1")
	if err != nil {
		t.Fatal(err)
	}
	issuedAgain, err := s.NewWorkerBindCodeForRequest(ctx, worker.ID, boss.ID, "issue-request-1")
	if err != nil || issuedAgain != issued {
		t.Fatalf("issue recovery code_equal=%v err=%v", issuedAgain == issued, err)
	}
	if _, got, err := s.WorkerBindCodeByRequest(ctx, boss.ID, "issue-request-1"); err != nil || got != issued {
		t.Fatalf("issue lookup code_equal=%v err=%v", got == issued, err)
	}
	if err := s.ClearWorkerBindCodeRecovery(ctx, boss.ID, "issue-request-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.WorkerBindCodeByRequest(ctx, boss.ID, "issue-request-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("持久调用结果后应清理恢复明文: %v", err)
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
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}

	ws, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "repo:nbco", "NBCO", claimed.ID, &task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID == 0 || ws.ScopeKey != "repo:nbco" || ws.UseCount != 1 {
		t.Fatalf("unexpected session: %+v", ws)
	}
	ws2, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "repo:nbco", "NBCO", claimed.ID, &task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ws2.ID != ws.ID || ws2.UseCount != 2 {
		t.Fatalf("session should be reused and counted: first=%+v second=%+v", ws, ws2)
	}
	fingerprint := strings.Repeat("a", 64)
	if err := s.UpdateWorkerSessionForClaim(ctx, ws.ID, worker.ID, claimed.ID, claimed.ClaimID,
		"执行中", "early-native-ref", fingerprint, "/root/src/nbco"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkerSessionForClaim(ctx, ws.ID, worker.ID, claimed.ID, "stale-claim",
		"stale", "stale-ref", strings.Repeat("b", 64), "/tmp/stale"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale claim must not overwrite worker session: %v", err)
	}
	if err := s.UpdateWorkerSession(ctx, ws.ID, worker.ID, claimed.ID, &task.ID, "完成了路由", "native-ref", fingerprint, "/root/src/nbco"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "repo:nbco", "NBCO", claimed.ID, &task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "完成了路由" || got.EngineSessionRef != "native-ref" || got.EngineRuntimeFingerprint != fingerprint || got.Workdir != "/root/src/nbco" {
		t.Fatalf("session update not persisted: %+v", got)
	}
	newerTask := mkTask(t, s, p.ID, boss.ID, worker.ID, "nbco 后续开发", nil)
	newerRun, err := s.LatestWorkerRunForTask(ctx, newerTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimWorkerSession(ctx, worker.ID, "codex", "repo", "repo:nbco", "NBCO", newerRun.ID, &newerTask.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWorkerSessionForClaim(ctx, ws.ID, worker.ID, claimed.ID, claimed.ClaimID,
		"旧执行迟到", "old-ref", strings.Repeat("c", 64), "/tmp/old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("older run must not overwrite a session already handed to a newer run: %v", err)
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
	skill, err := s.CreateSkill(ctx, "群入职处理", NewSkillContent(
		"群里有人要求加入", "先判断真人还是 worker", "先查群身份再邀请", "",
	), []string{"scope:telegram"}, boss.ID)
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

func TestRuleUpsertKeepsOneActiveLogicalRule(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "rule-upsert-owner", true)

	created, status, err := s.UpsertRule(ctx, "日本红日子", "节假日不上班", []string{"scope:global"}, boss.ID, false)
	if err != nil || status != RuleCreated {
		t.Fatalf("first upsert = %+v status=%q err=%v", created, status, err)
	}
	unchanged, status, err := s.UpsertRule(ctx, "日本红日子", "节假日不上班", []string{"scope:global"}, boss.ID, false)
	if err != nil || status != RuleUnchanged || unchanged.ID != created.ID {
		t.Fatalf("same upsert = %+v status=%q err=%v", unchanged, status, err)
	}
	updated, status, err := s.UpsertRule(ctx, " 日本红日子 ", "法定节假日不上班", []string{"scope:global"}, boss.ID, true)
	if err != nil || status != RuleUpdated || updated.ID != created.ID || !updated.Pinned || updated.Content != "法定节假日不上班" {
		t.Fatalf("updated upsert = %+v status=%q err=%v", updated, status, err)
	}
	versions, err := s.KnowledgeVersions(ctx, created.ID, 10)
	if err != nil || len(versions) != 1 || versions[0].Content != "节假日不上班" {
		t.Fatalf("upsert must snapshot changed rule: %+v err=%v", versions, err)
	}
	otherScope, status, err := s.UpsertRule(ctx, "日本红日子", "仅 Telegram 提醒", []string{"scope:telegram"}, boss.ID, false)
	if err != nil || status != RuleCreated || otherScope.ID == created.ID {
		t.Fatalf("different scope should be independent: %+v status=%q err=%v", otherScope, status, err)
	}
	all, err := s.ListRules(ctx, 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("active rules = %+v err=%v", all, err)
	}
}

func TestRuleUpsertSerializesConcurrentIdenticalCreates(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "rule-concurrent-owner", true)

	const workers = 8
	type result struct {
		id     int64
		status RuleWriteStatus
		err    error
	}
	results := make(chan result, workers)
	for range workers {
		go func() {
			k, status, err := s.UpsertRule(ctx, "并发规则", "只保留一条", []string{"scope:global"}, boss.ID, false)
			var id int64
			if k != nil {
				id = k.ID
			}
			results <- result{id: id, status: status, err: err}
		}()
	}
	var canonical int64
	created := 0
	for range workers {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if canonical == 0 {
			canonical = result.id
		}
		if result.id != canonical {
			t.Fatalf("concurrent upsert returned different ids: %d and %d", canonical, result.id)
		}
		if result.status == RuleCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created count = %d, want 1", created)
	}
	all, err := s.ListRules(ctx, 10)
	if err != nil || len(all) != 1 || all[0].ID != canonical {
		t.Fatalf("active rules = %+v err=%v", all, err)
	}
}

func TestMigration0065ArchivesExistingDuplicateRules(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	schema := fmt.Sprintf("migration_0065_%d", time.Now().UnixNano())
	if _, err := tx.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE users (id BIGINT PRIMARY KEY);
		INSERT INTO users VALUES (1);
		CREATE TABLE knowledge (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			title TEXT NOT NULL, content TEXT NOT NULL, tags TEXT[] NOT NULL DEFAULT '{}',
			author_id BIGINT NOT NULL REFERENCES users(id), kind TEXT NOT NULL DEFAULT 'fact',
			pinned BOOLEAN NOT NULL DEFAULT FALSE, active BOOLEAN NOT NULL DEFAULT TRUE,
			embedding REAL[], embed_model TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE knowledge_versions (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			knowledge_id BIGINT NOT NULL REFERENCES knowledge(id) ON DELETE CASCADE,
			version INT NOT NULL, title TEXT NOT NULL, content TEXT NOT NULL,
			tags TEXT[] NOT NULL DEFAULT '{}', kind TEXT NOT NULL DEFAULT 'fact',
			pinned BOOLEAN NOT NULL DEFAULT FALSE, active BOOLEAN NOT NULL DEFAULT TRUE,
			changed_by BIGINT REFERENCES users(id), change_note TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE (knowledge_id, version)
		);
		INSERT INTO knowledge (title, content, tags, author_id, kind) VALUES
			('日本红日子', 'v1', ARRAY['scope:global'], 1, 'policy'),
			(' 日本红日子 ', 'v2', ARRAY['scope:global'], 1, 'policy'),
			('日本红日子', 'v3', ARRAY['scope:global'], 1, 'policy'),
			('日本红日子', 'telegram', ARRAY['scope:telegram'], 1, 'policy');
	`); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationsFS.ReadFile("migrations/0065_rule_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	var activeGlobal, archived, snapshots int
	var activeID int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*), max(id) FROM knowledge
		 WHERE kind = 'policy' AND active AND 'scope:global' = ANY(tags)
	`).Scan(&activeGlobal, &activeID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM knowledge WHERE kind = 'policy' AND NOT active`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM knowledge_versions WHERE change_note = 'archived duplicate by migration 0065'`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if activeGlobal != 1 || activeID != 3 || archived != 2 || snapshots != 2 {
		t.Fatalf("migration result active=%d id=%d archived=%d snapshots=%d", activeGlobal, activeID, archived, snapshots)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO knowledge (title, content, tags, author_id, kind)
		VALUES ('日本红日子', 'duplicate', ARRAY['scope:global'], 1, 'policy')
	`); !errors.Is(wrapErr(err), ErrConflict) {
		t.Fatalf("post-migration duplicate insert err = %v", err)
	}
}

func TestMigration0097ConvertsAndConstrainsSkillContent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	schema := fmt.Sprintf("migration_0097_%d", time.Now().UnixNano())
	if _, err := tx.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE knowledge (
			id BIGSERIAL PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL,
			kind TEXT NOT NULL, embedding REAL[], embed_model TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE knowledge_versions (
			id BIGSERIAL PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL, kind TEXT NOT NULL
		);
		CREATE TABLE learning_candidates (
			id BIGSERIAL PRIMARY KEY, title TEXT NOT NULL, content TEXT NOT NULL,
			kind TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		INSERT INTO knowledge (title, content, kind) VALUES
			('群邀请流程', E'触发条件：群里有人申请加入\n摘要：核实身份后邀请\n执行方法：\n1. 查询身份\n2. 创建邀请\n限制与禁忌：\n不得公开凭据', 'skill');
		INSERT INTO knowledge_versions (title, content, kind) VALUES
			('群邀请流程', E'触发条件：群里有人申请加入\n摘要：旧版本\n执行方法：旧步骤', 'skill');
		INSERT INTO learning_candidates (title, content, kind) VALUES
			('简短流程', E'触发条件：需要处理\n摘要：简短流程\n执行方法：同一行步骤', 'skill');
	`); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationsFS.ReadFile("migrations/0097_structured_skill_content.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{"knowledge", "knowledge_versions", "learning_candidates"} {
		var raw string
		if err := tx.QueryRow(ctx, `SELECT content FROM `+table+` LIMIT 1`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		content, err := DecodeSkillContent(raw)
		if err != nil {
			t.Fatalf("%s content was not migrated: %q: %v", table, raw, err)
		}
		if content.Trigger == "" || content.Summary == "" || content.Procedure == "" {
			t.Fatalf("%s migrated incomplete content: %#v", table, content)
		}
	}
	var candidateRaw string
	if err := tx.QueryRow(ctx, `SELECT content FROM learning_candidates LIMIT 1`).Scan(&candidateRaw); err != nil {
		t.Fatal(err)
	}
	candidate, err := DecodeSkillContent(candidateRaw)
	if err != nil || candidate.Procedure != "同一行步骤" {
		t.Fatalf("single-line procedure migration = %#v err=%v", candidate, err)
	}

	if _, err := tx.Exec(ctx, `SAVEPOINT invalid_skill`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO knowledge (title, content, kind) VALUES ('bad', 'not-json', 'skill')`); err == nil {
		t.Fatal("skill constraint accepted unstructured content")
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT invalid_skill`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO knowledge (title, content, kind) VALUES ('fact', 'plain text', 'fact')`); err != nil {
		t.Fatalf("skill constraint must not affect other knowledge kinds: %v", err)
	}
}

func TestAutomationSnapshotIsImmutable(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// PostgreSQL timestamptz stores microseconds; align the expected value with
	// the database precision so this immutability assertion is deterministic.
	expires := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
	first, err := s.GetOrCreateAutomationSnapshot(ctx, "monthly-test", "2026-08", 7, "candidate", []int64{3, 2, 2, -1}, expires)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetOrCreateAutomationSnapshot(ctx, "monthly-test", "2026-08", 7, "candidate", []int64{99}, expires.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.ItemIDs, []int64{2, 3}) || !slices.Equal(second.ItemIDs, first.ItemIDs) {
		t.Fatalf("snapshot changed first=%v second=%v", first.ItemIDs, second.ItemIDs)
	}
	if second.ItemKind != "candidate" || second.ExpiresAt == nil || !second.ExpiresAt.Equal(expires) {
		t.Fatalf("snapshot metadata changed: %+v", second)
	}
	empty, err := s.GetOrCreateAutomationSnapshot(ctx, "monthly-test", "2026-08", 8, "candidate", nil, expires)
	if err != nil {
		t.Fatal(err)
	}
	if empty.ItemIDs == nil || len(empty.ItemIDs) != 0 {
		t.Fatalf("empty snapshot must persist an empty PostgreSQL array: %+v", empty.ItemIDs)
	}
}

func TestLearningCandidateMemoryAuthority(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "memory-authority-boss", true)
	knowledge, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindKnowledge, Title: "待分类事实", Content: "可能属于主数据", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if knowledge.MemoryClass != LearningMemoryUnclassified {
		t.Fatalf("knowledge class=%q", knowledge.MemoryClass)
	}
	if err := s.SetLearningCandidateMemoryClass(ctx, knowledge.ID, boss.ID, LearningMemoryCanonical); err != nil {
		t.Fatal(err)
	}
	classified, err := s.LearningCandidateByID(ctx, knowledge.ID)
	if err != nil {
		t.Fatal(err)
	}
	if classified.MemoryClass != LearningMemoryCanonical || classified.Status != LearningStatusRejected {
		t.Fatalf("classified candidate=%+v", classified)
	}
	candidateToClassify, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindKnowledge, Title: "重复分类边界", Content: "相同内容", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindKnowledge, Title: "重复分类边界", Content: "相同内容",
		MemoryClass: LearningMemoryDurable, CreatedBy: &boss.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLearningCandidateMemoryClass(ctx, candidateToClassify.ID, boss.ID, LearningMemoryDurable); !errors.Is(err, ErrConflict) {
		t.Fatalf("classification identity conflict = %v", err)
	}
	unchanged, err := s.LearningCandidateByID(ctx, candidateToClassify.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.MemoryClass != LearningMemoryUnclassified || unchanged.Status != LearningStatusPending {
		t.Fatalf("failed classification must roll back: %+v", unchanged)
	}
	rule, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindRule, Title: "长期规则", Content: "持续生效", CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.MemoryClass != LearningMemoryDurable {
		t.Fatalf("rule class=%q", rule.MemoryClass)
	}
	publishedKnowledge, err := s.CreateKnowledge(ctx, "误入知识库的主数据", "员工当前手机号", nil, boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	publishedCandidate, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindKnowledge, Title: publishedKnowledge.Title, Content: publishedKnowledge.Content,
		MemoryClass: LearningMemoryDurable, CreatedBy: &boss.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLearningCandidatePublished(ctx, publishedCandidate.ID, boss.ID, &publishedKnowledge.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLearningCandidateMemoryClass(ctx, publishedCandidate.ID, boss.ID, LearningMemoryCanonical); err != nil {
		t.Fatal(err)
	}
	archivedCandidate, err := s.LearningCandidateByID(ctx, publishedCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	archivedKnowledge, err := s.KnowledgeByID(ctx, publishedKnowledge.ID)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := s.KnowledgeVersions(ctx, publishedKnowledge.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if archivedCandidate.Status != LearningStatusRejected || archivedCandidate.MemoryClass != LearningMemoryCanonical ||
		archivedKnowledge.Active || len(versions) != 1 || !versions[0].Active {
		t.Fatalf("published reclassification candidate=%+v knowledge=%+v versions=%+v", archivedCandidate, archivedKnowledge, versions)
	}
}

func TestLearningGovernanceBoundaryIsCompleteAndOldestFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	boss := mkUser(t, s, "governance-boundary-boss", true)

	want := make([]int64, 0, 105)
	for i := range 105 {
		content, err := EncodeSkillContent(NewSkillContent(
			fmt.Sprintf("治理场景 %03d", i), fmt.Sprintf("可复用流程 %03d", i), "执行并验证结果", "",
		))
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
			Kind: LearningKindSkill, Title: fmt.Sprintf("治理候选 %03d", i),
			Content: content, CreatedBy: &boss.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, candidate.ID)
	}
	if _, err := s.CreateLearningCandidate(ctx, LearningCandidateInput{
		Kind: LearningKindProfile, Title: "权威主数据", Content: "不进入治理快照", CreatedBy: &boss.ID,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.LearningCandidateIDsForGovernance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("governance IDs were truncated or reordered: got=%v want=%v", got, want)
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

func TestTelegramBotMembershipUpdatesAndOutboxAreAtomicAndOrdered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: -2000, Type: "supergroup", Status: "member",
	}, true); err == nil {
		t.Fatal("membership writer accepted a non-authoritative observation without update_id")
	}

	// Upgrade compatibility: an existing active state predates the transition
	// marker and must not be announced as a new join.
	if err := s.SaveTelegramGroupState(ctx, TelegramGroupState{
		ChatID: -2001, Title: "旧群", Type: "supergroup", Status: "member", Listen: true,
	}); err != nil {
		t.Fatal(err)
	}
	joined, err := s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: -2001, Title: "旧群", Type: "supergroup", Status: "administrator", LastMembershipUpdateID: 100,
	}, true)
	if err != nil || joined {
		t.Fatalf("existing active group joined=%v err=%v", joined, err)
	}

	const chatID int64 = -2002
	const callers = 16
	var wg sync.WaitGroup
	joinedResults := make(chan bool, callers)
	errResults := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			joined, err := s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
				ChatID: chatID, Title: "并发群", Type: "supergroup", Status: "member", LastMembershipUpdateID: 200,
			}, true)
			joinedResults <- joined
			errResults <- err
		}()
	}
	wg.Wait()
	close(joinedResults)
	close(errResults)
	for err := range errResults {
		if err != nil {
			t.Fatal(err)
		}
	}
	joinCount := 0
	for joined := range joinedResults {
		if joined {
			joinCount++
		}
	}
	if joinCount != 1 {
		t.Fatalf("concurrent inactive-to-active outbox events = %d, want 1", joinCount)
	}
	var outboxCount int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM domain_outbox_events
		  WHERE topic=$1 AND payload->>'chat_id'=$2`,
		DomainTopicTelegramBotMembershipActivated, strconv.FormatInt(chatID, 10)).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("concurrent transition persisted %d outbox events, want 1", outboxCount)
	}
	if listen, err := s.GetKV(ctx, TelegramGroupListenKey(chatID)); err != nil || listen != "1" {
		t.Fatalf("join should enable listening: value=%q err=%v", listen, err)
	}

	// Active role changes preserve an explicit listener choice and do not
	// announce again.
	if err := s.SetTelegramGroupListen(ctx, chatID, false); err != nil {
		t.Fatal(err)
	}
	joined, err = s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: chatID, Title: "并发群", Type: "supergroup", Status: "administrator", LastMembershipUpdateID: 201,
	}, true)
	if err != nil || joined {
		t.Fatalf("active role change joined=%v err=%v", joined, err)
	}
	state, err := s.TelegramGroupState(ctx, chatID)
	if err != nil || state.Listen {
		t.Fatalf("role change should preserve disabled listening: state=%+v err=%v", state, err)
	}
	joined, err = s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: chatID, Title: "并发群", Type: "supergroup", Status: "left", LastMembershipUpdateID: 202,
	}, false)
	if err != nil || joined {
		t.Fatalf("leave joined=%v err=%v", joined, err)
	}
	joined, err = s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: chatID, Title: "并发群", Type: "supergroup", Status: "member", LastMembershipUpdateID: 203,
	}, true)
	if err != nil || !joined {
		t.Fatalf("rejoin joined=%v err=%v", joined, err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM domain_outbox_events
		  WHERE topic=$1 AND payload->>'chat_id'=$2`,
		DomainTopicTelegramBotMembershipActivated, strconv.FormatInt(chatID, 10)).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 2 {
		t.Fatalf("leave/rejoin persisted %d outbox events, want 2", outboxCount)
	}

	// Reordered webhook retries cannot roll membership backward. Replaying the
	// same authoritative update is idempotent.
	const orderedChatID int64 = -2003
	joined, err = s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: orderedChatID, Title: "乱序群", Type: "supergroup", Status: "member", LastMembershipUpdateID: 300,
	}, true)
	if err != nil || !joined {
		t.Fatalf("ordered first join joined=%v err=%v", joined, err)
	}
	joined, err = s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: orderedChatID, Title: "乱序群", Type: "supergroup", Status: "member", LastMembershipUpdateID: 300,
	}, true)
	if err != nil || joined {
		t.Fatalf("same update replay joined=%v err=%v", joined, err)
	}
	if _, err := s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: orderedChatID, Title: "乱序群", Type: "supergroup", Status: "left", LastMembershipUpdateID: 301,
	}, false); err != nil {
		t.Fatal(err)
	}
	joined, err = s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: orderedChatID, Title: "乱序群", Type: "supergroup", Status: "member", LastMembershipUpdateID: 299,
	}, true)
	if err != nil || joined {
		t.Fatalf("stale join must be ignored: joined=%v err=%v", joined, err)
	}
	state, err = s.TelegramGroupState(ctx, orderedChatID)
	if err != nil || state.Status != "left" || state.LastMembershipUpdateID != 301 {
		t.Fatalf("stale join changed state: state=%+v err=%v", state, err)
	}
	if err := s.MergeTelegramGroupObservation(ctx, TelegramGroupState{
		ChatID: orderedChatID, Title: "普通消息观测", Type: "supergroup", Status: "member", Listen: true,
	}); err != nil {
		t.Fatal(err)
	}
	state, err = s.TelegramGroupState(ctx, orderedChatID)
	if err != nil || state.Status != "left" || state.LastMembershipUpdateID != 301 || state.Title != "普通消息观测" {
		t.Fatalf("ordinary observation overwrote membership: state=%+v err=%v", state, err)
	}
	if state.Listen {
		t.Fatalf("ordinary observation overwrote authoritative listen switch: %+v", state)
	}
	if err := s.SetTelegramGroupTitle(ctx, orderedChatID, "新群名"); err != nil {
		t.Fatal(err)
	}
	state, err = s.TelegramGroupState(ctx, orderedChatID)
	if err != nil || state.Title != "新群名" || state.Status != "left" || state.LastMembershipUpdateID != 301 || state.Listen {
		t.Fatalf("title-only update overwrote group state: state=%+v err=%v", state, err)
	}
	joined, err = s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: orderedChatID, Title: "乱序群", Type: "supergroup", Status: "member", LastMembershipUpdateID: 302,
	}, true)
	if err != nil || !joined {
		t.Fatalf("newer rejoin joined=%v err=%v", joined, err)
	}

	// The aggregate and its event share one transaction. A conflicting immutable
	// occurrence must roll the state mutation back instead of committing an
	// active group without a corresponding side effect.
	const rollbackChatID int64 = -2004
	const rollbackUpdateID int64 = 400
	if _, _, err := s.EnqueueDomainOutboxEvent(ctx,
		telegramBotMembershipActivatedOccurrenceKey(rollbackChatID, rollbackUpdateID),
		DomainTopicTelegramBotMembershipActivated,
		json.RawMessage(`{"chat_id":-999,"type":"supergroup","status":"member","update_id":400}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyTelegramBotMembershipUpdate(ctx, TelegramGroupState{
		ChatID: rollbackChatID, Title: "回滚群", Type: "supergroup", Status: "member",
		LastMembershipUpdateID: rollbackUpdateID,
	}, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting outbox occurrence error = %v, want ErrConflict", err)
	}
	if _, err := s.TelegramGroupState(ctx, rollbackChatID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("membership committed without outbox event: err=%v", err)
	}
}

func TestDomainOutboxLifecycleIsDurableAndTopicScoped(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	payload := json.RawMessage(`{"chat_id":-7001,"update_id":81}`)
	id, created, err := s.EnqueueDomainOutboxEvent(ctx, "occurrence:81", "telegram.test", payload)
	if err != nil || !created || id <= 0 {
		t.Fatalf("enqueue id=%d created=%v err=%v", id, created, err)
	}
	if replayID, replayCreated, err := s.EnqueueDomainOutboxEvent(ctx, "occurrence:81", "telegram.test", payload); err != nil || replayCreated || replayID != id {
		t.Fatalf("replay id=%d created=%v err=%v", replayID, replayCreated, err)
	}
	if _, _, err := s.EnqueueDomainOutboxEvent(ctx, "occurrence:81", "telegram.test", json.RawMessage(`{"chat_id":-7002}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("payload conflict error = %v, want ErrConflict", err)
	}
	if events, err := s.ClaimDomainOutboxEvents(ctx, "wrong-consumer", []string{"other.topic"}, 10); err != nil || len(events) != 0 {
		t.Fatalf("unrelated topic claim events=%v err=%v", events, err)
	}
	events, err := s.ClaimDomainOutboxEvents(ctx, "consumer-a", []string{"telegram.test", "telegram.test", ""}, 10)
	if err != nil || len(events) != 1 || events[0].ID != id || events[0].ClaimedAt == nil {
		t.Fatalf("claim events=%+v err=%v", events, err)
	}
	claimedAt := *events[0].ClaimedAt
	if err := s.RetryDomainOutboxEvent(ctx, id, "consumer-a", claimedAt, "temporary"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE domain_outbox_events SET available_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	events, err = s.ClaimDomainOutboxEvents(ctx, "consumer-b", []string{"telegram.test"}, 1)
	if err != nil || len(events) != 1 || events[0].Attempts != 2 || events[0].ClaimedAt == nil {
		t.Fatalf("reclaim events=%+v err=%v", events, err)
	}
	if err := s.CompleteDomainOutboxEvent(ctx, id, "consumer-b", *events[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if events, err := s.ClaimDomainOutboxEvents(ctx, "consumer-c", []string{"telegram.test"}, 1); err != nil || len(events) != 0 {
		t.Fatalf("completed event reclaimed: events=%v err=%v", events, err)
	}

	failedID, _, err := s.EnqueueDomainOutboxEvent(ctx, "occurrence:failed", "telegram.test", payload)
	if err != nil {
		t.Fatal(err)
	}
	events, err = s.ClaimDomainOutboxEvents(ctx, "consumer-failed", []string{"telegram.test"}, 1)
	if err != nil || len(events) != 1 || events[0].ID != failedID || events[0].ClaimedAt == nil {
		t.Fatalf("failed-event claim=%+v err=%v", events, err)
	}
	if err := s.FailDomainOutboxEvent(ctx, failedID, "consumer-failed", *events[0].ClaimedAt, "permanent"); err != nil {
		t.Fatal(err)
	}
	backlogID, _, err := s.EnqueueDomainOutboxEvent(ctx, "occurrence:backlog", "telegram.test", payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE domain_outbox_events SET available_at=now()-interval '3 minutes' WHERE id=$1`, backlogID); err != nil {
		t.Fatal(err)
	}
	health, err := s.ProductHealthStats(ctx, time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if health.DomainOutboxFailures != 1 || health.DomainOutboxBacklog != 1 {
		t.Fatalf("domain outbox health = %+v", health)
	}
}

func TestTelegramGroupMessageStateIsMonotonic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const chatID int64 = -2010

	for _, messageID := range []int{102, 99, 101, 103, 100} {
		if err := s.SaveTelegramGroupLastMessage(ctx, chatID, messageID); err != nil {
			t.Fatal(err)
		}
	}
	lastMessageID, err := s.TelegramGroupLastMessage(ctx, chatID)
	if err != nil || lastMessageID != 103 {
		t.Fatalf("last Telegram message regressed: id=%d err=%v", lastMessageID, err)
	}

	newerSeenAt := time.Now().UTC().Truncate(time.Second)
	if err := s.SaveTelegramGroupSeenMember(ctx, TelegramGroupSeenMember{
		ChatID: chatID, UserID: 42, Name: "新名字", Username: "new", LastSeen: newerSeenAt,
		LastText: "最新发言", MessageID: 103,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTelegramGroupSeenMember(ctx, TelegramGroupSeenMember{
		ChatID: chatID, UserID: 42, Name: "旧名字", Username: "old", LastSeen: newerSeenAt.Add(time.Second),
		LastText: "旧发言", MessageID: 99,
	}); err != nil {
		t.Fatal(err)
	}
	members, err := s.ListTelegramGroupSeenMembers(ctx, chatID, 10)
	if err != nil || len(members) != 1 || members[0].Name != "新名字" || members[0].Username != "new" {
		t.Fatalf("older message regressed member identity: members=%+v err=%v", members, err)
	}
	if err := s.SaveTelegramGroupSeenMember(ctx, TelegramGroupSeenMember{
		ChatID: chatID, UserID: 42, Name: "成员事件名字", Username: "member-event", LastSeen: newerSeenAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	members, err = s.ListTelegramGroupSeenMembers(ctx, chatID, 10)
	if err != nil || len(members) != 1 {
		t.Fatalf("seen members=%+v err=%v", members, err)
	}
	got := members[0]
	if got.MessageID != 103 || got.LastText != "最新发言" {
		t.Fatalf("older observation erased latest message: %+v", got)
	}
	if got.Name != "成员事件名字" || got.Username != "member-event" || !got.LastSeen.Equal(newerSeenAt.Add(2*time.Second)) {
		t.Fatalf("membership observation was not merged: %+v", got)
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

func TestTelegramGroupMonitorAtomicUpdate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.UpdateTelegramGroupMonitor(ctx, -2001, func(mon *TelegramGroupMonitor) error {
				mon.Enabled = true
				mon.PendingCount++
				return nil
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mon, err := s.TelegramGroupMonitor(ctx, -2001)
	if err != nil || mon.PendingCount != writers {
		t.Fatalf("atomic monitor updates lost data: monitor=%+v err=%v", mon, err)
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

func TestRetireLegacyTextProtocolsMigrationIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "history-owner", false)
	sess, err := s.StartSession(ctx, u.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AppendMessage(ctx, sess.ID, "assistant",
		"[历史消息时间 old] [历史消息时间 newer] <b>已发送</b>")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMessageEmbedding(ctx, id, "test-model", []float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	sql, err := migrationsFS.ReadFile("migrations/0094_retire_legacy_text_protocols.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	var content, model string
	var embedding []float32
	if err := s.pool.QueryRow(ctx,
		`SELECT content, embedding, embed_model FROM chat_messages WHERE id = $1`, id).
		Scan(&content, &embedding, &model); err != nil {
		t.Fatal(err)
	}
	if content != "<b>已发送</b>" || embedding != nil || model != "" {
		t.Fatalf("legacy marker cleanup = content=%q embedding=%v model=%q", content, embedding, model)
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
	if err := s.SetScriptToolEnabled(ctx, tool.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("untested script must not enable: %v", err)
	}
	if err := s.RecordScriptToolTest(ctx, tool.ID, tool.Source, "ok", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScriptToolEnabled(ctx, tool.ID, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.ScriptToolByName(ctx, "format_roster")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || !got.LastTestOK || got.TestedSourceHash != ScriptToolSourceHash(got.Source) || got.LastTestResult != "ok" || got.RequiredAction != "create_project" {
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
	if updated.Name != "format_roster_v2" || updated.RequiredAction != "" || updated.Enabled || updated.LastTestOK || updated.TestedSourceHash != "" {
		t.Fatalf("UpdateScriptTool = %+v", updated)
	}
	if err := s.SetScriptToolEnabled(ctx, tool.ID, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("updated source must be retested before enable: %v", err)
	}
	if err := s.RecordScriptToolTest(ctx, tool.ID, tool.Source, "stale", true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale test result must not certify updated source: %v", err)
	}
}

func TestMemoryMiningQueueIsDurableAndIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "memory-owner", true)
	sess, err := s.StartSession(ctx, u.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	userMessageID, err := s.AppendMessage(ctx, sess.ID, "user", "以后默认不展示推理过程")
	if err != nil {
		t.Fatal(err)
	}
	assistantMessageID, err := s.AppendMessage(ctx, sess.ID, "assistant", "已记录")
	if err != nil {
		t.Fatal(err)
	}
	source := "以后默认不展示推理过程"
	for range 2 {
		if err := s.EnqueueMemoryMiningJob(ctx, u.ID, "telegram", sess.ID, userMessageID, assistantMessageID, "[tool] ok", &source, true); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.DueMemoryMiningJobs(ctx, 4)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("DueMemoryMiningJobs = %+v, %v", jobs, err)
	}
	job := jobs[0]
	if job.ClaimedAt == nil || !job.ExplicitCommit || job.ToolEvidence != "[tool] ok" || job.Attempts != 1 ||
		job.UserEvidenceText == nil || *job.UserEvidenceText != source {
		t.Fatalf("claimed job = %+v", job)
	}
	if err := s.CompleteMemoryMiningJob(ctx, job.ID, *job.ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if jobs, err := s.DueMemoryMiningJobs(ctx, 4); err != nil || len(jobs) != 0 {
		t.Fatalf("completed job was reclaimed: %+v, %v", jobs, err)
	}
}

func TestLearningContextIncludesPreviousTurnAndActuallyUsedAssets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	u := mkUser(t, s, "learning-context-owner", true)
	sess, err := s.StartSession(ctx, u.ID, "telegram", "eino")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := s.CreateRule(ctx, "页面设计标准", "页面必须适配移动端。", []string{"scope:global"}, u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := s.BeginConversationTurn(ctx, "learning-context:first", u.ID, sess.ID, "telegram", "创建一个员工概览页面")
	if err != nil || !created {
		t.Fatalf("BeginConversationTurn first = %+v %t %v", first, created, err)
	}
	assistantID, err := s.CompleteConversationTurn(ctx, ConversationTurnCompletion{
		TurnID: first.ID, AssistantText: "页面已经生成。", ResultText: "页面已经生成。",
		AssetUsages: []ConversationAssetUsage{{KnowledgeID: rule.ID, Phase: AssetPhaseInjected, TurnOutcome: AssetOutcomeCompleted}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := s.BeginConversationTurn(ctx, "learning-context:second", u.ID, sess.ID, "telegram", "刚才的排版不适合手机，以后要改进")
	if err != nil || !created || second.UserMessageID == nil {
		t.Fatalf("BeginConversationTurn second = %+v %t %v", second, created, err)
	}
	got, err := s.LearningContextBeforeMessage(ctx, sess.ID, *second.UserMessageID, 6, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[1].ID != assistantID || got.Messages[1].Content != "页面已经生成。" {
		t.Fatalf("learning context messages = %+v", got.Messages)
	}
	if len(got.Assets) != 1 || got.Assets[0].ID != rule.ID || got.Assets[0].Phase != AssetPhaseInjected {
		t.Fatalf("learning context assets = %+v", got.Assets)
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
	// 回填游标：短确认也是完整聊天事实，必须进入索引队列。
	shortID, err := s.AppendMessage(ctx, sess.ID, "user", "在")
	if err != nil {
		t.Fatal(err)
	}
	need, err := s.MessagesNeedingEmbeddingAfter(ctx, "m:2", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var hasShort bool
	for _, m := range need {
		hasShort = hasShort || m.ID == shortID
		if m.ID == shortID && !strings.Contains(m.PreviousContent, "PostgreSQL") {
			t.Fatalf("short message must carry previous context: %+v", m)
		}
		if m.ID == id1 {
			t.Fatal("已嵌入消息不应再进队列")
		}
	}
	if !hasShort {
		t.Fatal("短消息也应进入嵌入队列")
	}
	if err := s.MarkMessageVectorIndexed(ctx, id1, "m:2"); err != nil {
		t.Fatal(err)
	}
	need, err = s.MessagesNeedingEmbeddingAfter(ctx, "m:2", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var hasID1 bool
	for _, m := range need {
		hasID1 = hasID1 || m.ID == id1
	}
	if !hasID1 {
		t.Fatal("同模型但 PostgreSQL 向量为空的消息应重新回填")
	}
	if err := s.SetMessageEmbedding(ctx, id1, "m:2", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearLegacyMessageEmbeddings(ctx); err != nil {
		t.Fatal(err)
	}
	if vecs, err := s.EmbeddedMessagesOfUser(ctx, "m:2", boss.ID); err != nil || len(vecs) != 0 {
		t.Fatalf("旧消息向量应已清理: %+v err=%v", vecs, err)
	}
}

func TestGroupMessageSearchUsesExactSharedChannel(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "group-history-owner", false)
	first, err := s.StartGroupSession(ctx, owner.ID, "telegram:group:-1001", "test")
	if err != nil {
		t.Fatal(err)
	}
	sourceAt := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
	firstID, err := s.AppendMessageWithEnvelope(ctx, first.ID, "user", "视频项目本周发布路线图", MessageEnvelope{
		Provider: "telegram", ExternalChatRef: "-1001", ExternalMessageRef: "41",
		ActorUserID: &owner.ID, ActorDisplayName: "历史显示名", SourceCreatedAt: &sourceAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.StartGroupSession(ctx, owner.ID, "telegram:group:-1002", "test")
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := s.AppendMessage(ctx, second.ID, "user", "另一个群的视频项目路线图")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.SearchMessagesOfChannel(ctx, "telegram:group:-1001", "路线图", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != firstID {
		t.Fatalf("exact group search = %+v, %v", rows, err)
	}
	if rows[0].ActorDisplayName != "历史显示名" || !rows[0].EventAt().Equal(sourceAt) {
		t.Fatalf("lexical search dropped source provenance: %+v", rows[0])
	}
	if rows, err := s.SearchMessagesOfChannel(ctx, "telegram", "路线图", 10); err != nil || len(rows) != 0 {
		t.Fatalf("private channel must not enter shared-group search: %+v, %v", rows, err)
	}
	rows, err = s.SearchMessagesOfUser(ctx, owner.ID, "路线图", 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("private lexical history leaked group messages = %+v, %v", rows, err)
	}
	rows, err = s.MessagesByIDsForChannel(ctx, "telegram:group:-1001", []int64{secondID, firstID})
	if err != nil || len(rows) != 1 || rows[0].ID != firstID {
		t.Fatalf("group SQL reauthorization = %+v, %v", rows, err)
	}
	if rows[0].ActorDisplayName != "历史显示名" || !rows[0].EventAt().Equal(sourceAt) {
		t.Fatalf("semantic reauthorization dropped source provenance: %+v", rows[0])
	}
	rows, err = s.MessagesByIDsForUser(ctx, owner.ID, []int64{firstID, secondID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("private semantic reauthorization leaked group messages = %+v, %v", rows, err)
	}
	dataRows, err := s.ReadData(ctx, owner.ID, false, DataReadQuery{
		Source: "chat_messages", EntityIDs: []string{fmt.Sprint(firstID), fmt.Sprint(secondID)}, Limit: 10,
	})
	if err != nil || len(dataRows) != 0 {
		t.Fatalf("ordinary query_data leaked group messages = %s, %v", dataRows, err)
	}
	dataRows, err = s.ReadData(ctx, owner.ID, true, DataReadQuery{
		Source: "chat_messages", EntityIDs: []string{fmt.Sprint(firstID), fmt.Sprint(secondID)}, Limit: 10,
	})
	if err != nil || len(dataRows) != 2 {
		t.Fatalf("superadmin query_data group messages = %s, %v", dataRows, err)
	}
	actorRows, err := s.ReadData(ctx, owner.ID, true, DataReadQuery{
		Source: "chat_messages", EntityIDs: []string{fmt.Sprint(firstID)}, Limit: 1,
	})
	if err != nil || len(actorRows) != 1 {
		t.Fatalf("stable actor query = %s, %v", actorRows, err)
	}
	var actorRow struct {
		Provider         string     `json:"provider"`
		ActorUserID      int64      `json:"actor_user_id"`
		ActorName        string     `json:"actor_name"`
		ActorDisplayName string     `json:"actor_display_name"`
		SourceCreatedAt  *time.Time `json:"source_created_at"`
	}
	if err := json.Unmarshal(actorRows[0], &actorRow); err != nil {
		t.Fatal(err)
	}
	if actorRow.Provider != "telegram" || actorRow.ActorUserID != owner.ID || actorRow.ActorName != owner.Name ||
		actorRow.ActorDisplayName != "历史显示名" || actorRow.SourceCreatedAt == nil || !actorRow.SourceCreatedAt.Equal(sourceAt) {
		t.Fatalf("stable actor row = %+v", actorRow)
	}
	doc, err := s.MessageSemanticDocumentByID(ctx, firstID)
	if err != nil || doc.Channel != "telegram:group:-1001" || doc.UserID != owner.ID ||
		doc.ActorDisplayName != "历史显示名" || !doc.EventAt().Equal(sourceAt) {
		t.Fatalf("semantic group document = %+v, %v", doc, err)
	}
	pending, err := s.SemanticMessagesNeedingIndexAfter(ctx, "model@revision", 0, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending semantic messages = %+v, %v", pending, err)
	}
	if err := s.MarkMessageVectorIndexed(ctx, firstID, "model@revision"); err != nil {
		t.Fatal(err)
	}
	pending, err = s.SemanticMessagesNeedingIndexAfter(ctx, "model@revision", 0, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != secondID {
		t.Fatalf("durable message marker = %+v, %v", pending, err)
	}
	private, err := s.StartSession(ctx, owner.ID, "api:private:archive:group:notes", "test")
	if err != nil {
		t.Fatal(err)
	}
	privateID, err := s.AppendMessage(ctx, private.ID, "user", "私聊通道名包含 group 片段但仍属于个人历史")
	if err != nil {
		t.Fatal(err)
	}
	dataRows, err = s.ReadData(ctx, owner.ID, false, DataReadQuery{
		Source: "chat_messages", EntityIDs: []string{fmt.Sprint(privateID)}, Limit: 10,
	})
	if err != nil || len(dataRows) != 1 {
		t.Fatalf("structurally private query_data row was hidden by a substring match = %s, %v", dataRows, err)
	}
}

func TestSemanticMessageLookupIncludesImmediateContext(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "semantic-context-owner", false)
	session, err := s.StartSession(ctx, owner.ID, "telegram:private:semantic-context", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, session.ID, "assistant", "需要把视频项目的发布日期调整到周五吗？"); err != nil {
		t.Fatal(err)
	}
	answerID, err := s.AppendMessage(ctx, session.ID, "user", "就这个")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.MessagesByIDsForUser(ctx, owner.ID, []int64{answerID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("semantic context rows = %+v, %v", rows, err)
	}
	if !strings.Contains(rows[0].Content, "current_user: 就这个") ||
		!strings.Contains(rows[0].Content, "previous_assistant: 需要把视频项目的发布日期调整到周五吗？") {
		t.Fatalf("semantic context content = %q", rows[0].Content)
	}
}

func TestChatContextEligibilityPreservesAuditButExcludesRecall(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "context-quality-owner", true)
	session, err := s.StartSession(ctx, owner.ID, "telegram:private:context-quality", "test")
	if err != nil {
		t.Fatal(err)
	}
	keepID, err := s.AppendMessage(ctx, session.ID, "user", "保留这条项目决定")
	if err != nil {
		t.Fatal(err)
	}
	quarantinedID, err := s.AppendMessage(ctx, session.ID, "assistant", "截断碎片")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE chat_messages SET context_eligible = false WHERE id = $1`, quarantinedID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.MessagesAfter(ctx, session.ID, 0, 0)
	if err != nil || len(rows) != 1 || rows[0].ID != keepID {
		t.Fatalf("replay rows = %+v, %v", rows, err)
	}
	if _, err := s.MessageSemanticDocumentByID(ctx, quarantinedID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quarantined semantic document error = %v", err)
	}
	if rows, err := s.SearchMessagesOfUser(ctx, owner.ID, "截断碎片", 10); err != nil || len(rows) != 0 {
		t.Fatalf("quarantined lexical recall = %+v, %v", rows, err)
	}
	auditRows, err := s.ReadData(ctx, owner.ID, true, DataReadQuery{
		Source: "chat_messages", EntityIDs: []string{fmt.Sprint(quarantinedID)}, Limit: 10,
	})
	if err != nil || len(auditRows) != 1 || !strings.Contains(string(auditRows[0]), `"context_eligible": false`) {
		t.Fatalf("audit row = %s, %v", auditRows, err)
	}
}

func TestAutomaticHistoryCandidatesUseOnlyRawUserMessages(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "user-history-owner", false)
	session, err := s.StartSession(ctx, owner.ID, "telegram:private:user-history", "test")
	if err != nil {
		t.Fatal(err)
	}
	assistantID, err := s.AppendMessage(ctx, session.ID, "assistant", "项目暗号 alpha")
	if err != nil {
		t.Fatal(err)
	}
	userMessageID, err := s.AppendMessage(ctx, session.ID, "user", "项目暗号 alpha 由我确认")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.SearchUserMessagesOfUser(ctx, owner.ID, "alpha", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != userMessageID || rows[0].Role != "user" {
		t.Fatalf("user-only lexical rows = %+v, %v", rows, err)
	}
	rows, err = s.UserMessagesByIDsForUser(ctx, owner.ID, []int64{assistantID, userMessageID})
	if err != nil || len(rows) != 1 || rows[0].ID != userMessageID || rows[0].Content != "项目暗号 alpha 由我确认" {
		t.Fatalf("user-only semantic rows = %+v, %v", rows, err)
	}
}

func TestKnowledgeLifecycleArchivesWithoutDeletingAudit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner := mkUser(t, s, "knowledge-lifecycle-owner", true)
	k, err := s.CreateRule(ctx, "可归档流程", "旧流程内容", []string{"lifecycle"}, owner.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := s.SetKnowledgeActive(ctx, k.ID, false, owner.ID, "superseded")
	if err != nil {
		t.Fatal(err)
	}
	if archived.Active || !archived.Pinned {
		t.Fatalf("archived lifecycle state = active:%v pinned:%v", archived.Active, archived.Pinned)
	}
	if rows, err := s.SearchRules(ctx, "可归档流程", 10); err != nil || len(rows) != 0 {
		t.Fatalf("archived search rows = %+v, %v", rows, err)
	}
	if _, err := s.KnowledgeByID(ctx, k.ID); err != nil {
		t.Fatalf("archived audit lookup: %v", err)
	}
	if rows, err := s.KnowledgeByIDs(ctx, []int64{k.ID}); err != nil || len(rows) != 0 {
		t.Fatalf("archived semantic re-read = %+v, %v", rows, err)
	}
	auditRows, err := s.ReadData(ctx, owner.ID, true, DataReadQuery{
		Source: "knowledge", EntityIDs: []string{fmt.Sprint(k.ID)}, Limit: 10,
	})
	if err != nil || len(auditRows) != 1 || !strings.Contains(string(auditRows[0]), `"active": false`) {
		t.Fatalf("archived audit row = %s, %v", auditRows, err)
	}
	restored, err := s.SetKnowledgeActive(ctx, k.ID, true, owner.ID, "restore")
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Active || !restored.Pinned {
		t.Fatalf("restored lifecycle state = active:%v pinned:%v", restored.Active, restored.Pinned)
	}
	if rows, err := s.PinnedRules(ctx); err != nil || len(rows) != 1 || rows[0].ID != k.ID {
		t.Fatalf("restored pinned rules = %+v, %v", rows, err)
	}
	versions, err := s.KnowledgeVersions(ctx, k.ID, 10)
	if err != nil || len(versions) != 2 {
		t.Fatalf("lifecycle versions = %+v, %v", versions, err)
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
	claimed, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed.TaskID == nil || *claimed.TaskID != dev.ID {
		t.Fatalf("应先领到开发任务: %+v err=%v", claimed, err)
	}
	if _, err := s.ClaimNextWorkerRun(ctx, worker.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("测试任务被前置挡住，不应可领: %v", err)
	}
	// 提交+验收 dev 后，test 就绪。
	if _, _, _, _, err := s.CompleteWorkerRun(ctx, claimed.ID, worker.ID, claimed.ClaimID, "done", "", workerproto.OutcomeSucceeded, nil, testWorkerFinalization(claimed.ClaimID, "done")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AcceptTask(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := s.ReadyDependents(ctx, dev.ID)
	if err != nil || len(ready) != 1 || ready[0].ID != test.ID {
		t.Fatalf("ReadyDependents = %+v err=%v", ready, err)
	}
	claimed2, err := s.ClaimNextWorkerRun(ctx, worker.ID)
	if err != nil || claimed2.TaskID == nil || *claimed2.TaskID != test.ID {
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

package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

func openToolsTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 NBCO_TEST_PG_DSN，跳过 tools 集成测试")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	lockPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if _, err := lockPool.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(7767002)); err != nil {
		lockPool.Close()
		s.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = lockPool.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(7767002))
		lockPool.Close()
		s.Close()
	})
	if _, err := lockPool.Exec(ctx,
		`TRUNCATE notification_deliveries, external_action_receipts, users, projects, roles, bind_keys, audit_log, knowledge, kv_state, info_fields, ai_usage, pending_approvals, goals RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return s
}

func mkToolsUser(t *testing.T, s *store.Store, name string, super bool) *store.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name, super, store.Identity{Provider: "tools-test", ExternalID: name})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mkToolsProject(t *testing.T, s *store.Store, creatorID int64) *store.Project {
	t.Helper()
	pj, err := s.CreateProject(context.Background(), "测试项目", "", creatorID)
	if err != nil {
		t.Fatal(err)
	}
	return pj
}

func grantCreateProjectAll(t *testing.T, s *store.Store, userID, grantedBy int64) {
	t.Helper()
	if err := s.GrantPerm(context.Background(), store.Grant{
		Kind: store.KindActive, UserID: userID, Action: perm.ActCreateProject, Target: store.TargetAll, GrantedBy: grantedBy,
	}); err != nil {
		t.Fatal(err)
	}
}

func callToolByName(t *testing.T, ts []ai.Tool, name string, args any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range ts {
		if tl.Name == name {
			out, err := tl.Handler(context.Background(), raw)
			if err != nil {
				t.Fatalf("%s returned error: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("tool %s not found", name)
	return ""
}

func TestGoalToolsRequireGoalOwnerForWrites(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "owner", true)
	manager := mkToolsUser(t, s, "manager", false)
	grantCreateProjectAll(t, s, manager.ID, owner.ID)
	pj := mkToolsProject(t, s, manager.ID)
	g, err := s.CreateGoal(ctx, "提升留存", "", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMilestone(ctx, g.ID, "基线调研", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	d := Deps{Store: s, TZ: time.UTC}
	managerTools := ForUser(d, manager, nil)
	if got := callToolByName(t, managerTools, "add_milestone", map[string]any{
		"goal_id": g.ID, "title": "越权里程碑",
	}); !strings.Contains(got, "只有目标 owner") {
		t.Fatalf("add_milestone should reject non-owner, got %q", got)
	}
	if ms, err := s.MilestonesOfGoal(ctx, g.ID); err != nil || len(ms) != 1 {
		t.Fatalf("non-owner add_milestone should not create milestone, len=%d err=%v", len(ms), err)
	}

	if got := callToolByName(t, managerTools, "decompose_milestone", map[string]any{
		"milestone_id": m.ID,
		"project_id":   pj.ID,
		"tasks": []map[string]any{{
			"assignee_id": manager.ID,
			"title":       "越权任务",
			"description": "不应创建",
		}},
	}); !strings.Contains(got, "只有目标 owner") {
		t.Fatalf("decompose_milestone should reject non-owner, got %q", got)
	}
	if ts, err := s.TasksOfMilestone(ctx, m.ID); err != nil || len(ts) != 0 {
		t.Fatalf("non-owner decompose should not create tasks, len=%d err=%v", len(ts), err)
	}
}

func TestDecomposeMilestoneRejectsArchivedProject(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "owner", true)
	worker := mkToolsUser(t, s, "worker", false)
	pj := mkToolsProject(t, s, owner.ID)
	if err := s.SetProjectStatus(ctx, pj.ID, store.ProjectArchived); err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGoal(ctx, "提升交付", "", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMilestone(ctx, g.ID, "执行阶段", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	got := callToolByName(t, ForUser(Deps{Store: s, TZ: time.UTC}, owner, nil), "decompose_milestone", map[string]any{
		"milestone_id": m.ID,
		"project_id":   pj.ID,
		"tasks": []map[string]any{{
			"assignee_id": worker.ID,
			"title":       "归档项目任务",
			"description": "不应创建",
		}},
	})
	if !strings.Contains(got, "项目已归档") {
		t.Fatalf("decompose_milestone should reject archived project, got %q", got)
	}
	if ts, err := s.TasksOfMilestone(ctx, m.ID); err != nil || len(ts) != 0 {
		t.Fatalf("archived project decompose should not create tasks, len=%d err=%v", len(ts), err)
	}
}

// recEventer 记录所有 emit，用于断言 close_goal/close_milestone 是否触发复盘事件。
type recEventer struct {
	events []recEvent
}
type recEvent struct {
	kind    string
	decider int64
	detail  string
}

func (r *recEventer) Emit(kind string, deciderID int64, detail string) {
	r.events = append(r.events, recEvent{kind, deciderID, detail})
}

func TestReassignTaskPreservesHistoryAndNotifies(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	boss := mkToolsUser(t, s, "boss", true)
	alice := mkToolsUser(t, s, "alice", false) // 旧执行人
	bob := mkToolsUser(t, s, "bob", false)     // 新执行人
	pj := mkToolsProject(t, s, boss.ID)
	// boss 派给 alice，alice 写一条进度。
	tk, err := s.CreateTask(ctx, &store.Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T", Description: "做某事"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddProgress(ctx, tk.ID, alice.ID, "已调研"); err != nil {
		t.Fatal(err)
	}
	refID := tk.ID
	if _, err := s.UpsertDecisionItem(ctx, store.DecisionItem{
		OwnerID: boss.ID, Kind: "orphaned_task", Title: "改派孤儿任务：T",
		Detail: "执行人已停用。", RefType: "task", RefID: &refID, Priority: "high",
	}); err != nil {
		t.Fatal(err)
	}

	d := Deps{Store: s, TZ: time.UTC}
	// 非 assigner 不能改派。
	if got := callToolByName(t, ForUser(d, alice, nil), "reassign_task", map[string]any{
		"task_id": tk.ID, "assignee_id": bob.ID,
	}); !strings.Contains(got, "只有分配者") {
		t.Fatalf("非 assigner 改派应被拒, got %q", got)
	}
	// boss 改派给 bob，带原因。
	got := callToolByName(t, ForUser(d, boss, nil), "reassign_task", map[string]any{
		"task_id": tk.ID, "assignee_id": bob.ID, "reason": "alice 离线",
	})
	if !strings.Contains(got, "改派给 "+bob.Name) || !strings.Contains(got, "进度历史保留") {
		t.Fatalf("改派回复 = %q", got)
	}
	// 任务 ID 不变，assignee 变 bob，状态回 pending，进度历史 +1（含改派记录）。
	gotTask, err := s.TaskByID(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask.ID != tk.ID {
		t.Errorf("改派后 task id 变了: %d != %d", gotTask.ID, tk.ID)
	}
	if gotTask.AssigneeID != bob.ID {
		t.Errorf("改派后 assignee=%d, want %d", gotTask.AssigneeID, bob.ID)
	}
	if gotTask.Status != store.TaskPending {
		t.Errorf("改派后 status=%s, want pending", gotTask.Status)
	}
	openDecisions, err := s.ListDecisionItems(ctx, boss.ID, "open", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(openDecisions) != 0 {
		t.Fatalf("改派后应关闭该任务决策项，仍有 %#v", openDecisions)
	}
	prog, err := s.ProgressOf(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prog) != 2 { // 原进度 + 改派记录
		t.Errorf("进度应有 2 条（原+改派）, got %d", len(prog))
	}
	// 改派给当前执行人（bob 已是执行人）→ 拒绝。
	if got := callToolByName(t, ForUser(d, boss, nil), "reassign_task", map[string]any{
		"task_id": tk.ID, "assignee_id": bob.ID,
	}); !strings.Contains(got, "相同") {
		t.Fatalf("改派给当前执行人应拒绝, got %q", got)
	}
}

func TestReassignTaskRejectsDoneStatus(t *testing.T) {
	// 已提交待验收（done）的任务改派会丢失验收记录——应拒绝，提示先 reject。
	s := openToolsTestStore(t)
	ctx := context.Background()
	boss := mkToolsUser(t, s, "boss", true)
	alice := mkToolsUser(t, s, "alice", false)
	bob := mkToolsUser(t, s, "bob", false)
	pj := mkToolsProject(t, s, boss.ID)
	tk, err := s.CreateTask(ctx, &store.Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "T"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SubmitTask(ctx, tk.ID); err != nil { // → done
		t.Fatal(err)
	}
	d := Deps{Store: s, TZ: time.UTC}
	got := callToolByName(t, ForUser(d, boss, nil), "reassign_task", map[string]any{
		"task_id": tk.ID, "assignee_id": bob.ID,
	})
	if !strings.Contains(got, "待验收") || !strings.Contains(got, "reject_task") {
		t.Fatalf("改派 done 任务应拒绝并提示先 reject, got %q", got)
	}
	// 任务状态未变（仍 done，assignee 仍 alice）。
	if gt, _ := s.TaskByID(ctx, tk.ID); gt.Status != store.TaskDone || gt.AssigneeID != alice.ID {
		t.Errorf("拒绝改派后任务不应变, got status=%s assignee=%d", gt.Status, gt.AssigneeID)
	}
}

func TestReviewDecisionClosesOnAcceptAndReject(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	boss := mkToolsUser(t, s, "boss", true)
	alice := mkToolsUser(t, s, "alice", false)
	pj := mkToolsProject(t, s, boss.ID)
	d := Deps{Store: s, TZ: time.UTC}

	tk, err := s.CreateTask(ctx, &store.Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "验收项"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SubmitTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildDecisionQueue(ctx, boss.ID); err != nil {
		t.Fatal(err)
	}
	if got := callToolByName(t, ForUser(d, boss, nil), "accept_task", map[string]any{
		"task_id": tk.ID, "comment": "OK",
	}); !strings.Contains(got, "已验收通过") {
		t.Fatalf("accept_task = %q", got)
	}
	if items, err := s.ListDecisionItems(ctx, boss.ID, "open", 10); err != nil || len(items) != 0 {
		t.Fatalf("accept 后决策项应关闭，items=%#v err=%v", items, err)
	}

	tk, err = s.CreateTask(ctx, &store.Task{ProjectID: pj.ID, AssignerID: boss.ID, AssigneeID: alice.ID, Title: "打回项"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SubmitTask(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildDecisionQueue(ctx, boss.ID); err != nil {
		t.Fatal(err)
	}
	if got := callToolByName(t, ForUser(d, boss, nil), "reject_task", map[string]any{
		"task_id": tk.ID, "reason": "证据不足",
	}); !strings.Contains(got, "已打回") {
		t.Fatalf("reject_task = %q", got)
	}
	if items, err := s.ListDecisionItems(ctx, boss.ID, "open", 10); err != nil || len(items) != 0 {
		t.Fatalf("reject 后决策项应关闭，items=%#v err=%v", items, err)
	}
}

func TestCloseGoalEmitsEventOnAchievedByNonOwner(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "owner", false)
	// 关闭者是超管 admin，与 owner 不同——超管能关，且 owner≠actor 触发事件。
	admin := mkToolsUser(t, s, "admin", true)
	g, err := s.CreateGoal(ctx, "提升留存", "", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMilestone(ctx, g.ID, "基线调研", "", nil); err != nil {
		t.Fatal(err)
	}

	rec := &recEventer{}
	d := Deps{Store: s, TZ: time.UTC, Events: rec}
	got := callToolByName(t, ForUser(d, admin, nil), "close_goal", map[string]any{
		"goal_id": g.ID, "status": "achieved",
	})
	if !strings.Contains(got, "已达成") {
		t.Fatalf("close_goal achieved reply = %q", got)
	}
	if len(rec.events) != 1 {
		t.Fatalf("达成应 emit 1 事件, got %d", len(rec.events))
	}
	e := rec.events[0]
	if e.kind != "目标达成" || e.decider != owner.ID {
		t.Errorf("emit = {%s, decider=%d}, want {目标达成, %d}", e.kind, e.decider, owner.ID)
	}
	if !strings.Contains(e.detail, g.Title) || !strings.Contains(e.detail, "里程碑 0/1 达成") {
		t.Errorf("detail 应含目标与进度事实, got %q", e.detail)
	}
	for _, protocol := range []string{"回复", "save_knowledge", "跳过"} {
		if strings.Contains(e.detail, protocol) {
			t.Errorf("事件事实不应携带旧动作协议 %q: %q", protocol, e.detail)
		}
	}

	// 已关闭目标是终态，不能再从 achieved 改 archived，也不能再次触发事件。
	rec.events = nil
	if got := callToolByName(t, ForUser(d, admin, nil), "close_goal", map[string]any{
		"goal_id": g.ID, "status": "archived",
	}); !strings.Contains(got, "不能重复关闭") {
		t.Fatalf("close_goal terminal transition should be rejected, got %q", got)
	}
	if len(rec.events) != 0 {
		t.Errorf("重复关闭不应 emit, got %d", len(rec.events))
	}

	// owner 自己关闭自己的 fresh goal → 不自我打扰（不 emit）。
	selfGoal, err := s.CreateGoal(ctx, "owner 自己归档", "", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := callToolByName(t, ForUser(d, owner, nil), "close_goal", map[string]any{
		"goal_id": selfGoal.ID, "status": "archived",
	}); !strings.Contains(got, "已归档") {
		t.Fatalf("owner close_goal archived reply = %q", got)
	}
	if len(rec.events) != 0 {
		t.Errorf("owner 自行关闭不应 emit, got %d", len(rec.events))
	}
}

func TestCloseGoalNoEventOnArchive(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "owner", false)
	admin := mkToolsUser(t, s, "admin", true)
	g, err := s.CreateGoal(ctx, "废弃方向", "", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := &recEventer{}
	d := Deps{Store: s, TZ: time.UTC, Events: rec}
	if got := callToolByName(t, ForUser(d, admin, nil), "close_goal", map[string]any{
		"goal_id": g.ID, "status": "archived",
	}); !strings.Contains(got, "已归档") {
		t.Fatalf("close_goal archived reply = %q", got)
	}
	// 归档（放弃）无复盘价值，不 emit。
	if len(rec.events) != 0 {
		t.Errorf("归档不应 emit 复盘事件, got %d: %+v", len(rec.events), rec.events)
	}
}

func TestCloseMilestoneEmitsEventOnAchievedByNonOwner(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "owner", false)
	admin := mkToolsUser(t, s, "admin", true)
	g, err := s.CreateGoal(ctx, "G", "", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMilestone(ctx, g.ID, "基线调研", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	rec := &recEventer{}
	d := Deps{Store: s, TZ: time.UTC, Events: rec}
	got := callToolByName(t, ForUser(d, admin, nil), "close_milestone", map[string]any{
		"milestone_id": m.ID, "status": "achieved",
	})
	if !strings.Contains(got, "已达成") {
		t.Fatalf("close_milestone achieved reply = %q", got)
	}
	for _, want := range []string{"所属目标", "仍为 active", "不会自动关闭目标"} {
		if !strings.Contains(got, want) {
			t.Fatalf("close_milestone lifecycle boundary missing %q: %q", want, got)
		}
	}
	if len(rec.events) != 1 || rec.events[0].kind != "里程碑达成" || rec.events[0].decider != owner.ID {
		t.Fatalf("应 emit 里程碑达成 给 owner, got %+v", rec.events)
	}
	if !strings.Contains(rec.events[0].detail, m.Title) || !strings.Contains(rec.events[0].detail, g.Title) {
		t.Errorf("detail 应含里程碑与所属目标标题, got %q", rec.events[0].detail)
	}
	rec.events = nil
	got = callToolByName(t, ForUser(d, admin, nil), "close_milestone", map[string]any{
		"milestone_id": m.ID, "status": "archived",
	})
	if !strings.Contains(got, "不能重复关闭") {
		t.Fatalf("close_milestone terminal transition should be rejected, got %q", got)
	}
	if len(rec.events) != 0 {
		t.Errorf("重复关闭里程碑不应 emit, got %+v", rec.events)
	}
}

func TestUnlinkTaskMilestoneRequiresGoalOwner(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "owner", true)
	assigner := mkToolsUser(t, s, "assigner", false)
	pj := mkToolsProject(t, s, assigner.ID)
	g, err := s.CreateGoal(ctx, "公司目标", "", owner.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateMilestone(ctx, g.ID, "目标里程碑", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := s.CreateTask(ctx, &store.Task{
		ProjectID: pj.ID, AssignerID: assigner.ID, AssigneeID: assigner.ID,
		Title: "已归因任务", MilestoneID: &m.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := callToolByName(t, ForUser(Deps{Store: s, TZ: time.UTC}, assigner, nil), "link_task_to_milestone", map[string]any{
		"task_id":      tk.ID,
		"milestone_id": 0,
	})
	if !strings.Contains(got, "只有目标 owner") {
		t.Fatalf("unlink should reject non-owner assigner, got %q", got)
	}
	gotTask, err := s.TaskByID(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask.MilestoneID == nil || *gotTask.MilestoneID != m.ID {
		t.Fatalf("rejected unlink should keep milestone_id=%d, got %v", m.ID, gotTask.MilestoneID)
	}
}

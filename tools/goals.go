package tools

// 目标 / 里程碑工具：把模糊战略目标结构化拆解、跟踪进度。
// 三层 Goal→Milestone→Task：Goal 公司级（跨项目），Milestone 可验收关键节点，
// Task 通过可选 milestone_id 做战略归因（仍归 Project 执行）。状态手动流转
// （close_goal/close_milestone），不从任务自动推导——"达成"是判断题。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

func goalTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{

		// --- 建结构（toolPerm 兜 create_project 可见性） ---

		tool("create_goal",
			"创建一个战略目标（公司级，跨项目）。用于长期/方向性目标（如「提升留存」「下季度营收翻倍」），"+
				"不是单一执行项（那用 assign_task）。建后用 add_milestone 拆关键里程碑，decompose_milestone 落成任务。",
			obj(map[string]any{
				"title":       p("string", "目标标题（简短有力，如「提升付费留存到 60%」）"),
				"description": p("string", "目标说明：为什么、成功标准、边界（可选）"),
				"deadline":    p("string", "目标截止时间 ISO8601（可选）"),
			}, "title"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Title       string `json:"title"`
					Description string `json:"description"`
					Deadline    string `json:"deadline"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Title) == "" {
					return "目标标题不能为空。", nil
				}
				deadline, derr := parseDeadline(args.Deadline, d.TZ)
				if derr != nil {
					return derr.Error(), nil
				}
				g, err := d.Store.CreateGoal(ctx, strings.TrimSpace(args.Title), args.Description, u.ID, deadline)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("目标「%s」已创建（%s），当前状态 active（尚未达成或归档）。用 add_milestone 拆解里程碑。", g.Title, internalRef("目标", g.ID)), nil
			}),

		tool("add_milestone",
			"给战略目标加一个可验收的关键里程碑（如「基线调研」「流失归因」）。达成由你判断后用 close_milestone 标记（不自动从任务推导）。目标须为 active。",
			obj(map[string]any{
				"goal_id":     p("integer", "目标ID"),
				"title":       p("string", "里程碑标题"),
				"description": p("string", "里程碑说明/验收要点（可选）"),
				"deadline":    p("string", "截止时间 ISO8601（可选）"),
			}, "goal_id", "title"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					GoalID      int64  `json:"goal_id"`
					Title       string `json:"title"`
					Description string `json:"description"`
					Deadline    string `json:"deadline"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Title) == "" {
					return "里程碑标题不能为空。", nil
				}
				g, err := d.Store.GoalByID(ctx, args.GoalID)
				if err != nil {
					return fmt.Sprintf("目标 %d 不存在", args.GoalID), nil
				}
				if g.OwnerID != u.ID && !u.IsSuperadmin {
					return "只有目标 owner 或超管能添加里程碑。", nil
				}
				if g.Status != store.GoalActive {
					return fmt.Sprintf("目标「%s」已是 %s 状态，不能再加里程碑。", g.Title, g.Status), nil
				}
				deadline, derr := parseDeadline(args.Deadline, d.TZ)
				if derr != nil {
					return derr.Error(), nil
				}
				m, err := d.Store.CreateMilestone(ctx, g.ID, strings.TrimSpace(args.Title), args.Description, deadline)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("里程碑「%s」已加到目标「%s」（%s），当前状态 active。用 decompose_milestone 落成任务。", m.Title, g.Title, internalRef("里程碑", m.ID)), nil
			}),

		tool("decompose_milestone",
			"把里程碑批量拆成具体任务（单事务，一次建多个）。任务归指定项目执行，同时挂到该里程碑做战略归因。"+
				"assignee_id 省略=自动派给最合适的 AI 员工；depends_on 限同项目已存在任务（可跨里程碑）。对每个执行人逐个校验 create_project 权限。",
			obj(map[string]any{
				"milestone_id": p("integer", "里程碑ID"),
				"project_id":   p("integer", "任务归属的项目ID"),
				"tasks": map[string]any{
					"type": "array", "description": "任务列表",
					"items": obj(map[string]any{
						"assignee_id": p("integer", "执行人用户ID（省略=自动选 AI 员工）"),
						"title":       p("string", "任务标题"),
						"goal":        p("string", "为什么做（可选）"),
						"description": p("string", "做什么"),
						"acceptance":  p("string", "验收标准（可选）"),
						"deadline":    p("string", "截止时间 ISO8601（可选）"),
						"priority":    enumP("优先级，可选", "low", "normal", "high"),
						"depends_on":  arr("integer", "前置任务ID列表（同项目，可选）"),
					}, "title", "description"),
				},
			}, "milestone_id", "project_id", "tasks"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					MilestoneID int64 `json:"milestone_id"`
					ProjectID   int64 `json:"project_id"`
					Tasks       []struct {
						AssigneeID  int64   `json:"assignee_id"`
						Title       string  `json:"title"`
						Goal        string  `json:"goal"`
						Description string  `json:"description"`
						Acceptance  string  `json:"acceptance"`
						Deadline    string  `json:"deadline"`
						Priority    string  `json:"priority"`
						DependsOn   []int64 `json:"depends_on"`
					} `json:"tasks"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if len(args.Tasks) == 0 {
					return "至少要有一个任务。", nil
				}
				m, err := d.Store.MilestoneByID(ctx, args.MilestoneID)
				if err != nil {
					return fmt.Sprintf("里程碑 %d 不存在", args.MilestoneID), nil
				}
				if m.Status != store.GoalActive {
					return fmt.Sprintf("里程碑「%s」已是 %s 状态，不能再拆任务。", m.Title, m.Status), nil
				}
				g, err := d.Store.GoalByID(ctx, m.GoalID)
				if err != nil {
					return "", err
				}
				if g.Status != store.GoalActive {
					return fmt.Sprintf("目标「%s」已是 %s 状态，不能再拆任务。", g.Title, g.Status), nil
				}
				if g.OwnerID != u.ID && !u.IsSuperadmin {
					return "只有目标 owner 或超管能拆解里程碑。", nil
				}
				pj, err := d.Store.ProjectByID(ctx, args.ProjectID)
				if err != nil {
					return fmt.Sprintf("项目 %d 不存在", args.ProjectID), nil
				}
				if pj.CreatorID != u.ID && !u.IsSuperadmin {
					return "只能往自己创建的项目里派任务。", nil
				}
				if pj.Status != store.ProjectActive {
					return "项目已归档，不能再派任务。", nil
				}
				var grants []store.Grant
				if !u.IsSuperadmin {
					if grants, err = d.Store.PermsOf(ctx, u.ID); err != nil {
						return "", err
					}
				}
				subs := make([]*store.Task, 0, len(args.Tasks))
				assignees := make(map[int64]*store.User)
				mid := m.ID
				for _, st := range args.Tasks {
					if strings.TrimSpace(st.Title) == "" {
						return "每个任务都要有标题。", nil
					}
					assigneeID := st.AssigneeID
					if assigneeID == 0 {
						wid, _, perr := pickWorkerAssignee(ctx, d, u, st.Title, st.Description, st.Acceptance)
						if perr != nil {
							return perr.Error(), nil
						}
						assigneeID = wid
						if w, werr := d.Store.UserByID(ctx, wid); werr == nil {
							assignees[w.ID] = w
						}
					} else {
						au, merr := mustUser(ctx, d.Store, assigneeID)
						if merr != nil {
							return merr.Error(), nil
						}
						if !u.IsSuperadmin && au.ID != u.ID && !perm.CheckActive(grants, perm.ActCreateProject, au.ID) {
							return fmt.Sprintf("你没有对 %s 的 create_project 权限，不能把任务派给对方（可派给自己）。", au.Name), nil
						}
						assignees[au.ID] = au
					}
					deadline, derr := parseDeadline(st.Deadline, d.TZ)
					if derr != nil {
						return derr.Error(), nil
					}
					subs = append(subs, &store.Task{
						ProjectID: args.ProjectID, AssignerID: u.ID, AssigneeID: assigneeID,
						Title: st.Title, Goal: st.Goal, Description: st.Description,
						Acceptance: st.Acceptance, Priority: st.Priority, Deadline: deadline,
						DependsOn: st.DependsOn, MilestoneID: &mid,
					})
				}
				created, err := d.Store.CreateMilestoneTasks(ctx, subs)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "depends_on 里有不存在的任务ID（须为同项目已存在任务）。", nil
					}
					return "", err
				}
				inheritViewPerms(ctx, d, u, created)
				for _, t := range created {
					if t.AssigneeID != u.ID {
						notifyQuiet(ctx, d, t.AssigneeID,
							fmt.Sprintf("📌 %s 给你分配了任务「%s」（%s）\n%s", u.Name, t.Title, internalRef("任务", t.ID), t.Description))
					}
					wakeWorker(d, assignees[t.AssigneeID])
				}
				var b strings.Builder
				fmt.Fprintf(&b, "已为里程碑「%s」（%s）创建 %d 个任务：\n", m.Title, internalRef("里程碑", m.ID), len(created))
				for _, t := range created {
					fmt.Fprintf(&b, "- %s：%s → %s\n", internalRef("任务", t.ID), t.Title, userName(ctx, d.Store, t.AssigneeID))
				}
				return b.String(), nil
			}),

		// --- 改/关（人人可见工具，handler 内 owner 校验） ---

		tool("update_goal",
			"修改目标标题/说明/截止时间（省略的字段保持原值）。限目标 owner 或超管。",
			obj(map[string]any{
				"goal_id":     p("integer", "目标ID"),
				"title":       p("string", "新标题（可选）"),
				"description": p("string", "新说明（可选）"),
				"deadline":    p("string", "新截止时间 ISO8601（可选）"),
			}, "goal_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					GoalID      int64   `json:"goal_id"`
					Title       *string `json:"title"`
					Description *string `json:"description"`
					Deadline    string  `json:"deadline"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, err := d.Store.GoalByID(ctx, args.GoalID)
				if err != nil {
					return fmt.Sprintf("目标 %d 不存在", args.GoalID), nil
				}
				if g.OwnerID != u.ID && !u.IsSuperadmin {
					return "只有目标 owner 或超管能修改。", nil
				}
				var deadline *time.Time
				if strings.TrimSpace(args.Deadline) != "" {
					dl, derr := parseDeadline(args.Deadline, d.TZ)
					if derr != nil {
						return derr.Error(), nil
					}
					deadline = dl
				}
				g, err = d.Store.UpdateGoal(ctx, args.GoalID, args.Title, args.Description, deadline)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("目标「%s」已更新。", g.Title), nil
			}),

		tool("close_goal",
			"标记目标达成（achieved）或归档（archived）。达成是判断题：看 view_goals 的里程碑进度后由你拍板，不从任务自动推导。限 owner 或超管。",
			obj(map[string]any{
				"goal_id": p("integer", "目标ID"),
				"status":  enumP("目标终态", "achieved", "archived"),
			}, "goal_id", "status"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					GoalID int64  `json:"goal_id"`
					Status string `json:"status"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.Status != store.GoalAchieved && args.Status != store.GoalArchived {
					return "status 必须是 achieved 或 archived。", nil
				}
				g, err := d.Store.GoalByID(ctx, args.GoalID)
				if err != nil {
					return fmt.Sprintf("目标 %d 不存在", args.GoalID), nil
				}
				if g.OwnerID != u.ID && !u.IsSuperadmin {
					return "只有目标 owner 或超管能关闭。", nil
				}
				if g.Status != store.GoalActive {
					return fmt.Sprintf("目标「%s」已是 %s 状态，不能重复关闭。", g.Title, g.Status), nil
				}
				if err := d.Store.SetGoalStatus(ctx, g.ID, args.Status); err != nil {
					return "", err
				}
				label := "已标记为已达成"
				if args.Status == store.GoalArchived {
					label = "已归档"
				}
				// 达成（非归档）且非 owner 自行关闭时，发事件让 owner 侧的 AI 自决是否复盘：
				// 归档是放弃、无复盘价值；owner 自己关不必自我打扰（镜像任务提交的 AssignerID!=AssigneeID 守卫）。
				// 沉淀动作（save_knowledge/save_infos_on_user）由事件触发的 AI 轮次自决，不在工具内硬塞——
				// 与「达成靠判断、不自动推导」同理。
				if args.Status == store.GoalAchieved && g.OwnerID != u.ID {
					emitRequiredEvent(d, "目标达成", g.OwnerID, buildGoalClosedDetail(ctx, d.Store, g, u.Name))
				}
				return fmt.Sprintf("目标「%s」%s。", g.Title, label), nil
			}),

		tool("update_milestone",
			"修改里程碑标题/说明/截止时间。限其所属目标的 owner 或超管。",
			obj(map[string]any{
				"milestone_id": p("integer", "里程碑ID"),
				"title":        p("string", "新标题（可选）"),
				"description":  p("string", "新说明（可选）"),
				"deadline":     p("string", "新截止时间 ISO8601（可选）"),
			}, "milestone_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					MilestoneID int64   `json:"milestone_id"`
					Title       *string `json:"title"`
					Description *string `json:"description"`
					Deadline    string  `json:"deadline"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				m, g, err := loadMilestoneWithGoal(ctx, d.Store, args.MilestoneID)
				if err != nil {
					return fmt.Sprintf("里程碑 %d 不存在", args.MilestoneID), nil
				}
				if g.OwnerID != u.ID && !u.IsSuperadmin {
					return "只有目标 owner 或超管能修改。", nil
				}
				var deadline *time.Time
				if strings.TrimSpace(args.Deadline) != "" {
					dl, derr := parseDeadline(args.Deadline, d.TZ)
					if derr != nil {
						return derr.Error(), nil
					}
					deadline = dl
				}
				m, err = d.Store.UpdateMilestone(ctx, m.ID, args.Title, args.Description, deadline)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("里程碑「%s」已更新。", m.Title), nil
			}),

		tool("close_milestone",
			"标记里程碑达成（achieved）或归档（archived）。达成由你判断后手动标记（不自动从任务推导）。限其所属目标的 owner 或超管。",
			obj(map[string]any{
				"milestone_id": p("integer", "里程碑ID"),
				"status":       enumP("里程碑终态", "achieved", "archived"),
			}, "milestone_id", "status"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					MilestoneID int64  `json:"milestone_id"`
					Status      string `json:"status"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.Status != store.GoalAchieved && args.Status != store.GoalArchived {
					return "status 必须是 achieved 或 archived。", nil
				}
				m, g, err := loadMilestoneWithGoal(ctx, d.Store, args.MilestoneID)
				if err != nil {
					return fmt.Sprintf("里程碑 %d 不存在", args.MilestoneID), nil
				}
				if g.OwnerID != u.ID && !u.IsSuperadmin {
					return "只有目标 owner 或超管能关闭里程碑。", nil
				}
				if g.Status != store.GoalActive {
					return fmt.Sprintf("目标「%s」已是 %s 状态，不能再关闭里程碑。", g.Title, g.Status), nil
				}
				if m.Status != store.GoalActive {
					return fmt.Sprintf("里程碑「%s」已是 %s 状态，不能重复关闭。", m.Title, m.Status), nil
				}
				if err := d.Store.SetMilestoneStatus(ctx, m.ID, args.Status); err != nil {
					return "", err
				}
				label := "已标记为已达成"
				if args.Status == store.GoalArchived {
					label = "已归档"
				}
				// 达成（非归档）且非目标 owner 自行关闭时发事件，让其侧 AI 自决是否复盘。
				if args.Status == store.GoalAchieved && g.OwnerID != u.ID {
					emitRequiredEvent(d, "里程碑达成", g.OwnerID, buildMilestoneClosedDetail(ctx, d.Store, m, g, u.Name))
				}
				return fmt.Sprintf("里程碑「%s」%s；所属目标「%s」仍为 %s，本操作不会自动关闭目标或归档项目。", m.Title, label, g.Title, g.Status), nil
			}),

		tool("link_task_to_milestone",
			"把已有任务挂到某里程碑（战略归因），或传 milestone_id=0 解绑。限任务分配者且为目标 owner（或超管）。",
			obj(map[string]any{
				"task_id":      p("integer", "任务ID"),
				"milestone_id": p("integer", "里程碑ID（0=解绑）"),
			}, "task_id", "milestone_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID      int64 `json:"task_id"`
					MilestoneID int64 `json:"milestone_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				if t.AssignerID != u.ID && !u.IsSuperadmin {
					return "只有任务分配者能改它的里程碑归因。", nil
				}
				var mid *int64
				if args.MilestoneID != 0 {
					m, g, merr := loadMilestoneWithGoal(ctx, d.Store, args.MilestoneID)
					if merr != nil {
						return fmt.Sprintf("里程碑 %d 不存在", args.MilestoneID), nil
					}
					if g.OwnerID != u.ID && !u.IsSuperadmin {
						return "只有目标 owner 能把任务挂到其里程碑。", nil
					}
					if g.Status != store.GoalActive || m.Status != store.GoalActive {
						return "目标或里程碑已关闭，不能再调整任务归因。", nil
					}
					mid = &m.ID
				} else if t.MilestoneID != nil {
					_, g, merr := loadMilestoneWithGoal(ctx, d.Store, *t.MilestoneID)
					if merr != nil {
						return "", merr
					}
					if g.OwnerID != u.ID && !u.IsSuperadmin {
						return "只有目标 owner 能解除任务的里程碑归因。", nil
					}
				}
				if err := d.Store.SetTaskMilestone(ctx, t.ID, mid); err != nil {
					return "", err
				}
				if mid == nil {
					return fmt.Sprintf("任务「%s」已解除里程碑归因。", t.Title), nil
				}
				return fmt.Sprintf("任务「%s」已挂到里程碑（%s）。", t.Title, internalRef("里程碑", *mid)), nil
			}),

		// --- 查看（人人可见） ---

		tool("view_goals",
			"查看战略目标清单及进度（每个目标的里程碑达成率 + 各里程碑任务进度）。公司战略对齐用，人人可查。",
			obj(map[string]any{
				"active_only": p("boolean", "仅看推进中的目标（默认 true）"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ActiveOnly *bool `json:"active_only"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				activeOnly := true
				if args.ActiveOnly != nil {
					activeOnly = *args.ActiveOnly
				}
				goals, err := d.Store.ListGoals(ctx, activeOnly)
				if err != nil {
					return "", err
				}
				if len(goals) == 0 {
					return "（没有目标。用 create_goal 建一个。）", nil
				}
				goalIDs := make([]int64, len(goals))
				for i, g := range goals {
					goalIDs[i] = g.ID
				}
				gmc, err := d.Store.GoalMilestoneCounts(ctx, goalIDs)
				if err != nil {
					return "", err
				}
				perGoal := make(map[int64][]store.Milestone, len(goals))
				var allMIDs []int64
				for _, g := range goals {
					ms, err := d.Store.MilestonesOfGoal(ctx, g.ID)
					if err != nil {
						return "", err
					}
					perGoal[g.ID] = ms
					for _, m := range ms {
						allMIDs = append(allMIDs, m.ID)
					}
				}
				mtc, err := d.Store.MilestoneTaskCounts(ctx, allMIDs)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, g := range goals {
					renderGoalHeader(&b, &g, gmc[g.ID], d.TZ)
					for _, m := range perGoal[g.ID] {
						renderMilestoneLine(&b, &m, mtc[m.ID], d.TZ)
					}
				}
				return b.String(), nil
			}),

		tool("get_goal_detail",
			"查看单个目标详情：里程碑列表 + 每个里程碑的任务进度计数（不下钻任务细节，战略对齐视图，人人可查）。",
			obj(map[string]any{
				"goal_id": p("integer", "目标ID"),
			}, "goal_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					GoalID int64 `json:"goal_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, err := d.Store.GoalByID(ctx, args.GoalID)
				if err != nil {
					return fmt.Sprintf("目标 %d 不存在", args.GoalID), nil
				}
				ms, err := d.Store.MilestonesOfGoal(ctx, g.ID)
				if err != nil {
					return "", err
				}
				mIDs := make([]int64, len(ms))
				for i, m := range ms {
					mIDs[i] = m.ID
				}
				mtc, err := d.Store.MilestoneTaskCounts(ctx, mIDs)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "目标「%s」（%s）%s", g.Title, internalRef("目标", g.ID), g.Status)
				if g.Deadline != nil {
					fmt.Fprintf(&b, "，截止 %s", fmtTime(*g.Deadline, d.TZ))
				}
				b.WriteByte('\n')
				if strings.TrimSpace(g.Description) != "" {
					fmt.Fprintf(&b, "说明：%s\n", g.Description)
				}
				if len(ms) == 0 {
					b.WriteString("（尚无里程碑，用 add_milestone 拆解。）\n")
				} else {
					b.WriteString("里程碑：\n")
					for _, m := range ms {
						renderMilestoneLine(&b, &m, mtc[m.ID], d.TZ)
					}
				}
				return b.String(), nil
			}),

		tool("get_milestone_detail",
			"查看里程碑详情及其下任务列表（任务级下钻，涉执行分配，限所属目标 owner 或超管）。",
			obj(map[string]any{
				"milestone_id": p("integer", "里程碑ID"),
			}, "milestone_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					MilestoneID int64 `json:"milestone_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				m, g, err := loadMilestoneWithGoal(ctx, d.Store, args.MilestoneID)
				if err != nil {
					return fmt.Sprintf("里程碑 %d 不存在", args.MilestoneID), nil
				}
				if g.OwnerID != u.ID && !u.IsSuperadmin {
					return "只有目标 owner 或超管能看里程碑的任务细节。", nil
				}
				ts, err := d.Store.TasksOfMilestone(ctx, m.ID)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "里程碑「%s」（%s）%s，属目标「%s」\n", m.Title, internalRef("里程碑", m.ID), m.Status, g.Title)
				if strings.TrimSpace(m.Description) != "" {
					fmt.Fprintf(&b, "说明：%s\n", m.Description)
				}
				if len(ts) == 0 {
					b.WriteString("（尚无任务，用 decompose_milestone 拆解。）\n")
				} else {
					b.WriteString(renderTasks(ts, d.TZ))
				}
				return b.String(), nil
			}),
	}
}

// loadMilestoneWithGoal 取里程碑及其所属目标（写/关/看详情都要校验目标 owner）。
func loadMilestoneWithGoal(ctx context.Context, s *store.Store, milestoneID int64) (*store.Milestone, *store.Goal, error) {
	m, err := s.MilestoneByID(ctx, milestoneID)
	if err != nil {
		return nil, nil, err
	}
	g, err := s.GoalByID(ctx, m.GoalID)
	if err != nil {
		return nil, nil, err
	}
	return m, g, nil
}

func renderGoalHeader(b *strings.Builder, g *store.Goal, gmc store.GoalMilestoneCounts, tz *time.Location) {
	fmt.Fprintf(b, "目标「%s」（%s）%s", g.Title, internalRef("目标", g.ID), g.Status)
	if g.Deadline != nil {
		fmt.Fprintf(b, "，截止 %s", fmtTime(*g.Deadline, tz))
	}
	if gmc.Total > 0 {
		fmt.Fprintf(b, "，里程碑 %d/%d 达成", gmc.Achieved, gmc.Total)
	}
	b.WriteByte('\n')
}

func renderMilestoneLine(b *strings.Builder, m *store.Milestone, mp store.MilestoneProgress, tz *time.Location) {
	fmt.Fprintf(b, "  - %s（%s）%s", m.Title, internalRef("里程碑", m.ID), m.Status)
	if mp.Total > 0 {
		fmt.Fprintf(b, "：任务 %d/%d accepted", mp.Accepted, mp.Total)
	}
	b.WriteByte('\n')
}

// buildGoalClosedDetail 拼目标达成事件详情（自包含，供事件 AI 轮次判断是否复盘）。
// 失败的进度聚合只静默降级（事件本身仍发）——复盘价值不依赖这些计数。
func buildGoalClosedDetail(ctx context.Context, s *store.Store, g *store.Goal, closerName string) string {
	detail := fmt.Sprintf("战略目标「%s」（%s）已被 %s 标记为已达成。", g.Title, internalRef("目标", g.ID), closerName)
	if ms, err := s.MilestonesOfGoal(ctx, g.ID); err == nil && len(ms) > 0 {
		if gmc, err := s.GoalMilestoneCounts(ctx, []int64{g.ID}); err == nil {
			if c := gmc[g.ID]; c.Total > 0 {
				detail += fmt.Sprintf(" 里程碑 %d/%d 达成。", c.Achieved, c.Total)
			}
		}
	}
	detail += "值得复盘：达成原因、关键经验、相关成员表现。如确有沉淀价值，用 save_knowledge 存入知识库；否则回复「跳过」。"
	return detail
}

// buildMilestoneClosedDetail 拼里程碑达成事件详情。
func buildMilestoneClosedDetail(ctx context.Context, s *store.Store, m *store.Milestone, g *store.Goal, closerName string) string {
	detail := fmt.Sprintf("里程碑「%s」（%s，属目标「%s」）已被 %s 标记为已达成。",
		m.Title, internalRef("里程碑", m.ID), g.Title, closerName)
	if mtc, err := s.MilestoneTaskCounts(ctx, []int64{m.ID}); err == nil {
		if p := mtc[m.ID]; p.Total > 0 {
			detail += fmt.Sprintf(" 任务 %d/%d accepted。", p.Accepted, p.Total)
		}
	}
	detail += "值得复盘：节点经验、踩过的坑。如确有沉淀价值，用 save_knowledge 存入知识库；否则回复「跳过」。"
	return detail
}

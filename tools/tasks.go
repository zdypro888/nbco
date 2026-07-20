package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

// taskTools 项目与任务（用户视角 + 管理视角）。
func taskTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		// --- 我的视角 ---
		tool("get_my_projects", "查看我参与的项目（有我任务的项目）。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ps, err := d.Store.ProjectsOfAssignee(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return renderProjects(ps), nil
			}),

		tool("get_my_tasks", "查看当前用户作为执行人的普通工作任务（待处理+进行中），不包含定时提醒、计划推送或持久自动化；后者使用 list_schedules。只代表个人执行范围，不能据此判断全公司、系统或项目是否空闲。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ts, err := d.Store.TasksOfAssignee(ctx, u.ID, true)
				if err != nil {
					return "", err
				}
				return renderTasks(ts, d.TZ), nil
			}),

		tool("get_my_all_tasks", "查看当前用户相关的所有普通工作任务（含负责、协作、验收、关注以及已完成/已拆分任务），不包含定时提醒、计划推送或持久自动化；后者使用 list_schedules。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ts, err := d.Store.TasksOfAssignee(ctx, u.ID, false)
				if err != nil {
					return "", err
				}
				return renderTasks(ts, d.TZ), nil
			}),

		tool("get_task_detail", "查看任务详情（含责任人、参与者、描述、验收标准、清单、进度日志、附件）。需要是任务相关人。",
			obj(map[string]any{"task_id": p("integer", "任务ID")}, "task_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID int64 `json:"task_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanView {
					return "你不是该任务的相关人。", nil
				}
				return renderTaskDetail(ctx, d, t)
			}),

		tool("view_my_task_tree", "查看我的某个任务的完整拆分树。",
			obj(map[string]any{"task_id": p("integer", "任务ID")}, "task_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID int64 `json:"task_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanView {
					return "你不是该任务的相关人。", nil
				}
				var b strings.Builder
				if err := renderTree(ctx, d.Store, t, 0, &b, d.TZ); err != nil {
					return "", err
				}
				return b.String(), nil
			}),

		tool("update_my_task_status", "更新我负责或协作的任务状态。status: pending/in_progress/done。done=提交结果，系统按该任务的完成策略进入验收或归档。",
			obj(map[string]any{
				"task_id": p("integer", "任务ID"),
				"status":  enumP("任务状态", store.TaskPending, store.TaskInProgress, store.TaskDone),
			}, "task_id", "status"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID int64  `json:"task_id"`
					Status string `json:"status"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				switch args.Status {
				case store.TaskPending, store.TaskInProgress, store.TaskDone:
				default:
					return "status 必须是 pending/in_progress/done。", nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanContribute {
					return "只有任务责任人或协作者能更新状态。", nil
				}
				if t.Status == store.TaskSplit {
					return "该任务已拆分，状态由子任务决定。", nil
				}
				if t.Status == store.TaskAccepted {
					return "任务已验收通过，状态不可再改。", nil
				}
				if t.Status == store.TaskCancelled {
					return "任务已取消，状态不可再改。", nil
				}
				if args.Status != store.TaskDone {
					if _, err := d.Store.UpdateTaskStatus(ctx, t.ID, args.Status); err != nil {
						return "", err
					}
					if err := d.Store.AddProgress(ctx, t.ID, u.ID, "📌 状态更新为 "+args.Status+"。"); err != nil {
						return "", err
					}
					return "已更新为 " + args.Status + "。", nil
				}
				// 提交完成：状态机按持久化完成策略进入 accepted 或 done。
				t2, chain, err := d.Store.SubmitTaskBy(ctx, t.ID, u.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "任务当前状态不允许提交。", nil
					}
					return "", err
				}
				if t2.Status == store.TaskDone {
					if err := d.Store.AddProgress(ctx, t.ID, u.ID, "✅ 已提交，等待分配者验收。"); err != nil {
						return "", err
					}
					// 任务提交待验收交分配者的 AI 分析（与 worker HTTP 提交路径合一）：
					// AI 结合会话上下文给出验收建议再通知，而非死板模板。
					emitRequiredEvent(d, "任务提交待验收", t.AssignerID,
						fmt.Sprintf("「%s」提交了任务「%s」（%s）待你验收。", u.Name, t.Title, internalRef("任务", t.ID)))
					reviewers, _ := d.Store.TaskParticipantIDs(ctx, t.ID, store.TaskParticipantReviewer)
					for _, reviewerID := range reviewers {
						if reviewerID != t.AssignerID {
							emitRequiredEvent(d, "任务提交待验收", reviewerID,
								fmt.Sprintf("「%s」提交了任务「%s」（%s），你是指定验收人。", u.Name, t.Title, internalRef("任务", t.ID)))
						}
					}
					return "已提交，等待分配者验收。", nil
				}
				if err := d.Store.AddProgress(ctx, t.ID, u.ID, "✅ 已完成，按任务完成策略归档。"); err != nil {
					return "", err
				}
				notifyChain(ctx, d, u, chain)
				return "已完成，任务已按完成策略归档。", nil
			}),

		tool("get_review_queue", "查看我分配出去、已提交待我验收的任务。只代表“需要我验收”的范围。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ts, err := d.Store.TasksAwaitingReview(ctx, u.ID)
				if err != nil {
					return "", err
				}
				if len(ts) == 0 {
					return "（没有待验收的任务）", nil
				}
				return renderTasks(ts, d.TZ), nil
			}),

		tool("accept_task", "验收通过我分配的任务。子任务全部通过时上级任务自动进入待验收。",
			obj(map[string]any{
				"task_id": p("integer", "任务ID"),
				"comment": p("string", "验收评语（可选，写入进度记录）"),
			}, "task_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID  int64  `json:"task_id"`
					Comment string `json:"comment"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanReview {
					return "只有任务分配者或指定验收人能验收。", nil
				}
				_, chain, err := d.Store.AcceptTask(ctx, t.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "任务不在待验收状态。", nil
					}
					return "", err
				}
				// 依赖编排：本次验收（含级联的父任务）可能让下游任务全部前置就绪。
				FireReadyDependents(ctx, d, t.ID)
				for _, c := range chain {
					FireReadyDependents(ctx, d, c.ID)
				}
				msg := "✅ 验收通过。"
				if c := strings.TrimSpace(args.Comment); c != "" {
					msg = "✅ 验收通过：" + c
				}
				if err := d.Store.AddProgress(ctx, t.ID, u.ID, msg); err != nil {
					return "", err
				}
				recordTaskOutcome(ctx, d, t, u.ID, store.TaskOutcomeAccepted, args.Comment)
				closeTaskDecisions(ctx, d, t.AssignerID, t.ID)
				if t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("✅ 你的任务「%s」（%s）验收通过。", t.Title, internalRef("任务", t.ID)))
				}
				notifyTaskParticipants(ctx, d, t, u.ID,
					fmt.Sprintf("✅ 任务「%s」（%s）已验收通过。", t.Title, internalRef("任务", t.ID)))
				notifyChain(ctx, d, u, chain)
				return "已验收通过。", nil
			}),

		tool("reject_task", "验收打回我分配的任务（回到进行中），必须给出理由。",
			obj(map[string]any{
				"task_id": p("integer", "任务ID"),
				"reason":  p("string", "打回理由"),
			}, "task_id", "reason"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID int64  `json:"task_id"`
					Reason string `json:"reason"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Reason) == "" {
					return "打回必须给出理由。", nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanReview {
					return "只有任务分配者或指定验收人能验收。", nil
				}
				rejected, err := d.Store.RejectTask(ctx, t.ID, u.ID, args.Reason)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "任务不在待验收状态。", nil
					}
					return "", err
				}
				recordTaskOutcome(ctx, d, t, u.ID, store.TaskOutcomeRejected, args.Reason)
				if t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("🔁 任务「%s」（%s）验收未通过：%s\n请修改后重新提交。", t.Title, internalRef("任务", t.ID), args.Reason))
				}
				notifyTaskParticipants(ctx, d, t, u.ID,
					fmt.Sprintf("🔁 任务「%s」（%s）验收未通过：%s", t.Title, internalRef("任务", t.ID), args.Reason))
				closeTaskDecisions(ctx, d, t.AssignerID, t.ID)
				// 执行人是 worker：RejectTask 已在同一事务中回到 pending，打回
				// 理由会随任务历史进入下一轮 prompt；这里只负责实时唤醒。
				if au, uerr := d.Store.UserByID(ctx, t.AssigneeID); uerr == nil && au.IsWorker {
					wakeWorker(d, au)
					return "已打回；AI 员工将重新领取并按打回理由返工。", nil
				}
				return "已打回，任务回到" + rejected.Status + "。", nil
			}),

		tool("save_checklist", "保存任务的工作清单（整体替换）。根据任务描述归纳生成。需要是任务责任人或协作者。",
			obj(map[string]any{
				"task_id": p("integer", "任务ID"),
				"items":   arr("string", "清单条目"),
			}, "task_id", "items"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID int64    `json:"task_id"`
					Items  []string `json:"items"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanContribute {
					return "只有任务责任人或协作者能编辑清单。", nil
				}
				if err := d.Store.ReplaceChecklist(ctx, t.ID, args.Items); err != nil {
					return "", err
				}
				return fmt.Sprintf("已保存 %d 条清单。", len(args.Items)), nil
			}),

		tool("toggle_checklist", "勾选或取消勾选清单条目（按序号，从1开始）。",
			obj(map[string]any{
				"task_id":  p("integer", "任务ID"),
				"position": p("integer", "条目序号（从1开始）"),
				"done":     p("boolean", "是否完成"),
			}, "task_id", "position", "done"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID   int64 `json:"task_id"`
					Position int   `json:"position"`
					Done     bool  `json:"done"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanContribute {
					return "只有任务责任人或协作者能勾选清单。", nil
				}
				if err := d.Store.ToggleChecklist(ctx, t.ID, args.Position-1, args.Done); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "清单条目不存在。", nil
					}
					return "", err
				}
				return "已更新。", nil
			}),

		tool("add_progress", "给任务添加进度记录（把用户汇报总结后写入）。需要是任务责任人或协作者。",
			obj(map[string]any{
				"task_id": p("integer", "任务ID"),
				"content": p("string", "进度内容"),
			}, "task_id", "content"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID  int64  `json:"task_id"`
					Content string `json:"content"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanContribute {
					return "只有任务责任人或协作者能记录进度。", nil
				}
				if err := d.Store.AddProgress(ctx, t.ID, u.ID, args.Content); err != nil {
					return "", err
				}
				return "已记录。", nil
			}),

		tool("attach_to_task", "给任务附加文件。优先传 file_id（系统真实文件ID）；也兼容旧 file_ref（如 Telegram file_id）。需要是任务责任人、协作者或分配者。",
			obj(map[string]any{
				"task_id":  p("integer", "任务ID"),
				"file_id":  p("integer", "系统文件ID（/api/files 上传返回的 id，可选）"),
				"file_ref": p("string", "旧文件引用（如 Telegram file_id，可选）"),
				"caption":  p("string", "说明（可选）"),
			}, "task_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID  int64  `json:"task_id"`
					FileID  int64  `json:"file_id"`
					FileRef string `json:"file_ref"`
					Caption string `json:"caption"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, t, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanContribute && !access.CanManage {
					return "你不能给该任务附加文件。", nil
				}
				if args.FileID > 0 {
					ok, err := d.Store.UserCanAccessFile(ctx, u.ID, u.IsSuperadmin, args.FileID)
					if err != nil {
						return "", err
					}
					if !ok {
						return "你无权访问这个文件。", nil
					}
					inserted, err := d.Store.AddTaskAttachmentFileOnce(ctx, t.ID, args.FileID, args.Caption)
					if err != nil {
						return "", err
					}
					if !inserted {
						return "该文件已经附加到任务，无需重复操作。", nil
					}
					if err := d.Store.AddProgress(ctx, t.ID, u.ID, "📎 已附加文件。"); err != nil {
						return "", err
					}
					return "已附加文件。", nil
				}
				args.FileRef = strings.TrimSpace(args.FileRef)
				if args.FileRef == "" {
					return "file_id 或 file_ref 至少填写一个。", nil
				}
				inserted, err := d.Store.AddAttachmentOnce(ctx, store.Attachment{
					TaskID: t.ID, Kind: "file", FileRef: args.FileRef, Caption: args.Caption,
				})
				if err != nil {
					return "", err
				}
				if !inserted {
					return "该文件引用已经附加到任务，无需重复操作。", nil
				}
				if err := d.Store.AddProgress(ctx, t.ID, u.ID, "📎 已附加文件。"); err != nil {
					return "", err
				}
				return "已附加。", nil
			}),

		tool("split_my_task", "拆分我的任务并分配给他人（也可分给自己）。原任务标记为已拆分，执行转移到子任务。用于复杂、需多人并行或有依赖的任务；单一明确任务直接 assign_task。",
			obj(map[string]any{
				"task_id": p("integer", "要拆分的任务ID"),
				"subtasks": map[string]any{
					"type": "array", "description": "子任务列表",
					"items": obj(map[string]any{
						"assignee_id": p("integer", "执行人用户ID"),
						"title":       p("string", "标题"),
						"goal":        p("string", "为什么做（可选）"),
						"description": p("string", "做什么"),
						"acceptance":  p("string", "验收标准（可选）"),
						"deadline":    p("string", "截止时间 ISO8601（可选）"),
					}, "assignee_id", "title", "description"),
				},
			}, "task_id", "subtasks"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID   int64 `json:"task_id"`
					Subtasks []struct {
						AssigneeID  int64  `json:"assignee_id"`
						Title       string `json:"title"`
						Goal        string `json:"goal"`
						Description string `json:"description"`
						Acceptance  string `json:"acceptance"`
						Deadline    string `json:"deadline"`
					} `json:"subtasks"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if len(args.Subtasks) == 0 {
					return "至少要有一个子任务。", nil
				}
				parent, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				if parent.AssigneeID != u.ID {
					return "只能拆分分配给你的任务。", nil
				}
				// 拆分即派活：子任务派给别人同样需要对其的 create_project 权限，
				// 否则任何接过任务的人都能绕过派活权限把活转给任意用户/他人 worker。
				var grants []store.Grant
				if !u.IsSuperadmin {
					if grants, err = d.Store.PermsOf(ctx, u.ID); err != nil {
						return "", err
					}
				}
				subs := make([]*store.Task, 0, len(args.Subtasks))
				assignees := make(map[int64]*store.User, len(args.Subtasks))
				for _, st := range args.Subtasks {
					au, err := mustUser(ctx, d.Store, st.AssigneeID)
					if err != nil {
						return err.Error(), nil
					}
					if !u.IsSuperadmin && au.ID != u.ID && !perm.CheckActive(grants, perm.ActCreateProject, au.ID) {
						return fmt.Sprintf("你没有对 %s 的 create_project 权限，不能把子任务派给对方（可拆给自己）。", au.Name), nil
					}
					assignees[au.ID] = au
					deadline, derr := parseDeadline(st.Deadline, d.TZ)
					if derr != nil {
						return derr.Error(), nil
					}
					subs = append(subs, &store.Task{
						ProjectID: parent.ProjectID, AssignerID: u.ID, AssigneeID: st.AssigneeID,
						Title: st.Title, Goal: st.Goal, Description: st.Description,
						Acceptance: st.Acceptance, Deadline: deadline, MilestoneID: parent.MilestoneID,
					})
				}
				created, err := d.Store.SplitTask(ctx, parent.ID, subs)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "任务当前状态不允许拆分（可能已完成或已拆分）。", nil
					}
					return "", err
				}
				// 权限继承：子任务执行人继承拆分者的 view_self_intro 范围（超管 → _all）。
				inheritViewPerms(ctx, d, u, created)
				// 通知各执行人；worker 执行人推实时唤醒。
				for _, t := range created {
					if t.AssigneeID != u.ID {
						notifyQuiet(ctx, d, t.AssigneeID,
							fmt.Sprintf("📌 %s 给你分配了任务「%s」（%s）\n%s", u.Name, t.Title, internalRef("任务", t.ID), t.Description))
					}
					wakeWorker(d, assignees[t.AssigneeID])
				}
				var b strings.Builder
				fmt.Fprintf(&b, "已拆分为 %d 个子任务：\n", len(created))
				for _, t := range created {
					name := userName(ctx, d.Store, t.AssigneeID)
					if au := assignees[t.AssigneeID]; au != nil {
						name = au.Name
					}
					fmt.Fprintf(&b, "- %s：%s → %s\n", internalRef("任务", t.ID), t.Title, name)
				}
				return b.String(), nil
			}),

		tool("get_assigned_tasks", "查看我分配出去的任务及其进度。只代表“由我分配”的范围，不能据此判断全公司/系统/项目是否空闲。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ts, err := d.Store.TasksOfAssigner(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return renderTasks(ts, d.TZ), nil
			}),

		tool("update_assigned_task", "修改我分配出去的任务的目标/描述/验收标准/截止时间。若 AI 员工正在等待补充信息，本操作会让任务恢复排队。只有分配者能改。",
			obj(map[string]any{
				"task_id":     p("integer", "任务ID"),
				"goal":        p("string", "新目标（可选）"),
				"description": p("string", "新描述（可选）"),
				"acceptance":  p("string", "新验收标准（可选）"),
				"deadline":    p("string", "新截止时间 ISO8601（可选）"),
			}, "task_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID      int64   `json:"task_id"`
					Goal        *string `json:"goal"`
					Description *string `json:"description"`
					Acceptance  *string `json:"acceptance"`
					Deadline    string  `json:"deadline"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				if t.AssignerID != u.ID {
					return "只有分配者能修改任务。", nil
				}
				if args.Goal == nil && args.Description == nil && args.Acceptance == nil && strings.TrimSpace(args.Deadline) == "" {
					return "没有提供要修改的任务内容。", nil
				}
				activeRun, _ := d.Store.ActiveWorkerRunForTask(ctx, t.ID)
				deadline, derr := parseDeadline(args.Deadline, d.TZ)
				if derr != nil {
					return derr.Error(), nil
				}
				_, err = d.Store.UpdateTaskContent(ctx, t.ID, args.Goal, args.Description, args.Acceptance, deadline)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "任务状态已经变化，只有待办、执行中或待补充信息的任务可以修改。", nil
					}
					return "", err
				}
				if au, uerr := d.Store.UserByID(ctx, t.AssigneeID); uerr == nil && au.IsWorker {
					switch t.Status {
					case store.TaskInProgress:
						_ = d.Store.AddProgress(ctx, t.ID, u.ID, "✏️ 任务要求已更新：请按最新目标/描述/验收标准重新执行。")
						if d.Workers != nil && activeRun != nil {
							d.Workers.Cancel(au.ID, activeRun.ID)
						}
						wakeWorker(d, au)
						return "已更新；AI 员工当前执行已终止，将按新要求重新领取。", nil
					case store.TaskPending:
						wakeWorker(d, au)
					case store.TaskAwaitingInput:
						_ = d.Store.AddProgress(ctx, t.ID, u.ID, "💬 已补充任务信息，AI 员工可以继续执行。")
						wakeWorker(d, au)
						return "已补充信息；AI 员工任务已恢复排队。", nil
					}
				}
				if t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("✏️ 任务「%s」（%s）的要求被 %s 更新了，请查看详情。", t.Title, internalRef("任务", t.ID), u.Name))
				}
				notifyTaskParticipants(ctx, d, t, u.ID,
					fmt.Sprintf("✏️ 任务「%s」（%s）的要求被 %s 更新了，请查看详情。", t.Title, internalRef("任务", t.ID), u.Name))
				return "已更新。", nil
			}),

		tool("set_task_participants", "整体设置任务参与者。责任人仍由 assignee_id 表示；collaborator_ids 可参与执行和提交，reviewer_ids 可验收/打回，watcher_ids 只读并接收关键通知。只有任务分配者或超管可设置；AI Worker 应使用独立子任务，不能作为参与者。",
			obj(map[string]any{
				"task_id":          p("integer", "任务ID"),
				"collaborator_ids": arr("integer", "协作者用户ID列表；空数组表示清空"),
				"reviewer_ids":     arr("integer", "验收人用户ID列表；空数组表示清空"),
				"watcher_ids":      arr("integer", "观察者用户ID列表；空数组表示清空"),
			}, "task_id", "collaborator_ids", "reviewer_ids", "watcher_ids"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID          int64   `json:"task_id"`
					CollaboratorIDs []int64 `json:"collaborator_ids"`
					ReviewerIDs     []int64 `json:"reviewer_ids"`
					WatcherIDs      []int64 `json:"watcher_ids"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				task, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, task, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanManage {
					return "只有任务分配者或超级管理员能设置参与者。", nil
				}
				inputs, _, validation, err := resolveTaskParticipantInputs(
					ctx, d, u, task.AssigneeID, args.CollaboratorIDs, args.ReviewerIDs, args.WatcherIDs)
				if err != nil {
					return "", err
				}
				if validation != "" {
					return validation, nil
				}
				before, err := d.Store.TaskParticipants(ctx, task.ID)
				if err != nil {
					return "", err
				}
				after, err := d.Store.ReplaceTaskParticipants(ctx, task.ID, u.ID, inputs)
				if err != nil {
					return "", err
				}
				inheritViewPermsForTaskPeople(ctx, d, u, task, after)
				beforeRole := map[int64]string{}
				for _, participant := range before {
					beforeRole[participant.UserID] = participant.Role
				}
				afterSet := map[int64]bool{}
				for _, participant := range after {
					afterSet[participant.UserID] = true
					if beforeRole[participant.UserID] != participant.Role {
						notifyQuiet(ctx, d, participant.UserID,
							fmt.Sprintf("📌 %s 将你设为任务「%s」（%s）的%s。", u.Name, task.Title, internalRef("任务", task.ID), taskParticipantRoleLabel(participant.Role)))
					}
				}
				for _, participant := range before {
					if !afterSet[participant.UserID] {
						notifyQuiet(ctx, d, participant.UserID,
							fmt.Sprintf("任务「%s」（%s）的参与关系已由 %s 移除。", task.Title, internalRef("任务", task.ID), u.Name))
					}
				}
				summary := renderTaskParticipantSummary(after)
				if summary == "" {
					summary = "当前没有额外参与者。"
				}
				return "已更新任务参与者。" + summary, nil
			}),

		tool("cancel_assigned_task", "取消我分配的未终态任务并保留全部历史。重复任务合并时传 superseded_by 指向保留任务，系统会重连依赖；不要用物理删除掩盖已发生的业务记录。只有任务分配者或超管可操作。",
			obj(map[string]any{
				"task_id":       p("integer", "要取消的任务ID"),
				"reason":        p("string", "取消原因，必填"),
				"superseded_by": p("integer", "替代任务ID（可选，用于合并重复任务）"),
			}, "task_id", "reason"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID       int64  `json:"task_id"`
					Reason       string `json:"reason"`
					SupersededBy int64  `json:"superseded_by"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Reason) == "" {
					return "取消任务必须给出原因。", nil
				}
				task, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				access, err := d.Store.TaskAccessForUser(ctx, task, u.ID, u.IsSuperadmin)
				if err != nil {
					return "", err
				}
				if !access.CanManage {
					return "只有任务分配者或超级管理员能取消任务。", nil
				}
				activeRun, _ := d.Store.ActiveWorkerRunForTask(ctx, task.ID)
				var replacement *int64
				if args.SupersededBy > 0 {
					replacement = &args.SupersededBy
				}
				cancelled, chain, err := d.Store.CancelTask(ctx, task.ID, args.Reason, replacement)
				if err != nil {
					switch {
					case errors.Is(err, store.ErrNotFound):
						return "任务已完成、已拆分、已取消，或当前状态不允许取消。", nil
					case errors.Is(err, store.ErrConflict):
						return "替代任务无效：必须是同项目中另一条未取消任务。", nil
					default:
						return "", err
					}
				}
				if d.Workers != nil && activeRun != nil {
					if owner, uerr := d.Store.UserByID(ctx, task.AssigneeID); uerr == nil && owner.IsWorker {
						d.Workers.Cancel(owner.ID, activeRun.ID)
					}
				}
				recipients := map[int64]bool{task.AssigneeID: true}
				participantIDs, _ := d.Store.TaskParticipantIDs(ctx, task.ID)
				for _, id := range participantIDs {
					recipients[id] = true
				}
				notice := fmt.Sprintf("任务「%s」（%s）已取消：%s", task.Title, internalRef("任务", task.ID), args.Reason)
				if replacement != nil {
					notice = fmt.Sprintf("重复任务「%s」（%s）已合并到 %s：%s", task.Title, internalRef("任务", task.ID), internalRef("任务", *replacement), args.Reason)
					if kept, kerr := d.Store.TaskByID(ctx, *replacement); kerr == nil {
						recipients[kept.AssigneeID] = true
						keptParticipantIDs, _ := d.Store.TaskParticipantIDs(ctx, kept.ID)
						for _, id := range keptParticipantIDs {
							recipients[id] = true
						}
					}
				}
				for id := range recipients {
					if id != u.ID {
						notifyQuiet(ctx, d, id, notice)
					}
				}
				closeTaskDecisions(ctx, d, task.AssignerID, task.ID)
				notifyChain(ctx, d, u, chain)
				reply := fmt.Sprintf("任务「%s」（%s）已取消，历史保留。", cancelled.Title, internalRef("任务", cancelled.ID))
				if replacement != nil {
					reply += fmt.Sprintf(" 后续统一以任务 %s 为准。", internalRef("任务", *replacement))
				}
				return reply, nil
			}),

		tool("delete_assigned_task", "删除我分配出去的任务（递归删除其子任务）。只有分配者能删。",
			obj(map[string]any{"task_id": p("integer", "任务ID")}, "task_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID int64 `json:"task_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				if t.AssignerID != u.ID {
					return "只有分配者能删除任务。", nil
				}
				activeRun, _ := d.Store.ActiveWorkerRunForTask(ctx, t.ID)
				if err := d.Store.DeleteTask(ctx, t.ID); err != nil {
					return "", err
				}
				// 执行人是 worker 且正在干这单：推实时取消，终止执行。
				if d.Workers != nil && activeRun != nil {
					if au, uerr := d.Store.UserByID(ctx, t.AssigneeID); uerr == nil && au.IsWorker {
						d.Workers.Cancel(au.ID, activeRun.ID)
					}
				}
				return "已删除。", nil
			}),

		tool("reassign_task", "把任务改派给另一个人（保留任务ID、进度历史、依赖与拆分关系）。"+
			"用于换执行人而非重开一单：旧执行人若正在跑会被实时终止；任务回到 pending，新执行人可立即领取。"+
			"assignee_id 省略时自动派给最合适的 AI 员工。只有分配者能改派；改派给某人需对该人的 create_project 权限。"+
			"若因原执行人不胜任/离线，优先用此工具而非 delete+assign——后者会销毁进度历史。",
			obj(map[string]any{
				"task_id":     p("integer", "任务ID"),
				"assignee_id": p("integer", "新执行人用户ID（可选；省略=自动派给最合适的 AI 员工）"),
				"reason":      p("string", "改派原因（可选，会记入进度记录供新执行人了解背景）"),
			}, "task_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID     int64  `json:"task_id"`
					AssigneeID int64  `json:"assignee_id"`
					Reason     string `json:"reason"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				if t.AssignerID != u.ID && !u.IsSuperadmin {
					return "只有分配者能改派任务。", nil
				}
				// 已终态（accepted）的任务改派意义不大——它已完成。split 父任务的工作由子任务承载。
				// done（已提交待验收）在验收队列里，改派会丢失验收记录——要换人应先 reject_task 打回。
				if t.Status == store.TaskAccepted {
					return "任务已验收通过，无需改派；如需重做请新建任务。", nil
				}
				if t.Status == store.TaskSplit {
					return "该任务已拆分，请改派其子任务。", nil
				}
				if t.Status == store.TaskDone {
					return "任务已提交待验收，改派会丢失验收记录；如要换人请先 reject_task 打回。", nil
				}
				oldAssigneeID := t.AssigneeID
				activeRun, _ := d.Store.ActiveWorkerRunForTask(ctx, t.ID)
				autoPickNote := ""
				if args.AssigneeID == 0 {
					picked, note, perr := pickWorkerAssignee(ctx, d, u, t.Title, t.Description, t.Acceptance)
					if perr != nil {
						return perr.Error(), nil
					}
					args.AssigneeID, autoPickNote = picked, note
				}
				if args.AssigneeID == oldAssigneeID {
					return "新执行人与当前相同，无需改派。", nil
				}
				newAssignee, err := mustUser(ctx, d.Store, args.AssigneeID)
				if err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin && args.AssigneeID != u.ID {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActCreateProject, args.AssigneeID) {
						return "你没有对该用户的 create_project 权限。", nil
					}
				}
				// 记改派背景入进度历史，供新执行人了解来龙去脉。
				note := fmt.Sprintf("🔁 %s 将任务改派给 %s。", u.Name, newAssignee.Name)
				if r := strings.TrimSpace(args.Reason); r != "" {
					note += " 原因：" + r
				}
				// 改派与旧 Worker Run 取消在同一事务内完成；旧租约随后无法再写进度或提交。
				// WebSocket Cancel 只负责尽快停掉已在跑的本地进程。
				t, err = d.Store.ReassignTaskWithProgress(ctx, t.ID, args.AssigneeID, u.ID, note)
				if err != nil {
					return "", err
				}
				// 只关 orphaned_task 决策（改派解决的正是孤儿状态）；overdue_task 可能仍有效，保留。
				closeTaskDecisionsByKind(ctx, d, t.AssignerID, t.ID, "orphaned_task")
				if oldAu, uerr := d.Store.UserByID(ctx, oldAssigneeID); uerr == nil && oldAu.IsWorker && d.Workers != nil && activeRun != nil {
					d.Workers.Cancel(oldAu.ID, activeRun.ID)
				}
				// 通知新旧双方，唤醒新执行人。
				notifyQuiet(ctx, d, oldAssigneeID,
					fmt.Sprintf("🔁 任务「%s」（%s）已由 %s 改派给 %s。", t.Title, internalRef("任务", t.ID), u.Name, newAssignee.Name))
				notifyQuiet(ctx, d, args.AssigneeID,
					fmt.Sprintf("📌 %s 把任务「%s」（%s）改派给你\n%s", u.Name, t.Title, internalRef("任务", t.ID), t.Description))
				if len(t.DependsOn) == 0 {
					wakeWorker(d, newAssignee)
				}
				reply := fmt.Sprintf("任务「%s」（%s）已改派给 %s，进度历史保留。", t.Title, internalRef("任务", t.ID), newAssignee.Name)
				if autoPickNote != "" {
					reply += autoPickNote
				}
				if len(t.DependsOn) > 0 {
					reply += fmt.Sprintf("前置任务 %v 全部验收通过后才可开工。", t.DependsOn)
				}
				return reply, nil
			}),

		// --- 管理视角 ---
		tool("create_project", "创建一个新项目。需要 create_project 权限。",
			obj(map[string]any{
				"name":        p("string", "项目名"),
				"description": p("string", "项目描述（可选）"),
			}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !hasAnyActive(grants, perm.ActCreateProject) {
						return "你没有 create_project 权限。", nil
					}
				}
				pj, err := d.Store.CreateProject(ctx, args.Name, args.Description, u.ID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("项目「%s」已创建（%s）。", pj.Name, internalRef("项目", pj.ID)), nil
			}),

		tool("list_my_projects", "查看我创建的项目。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ps, err := d.Store.ProjectsByCreator(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return renderProjects(ps), nil
			}),

		tool("assign_task", "为一个明确交付物创建一个任务并指定唯一责任人。多人共同完成同一交付物时用 collaborator_ids/reviewer_ids/watcher_ids 绑定到同一任务，绝不要为每个人复制同名任务；只有需要彼此独立产出时才传 allow_parallel=true，且应给出不同范围或验收标准。需要对所有参与人的 create_project 权限。assignee_id 省略时自动派给最合适的 AI 员工。depends_on 可指定前置任务。复杂工作可先拆成目标不同的子任务。",
			obj(map[string]any{
				"project_id":       p("integer", "项目ID"),
				"assignee_id":      p("integer", "唯一责任人用户ID（可选；省略=自动派给最合适的 AI 员工）"),
				"collaborator_ids": arr("integer", "协作者用户ID列表：可查看、写进度/清单/附件并提交同一任务"),
				"reviewer_ids":     arr("integer", "指定验收人用户ID列表：可查看并验收/打回"),
				"watcher_ids":      arr("integer", "观察者用户ID列表：只读并接收关键通知"),
				"allow_parallel":   p("boolean", "明确需要独立产出时才设 true；默认合并等价未终态任务"),
				"title":            p("string", "标题"),
				"goal":             p("string", "为什么做（可选）"),
				"description":      p("string", "做什么"),
				"acceptance":       p("string", "验收标准（可选）"),
				"deadline":         p("string", "截止时间 ISO8601（可选）"),
				"priority":         enumP("优先级，可选", "low", "normal", "high"),
				"depends_on":       arr("integer", "前置任务ID列表（可选；须为已存在的任务）"),
			}, "project_id", "title", "description"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ProjectID       int64   `json:"project_id"`
					AssigneeID      int64   `json:"assignee_id"`
					CollaboratorIDs []int64 `json:"collaborator_ids"`
					ReviewerIDs     []int64 `json:"reviewer_ids"`
					WatcherIDs      []int64 `json:"watcher_ids"`
					AllowParallel   bool    `json:"allow_parallel"`
					Title           string  `json:"title"`
					Goal            string  `json:"goal"`
					Description     string  `json:"description"`
					Acceptance      string  `json:"acceptance"`
					Deadline        string  `json:"deadline"`
					Priority        string  `json:"priority"`
					DependsOn       []int64 `json:"depends_on"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				pj, err := d.Store.ProjectByID(ctx, args.ProjectID)
				if err != nil {
					return fmt.Sprintf("项目 %d 不存在", args.ProjectID), nil
				}
				if pj.CreatorID != u.ID && !u.IsSuperadmin {
					return "只有项目创建者能在项目中派任务。", nil
				}
				if pj.Status != store.ProjectActive {
					return "项目已归档，不能再派任务。", nil
				}
				// 依赖校验：只能指向本项目内已存在的任务（新任务 id 恒大于依赖 →
				// 天然无环；限同项目防跨项目耦合与任务 ID 探测）。
				for _, dep := range args.DependsOn {
					dt, err := d.Store.TaskByID(ctx, dep)
					if err != nil {
						return fmt.Sprintf("前置任务 %d 不存在。", dep), nil
					}
					if dt.ProjectID != args.ProjectID {
						return fmt.Sprintf("前置任务 %d 不在本项目内，依赖只能指向同项目任务。", dep), nil
					}
				}
				autoPickNote := ""
				if args.AssigneeID == 0 {
					picked, note, perr := pickWorkerAssignee(ctx, d, u, args.Title, args.Description, args.Acceptance)
					if perr != nil {
						return perr.Error(), nil
					}
					args.AssigneeID, autoPickNote = picked, note
				}
				assignee, err := mustUser(ctx, d.Store, args.AssigneeID)
				if err != nil {
					return err.Error(), nil
				}
				participantInputs, participantUsers, validation, err := resolveTaskParticipantInputs(
					ctx, d, u, args.AssigneeID, args.CollaboratorIDs, args.ReviewerIDs, args.WatcherIDs)
				if err != nil {
					return "", err
				}
				if validation != "" {
					return validation, nil
				}
				deadline, derr := parseDeadline(args.Deadline, d.TZ)
				if derr != nil {
					return derr.Error(), nil
				}
				created, err := d.Store.CreateOrMergeTask(ctx, &store.Task{
					ProjectID: pj.ID, AssignerID: u.ID, AssigneeID: args.AssigneeID,
					Title: args.Title, Goal: args.Goal, Description: args.Description,
					Acceptance: args.Acceptance, Priority: args.Priority, Deadline: deadline,
					DependsOn: args.DependsOn,
				}, participantInputs, u.ID, args.AllowParallel)
				if err != nil {
					switch {
					case errors.Is(err, store.ErrWorkerTaskParticipant):
						return "AI Worker 不能作为共享任务协作者；需要多个 Worker 独立产出时请拆成范围不同的子任务。", nil
					case errors.Is(err, store.ErrConflict):
						return "发现等价的未终态任务，但本次执行约束与现有任务冲突；请明确更新现有任务，或仅在确需独立交付时设置 allow_parallel=true。", nil
					}
					return "", err
				}
				t := created.Task
				inheritViewPermsForTaskPeople(ctx, d, u, t, created.Participants)
				if created.Created && t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("📌 %s 给你分配了任务「%s」（%s）\n%s", u.Name, t.Title, internalRef("任务", t.ID), t.Description))
				}
				for _, participant := range created.ChangedParticipants {
					label := taskParticipantRoleLabel(participant.Role)
					notifyQuiet(ctx, d, participant.UserID,
						fmt.Sprintf("📌 %s 将你设为任务「%s」（%s）的%s。", u.Name, t.Title, internalRef("任务", t.ID), label))
				}
				if !created.Created && t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("任务「%s」（%s）合并了等价派发，参与人员已更新，请查看任务详情。", t.Title, internalRef("任务", t.ID)))
				}
				if created.Created && len(t.DependsOn) == 0 {
					wakeWorker(d, assignee) // 有前置的任务此刻还领不了，就绪时再唤醒
				}
				ownerName := userName(ctx, d.Store, t.AssigneeID)
				if owner := participantUsers[t.AssigneeID]; owner != nil {
					ownerName = owner.Name
				}
				reply := fmt.Sprintf("任务「%s」已创建（%s），责任人 %s。", t.Title, internalRef("任务", t.ID), ownerName)
				if !created.Created {
					reply = fmt.Sprintf("发现等价的未终态任务「%s」（%s），没有重复创建；已合并本次参与人和执行约束。责任人 %s。", t.Title, internalRef("任务", t.ID), ownerName)
					if len(created.UpdatedFields) > 0 {
						reply += " 已更新字段：" + strings.Join(created.UpdatedFields, "、") + "。"
					}
				}
				if people := renderTaskParticipantSummary(created.Participants); people != "" {
					reply += " " + people
				}
				if autoPickNote != "" {
					reply += autoPickNote
				}
				if len(t.DependsOn) > 0 {
					reply += fmt.Sprintf("前置任务 %v 全部验收通过后才可开工。", t.DependsOn)
				}
				return reply, nil
			}),

		tool("view_project", "查看项目的完整任务树。需要是项目创建者。",
			obj(map[string]any{"project_id": p("integer", "项目ID")}, "project_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ProjectID int64 `json:"project_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				pj, err := d.Store.ProjectByID(ctx, args.ProjectID)
				if err != nil {
					return fmt.Sprintf("项目 %d 不存在", args.ProjectID), nil
				}
				if pj.CreatorID != u.ID && !u.IsSuperadmin {
					return "只有项目创建者能查看全景。", nil
				}
				ts, err := d.Store.TasksOfProject(ctx, pj.ID)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "项目「%s」（%s）\n", pj.Name, pj.Status)
				for _, t := range ts {
					if t.ParentID == nil {
						if err := renderTree(ctx, d.Store, t, 0, &b, d.TZ); err != nil {
							return "", err
						}
					}
				}
				return b.String(), nil
			}),

		tool("archive_project", "归档项目。需要是项目创建者。",
			obj(map[string]any{"project_id": p("integer", "项目ID")}, "project_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				return setProjectStatus(ctx, d, u, raw, store.ProjectArchived)
			}),

		tool("delete_project", "删除项目及其所有任务。不可恢复，需要是项目创建者。",
			obj(map[string]any{"project_id": p("integer", "项目ID")}, "project_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ProjectID int64 `json:"project_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				pj, err := d.Store.ProjectByID(ctx, args.ProjectID)
				if err != nil {
					return fmt.Sprintf("项目 %d 不存在", args.ProjectID), nil
				}
				if pj.CreatorID != u.ID && !u.IsSuperadmin {
					return "只有项目创建者能删除项目。", nil
				}
				if err := d.Store.DeleteProject(ctx, pj.ID); err != nil {
					return "", err
				}
				return "项目及其任务已删除。", nil
			}),
	}
}

func setProjectStatus(ctx context.Context, d Deps, u *store.User, raw json.RawMessage, status string) (string, error) {
	var args struct {
		ProjectID int64 `json:"project_id"`
	}
	if err := decode(raw, &args); err != nil {
		return err.Error(), nil
	}
	pj, err := d.Store.ProjectByID(ctx, args.ProjectID)
	if err != nil {
		return fmt.Sprintf("项目 %d 不存在", args.ProjectID), nil
	}
	if pj.CreatorID != u.ID && !u.IsSuperadmin {
		return "只有项目创建者能操作。", nil
	}
	if err := d.Store.SetProjectStatus(ctx, pj.ID, status); err != nil {
		return "", err
	}
	return "已更新为 " + status + "。", nil
}

func hasAnyActive(grants []store.Grant, action string) bool {
	for _, g := range grants {
		if g.Kind == store.KindActive && g.Action == action {
			return true
		}
	}
	return false
}

func resolveTaskParticipantInputs(ctx context.Context, d Deps, assigner *store.User, ownerID int64, collaborators, reviewers, watchers []int64) ([]store.TaskParticipantInput, map[int64]*store.User, string, error) {
	people := map[int64]*store.User{}
	owner, err := mustUser(ctx, d.Store, ownerID)
	if err != nil {
		return nil, nil, err.Error(), nil
	}
	people[owner.ID] = owner
	var grants []store.Grant
	if !assigner.IsSuperadmin {
		grants, err = d.Store.PermsOf(ctx, assigner.ID)
		if err != nil {
			return nil, nil, "", err
		}
	}
	canAssign := func(targetID int64) bool {
		return assigner.IsSuperadmin || targetID == assigner.ID || perm.CheckActive(grants, perm.ActCreateProject, targetID)
	}
	if !canAssign(owner.ID) {
		return nil, nil, "你没有对责任人的 create_project 权限。", nil
	}

	type group struct {
		role string
		ids  []int64
	}
	groups := []group{
		{role: store.TaskParticipantCollaborator, ids: collaborators},
		{role: store.TaskParticipantReviewer, ids: reviewers},
		{role: store.TaskParticipantWatcher, ids: watchers},
	}
	seen := map[int64]string{}
	var inputs []store.TaskParticipantInput
	for _, group := range groups {
		for _, id := range group.ids {
			if id <= 0 || id == owner.ID || id == assigner.ID {
				continue
			}
			if prior, ok := seen[id]; ok {
				if prior != group.role {
					return nil, nil, fmt.Sprintf("同一用户不能同时作为%s和%s；请选择一个角色。", taskParticipantRoleLabel(prior), taskParticipantRoleLabel(group.role)), nil
				}
				continue
			}
			person, err := mustUser(ctx, d.Store, id)
			if err != nil {
				return nil, nil, err.Error(), nil
			}
			if person.IsWorker {
				return nil, nil, fmt.Sprintf("%s 是 AI Worker，不能作为共享任务参与者；请给 Worker 创建独立子任务。", person.Name), nil
			}
			if !canAssign(person.ID) {
				return nil, nil, fmt.Sprintf("你没有对 %s 的 create_project 权限。", person.Name), nil
			}
			seen[id] = group.role
			people[id] = person
			inputs = append(inputs, store.TaskParticipantInput{UserID: id, Role: group.role})
		}
	}
	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].Role != inputs[j].Role {
			return inputs[i].Role < inputs[j].Role
		}
		return inputs[i].UserID < inputs[j].UserID
	})
	return inputs, people, "", nil
}

func taskParticipantRoleLabel(role string) string {
	switch role {
	case store.TaskParticipantCollaborator:
		return "协作者"
	case store.TaskParticipantReviewer:
		return "验收人"
	case store.TaskParticipantWatcher:
		return "观察者"
	default:
		return "参与者"
	}
}

func renderTaskParticipantSummary(participants []store.TaskParticipant) string {
	if len(participants) == 0 {
		return ""
	}
	groups := map[string][]string{}
	for _, participant := range participants {
		groups[participant.Role] = append(groups[participant.Role], participant.UserName)
	}
	var parts []string
	for _, role := range []string{store.TaskParticipantCollaborator, store.TaskParticipantReviewer, store.TaskParticipantWatcher} {
		if names := groups[role]; len(names) > 0 {
			parts = append(parts, taskParticipantRoleLabel(role)+"："+strings.Join(names, "、"))
		}
	}
	return strings.Join(parts, "；") + "。"
}

// inheritViewPerms 派任务时的权限继承：执行人获得分配者 view_self_intro 的可见范围。
// 失败只记日志，不阻断派发。
func inheritViewPerms(ctx context.Context, d Deps, assigner *store.User, created []*store.Task) {
	ids := make([]int64, 0, len(created))
	for _, task := range created {
		if task != nil {
			ids = append(ids, task.AssigneeID)
		}
	}
	inheritViewPermsToUsers(ctx, d, assigner, ids)
}

func inheritViewPermsForTaskPeople(ctx context.Context, d Deps, assigner *store.User, task *store.Task, participants []store.TaskParticipant) {
	if task == nil {
		return
	}
	ids := []int64{task.AssigneeID}
	for _, participant := range participants {
		if participant.Role != store.TaskParticipantWatcher {
			ids = append(ids, participant.UserID)
		}
	}
	inheritViewPermsToUsers(ctx, d, assigner, ids)
}

func inheritViewPermsToUsers(ctx context.Context, d Deps, assigner *store.User, userIDs []int64) {
	grantTo := func(assigneeID int64, target string) {
		err := d.Store.GrantPerm(ctx, store.Grant{
			Kind: store.KindActive, UserID: assigneeID, Action: perm.ActViewSelfIntro,
			Target: target, GrantedBy: assigner.ID,
		})
		if err != nil && !errors.Is(err, store.ErrConflict) {
			slog.Warn("权限继承失败", "assignee", assigneeID, "err", err)
		}
	}
	var all bool
	var targets []int64
	if assigner.IsSuperadmin {
		all = true
	} else {
		grants, err := d.Store.PermsOf(ctx, assigner.ID)
		if err != nil {
			slog.Warn("权限继承读取失败", "err", err)
			return
		}
		all, targets = perm.ViewIntroTargets(grants)
	}
	seen := map[int64]bool{}
	for _, assigneeID := range userIDs {
		if assigneeID <= 0 || assigneeID == assigner.ID || seen[assigneeID] {
			continue
		}
		seen[assigneeID] = true
		if all {
			grantTo(assigneeID, store.TargetAll)
			continue
		}
		for _, tg := range targets {
			grantTo(assigneeID, fmt.Sprintf("%d", tg))
		}
	}
}

// notifyChain 验收级联改变了状态的祖先任务：
// 转入待验收（done）的通知其分配者来验收；按完成策略直接 accepted 的祖先
// 无需再发验收通知。
func notifyChain(ctx context.Context, d Deps, operator *store.User, chain []*store.Task) {
	for _, a := range chain {
		if a.Status == store.TaskDone && a.AssignerID != operator.ID {
			// 级联转入待验收同样交分配者的 AI 分析（与提交待验收一致）。
			emitRequiredEvent(d, "任务提交待验收", a.AssignerID,
				fmt.Sprintf("任务「%s」（%s）的全部子任务已验收通过，待你验收。", a.Title, internalRef("任务", a.ID)))
		}
	}
}

func notifyQuiet(ctx context.Context, d Deps, userID int64, text string) {
	if d.Notifier == nil {
		return
	}
	if err := d.Notifier.Send(ctx, userID, text); err != nil {
		slog.Warn("通知投递失败", "user", userID, "err", err)
	}
}

func notifyTaskParticipants(ctx context.Context, d Deps, task *store.Task, excludeUserID int64, text string) {
	if task == nil || d.Store == nil {
		return
	}
	participants, err := d.Store.TaskParticipants(ctx, task.ID)
	if err != nil {
		slog.Warn("读取任务参与者通知名单失败", "task", task.ID, "err", err)
		return
	}
	for _, participant := range participants {
		if participant.UserID != excludeUserID && participant.UserID != task.AssigneeID {
			notifyQuiet(ctx, d, participant.UserID, text)
		}
	}
}

func recordTaskOutcome(ctx context.Context, d Deps, t *store.Task, reviewerID int64, outcome, reason string) {
	if d.Store == nil || t == nil {
		return
	}
	kind := store.InferTaskKind(t.Title, t.Goal, t.Description, t.Acceptance, "")
	if err := d.Store.RecordTaskOutcome(ctx, store.TaskOutcomeInput{
		TaskID:     t.ID,
		AssigneeID: t.AssigneeID,
		ReviewerID: reviewerID,
		Outcome:    outcome,
		TaskKind:   kind,
		Reason:     reason,
	}); err != nil {
		slog.Warn("任务结果学习账本写入失败", "task", t.ID, "assignee", t.AssigneeID, "kind", kind, "outcome", outcome, "err", err)
	}
}

// parseDeadline 解析 ISO8601；不带时区时按配置时区解释。
func parseDeadline(s string, tz *time.Location) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, tz); err == nil {
		return &t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, tz); err == nil {
		return &t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, tz); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("时间格式无法解析: %q（请用 ISO8601，如 2026-07-05T18:00:00+08:00）", s)
}

func renderProjects(ps []store.Project) string {
	if len(ps) == 0 {
		return "（无项目）"
	}
	var b strings.Builder
	for _, pj := range ps {
		fmt.Fprintf(&b, "- %s：%s（%s）%s\n", internalRef("项目", pj.ID), pj.Name, pj.Status, pj.Description)
	}
	return b.String()
}

func renderTasks(ts []*store.Task, tz *time.Location) string {
	if len(ts) == 0 {
		return "（无任务）"
	}
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(taskLine(t, tz))
		b.WriteByte('\n')
	}
	return b.String()
}

func taskLine(t *store.Task, tz *time.Location) string {
	line := fmt.Sprintf("%s [%s] %s", internalRef("任务", t.ID), t.Status, t.Title)
	if t.Deadline != nil {
		line += " 截止 " + fmtTime(*t.Deadline, tz)
	}
	if t.Priority != "" && t.Priority != "normal" {
		line += " 优先级 " + t.Priority
	}
	return line
}

func renderTaskDetail(ctx context.Context, d Deps, t *store.Task) (string, error) {
	var b strings.Builder
	b.WriteString(taskLine(t, d.TZ))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "责任人: %s\n分配者: %s\n", userName(ctx, d.Store, t.AssigneeID), userName(ctx, d.Store, t.AssignerID))
	participants, err := d.Store.TaskParticipants(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if summary := renderTaskParticipantSummary(participants); summary != "" {
		fmt.Fprintf(&b, "参与者: %s\n", strings.TrimSuffix(summary, "。"))
	}
	if t.SubmittedBy != nil {
		fmt.Fprintf(&b, "最近提交人: %s", userName(ctx, d.Store, *t.SubmittedBy))
		if t.SubmittedAt != nil {
			fmt.Fprintf(&b, "（%s）", fmtTime(*t.SubmittedAt, d.TZ))
		}
		b.WriteByte('\n')
	}
	if t.Status == store.TaskCancelled {
		fmt.Fprintf(&b, "取消原因: %s\n", t.CancelReason)
		if t.SupersededBy != nil {
			fmt.Fprintf(&b, "替代任务: %s\n", internalRef("任务", *t.SupersededBy))
		}
	}
	if t.Goal != "" {
		fmt.Fprintf(&b, "目标: %s\n", t.Goal)
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "描述: %s\n", t.Description)
	}
	if t.Acceptance != "" {
		fmt.Fprintf(&b, "验收标准: %s\n", t.Acceptance)
	}
	if run, rerr := d.Store.LatestWorkerRunForTask(ctx, t.ID); rerr == nil {
		fmt.Fprintf(&b, "Worker 执行: %s（%s，尝试 %d 次）\n", internalRef("执行", run.ID), run.Status, run.Attempts)
		if strings.TrimSpace(run.LastError) != "" {
			fmt.Fprintf(&b, "Worker 最近错误: %s\n", run.LastError)
		}
	}
	items, err := d.Store.Checklist(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(items) > 0 {
		b.WriteString("清单:\n")
		for _, it := range items {
			mark := "☐"
			if it.Done {
				mark = "☑"
			}
			fmt.Fprintf(&b, "  %s %d. %s\n", mark, it.Position+1, it.Item)
		}
	}
	progress, err := d.Store.ProgressOf(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(progress) > 0 {
		b.WriteString("进度:\n")
		for _, pr := range progress {
			fmt.Fprintf(&b, "  [%s] %s\n", fmtTime(pr.CreatedAt, d.TZ), pr.Content)
		}
	}
	atts, err := d.Store.AttachmentsOf(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(atts) > 0 {
		fmt.Fprintf(&b, "附件: %d 个\n", len(atts))
	}
	files, err := d.Store.TaskFileAttachments(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(files) > 0 {
		b.WriteString("文件附件:\n")
		for _, f := range files {
			fmt.Fprintf(&b, "  %s：%s（%s）\n", internalRef("文件", f.ID), f.OriginalName, formatBytes(f.SizeBytes))
		}
	}
	arts, err := d.Store.TaskArtifacts(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(arts) > 0 {
		b.WriteString("交付产物:\n")
		for _, a := range arts {
			fmt.Fprintf(&b, "  %s：%s（%s）\n", internalRef("文件", a.File.ID), a.File.OriginalName, formatBytes(a.File.SizeBytes))
		}
	}
	return b.String(), nil
}

// formatBytes 转发到 textfmt.FormatBytes（跨包共享实现）。
func formatBytes(n int64) string { return textfmt.FormatBytes(n) }

func renderTree(ctx context.Context, s *store.Store, t *store.Task, depth int, b *strings.Builder, tz *time.Location) error {
	if depth > 32 {
		return nil // 防御环
	}
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString(taskLine(t, tz))
	b.WriteByte('\n')
	subs, err := s.SubTasks(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if err := renderTree(ctx, s, sub, depth+1, b, tz); err != nil {
			return err
		}
	}
	return nil
}

// pickWorkerAssignee 免指派自动选人：候选 = 自己名下（超管=全部）的在册 AI 员工，
// 综合 worker 上报能力、同类任务验收结果、当前负载与在线状态排序取首。
// 这是资源调度（同 SKIP LOCKED 一类的机制层），不是业务政策：派谁的最终决定权
// 仍在调用方——AI 想自己挑人就显式传 assignee_id。
type workerCandidate struct {
	w        *store.User
	open     int64
	accepted int64
	online   bool
	capScore int
	cap      *store.WorkerCapability
	kind     string
	outcome  *store.TaskOutcomeStats
	rank     float64
}

func pickWorkerAssignee(ctx context.Context, d Deps, u *store.User, parts ...string) (int64, string, error) {
	owner := u.ID
	if u.IsSuperadmin {
		owner = 0
	}
	ws, err := d.Store.ListWorkers(ctx, owner)
	if err != nil {
		return 0, "", err
	}
	ids := make([]int64, 0, len(ws))
	for _, w := range ws {
		ids = append(ids, w.ID)
	}
	caps, err := d.Store.WorkerCapabilities(ctx, ids)
	if err != nil {
		return 0, "", err
	}
	taskText := strings.ToLower(strings.Join(parts, "\n"))
	taskKind := store.InferTaskKind(parts...)
	var cands []workerCandidate
	for _, w := range ws {
		if w.Status != store.UserActive {
			continue
		}
		st, err := d.Store.StatsOfAssignee(ctx, w.ID)
		if err != nil {
			return 0, "", err
		}
		outcome, err := d.Store.TaskOutcomeStatsFor(ctx, w.ID, taskKind)
		if err != nil {
			return 0, "", err
		}
		online := d.Workers != nil && d.Workers.Online(w.ID)
		cap := caps[w.ID]
		c := workerCandidate{
			w:        w,
			open:     st.Open,
			accepted: st.Accepted,
			online:   online,
			cap:      cap,
			capScore: workerCapabilityScore(taskText, cap),
			kind:     taskKind,
			outcome:  outcome,
		}
		c.rank = workerDispatchRank(c)
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return 0, "", fmt.Errorf("没有可用的 AI 员工可自动指派；请显式指定 assignee_id，或先 create_worker")
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank > cands[j].rank
		}
		if cands[i].open != cands[j].open {
			return cands[i].open < cands[j].open
		}
		if cands[i].online != cands[j].online {
			return cands[i].online
		}
		return cands[i].accepted > cands[j].accepted
	})
	best := cands[0]
	note := fmt.Sprintf("（自动指派给 %s：类型 %s、在办 %d 个、历史通过 %d 个", best.w.Name, best.kind, best.open, best.accepted)
	if best.outcome != nil && best.outcome.Total() > 0 {
		note += fmt.Sprintf("、同类通过 %d/%d", best.outcome.Accepted, best.outcome.Total())
	}
	if best.capScore > 0 {
		note += fmt.Sprintf("、能力匹配 %d", best.capScore)
	}
	if best.online {
		note += "、当前在线"
	}
	if best.cap != nil && len(best.cap.Capabilities) > 0 {
		note += "、" + strings.Join(best.cap.Capabilities, "/")
	}
	note += "）"
	return best.w.ID, note, nil
}

func workerDispatchRank(c workerCandidate) float64 {
	rank := float64(c.capScore) * 20
	if c.outcome != nil {
		// Bayesian smoothing: no samples starts neutral, enough samples move the
		// worker up/down without letting one unlucky rejection dominate forever.
		total := c.outcome.Total()
		rate := float64(c.outcome.Accepted+1) / float64(total+2)
		rank += rate * 40
		if total > 0 {
			rank += min(float64(total), 10) // known workers beat unknown ties slightly
		}
	} else {
		rank += 20
	}
	rank -= float64(c.open) * 5
	if c.online {
		rank += 5
	}
	return rank
}

func workerCapabilityScore(taskText string, cap *store.WorkerCapability) int {
	if cap == nil {
		return 0
	}
	have := map[string]bool{}
	for _, c := range cap.Capabilities {
		have[strings.ToLower(c)] = true
	}
	score := 0
	addIf := func(need string, terms ...string) {
		for _, term := range terms {
			if strings.Contains(taskText, term) {
				if have[need] {
					score += 3
				} else {
					score--
				}
				return
			}
		}
	}
	addIf("code", "代码", "repo", "repository", "git", "go test", "部署", "commit", "push")
	addIf("pdf", "pdf", "合同", "制度", "文档")
	addIf("xlsx", "xlsx", "excel", "表格", "值日表")
	addIf("images", "图片", "照片", "image", "photo")
	addIf("python", "python", "脚本", "数据处理")
	addIf("go", "golang", "go语言", "go ")
	if have["interactive-pty"] {
		score++
	}
	if cap.Engine == "codex" || cap.Engine == "claude" {
		score++
	}
	return score
}

func closeTaskDecisions(ctx context.Context, d Deps, ownerID, taskID int64) {
	if _, err := d.Store.CloseDecisionsByRef(ctx, ownerID, "task", taskID); err != nil {
		slog.Warn("关闭任务决策项失败", "owner", ownerID, "task", taskID, "err", err)
	}
}

// closeTaskDecisionsByKind 只关指定 kind 的决策项。改派用：只关 orphaned_task（改派解决的
// 就是孤儿状态），不关 overdue_task——任务改派后可能仍过期，该决策仍有效。
func closeTaskDecisionsByKind(ctx context.Context, d Deps, ownerID, taskID int64, kind string) {
	if _, err := d.Store.CloseDecisionsByKindRef(ctx, ownerID, kind, "task", taskID); err != nil {
		slog.Warn("关闭任务决策项失败", "owner", ownerID, "task", taskID, "kind", kind, "err", err)
	}
}

// FireReadyDependents 任务验收通过后触发依赖编排：找出因此全部前置就绪的下游
// 任务，唤醒 worker 立即领取，并把事件交派活人的 AI 分析（要不要通知、推进什么）。
// httpapi 的 worker 自派提交路径同样调用。
func FireReadyDependents(ctx context.Context, d Deps, acceptedID int64) {
	deps, err := d.Store.ReadyDependents(ctx, acceptedID)
	if err != nil {
		slog.Warn("查询就绪下游任务失败", "task", acceptedID, "err", err)
		return
	}
	for _, t := range deps {
		assigneeName := userName(ctx, d.Store, t.AssigneeID)
		if assignee, err := d.Store.UserByID(ctx, t.AssigneeID); err == nil {
			wakeWorker(d, assignee)
		}
		emitRequiredEvent(d, "前置任务完成",
			t.AssignerID,
			fmt.Sprintf("任务「%s」（%s）的全部前置已验收通过，现在可以开工（执行人：%s）。", t.Title, internalRef("任务", t.ID), assigneeName))
	}
}

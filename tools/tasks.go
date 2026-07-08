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

		tool("get_my_tasks", "查看我的待办任务（待处理+进行中）。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ts, err := d.Store.TasksOfAssignee(ctx, u.ID, true)
				if err != nil {
					return "", err
				}
				return renderTasks(ts, d.TZ), nil
			}),

		tool("get_my_all_tasks", "查看我的所有任务（含已完成和已拆分）。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ts, err := d.Store.TasksOfAssignee(ctx, u.ID, false)
				if err != nil {
					return "", err
				}
				return renderTasks(ts, d.TZ), nil
			}),

		tool("get_task_detail", "查看任务详情（含描述、验收标准、清单、进度日志、附件）。需要是任务的执行人或分配者。",
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
				if !canSeeTask(u, t) {
					return "你不是该任务的执行人或分配者。", nil
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
				if !canSeeTask(u, t) {
					return "你不是该任务的执行人或分配者。", nil
				}
				var b strings.Builder
				if err := renderTree(ctx, d.Store, t, 0, &b, d.TZ); err != nil {
					return "", err
				}
				return b.String(), nil
			}),

		tool("update_my_task_status", "更新我的任务状态。status: pending/in_progress/done。done=提交给分配者验收（自派任务直接完成）。",
			obj(map[string]any{
				"task_id": p("integer", "任务ID"),
				"status":  p("string", "pending | in_progress | done"),
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
				if t.AssigneeID != u.ID {
					return "只有任务执行人能更新状态。", nil
				}
				if t.Status == store.TaskSplit {
					return "该任务已拆分，状态由子任务决定。", nil
				}
				if t.Status == store.TaskAccepted {
					return "任务已验收通过，状态不可再改。", nil
				}
				if args.Status != store.TaskDone {
					if _, err := d.Store.UpdateTaskStatus(ctx, t.ID, args.Status); err != nil {
						return "", err
					}
					return "已更新为 " + args.Status + "。", nil
				}
				// 提交完成：自派任务免验收直接 accepted 并向上级联；否则进入待验收。
				t2, chain, err := d.Store.SubmitTask(ctx, t.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "任务当前状态不允许提交。", nil
					}
					return "", err
				}
				if t2.Status == store.TaskDone {
					// 任务提交待验收交分配者的 AI 分析（与 worker HTTP 提交路径合一）：
					// AI 结合会话上下文给出验收建议再通知，而非死板模板。
					emitEvent(d, "任务提交待验收", t.AssignerID,
						fmt.Sprintf("「%s」提交了任务「%s」（%s）待你验收。", u.Name, t.Title, internalRef("任务", t.ID)))
					return "已提交，等待分配者验收。", nil
				}
				notifyChain(ctx, d, u, chain)
				return "已完成（自派任务免验收）。", nil
			}),

		tool("get_review_queue", "查看我分配出去、已提交待我验收的任务。", obj(nil),
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
				if t.AssignerID != u.ID && !u.IsSuperadmin {
					return "只有任务分配者能验收。", nil
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
				if c := strings.TrimSpace(args.Comment); c != "" {
					if err := d.Store.AddProgress(ctx, t.ID, u.ID, "✅ 验收通过："+c); err != nil {
						return "", err
					}
				}
				if t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("✅ 你的任务「%s」（%s）验收通过。", t.Title, internalRef("任务", t.ID)))
				}
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
				if t.AssignerID != u.ID && !u.IsSuperadmin {
					return "只有任务分配者能验收。", nil
				}
				if _, err := d.Store.RejectTask(ctx, t.ID, u.ID, args.Reason); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "任务不在待验收状态。", nil
					}
					return "", err
				}
				if t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("🔁 任务「%s」（%s）验收未通过：%s\n请修改后重新提交。", t.Title, internalRef("任务", t.ID), args.Reason))
				}
				// 执行人是 worker：回到 pending 让它重新认领返工（打回理由已在
				// 过程记录里，会随任务历史进入下一轮 prompt），并推实时唤醒。
				if au, uerr := d.Store.UserByID(ctx, t.AssigneeID); uerr == nil && au.IsWorker {
					if _, serr := d.Store.UpdateTaskStatus(ctx, t.ID, store.TaskPending); serr == nil {
						wakeWorker(d, au)
						return "已打回；AI 员工将重新领取并按打回理由返工。", nil
					}
				}
				return "已打回，任务回到进行中。", nil
			}),

		tool("save_checklist", "保存任务的工作清单（整体替换）。根据任务描述归纳生成。需要是任务执行人。",
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
				if t.AssigneeID != u.ID {
					return "只有任务执行人能编辑清单。", nil
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
				if t.AssigneeID != u.ID {
					return "只有任务执行人能勾选清单。", nil
				}
				if err := d.Store.ToggleChecklist(ctx, t.ID, args.Position-1, args.Done); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "清单条目不存在。", nil
					}
					return "", err
				}
				return "已更新。", nil
			}),

		tool("add_progress", "给任务添加进度记录（把用户汇报总结后写入）。需要是任务执行人。",
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
				if t.AssigneeID != u.ID {
					return "只有任务执行人能记录进度。", nil
				}
				if err := d.Store.AddProgress(ctx, t.ID, u.ID, args.Content); err != nil {
					return "", err
				}
				return "已记录。", nil
			}),

		tool("attach_to_task", "给任务附加文件。优先传 file_id（系统真实文件ID）；也兼容旧 file_ref（如 Telegram file_id）。需要是任务执行人或分配者。",
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
				if !canSeeTask(u, t) {
					return "你不是该任务的执行人或分配者。", nil
				}
				if args.FileID > 0 {
					ok, err := d.Store.UserCanAccessFile(ctx, u.ID, u.IsSuperadmin, args.FileID)
					if err != nil {
						return "", err
					}
					if !ok {
						return "你无权访问这个文件。", nil
					}
					if err := d.Store.AddTaskAttachmentFile(ctx, t.ID, args.FileID, args.Caption); err != nil {
						return "", err
					}
					return "已附加文件。", nil
				}
				args.FileRef = strings.TrimSpace(args.FileRef)
				if args.FileRef == "" {
					return "file_id 或 file_ref 至少填写一个。", nil
				}
				if err := d.Store.AddAttachment(ctx, store.Attachment{
					TaskID: t.ID, Kind: "file", FileRef: args.FileRef, Caption: args.Caption,
				}); err != nil {
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

		tool("get_assigned_tasks", "查看我分配出去的任务及其进度。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ts, err := d.Store.TasksOfAssigner(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return renderTasks(ts, d.TZ), nil
			}),

		tool("update_assigned_task", "修改我分配出去的任务的目标/描述/验收标准/截止时间。只有分配者能改。",
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
				deadline, derr := parseDeadline(args.Deadline, d.TZ)
				if derr != nil {
					return derr.Error(), nil
				}
				if _, err := d.Store.UpdateTaskContent(ctx, t.ID, args.Goal, args.Description, args.Acceptance, deadline); err != nil {
					return "", err
				}
				if au, uerr := d.Store.UserByID(ctx, t.AssigneeID); uerr == nil && au.IsWorker {
					switch t.Status {
					case store.TaskInProgress:
						_ = d.Store.AddProgress(ctx, t.ID, u.ID, "✏️ 任务要求已更新：请按最新目标/描述/验收标准重新执行。")
						if _, serr := d.Store.UpdateTaskStatus(ctx, t.ID, store.TaskPending); serr != nil {
							return "", serr
						}
						if d.Workers != nil {
							d.Workers.Cancel(au.ID, t.ID)
						}
						wakeWorker(d, au)
						return "已更新；AI 员工当前执行已终止，将按新要求重新领取。", nil
					case store.TaskPending:
						wakeWorker(d, au)
					}
				}
				if t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("✏️ 任务「%s」（%s）的要求被 %s 更新了，请查看详情。", t.Title, internalRef("任务", t.ID), u.Name))
				}
				return "已更新。", nil
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
				if err := d.Store.DeleteTask(ctx, t.ID); err != nil {
					return "", err
				}
				// 执行人是 worker 且正在干这单：推实时取消，终止执行。
				if d.Workers != nil {
					if au, uerr := d.Store.UserByID(ctx, t.AssigneeID); uerr == nil && au.IsWorker {
						d.Workers.Cancel(au.ID, t.ID)
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
				_ = d.Store.AddProgress(ctx, t.ID, u.ID, note)
				// 先清 claim + 换人（ReassignTask 把 worker_claim_id 置空、状态回 pending）：
				// 旧执行人的 submit/add_progress 是 claim 守卫的，claim 一空就写不进，杜绝竞态。
				// 再 Cancel 只是让在跑的进程停手——顺序与 update_assigned_task 一致。
				t, err = d.Store.ReassignTask(ctx, t.ID, args.AssigneeID)
				if err != nil {
					return "", err
				}
				if oldAu, uerr := d.Store.UserByID(ctx, oldAssigneeID); uerr == nil && oldAu.IsWorker && d.Workers != nil {
					d.Workers.Cancel(oldAu.ID, t.ID)
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

		tool("assign_task", "在项目中创建任务并分配给某人。需要对该人的 create_project 权限。assignee_id 省略时自动派给最合适的 AI 员工（负载最低、通过率优先、在线优先，仅限自己名下）。depends_on 可指定前置任务：全部验收通过前 worker 领不到本任务，用于串流水线（如开发→测试→审查）。用于单一明确任务；复杂或需多人并行的任务先 split_my_task 拆分，已提交待验收任务要深度审核用 delegate_review。",
			obj(map[string]any{
				"project_id":  p("integer", "项目ID"),
				"assignee_id": p("integer", "执行人用户ID（可选；省略=自动派给最合适的 AI 员工）"),
				"title":       p("string", "标题"),
				"goal":        p("string", "为什么做（可选）"),
				"description": p("string", "做什么"),
				"acceptance":  p("string", "验收标准（可选）"),
				"deadline":    p("string", "截止时间 ISO8601（可选）"),
				"priority":    p("string", "low/normal/high（可选）"),
				"depends_on":  arr("integer", "前置任务ID列表（可选；须为已存在的任务）"),
			}, "project_id", "title", "description"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ProjectID   int64   `json:"project_id"`
					AssigneeID  int64   `json:"assignee_id"`
					Title       string  `json:"title"`
					Goal        string  `json:"goal"`
					Description string  `json:"description"`
					Acceptance  string  `json:"acceptance"`
					Deadline    string  `json:"deadline"`
					Priority    string  `json:"priority"`
					DependsOn   []int64 `json:"depends_on"`
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
				if !u.IsSuperadmin && args.AssigneeID != u.ID {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActCreateProject, args.AssigneeID) {
						return "你没有对该用户的 create_project 权限。", nil
					}
				}
				deadline, derr := parseDeadline(args.Deadline, d.TZ)
				if derr != nil {
					return derr.Error(), nil
				}
				t, err := d.Store.CreateTask(ctx, &store.Task{
					ProjectID: pj.ID, AssignerID: u.ID, AssigneeID: args.AssigneeID,
					Title: args.Title, Goal: args.Goal, Description: args.Description,
					Acceptance: args.Acceptance, Priority: args.Priority, Deadline: deadline,
					DependsOn: args.DependsOn,
				})
				if err != nil {
					return "", err
				}
				inheritViewPerms(ctx, d, u, []*store.Task{t})
				if t.AssigneeID != u.ID {
					notifyQuiet(ctx, d, t.AssigneeID,
						fmt.Sprintf("📌 %s 给你分配了任务「%s」（%s）\n%s", u.Name, t.Title, internalRef("任务", t.ID), t.Description))
				}
				if len(t.DependsOn) == 0 {
					wakeWorker(d, assignee) // 有前置的任务此刻还领不了，就绪时再唤醒
				}
				reply := fmt.Sprintf("任务「%s」已创建（%s）并分配给 %s。", t.Title, internalRef("任务", t.ID), assignee.Name)
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

func canSeeTask(u *store.User, t *store.Task) bool {
	return u.IsSuperadmin || t.AssigneeID == u.ID || t.AssignerID == u.ID
}

func hasAnyActive(grants []store.Grant, action string) bool {
	for _, g := range grants {
		if g.Kind == store.KindActive && g.Action == action {
			return true
		}
	}
	return false
}

// inheritViewPerms 派任务时的权限继承：执行人获得分配者 view_self_intro 的可见范围。
// 失败只记日志，不阻断派发。
func inheritViewPerms(ctx context.Context, d Deps, assigner *store.User, created []*store.Task) {
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
	for _, t := range created {
		if t.AssigneeID == assigner.ID {
			continue
		}
		if all {
			grantTo(t.AssigneeID, store.TargetAll)
			continue
		}
		for _, tg := range targets {
			grantTo(t.AssigneeID, fmt.Sprintf("%d", tg))
		}
	}
}

// notifyChain 验收级联改变了状态的祖先任务：
// 转入待验收（done）的通知其分配者来验收；自动验收通过（accepted，自派任务）的
// 相关人就是级联的触发者本人，无需通知。
func notifyChain(ctx context.Context, d Deps, operator *store.User, chain []*store.Task) {
	for _, a := range chain {
		if a.Status == store.TaskDone && a.AssignerID != operator.ID {
			// 级联转入待验收同样交分配者的 AI 分析（与提交待验收一致）。
			emitEvent(d, "任务提交待验收", a.AssignerID,
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
	if t.Goal != "" {
		fmt.Fprintf(&b, "目标: %s\n", t.Goal)
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "描述: %s\n", t.Description)
	}
	if t.Acceptance != "" {
		fmt.Fprintf(&b, "验收标准: %s\n", t.Acceptance)
	}
	if t.WorkerCommand != "" {
		mode := "pipe"
		if t.WorkerCommandPTY {
			mode = "pty"
		}
		fmt.Fprintf(&b, "Worker 命令(%s): %s\n", mode, t.WorkerCommand)
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

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	units := "KMGTPE"
	for v := n / unit; v >= unit && exp < len(units)-1; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), units[exp])
}

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
// 先按任务文字匹配 worker 上报能力，再按「当前在办任务数最少 → 实时在线优先 →
// 历史验收通过数多」排序取首。
// 这是资源调度（同 SKIP LOCKED 一类的机制层），不是业务政策：派谁的最终决定权
// 仍在调用方——AI 想自己挑人就显式传 assignee_id。
func pickWorkerAssignee(ctx context.Context, d Deps, u *store.User, parts ...string) (int64, string, error) {
	owner := u.ID
	if u.IsSuperadmin {
		owner = 0
	}
	ws, err := d.Store.ListWorkers(ctx, owner)
	if err != nil {
		return 0, "", err
	}
	type cand struct {
		w        *store.User
		open     int64
		accepted int64
		online   bool
		capScore int
		cap      *store.WorkerCapability
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
	var cands []cand
	for _, w := range ws {
		if w.Status != store.UserActive {
			continue
		}
		st, err := d.Store.StatsOfAssignee(ctx, w.ID)
		if err != nil {
			return 0, "", err
		}
		online := d.Workers != nil && d.Workers.Online(w.ID)
		cap := caps[w.ID]
		cands = append(cands, cand{w: w, open: st.Open, accepted: st.Accepted, online: online, cap: cap, capScore: workerCapabilityScore(taskText, cap)})
	}
	if len(cands) == 0 {
		return 0, "", fmt.Errorf("没有可用的 AI 员工可自动指派；请显式指定 assignee_id，或先 create_worker")
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].capScore != cands[j].capScore {
			return cands[i].capScore > cands[j].capScore
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
	note := fmt.Sprintf("（自动指派给 %s：在办 %d 个、历史通过 %d 个", best.w.Name, best.open, best.accepted)
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
		emitEvent(d, "前置任务完成",
			t.AssignerID,
			fmt.Sprintf("任务「%s」（%s）的全部前置已验收通过，现在可以开工（执行人：%s）。", t.Title, internalRef("任务", t.ID), assigneeName))
	}
}

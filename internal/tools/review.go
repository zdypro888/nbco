package tools

// 审核委派：中枢是调度层，深度审核不该在对话里做，也不该压给老板——
// 把待验收任务打包成一份完整审核简报，作为新任务派给审核角色（通常是 AI 员工，
// worker 会用交互式 CLI 实地核查代码与交付），结论回流后分配者只做最终拍板。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/store"
)

// reviewBriefMaxProgress 简报里最多带多少条最近的过程记录。
const reviewBriefMaxProgress = 30

func reviewTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("delegate_review",
			"把一个待验收任务的深度审核委派出去：系统会打包任务要素、执行过程与完成汇报生成审核简报，"+
				"在同项目建一个高优先级审核任务派给审核人（推荐 AI 员工，用 list_workers 查）。"+
				"审核结论会出现在审核任务的完成汇报里，分配者据此验收或打回原任务。限原任务分配者或超管。",
			obj(map[string]any{
				"task_id":     p("integer", "待验收的任务ID"),
				"reviewer_id": p("integer", "审核人用户ID（推荐 AI 员工）"),
				"note":        p("string", "给审核人的补充要求（可选，如重点关注的方面）"),
			}, "task_id", "reviewer_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					TaskID     int64  `json:"task_id"`
					ReviewerID int64  `json:"reviewer_id"`
					Note       string `json:"note"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				t, err := d.Store.TaskByID(ctx, args.TaskID)
				if err != nil {
					return fmt.Sprintf("任务 %d 不存在", args.TaskID), nil
				}
				if t.AssignerID != u.ID && !u.IsSuperadmin {
					return "只有任务分配者能委派审核。", nil
				}
				if t.Status != store.TaskDone {
					return "任务不在待验收状态（只有已提交的任务才能委派审核）。", nil
				}
				reviewer, err := mustUser(ctx, d.Store, args.ReviewerID)
				if err != nil {
					return err.Error(), nil
				}
				if reviewer.ID == t.AssigneeID {
					return "审核人不能是任务执行人自己。", nil
				}

				executor := fmt.Sprintf("用户%d", t.AssigneeID)
				if eu, uerr := d.Store.UserByID(ctx, t.AssigneeID); uerr == nil {
					executor = eu.Name
				}
				progress, err := d.Store.ProgressOf(ctx, t.ID)
				if err != nil {
					return "", err
				}
				brief := buildReviewBrief(t, executor, progress, args.Note, d.TZ)

				rt, err := d.Store.CreateTask(ctx, &store.Task{
					ProjectID:  t.ProjectID,
					AssignerID: u.ID,
					AssigneeID: reviewer.ID,
					Title:      fmt.Sprintf("审核任务 #%d：%s", t.ID, t.Title),
					Goal:       "把关交付质量：核实交付是否真正满足验收标准，给出可执行的验收结论。",
					Description: brief,
					Acceptance: "完成汇报第一句必须是「建议通过」或「建议打回：<理由>」，并附核查依据。",
					Priority:   "high",
				})
				if err != nil {
					return "", err
				}
				if reviewer.ID != u.ID {
					notifyQuiet(ctx, d, reviewer.ID,
						fmt.Sprintf("🔍 %s 委派你审核「%s」（#%d）的交付，见审核任务 #%d。", u.Name, t.Title, t.ID, rt.ID))
				}
				wakeWorker(d, reviewer)
				return fmt.Sprintf("已委派 %s 审核任务 #%d（审核任务 #%d，高优先级）。"+
					"审核结论会随其完成汇报回来，届时你再对原任务验收或打回。", reviewer.Name, t.ID, rt.ID), nil
			}),
	}
}

// buildReviewBrief 服务端打包完整审核简报：任务要素 + 执行过程 + 核查要求。
// 中枢只发一次工具调用，重上下文由服务端组装——这正是「调度层不干深活」的分工。
func buildReviewBrief(t *store.Task, executor string, progress []store.Progress, note string, tz *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【审核委派】请以质量审核员的身份，深度核查下面这个已提交待验收的任务交付，给出验收结论。\n\n")
	fmt.Fprintf(&b, "■ 被审核任务 #%d：%s\n", t.ID, t.Title)
	if t.Goal != "" {
		fmt.Fprintf(&b, "- 目标：%s\n", t.Goal)
	}
	if t.Description != "" {
		fmt.Fprintf(&b, "- 要求：%s\n", t.Description)
	}
	if t.Acceptance != "" {
		fmt.Fprintf(&b, "- 验收标准：%s\n", t.Acceptance)
	}
	fmt.Fprintf(&b, "- 执行人：%s\n", executor)
	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "- 分配者补充要求：%s\n", note)
	}

	b.WriteString("\n■ 执行过程记录（含完成汇报，按时间序）：\n")
	if len(progress) == 0 {
		b.WriteString("（无过程记录）\n")
	}
	start := 0
	if len(progress) > reviewBriefMaxProgress {
		start = len(progress) - reviewBriefMaxProgress
		fmt.Fprintf(&b, "（共 %d 条，仅列最近 %d 条）\n", len(progress), reviewBriefMaxProgress)
	}
	for _, pr := range progress[start:] {
		fmt.Fprintf(&b, "[%s] %s\n", fmtTime(pr.CreatedAt, tz), pr.Content)
	}

	fmt.Fprintf(&b, "\n■ 工作目录：若本机存在 ~/nbco-work/task-%d/，进入实地核查（读代码改动、跑测试、逐条验证验收标准）。\n", t.ID)
	b.WriteString("\n■ 审核要求：\n")
	b.WriteString("1. 不要轻信汇报文字：能实际运行验证的必须实际验证，逐条对照验收标准。\n")
	b.WriteString("2. 检查明显缺陷：错误处理、边界条件、安全隐患、未完成的 TODO。\n")
	b.WriteString("3. 你的完成总结第一句必须是「建议通过」或「建议打回：<一句话理由>」，随后列出核查依据与发现的问题。\n")
	return b.String()
}

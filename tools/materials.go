package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

const materialLearningMarker = "NBCO_LEARNING_CANDIDATES_JSON:"

type materialAnalysisArgs struct {
	FileIDs     []int64 `json:"file_ids"`
	Instruction string  `json:"instruction"`
	WorkerID    int64   `json:"worker_id"`
	Title       string  `json:"title"`
}

func materialTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("analyze_company_materials", "把已上传到 nbco 的公司资料文件交给发起人名下的 worker 深度分析，并要求它输出结构化学习候选。适合 PDF/XLSX/TXT/照片等资料整理；nbco 会在 worker 提交后抽取候选，供入库审核/发布。简单文本信息直接保存，不必派 worker。",
			obj(map[string]any{
				"file_ids":    arr("integer", "系统文件 ID 列表（/api/files 上传或任务附件里的真实文件 ID）"),
				"instruction": p("string", "整理目标，例如：提炼公司基本信息、制度、联系人、项目背景、风险点"),
				"worker_id":   p("integer", "指定自己名下 worker，可选；不填自动选择自己名下在线/最近在线 worker"),
				"title":       p("string", "任务标题，可选"),
			}, "file_ids", "instruction"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args materialAnalysisArgs
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				return startMaterialAnalysis(ctx, d, u, args)
			}),
	}
}

func startMaterialAnalysis(ctx context.Context, d Deps, u *store.User, args materialAnalysisArgs) (string, error) {
	if d.Store == nil {
		return "资料分析工作流需要可用的存储服务；当前入口未装配 Store。", nil
	}
	if len(args.FileIDs) == 0 {
		return "file_ids 不能为空。请先通过 /api/files 上传文件，或使用已有系统文件 ID。", nil
	}
	instruction := strings.TrimSpace(args.Instruction)
	if instruction == "" {
		return "instruction 不能为空。", nil
	}
	worker, err := pickMaterialWorker(ctx, d, u, args.WorkerID)
	if err != nil {
		return err.Error(), nil
	}
	for _, id := range args.FileIDs {
		ok, err := d.Store.UserCanAccessFile(ctx, u.ID, u.IsSuperadmin, id)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("你无权访问%s。", internalRef("文件", id)), nil
		}
	}
	pj, err := d.Store.EnsureCompanyIntelligenceProject(ctx, u.ID)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = "整理公司资料"
	}
	t, err := d.Store.CreateTask(ctx, &store.Task{
		ProjectID: pj.ID, AssignerID: u.ID, AssigneeID: worker.ID,
		Title:       title,
		Goal:        "把公司资料读成可复用、可审计、可检索的 nbco 学习资产。",
		Description: materialAnalysisPrompt(instruction),
		Acceptance:  "完成汇报必须包含自然语言摘要，并在末尾输出 " + materialLearningMarker + " 后接严格 JSON。",
		Priority:    "high",
	})
	if err != nil {
		return "", err
	}
	for _, id := range args.FileIDs {
		if err := d.Store.AddTaskAttachmentFile(ctx, t.ID, id, "公司资料分析输入"); err != nil {
			return "", err
		}
	}
	wakeWorker(d, worker)
	return fmt.Sprintf("已创建资料分析任务（%s），分配给你的 worker %s（%s），已挂载 %d 个文件。worker 提交后 nbco 会抽取学习候选。", internalRef("任务", t.ID), worker.Name, internalRef("worker", worker.ID), len(args.FileIDs)), nil
}

func pickMaterialWorker(ctx context.Context, d Deps, u *store.User, workerID int64) (*store.User, error) {
	if workerID > 0 {
		w, err := d.Store.UserByID(ctx, workerID)
		if err != nil || !w.IsWorker {
			return nil, errors.New("指定的 worker 不存在")
		}
		if !u.IsSuperadmin && (w.OwnerID == nil || *w.OwnerID != u.ID) {
			return nil, errors.New("只能指定自己名下的 worker")
		}
		if w.Status != store.UserActive {
			return nil, errors.New("指定 worker 已停用")
		}
		return w, nil
	}
	// 自动选择严格按发起人 owner 维度：谁安排资料分析，就调用谁名下的
	// worker。超管也默认用自己名下 worker；确需调别人的 worker 时显式传 worker_id。
	ws, err := d.Store.ListWorkers(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	if len(ws) == 0 {
		return nil, errors.New("你名下没有可用 worker。请先 create_worker 并完成绑定")
	}
	var active []*store.User
	for _, w := range ws {
		if w.Status == store.UserActive {
			active = append(active, w)
		}
	}
	if len(active) == 0 {
		return nil, errors.New("你名下 worker 都已停用")
	}
	if d.Workers != nil {
		for _, w := range active {
			if w.IsSuperadmin && d.Workers.Online(w.ID) {
				return w, nil
			}
		}
		for _, w := range active {
			if d.Workers.Online(w.ID) {
				return w, nil
			}
		}
	}
	for _, w := range active {
		if w.IsSuperadmin {
			return w, nil
		}
	}
	return active[0], nil
}

func materialAnalysisPrompt(instruction string) string {
	return strings.TrimSpace(fmt.Sprintf(`请分析本任务附件中的公司资料。

用户目标：
%s

工作要求：
1. 下载并阅读 worker 任务提示中列出的全部附件路径；PDF、XLSX、TXT、图片都要尽力提取事实。
2. 输出一份简洁摘要：资料讲了什么、确认了哪些公司事实、有哪些疑问或冲突。
3. 只提取有长期价值的信息，不要把一次性状态、寒暄、无证据猜测写成学习候选。
4. 对每条候选保留 evidence，写明来自哪个文件/工作表/页码/图片观察；不确定时降低 confidence。
5. 不要直接改 nbco 数据库；由 nbco 中枢解析你的结构化结果后入库或进入审核。

完成汇报末尾必须输出：
%s
{
	  "knowledge": [{"title":"","content":"","tags":[],"confidence":0.8,"evidence":{"files":[],"notes":""}}],
	  "entities": [{"entity_type":"customer|project|contract|policy|contact|asset|system","name":"","content":"","file_id":0,"confidence":0.8,"evidence":{"files":[],"notes":""}}],
	  "rules": [{"title":"","content":"","scope":"global","tags":[],"confidence":0.8,"evidence":{"files":[],"notes":""}}],
  "skills": [{"title":"","trigger":"","summary":"","procedure":"","constraints":"","scope":"global","tags":[],"confidence":0.8,"evidence":{"files":[],"notes":""}}],
  "questions": [{"title":"","content":"","evidence":{"files":[],"notes":""}}]
}

JSON 必须严格合法；没有的数组给 []。`, instruction, materialLearningMarker))
}

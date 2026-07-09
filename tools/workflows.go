package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

type WorkflowTemplate struct {
	Name        string
	Title       string
	Domain      string
	Risk        string
	Description string
	Args        map[string]any
}

var workflowTemplates = []WorkflowTemplate{
	{
		Name:        "material_intake",
		Title:       "资料分析入库",
		Domain:      CapabilityWorkers,
		Risk:        "normal",
		Description: "用户上传 PDF/XLSX/TXT/图片等公司资料后，派发起人名下 worker 深度分析，产出知识、规则、skill、实体候选，进入学习审核。",
		Args: map[string]any{
			"file_ids":    "必填，系统文件 ID 数组",
			"instruction": "必填，整理目标",
			"worker_id":   "可选，指定自己名下 worker",
			"title":       "可选，任务标题",
		},
	},
	{
		Name:        "nbco_upgrade",
		Title:       "nbco 单 worker 升级",
		Domain:      CapabilityOps,
		Risk:        "high",
		Description: "超级管理员用一个 admin worker 创建一次显式命令任务，执行 scripts/upgrade-nbco.sh，完成拉取、测试、构建、重启、健康检查和自动回滚。",
		Args: map[string]any{
			"worker_id": "可选，指定 admin worker；不填自动选择可用 admin worker",
			"ref":       "可选，Git ref，默认 origin/main",
			"repo_dir":  "可选，源码目录；不填使用远端 NBCO_REPO_DIR 或 $HOME/src/nbco",
			"title":     "可选，任务标题",
			"confirm":   "必填 true，高风险升级必须显式确认",
		},
	},
}

var workflowRefRe = regexp.MustCompile(`^[A-Za-z0-9._/@-]+$`)

func workflowTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("list_workflows", "列出 nbco 已固化的标准工作流模板。遇到资料分析、代码升级、入职、群监控等标准流程时先看是否有匹配模板；没有模板时继续用底层工具或 skill 组合完成。",
			obj(map[string]any{"domain": p("string", "可选：workers/ops/...")}),
			func(_ context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Domain string `json:"domain"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				return renderWorkflowTemplates(args.Domain), nil
			}),

		tool("start_workflow", "启动一个确定性标准工作流。支持 material_intake（资料分析入库）与 nbco_upgrade（已提交版本的安全部署）。需要 AI 员工管理权限；高风险工作流还会执行自身权限校验，且必须在 args.confirm=true 后才会创建任务。可学习、可调整的执行流程应先 search_skills/load_skill，再用 start_worker_skill 派发。",
			obj(map[string]any{
				"name": p("string", "工作流名：material_intake 或 nbco_upgrade"),
				"args": map[string]any{
					"type":        "object",
					"description": "工作流参数对象；先用 list_workflows 查看每个模板需要的参数。",
				},
			}, "name", "args"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var req struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				}
				if err := decode(raw, &req); err != nil {
					return err.Error(), nil
				}
				return StartWorkflow(ctx, d, u, req.Name, req.Args)
			}),
	}
}

func ListWorkflowTemplates() []WorkflowTemplate {
	out := make([]WorkflowTemplate, len(workflowTemplates))
	for i, wf := range workflowTemplates {
		out[i] = wf
		if wf.Args != nil {
			out[i].Args = make(map[string]any, len(wf.Args))
			for k, v := range wf.Args {
				out[i].Args[k] = v
			}
		}
	}
	return out
}

func CanStartWorkflow(ctx context.Context, d Deps, u *store.User, name string) (bool, string, error) {
	if u == nil {
		return false, "未认证", nil
	}
	if u.IsSuperadmin {
		return true, "", nil
	}
	if workflowRequiresSuperadmin(name) {
		return false, strings.TrimSpace(name) + " 只能由超级管理员启动。", nil
	}
	if d.Store == nil {
		return false, "工作流权限校验需要可用的存储服务。", nil
	}
	grants, err := d.Store.PermsOf(ctx, u.ID)
	if err != nil {
		return false, "", err
	}
	if !hasAnyActive(grants, perm.ActManageWorker) {
		return false, "需要 AI 员工管理权限。", nil
	}
	return true, "", nil
}

func StartWorkflow(ctx context.Context, d Deps, u *store.User, name string, args json.RawMessage) (string, error) {
	name = strings.TrimSpace(name)
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	switch name {
	case "material_intake":
		return startMaterialWorkflow(ctx, d, u, args)
	case "nbco_upgrade":
		return startNBCOUpgradeWorkflow(ctx, d, u, args)
	default:
		return "未知工作流。请先调用 list_workflows 查看可用模板。", nil
	}
}

func workflowRequiresSuperadmin(name string) bool {
	switch strings.TrimSpace(name) {
	case "nbco_upgrade":
		return true
	default:
		return false
	}
}

func renderWorkflowTemplates(domain string) string {
	domain = strings.TrimSpace(domain)
	var b strings.Builder
	b.WriteString("标准工作流模板\n")
	found := false
	for _, wf := range workflowTemplates {
		if domain != "" && wf.Domain != domain {
			continue
		}
		found = true
		fmt.Fprintf(&b, "\n• %s（%s，风险=%s）\n", wf.Name, wf.Title, wf.Risk)
		fmt.Fprintf(&b, "  %s\n", wf.Description)
		if len(wf.Args) > 0 {
			keys := make([]string, 0, len(wf.Args))
			for k := range wf.Args {
				keys = append(keys, k)
			}
			// Deterministic enough for prompts and tests.
			for i := 0; i < len(keys)-1; i++ {
				for j := i + 1; j < len(keys); j++ {
					if keys[j] < keys[i] {
						keys[i], keys[j] = keys[j], keys[i]
					}
				}
			}
			for _, k := range keys {
				fmt.Fprintf(&b, "  - %s：%v\n", k, wf.Args[k])
			}
		}
	}
	if !found {
		return "（没有匹配的工作流模板）"
	}
	return strings.TrimSpace(b.String())
}

func startMaterialWorkflow(ctx context.Context, d Deps, u *store.User, raw json.RawMessage) (string, error) {
	var args materialAnalysisArgs
	if err := decode(raw, &args); err != nil {
		return err.Error(), nil
	}
	out, err := startMaterialAnalysis(ctx, d, u, args)
	if err != nil || strings.HasPrefix(out, "file_ids 不能为空") || strings.Contains(out, "不能为空") || strings.Contains(out, "无权") {
		return out, err
	}
	return "已启动工作流「资料分析入库」。\n" + out, nil
}

func startNBCOUpgradeWorkflow(ctx context.Context, d Deps, u *store.User, raw json.RawMessage) (string, error) {
	if u == nil || !u.IsSuperadmin {
		return "nbco_upgrade 只能由超级管理员启动。", nil
	}
	if d.Store == nil {
		return "nbco_upgrade 工作流需要可用的存储服务；当前入口未装配 Store。", nil
	}
	var args struct {
		WorkerID int64  `json:"worker_id"`
		Ref      string `json:"ref"`
		RepoDir  string `json:"repo_dir"`
		Title    string `json:"title"`
		Confirm  bool   `json:"confirm"`
	}
	if err := decode(raw, &args); err != nil {
		return err.Error(), nil
	}
	if !args.Confirm {
		return "nbco_upgrade 会重启生产服务，必须由用户明确确认。确认后再次调用 start_workflow，name=nbco_upgrade，并在 args 里设置 confirm=true。", nil
	}
	ref := strings.TrimSpace(args.Ref)
	if ref == "" {
		ref = "origin/main"
	}
	if !workflowRefRe.MatchString(ref) {
		return "ref 只能包含字母、数字、点、下划线、斜杠、@ 和短横线。", nil
	}
	w, msg, err := pickAdminWorkflowWorker(ctx, d, u, args.WorkerID)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return msg, nil
	}
	command := nbcoUpgradeCommand(args.RepoDir, ref)
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = "升级 nbco 到 " + ref
	}
	t, err := createWorkerCommandTask(ctx, d, u, w, workerCommandTaskArgs{
		Title:       title,
		Command:     command,
		PTY:         false,
		Description: "标准工作流 nbco_upgrade：在一个 worker 命令任务内执行升级脚本。脚本负责拉取目标 ref、运行测试、构建、重启、healthz 检查；失败自动回滚。不要把同一次升级拆成多个并发任务。",
		Acceptance:  "完成汇报必须包含目标版本、测试结果、部署结果、healthz 状态；失败时说明是否已回滚。",
		Priority:    "high",
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已启动工作流「nbco 单 worker 升级」：创建任务（%s）并分配给 admin worker %s。命令会以 pipe 模式执行，升级过程保持在同一个 worker 任务里。", internalRef("任务", t.ID), w.Name), nil
}

func pickAdminWorkflowWorker(ctx context.Context, d Deps, u *store.User, workerID int64) (*store.User, string, error) {
	if workerID > 0 {
		w, msg := mustOwnWorker(ctx, d, u, workerID)
		if msg != "" {
			return nil, msg, nil
		}
		if w.Status != store.UserActive {
			return nil, "目标 worker 已停用。", nil
		}
		if !w.IsSuperadmin {
			return nil, "nbco_upgrade 只能分配给 admin worker。请先用 set_worker_admin 授权可信工作机。", nil
		}
		return w, "", nil
	}
	ws, err := d.Store.ListAdminWorkers(ctx, u.ID)
	if err != nil {
		return nil, "", err
	}
	if len(ws) == 0 {
		return nil, "你名下没有可用 admin worker。请先创建/绑定自己的 worker，并用 set_worker_admin 标记为 admin worker；如需调用他人名下 admin worker，请显式传 worker_id。", nil
	}
	if d.Workers != nil {
		for _, w := range ws {
			if d.Workers.Online(w.ID) {
				return w, "", nil
			}
		}
	}
	return ws[0], "", nil
}

func nbcoUpgradeCommand(repoDir, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "origin/main"
	}
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return "cd \"${NBCO_REPO_DIR:-$HOME/src/nbco}\" && scripts/upgrade-nbco.sh " + shellQuote(ref)
	}
	return "cd " + shellQuote(repoDir) + " && scripts/upgrade-nbco.sh " + shellQuote(ref)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func workflowTemplateByName(name string) (*WorkflowTemplate, error) {
	for i := range workflowTemplates {
		if workflowTemplates[i].Name == name {
			return &workflowTemplates[i], nil
		}
	}
	return nil, errors.New("workflow not found")
}

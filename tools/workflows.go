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
	{
		Name:        "nbco_code_change",
		Title:       "nbco 代码变更与可选上线",
		Domain:      CapabilityOps,
		Risk:        "high",
		Description: "超级管理员用一个 admin worker 承接 nbco 源码开发/修复/测试/提交/可选部署。适合“给 nbco 增加某功能然后升级”这类需要先改代码再上线的请求；同一个目标保持在同一个 worker/agent session scope。",
		Args: map[string]any{
			"goal":        "必填，代码变更目标",
			"worker_id":   "可选，指定 admin worker；不填自动选择可用 admin worker",
			"repo_dir":    "可选，worker 工作机上的 nbco 源码目录；不填让 worker 自动探测",
			"repo_url":    "可选，首次 clone 用的仓库地址；找不到源码且未提供时，worker 必须回报缺少源码位置",
			"branch":      "可选，工作分支名；不填由 worker 生成安全分支名",
			"commit_push": "可选，true=测试通过后提交并 push",
			"deploy":      "可选，true=提交/push 后部署到生产；deploy=true 时 confirm 必须 true",
			"confirm":     "deploy=true 时必填 true",
			"title":       "可选，任务标题",
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

		asynchronousTool(tool("start_workflow", "启动一个确定性标准工作流。支持 material_intake（资料分析入库）、nbco_upgrade（已提交版本安全部署）、nbco_code_change（先改代码再可选上线）。需要 AI 员工管理权限；高风险工作流还会执行自身权限校验，部署必须在 args.confirm=true 后才会创建任务。",
			obj(map[string]any{
				"name": enumP("工作流名", "material_intake", "nbco_upgrade", "nbco_code_change"),
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
			})),
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
	case "nbco_code_change":
		return startNBCOCodeChangeWorkflow(ctx, d, u, args)
	default:
		return "未知工作流。请先调用 list_workflows 查看可用模板。", nil
	}
}

func workflowRequiresSuperadmin(name string) bool {
	switch strings.TrimSpace(name) {
	case "nbco_upgrade", "nbco_code_change":
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
	if err != nil || !ToolResultAccepted(out) {
		return out, err
	}
	result, _ := ParseToolResult(out)
	return asynchronousAcceptedResult("已启动工作流「资料分析入库」。\n" + result.Message), nil
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
		Description: "标准工作流 nbco_upgrade：在一个 worker 命令任务内执行升级脚本。脚本负责拉取目标 ref、运行测试、构建、重启、readyz 与版本检查；失败自动回滚。不要把同一次升级拆成多个并发任务。",
		Acceptance:  "完成汇报必须包含目标版本、测试结果、部署结果、readyz 与版本状态；失败时说明是否已回滚。",
		Priority:    "high",
		ScopeType:   "repo",
		ScopeKey:    "repo:nbco",
		ScopeTitle:  "NBCO codebase and deployment",
	})
	if err != nil {
		return "", err
	}
	return asynchronousAcceptedResult(fmt.Sprintf("已启动工作流「nbco 单 worker 升级」：创建任务（%s）并分配给 admin worker %s。命令会以 pipe 模式执行，升级过程保持在同一个 worker 任务里。", internalRef("任务", t.ID), w.Name)), nil
}

func startNBCOCodeChangeWorkflow(ctx context.Context, d Deps, u *store.User, raw json.RawMessage) (string, error) {
	if u == nil || !u.IsSuperadmin {
		return "nbco_code_change 只能由超级管理员启动。", nil
	}
	if d.Store == nil {
		return "nbco_code_change 工作流需要可用的存储服务；当前入口未装配 Store。", nil
	}
	var args struct {
		Goal       string `json:"goal"`
		WorkerID   int64  `json:"worker_id"`
		RepoDir    string `json:"repo_dir"`
		RepoURL    string `json:"repo_url"`
		Branch     string `json:"branch"`
		CommitPush bool   `json:"commit_push"`
		Deploy     bool   `json:"deploy"`
		Confirm    bool   `json:"confirm"`
		Title      string `json:"title"`
	}
	if err := decode(raw, &args); err != nil {
		return err.Error(), nil
	}
	goal := strings.TrimSpace(args.Goal)
	if goal == "" {
		return "goal 不能为空。请写清楚要修改/新增的 nbco 功能。", nil
	}
	if args.Deploy && !args.Confirm {
		return "nbco_code_change 带 deploy=true 会改动并重启生产服务，必须由用户明确确认。确认后再次调用 start_workflow，name=nbco_code_change，并在 args 里设置 confirm=true。", nil
	}
	if args.Branch != "" && !workflowRefRe.MatchString(args.Branch) {
		return "branch 只能包含字母、数字、点、下划线、斜杠、@ 和短横线。", nil
	}
	w, msg, err := pickAdminWorkflowWorker(ctx, d, u, args.WorkerID)
	if err != nil {
		return "", err
	}
	if msg != "" {
		return msg, nil
	}
	pj, err := d.Store.EnsureWorkerOperationsProject(ctx, u.ID)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		title = "修改 nbco：" + textTitle(goal, 32)
	}
	t, err := d.Store.CreateTask(ctx, &store.Task{
		ProjectID:        pj.ID,
		AssignerID:       u.ID,
		AssigneeID:       w.ID,
		Title:            title,
		Goal:             "用一个可信 admin worker 完成 nbco 代码变更、验证与可选上线。",
		Description:      nbcoCodeChangePrompt(goal, args.RepoDir, args.RepoURL, args.Branch, args.CommitPush, args.Deploy),
		Acceptance:       "完成汇报必须包含：使用/创建的 agent session scope、源码位置、变更摘要、测试命令与结果、git 状态；如果 commit_push=true，包含提交与 push 结果；如果 deploy=true，包含部署命令、readyz、版本检查和失败回滚状态。",
		Priority:         "high",
		WorkerScopeType:  "repo",
		WorkerScopeKey:   "repo:nbco",
		WorkerScopeTitle: "NBCO codebase and deployment",
	})
	if err != nil {
		return "", err
	}
	wakeWorker(d, w)
	mode := "开发/测试"
	if args.CommitPush {
		mode += "/提交推送"
	}
	if args.Deploy {
		mode += "/部署"
	}
	return asynchronousAcceptedResult(fmt.Sprintf("已启动工作流「nbco 代码变更与可选上线」：创建任务（%s）并分配给 admin worker %s。模式：%s；同一目标会在同一个 worker 任务上下文里推进。", internalRef("任务", t.ID), w.Name, mode)), nil
}

func nbcoCodeChangePrompt(goal, repoDir, repoURL, branch string, commitPush, deploy bool) string {
	repoDir = strings.TrimSpace(repoDir)
	repoURL = strings.TrimSpace(repoURL)
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = "由 worker 基于目标生成安全工作分支名"
	}
	var b strings.Builder
	b.WriteString("你是 nbco 的可信 admin worker，本任务要修改 nbco 项目本身。\n\n")
	b.WriteString("用户目标：\n")
	b.WriteString(goal)
	b.WriteString("\n\n工作约束：\n")
	b.WriteString("1. 使用 worker 支持的 Codex/Claude 交互式 PTY agent 做开发工作；不要使用 claude -p 这类非交互 pipe 模式。\n")
	b.WriteString("2. 同一个 nbco 目标保持一个 agent session scope（建议 repo:nbco / feature:当前目标）；如果已有匹配 session，优先 resume；没有再新建。\n")
	b.WriteString("3. 先找到源码。")
	switch {
	case repoDir != "":
		fmt.Fprintf(&b, "优先使用 repo_dir=%s。", repoDir)
	case repoURL != "":
		fmt.Fprintf(&b, "若本机没有源码，用 repo_url=%s clone。", repoURL)
	default:
		b.WriteString("优先探测 NBCO_REPO_DIR、$HOME/src/nbco、$HOME/nbco、当前任务附件/工作目录；如果都找不到，不要猜仓库地址，先在进度里明确请求 repo_dir 或 repo_url。")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "4. 工作分支：%s。改动前记录 git status；不要覆盖用户未提交改动。\n", branch)
	b.WriteString("5. 修改后运行匹配测试，至少覆盖被改模块；测试失败先修复，不要把失败包装成完成。\n")
	if commitPush {
		b.WriteString("6. 测试通过后提交并 push；提交信息要描述实际变更。\n")
	} else {
		b.WriteString("6. 本任务未授权 commit_push；除非后续明确授权，不要提交或 push。\n")
	}
	if deploy {
		b.WriteString("7. 已授权部署：提交/push 完成后，使用项目标准升级脚本 scripts/upgrade-nbco.sh 或当前部署文档执行上线，完成 readyz 与版本检查；失败时按脚本/文档回滚并汇报。\n")
	} else {
		b.WriteString("7. 本任务未授权部署；不要重启生产服务。\n")
	}
	b.WriteString("8. 不要泄露任何 token、API key、worker access token 或数据库密码；日志和汇报都要脱敏。\n")
	return b.String()
}

func textTitle(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
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

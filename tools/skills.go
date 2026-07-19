package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

const skillSearchLimit = 8

// skillTools 执行方法库（Skill Memory）：短摘要按需注入系统提示，完整流程由 load_skill
// 读取。创建/修改影响系统行为，限超管；搜索/加载所有员工可用。
func skillTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("search_skills", "检索可复用执行方法（Skill Memory）。用户问“这类事怎么做/以后按什么流程/你会不会记得执行方法”或当前任务明显匹配某个流程时调用。",
			obj(map[string]any{"query": p("string", "查询（自然语言或关键词皆可）")}, "query"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Query string `json:"query"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				q := strings.TrimSpace(args.Query)
				if q == "" {
					return "查询不能为空。", nil
				}
				ks, err := d.searchSkills(ctx, q, skillSearchLimit)
				if err != nil {
					return "", err
				}
				return renderSkillList(ks), nil
			}),

		tool("load_skill", "读取一条执行方法的完整内容。系统提示只会注入相关 skill 摘要；真正执行前如需步骤细节，调用本工具。",
			obj(map[string]any{"id": p("integer", "skill ID")}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				k, err := d.Store.KnowledgeByID(ctx, args.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "skill 不存在。", nil
					}
					return "", err
				}
				if k.Kind != store.KnowledgeKindSkill {
					return "该条目不是 skill。", nil
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s：%s\n", internalRef("skill", k.ID), k.Title)
				if len(k.Tags) > 0 {
					fmt.Fprintf(&b, "标签: %s\n", strings.Join(k.Tags, ", "))
				}
				fmt.Fprintf(&b, "更新于 %s\n\n%s", fmtTime(k.UpdatedAt, d.TZ), k.Content)
				return b.String(), nil
			}),

		tool("start_worker_skill", "按一条已保存的 skill 创建 worker 执行任务。适合把可复用 SOP 交给发起人名下 worker 执行；工具本身不内置具体业务流程，流程来自 skill 内容与标签。高风险/admin worker skill 必须 confirm=true。",
			obj(map[string]any{
				"skill_id":    p("integer", "要执行的 skill ID"),
				"instruction": p("string", "本次具体目标/补充要求"),
				"worker_id":   p("integer", "可选，指定自己名下 worker；超管可显式指定任意 worker"),
				"file_ids":    arr("integer", "可选，随任务挂载的系统文件 ID"),
				"title":       p("string", "任务标题，可选"),
				"confirm":     p("boolean", "高风险或 admin worker skill 必须显式 true"),
			}, "skill_id", "instruction"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if d.Store == nil {
					return "start_worker_skill 需要可用的存储服务；当前入口未装配 Store。", nil
				}
				var args struct {
					SkillID     int64   `json:"skill_id"`
					Instruction string  `json:"instruction"`
					WorkerID    int64   `json:"worker_id"`
					FileIDs     []int64 `json:"file_ids"`
					Title       string  `json:"title"`
					Confirm     bool    `json:"confirm"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				k, err := d.Store.KnowledgeByID(ctx, args.SkillID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "skill 不存在。", nil
					}
					return "", err
				}
				if k.Kind != store.KnowledgeKindSkill {
					return "该条目不是 skill。", nil
				}
				instruction := strings.TrimSpace(args.Instruction)
				if instruction == "" {
					return "instruction 不能为空。", nil
				}
				if skillHasAnyTag(k, "requires:superadmin", "superadmin_only") && !u.IsSuperadmin {
					return "这条 skill 只能由超级管理员执行。", nil
				}
				requiresAdmin := skillHasAnyTag(k, "requires:admin_worker", "admin_worker", "worker:admin")
				highRisk := requiresAdmin || skillHasAnyTag(k, "risk:high", "high_risk")
				if highRisk && !args.Confirm {
					return "这条 skill 标记为高风险或需要 admin worker，必须由用户明确确认。确认后再次调用 start_worker_skill，并设置 confirm=true。", nil
				}
				var worker *store.User
				if requiresAdmin {
					w, msg, err := pickAdminWorkflowWorker(ctx, d, u, args.WorkerID)
					if err != nil {
						return "", err
					}
					if msg != "" {
						return msg, nil
					}
					worker = w
				} else {
					w, err := pickMaterialWorker(ctx, d, u, args.WorkerID)
					if err != nil {
						return err.Error(), nil
					}
					worker = w
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
				pj, err := d.Store.EnsureWorkerOperationsProject(ctx, u.ID)
				if err != nil {
					return "", err
				}
				title := strings.TrimSpace(args.Title)
				if title == "" {
					title = "执行 skill：" + k.Title
				}
				t, err := d.Store.CreateTaskWithFileAttachments(ctx, &store.Task{
					ProjectID:       pj.ID,
					AssignerID:      u.ID,
					AssigneeID:      worker.ID,
					Title:           title,
					Goal:            "按已保存的 skill 执行本次任务，并把可复用结果沉淀回 nbco。",
					Description:     workerSkillTaskPrompt(k, instruction),
					Acceptance:      "完成汇报必须包含执行了哪条 skill、关键步骤结果、验证情况、产物清单；失败时说明阻塞点和下一步需要什么。",
					Priority:        skillPriority(highRisk),
					WorkerScopeType: "skill", WorkerScopeKey: fmt.Sprintf("skill:%d", k.ID),
					WorkerScopeTitle: k.Title,
				}, args.FileIDs, "skill 执行输入")
				if err != nil {
					return "", err
				}
				wakeWorker(d, worker)
				return fmt.Sprintf("已按 skill「%s」创建 worker 任务（%s），分配给 %s。", k.Title, internalRef("任务", t.ID), worker.Name), nil
			}),

		tool("save_skill", "把用户教给系统的可复用执行方法保存为 skill。适用于“以后遇到 X 要按这些步骤做”“这种场景流程是…”“把刚才方法记成 skill”。只保存可执行流程，不保存普通事实。",
			obj(map[string]any{
				"title":       p("string", "标题（一句话说清这个 skill 处理什么）"),
				"trigger":     p("string", "触发条件：什么情况下应该想到这个 skill"),
				"summary":     p("string", "短摘要：系统提示里按需注入的 1-2 句话"),
				"procedure":   p("string", "完整执行方法：步骤、要调用的工具、判断分支"),
				"constraints": p("string", "限制/禁忌/例外条件，可选"),
				"scope":       p("string", "作用域：global（默认）| telegram | api | worker | user:<用户ID>"),
				"tags":        arr("string", "标签（可选）"),
			}, "title", "trigger", "summary", "procedure"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if !u.IsSuperadmin {
					return "只有超级管理员可以保存公司级 skill。", nil
				}
				var args skillArgs
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				content, tags, msg := buildSkillContent(args)
				if msg != "" {
					return msg, nil
				}
				k, err := d.saveSkill(ctx, strings.TrimSpace(args.Title), content, tags, u.ID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已保存 skill（%s）。之后相关对话会自动召回摘要，必要时可 load_skill 读取完整步骤。", internalRef("skill", k.ID)), nil
			}),

		tool("update_skill", "更新一条 skill（空字段不改）。用于修正旧执行方法、补充禁忌或更新触发条件。仅超管可用。",
			obj(map[string]any{
				"id":          p("integer", "skill ID"),
				"title":       p("string", "新标题（可选）"),
				"trigger":     p("string", "新触发条件（可选）"),
				"summary":     p("string", "新短摘要（可选）"),
				"procedure":   p("string", "新完整执行方法（可选）"),
				"constraints": p("string", "新限制/禁忌/例外条件（可选）"),
				"scope":       p("string", "新作用域（可选）"),
				"tags":        arr("string", "新标签（可选，整体替换）"),
			}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if !u.IsSuperadmin {
					return "只有超级管理员可以修改公司级 skill。", nil
				}
				var args skillUpdateArgs
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				k, err := d.Store.KnowledgeByID(ctx, args.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "skill 不存在。", nil
					}
					return "", err
				}
				if k.Kind != store.KnowledgeKindSkill {
					return "该条目不是 skill。", nil
				}
				next := parseSkillContent(k.Content)
				if strings.TrimSpace(args.Trigger) != "" {
					next.Trigger = args.Trigger
				}
				if strings.TrimSpace(args.Summary) != "" {
					next.Summary = args.Summary
				}
				if strings.TrimSpace(args.Procedure) != "" {
					next.Procedure = args.Procedure
				}
				if strings.TrimSpace(args.Constraints) != "" {
					next.Constraints = args.Constraints
				}
				scope := args.Scope
				if strings.TrimSpace(scope) == "" {
					scope = skillScopeOf(k.Tags)
				}
				content, tags, msg := buildSkillContent(skillArgs{
					Title:       firstNonEmpty(args.Title, k.Title),
					Trigger:     next.Trigger,
					Summary:     next.Summary,
					Procedure:   next.Procedure,
					Constraints: next.Constraints,
					Scope:       scope,
					Tags:        chooseSkillTags(args.Tags, k.Tags, scope),
				})
				if msg != "" {
					return msg, nil
				}
				var title *string
				if strings.TrimSpace(args.Title) != "" {
					title = &args.Title
				}
				updated, err := d.Store.UpdateKnowledge(ctx, args.ID, title, &content, tags)
				if err != nil {
					return "", err
				}
				if d.Knowledge != nil {
					d.Knowledge.Reembed(ctx, updated)
				}
				return "已更新 skill。", nil
			}),
	}
}

type skillArgs struct {
	Title       string   `json:"title"`
	Trigger     string   `json:"trigger"`
	Summary     string   `json:"summary"`
	Procedure   string   `json:"procedure"`
	Constraints string   `json:"constraints"`
	Scope       string   `json:"scope"`
	Tags        []string `json:"tags"`
}

type skillUpdateArgs struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Trigger     string   `json:"trigger"`
	Summary     string   `json:"summary"`
	Procedure   string   `json:"procedure"`
	Constraints string   `json:"constraints"`
	Scope       string   `json:"scope"`
	Tags        []string `json:"tags"`
}

type skillParts struct {
	Trigger     string
	Summary     string
	Procedure   string
	Constraints string
}

func buildSkillContent(args skillArgs) (string, []string, string) {
	title := strings.TrimSpace(args.Title)
	trigger := strings.TrimSpace(args.Trigger)
	summary := strings.TrimSpace(args.Summary)
	procedure := strings.TrimSpace(args.Procedure)
	if title == "" || trigger == "" || summary == "" || procedure == "" {
		return "", nil, "title、trigger、summary、procedure 都不能为空。"
	}
	scope := strings.TrimSpace(args.Scope)
	if scope == "" {
		scope = "global"
	}
	if msg := validateSkillScope(scope); msg != "" {
		return "", nil, msg
	}
	tags := normalizeSkillTags(args.Tags, scope)
	var b strings.Builder
	fmt.Fprintf(&b, "触发条件：%s\n", trigger)
	fmt.Fprintf(&b, "摘要：%s\n", summary)
	fmt.Fprintf(&b, "执行方法：\n%s\n", procedure)
	if c := strings.TrimSpace(args.Constraints); c != "" {
		fmt.Fprintf(&b, "限制与禁忌：\n%s\n", c)
	}
	return strings.TrimSpace(b.String()), tags, ""
}

func validateSkillScope(scope string) string {
	if ruleScopes[scope] {
		return ""
	}
	if id, ok := strings.CutPrefix(scope, "user:"); ok {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 {
			return ""
		}
	}
	return "scope 必须是 global/telegram/api/worker 或 user:<数字用户ID>。"
}

// normalizeSkillTags 转发到 textfmt.NormalizeScopeTags（跨包共享实现）。
func normalizeSkillTags(tags []string, scope string) []string {
	return textfmt.NormalizeScopeTags(tags, scope)
}

func chooseSkillTags(argsTags, oldTags []string, scope string) []string {
	if argsTags != nil {
		return argsTags
	}
	var out []string
	for _, tag := range oldTags {
		if !strings.HasPrefix(tag, "scope:") {
			out = append(out, tag)
		}
	}
	return out
}

func parseSkillContent(content string) skillParts {
	var p skillParts
	var section string
	for _, line := range strings.Split(content, "\n") {
		switch {
		case strings.HasPrefix(line, "触发条件："):
			p.Trigger = strings.TrimSpace(strings.TrimPrefix(line, "触发条件："))
			section = ""
		case strings.HasPrefix(line, "摘要："):
			p.Summary = strings.TrimSpace(strings.TrimPrefix(line, "摘要："))
			section = ""
		case strings.HasPrefix(line, "执行方法："):
			section = "procedure"
		case strings.HasPrefix(line, "限制与禁忌："):
			section = "constraints"
		default:
			switch section {
			case "procedure":
				p.Procedure += line + "\n"
			case "constraints":
				p.Constraints += line + "\n"
			}
		}
	}
	p.Procedure = strings.TrimSpace(p.Procedure)
	p.Constraints = strings.TrimSpace(p.Constraints)
	return p
}

func renderSkillList(ks []*store.Knowledge) string {
	if len(ks) == 0 {
		return "（没有匹配的 skill）"
	}
	var b strings.Builder
	for _, k := range ks {
		parts := parseSkillContent(k.Content)
		fmt.Fprintf(&b, "- %s：%s", internalRef("skill", k.ID), k.Title)
		if scope := skillScopeOf(k.Tags); scope != "global" {
			fmt.Fprintf(&b, "（%s）", scope)
		}
		if parts.Summary != "" {
			fmt.Fprintf(&b, "：%s", parts.Summary)
		}
		b.WriteByte('\n')
	}
	b.WriteString("需要完整步骤时用 load_skill。")
	return b.String()
}

func skillScopeOf(tags []string) string {
	return ruleScopeOf(tags)
}

func skillHasAnyTag(k *store.Knowledge, wants ...string) bool {
	if k == nil {
		return false
	}
	seen := map[string]bool{}
	for _, tag := range k.Tags {
		seen[strings.ToLower(strings.TrimSpace(tag))] = true
	}
	for _, want := range wants {
		if seen[strings.ToLower(strings.TrimSpace(want))] {
			return true
		}
	}
	return false
}

func skillPriority(highRisk bool) string {
	if highRisk {
		return "high"
	}
	return "normal"
}

func workerSkillTaskPrompt(k *store.Knowledge, instruction string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "请按 nbco 已保存的 skill 执行本次任务。\n\n")
	fmt.Fprintf(&b, "本次目标：\n%s\n\n", strings.TrimSpace(instruction))
	fmt.Fprintf(&b, "skill：%s\n", k.Title)
	if len(k.Tags) > 0 {
		fmt.Fprintf(&b, "skill 标签：%s\n", strings.Join(k.Tags, ", "))
	}
	fmt.Fprintf(&b, "\n%s\n\n", strings.TrimSpace(k.Content))
	b.WriteString("通用执行要求：\n")
	b.WriteString("1. 以 skill 内容为准执行；如果 skill 与本次目标冲突，先解释冲突并按更高权限/更具体的用户要求处理。\n")
	b.WriteString("2. 需要读取附件、改文件、运行命令或生成产物时，在 worker 工作机内完成；产物写入任务提示指定的产物目录。\n")
	b.WriteString("3. 不要泄露密钥、token、数据库 DSN、绑定码或内部凭据。\n")
	b.WriteString("4. 如果信息不足或前置条件不存在，停止并汇报缺什么，不要猜。\n")
	b.WriteString("5. 完成后总结可复用经验；确有长期价值时写入 lessons，供 nbco 审核沉淀。\n")
	return strings.TrimSpace(b.String())
}

func firstNonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

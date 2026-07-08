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

func normalizeSkillTags(tags []string, scope string) []string {
	out := make([]string, 0, len(tags)+1)
	seen := map[string]bool{}
	add := func(tag string) {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			return
		}
		seen[tag] = true
		out = append(out, tag)
	}
	add("scope:" + scope)
	for _, tag := range tags {
		if strings.HasPrefix(tag, "scope:") {
			continue
		}
		add(tag)
	}
	return out
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

func firstNonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

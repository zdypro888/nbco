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

// ruleScopes save_rule 接受的作用域值（user:<id> 另行校验前缀）。
var ruleScopes = map[string]bool{"global": true, "telegram": true, "api": true, "worker": true}

const rulesListLimit = 50

// ruleTools 行为规则（Policy Memory）：与知识库同表（kind=policy），但语义不同——
// 知识是「公司知道什么」，规则是「系统该怎么做」。pinned 规则常驻系统提示，
// 其余按语义相关度逐轮注入。影响所有人，仅组装给超管（注册表再拦一层）。
func ruleTools(d Deps, u *store.User) []ai.Tool {
	if !u.IsSuperadmin {
		return nil
	}
	return []ai.Tool{
		tool("save_rule", "当用户对系统/AI 的行为提出持久性要求、禁令或默认做法时调用（句式如「以后不要…」「默认…」「记住以后都…」「永远…」），把它存成一条行为规则；之后每轮对话会自动注入相关规则并被遵守。行为规则必须用这个工具而不是 save_knowledge。少数底线规则可设 pinned 常驻每轮提示，普通规则留给语义召回。",
			obj(map[string]any{
				"title":   p("string", "规则标题（一句话说清约束什么，便于检索）"),
				"content": p("string", "规则正文：明确、可执行、自包含（含例外条件，如「除非超管明确要求」）"),
				"scope":   p("string", "作用域：global（默认，处处生效）| telegram | api | worker | user:<用户ID>"),
				"pinned":  p("boolean", "是否常驻每轮系统提示（默认 false）。只给不容遗漏的底线规则用，常驻规则越少越好"),
			}, "title", "content"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Title   string `json:"title"`
					Content string `json:"content"`
					Scope   string `json:"scope"`
					Pinned  bool   `json:"pinned"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Content) == "" {
					return "标题和正文都不能为空。", nil
				}
				scope := strings.TrimSpace(args.Scope)
				if scope == "" {
					scope = "global"
				}
				if !ruleScopes[scope] {
					id, ok := strings.CutPrefix(scope, "user:")
					if _, err := strconv.ParseInt(id, 10, 64); !ok || err != nil {
						return "scope 必须是 global/telegram/api/worker 或 user:<数字用户ID>。", nil
					}
				}
				var k *store.Knowledge
				var err error
				if d.Knowledge != nil {
					k, err = d.Knowledge.SaveRule(ctx, args.Title, args.Content, []string{"scope:" + scope}, u.ID, args.Pinned)
				} else {
					k, err = d.Store.CreateRule(ctx, args.Title, args.Content, []string{"scope:" + scope}, u.ID, args.Pinned)
				}
				if err != nil {
					return "", err
				}
				mode := "按语义相关度逐轮注入"
				if args.Pinned {
					mode = "常驻每轮系统提示"
				}
				return fmt.Sprintf("已保存规则（%s，作用域 %s，%s），即刻生效。", internalRef("规则", k.ID), scope, mode), nil
			}),

		tool("list_rules", "查看全部行为规则（常驻在前）。改正文用 update_knowledge，删除用 delete_knowledge，调整常驻用 set_rule_pinned。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ks, err := d.Store.ListRules(ctx, rulesListLimit)
				if err != nil {
					return "", err
				}
				if len(ks) == 0 {
					return "（还没有行为规则。用户提出「以后都…」类要求时用 save_rule 沉淀。）", nil
				}
				var b strings.Builder
				for _, k := range ks {
					marker := "·"
					if k.Pinned {
						marker = "📌"
					}
					fmt.Fprintf(&b, "%s %s：%s", marker, internalRef("规则", k.ID), k.Title)
					if scope := ruleScopeOf(k.Tags); scope != "global" {
						fmt.Fprintf(&b, "（%s）", scope)
					}
					fmt.Fprintf(&b, "\n  %s\n", k.Content)
				}
				return b.String(), nil
			}),

		tool("set_rule_pinned", "调整一条规则是否常驻每轮系统提示。常驻规则越少越好，只留底线规则。",
			obj(map[string]any{
				"id":     p("integer", "规则ID"),
				"pinned": p("boolean", "true=常驻，false=改回语义召回"),
			}, "id", "pinned"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID     int64 `json:"id"`
					Pinned bool  `json:"pinned"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.SetRulePinned(ctx, args.ID, args.Pinned); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该规则不存在（注意 ID 必须是 kind=policy 的规则条目）。", nil
					}
					return "", err
				}
				if args.Pinned {
					return "已设为常驻规则。", nil
				}
				return "已改回语义召回。", nil
			}),
	}
}

// ruleScopeOf 从 tags 提取 scope（无标签视同 global）。
func ruleScopeOf(tags []string) string {
	for _, t := range tags {
		if v, ok := strings.CutPrefix(t, "scope:"); ok {
			return v
		}
	}
	return "global"
}

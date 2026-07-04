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

const knowledgeSearchLimit = 10

// knowledgeTools 公司知识库：全员可读可写（越用越值钱的共享资产），删除限作者与超管。
func knowledgeTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("save_knowledge", "把有复用价值的结论存入公司知识库（决策、方案、流程、客户约定等）。标题要能被搜到，正文自包含。",
			obj(map[string]any{
				"title":   p("string", "标题（一句话说清是什么）"),
				"content": p("string", "正文（自包含，不依赖当前对话上下文）"),
				"tags":    arr("string", "标签（可选，便于检索）"),
			}, "title", "content"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Title   string   `json:"title"`
					Content string   `json:"content"`
					Tags    []string `json:"tags"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Title) == "" || strings.TrimSpace(args.Content) == "" {
					return "标题和正文都不能为空。", nil
				}
				k, err := d.saveKnowledge(ctx, args.Title, args.Content, args.Tags, u.ID)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已存入知识库（#%d）。", k.ID), nil
			}),

		tool("search_knowledge", "检索公司知识库（语义+关键词混合召回，按相关度排序）。回答公司事实类问题、决策/方案/流程前先查这里。",
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
				ks, err := d.searchKnowledge(ctx, q, knowledgeSearchLimit)
				if err != nil {
					return "", err
				}
				return renderKnowledgeList(ks), nil
			}),

		tool("get_knowledge", "查看一条知识的完整内容。",
			obj(map[string]any{"id": p("integer", "知识ID")}, "id"),
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
						return "知识条目不存在。", nil
					}
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "#%d %s\n", k.ID, k.Title)
				if len(k.Tags) > 0 {
					fmt.Fprintf(&b, "标签: %s\n", strings.Join(k.Tags, ", "))
				}
				fmt.Fprintf(&b, "作者: 用户%d · 更新于 %s\n\n%s", k.AuthorID, fmtTime(k.UpdatedAt, d.TZ), k.Content)
				return b.String(), nil
			}),

		tool("update_knowledge", "更新一条知识（空字段不改）。仅作者或超管可改。",
			obj(map[string]any{
				"id":      p("integer", "知识ID"),
				"title":   p("string", "新标题（可选）"),
				"content": p("string", "新正文（可选）"),
				"tags":    arr("string", "新标签（可选，整体替换）"),
			}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID      int64    `json:"id"`
					Title   string   `json:"title"`
					Content string   `json:"content"`
					Tags    []string `json:"tags"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				k, err := d.Store.KnowledgeByID(ctx, args.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "知识条目不存在。", nil
					}
					return "", err
				}
				if k.AuthorID != u.ID && !u.IsSuperadmin {
					return "只有作者或超管能修改。", nil
				}
				var title, content *string
				if strings.TrimSpace(args.Title) != "" {
					title = &args.Title
				}
				if strings.TrimSpace(args.Content) != "" {
					content = &args.Content
				}
				updated, err := d.Store.UpdateKnowledge(ctx, args.ID, title, content, args.Tags)
				if err != nil {
					return "", err
				}
				// 正文变了就重算 embedding（best-effort）。
				if content != nil && d.Knowledge != nil {
					d.Knowledge.Reembed(ctx, updated)
				}
				return "已更新。", nil
			}),

		tool("delete_knowledge", "删除一条知识。仅作者或超管可删。",
			obj(map[string]any{"id": p("integer", "知识ID")}, "id"),
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
						return "知识条目不存在。", nil
					}
					return "", err
				}
				if k.AuthorID != u.ID && !u.IsSuperadmin {
					return "只有作者或超管能删除。", nil
				}
				if err := d.Store.DeleteKnowledge(ctx, args.ID); err != nil {
					return "", err
				}
				return "已删除。", nil
			}),

		tool("list_recent_knowledge", "查看最近入库的知识条目。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ks, err := d.Store.RecentKnowledge(ctx, knowledgeSearchLimit)
				if err != nil {
					return "", err
				}
				return renderKnowledgeList(ks), nil
			}),
	}
}

func renderKnowledgeList(ks []*store.Knowledge) string {
	if len(ks) == 0 {
		return "（没有匹配的知识条目）"
	}
	var b strings.Builder
	for _, k := range ks {
		fmt.Fprintf(&b, "- #%d %s", k.ID, k.Title)
		if len(k.Tags) > 0 {
			fmt.Fprintf(&b, "（%s）", strings.Join(k.Tags, ", "))
		}
		b.WriteByte('\n')
	}
	b.WriteString("用 get_knowledge 查看完整内容。")
	return b.String()
}

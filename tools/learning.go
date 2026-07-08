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

func learningTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("list_learning_candidates", "查看系统自动归纳出来的学习候选（知识/规则/skill/脚本/画像/总结）。用于审核 AI 是否学对了、合并前先看证据。",
			obj(map[string]any{
				"status": p("string", "pending/published/rejected，可选，默认 pending"),
				"kind":   p("string", "knowledge/rule/skill/script/profile/summary，可选"),
				"limit":  p("integer", "返回条数，默认 20，最多 100"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Status string `json:"status"`
					Kind   string `json:"kind"`
					Limit  int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				status := strings.TrimSpace(args.Status)
				if status == "" {
					status = store.LearningStatusPending
				}
				items, err := d.Store.ListLearningCandidates(ctx, status, strings.TrimSpace(args.Kind), args.Limit)
				if err != nil {
					return "", err
				}
				return renderLearningCandidates(items), nil
			}),

		tool("approve_learning_candidate", "批准一条学习候选并发布到正式记忆。knowledge 会入知识库；rule 会入行为规则；skill 会入 Skill Memory。脚本/画像/总结候选暂只标记为已发布，后续用对应工具细化。",
			obj(map[string]any{
				"id": p("integer", "learning candidate ID"),
			}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				c, err := d.Store.LearningCandidateByID(ctx, args.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "学习候选不存在。", nil
					}
					return "", err
				}
				if c.Status == store.LearningStatusPublished {
					return "这条候选已经发布过。", nil
				}
				kid, msg, err := publishLearningCandidate(ctx, d, u, c)
				if err != nil {
					return "", err
				}
				if err := d.Store.MarkLearningCandidatePublished(ctx, c.ID, u.ID, kid); err != nil {
					return "", err
				}
				return msg, nil
			}),

		tool("reject_learning_candidate", "拒绝一条学习候选。用于清理误归纳、过时或重复的学习结果。",
			obj(map[string]any{"id": p("integer", "learning candidate ID")}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.RejectLearningCandidate(ctx, args.ID, u.ID); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "学习候选不存在。", nil
					}
					return "", err
				}
				return "已拒绝。", nil
			}),
	}
}

func publishLearningCandidate(ctx context.Context, d Deps, u *store.User, c *store.LearningCandidate) (*int64, string, error) {
	title := strings.TrimSpace(c.Title)
	content := strings.TrimSpace(c.Content)
	if title == "" || content == "" {
		return nil, "候选标题或内容为空，不能发布。", nil
	}
	tags := append([]string{}, c.Tags...)
	switch c.Kind {
	case store.LearningKindKnowledge:
		k, err := d.saveKnowledge(ctx, title, content, tags, u.ID)
		if err != nil {
			return nil, "", err
		}
		return &k.ID, fmt.Sprintf("已发布为知识 #%d。", k.ID), nil
	case store.LearningKindRule:
		scope := strings.TrimSpace(c.Scope)
		if scope == "" {
			scope = "global"
		}
		tags = ensureTag(tags, "scope:"+scope)
		k, err := d.Store.CreateRule(ctx, title, content, tags, u.ID, false)
		if err != nil {
			return nil, "", err
		}
		if d.Knowledge != nil {
			d.Knowledge.Reembed(ctx, k)
		}
		return &k.ID, fmt.Sprintf("已发布为行为规则 #%d。", k.ID), nil
	case store.LearningKindSkill:
		k, err := d.saveSkill(ctx, title, content, tags, u.ID)
		if err != nil {
			return nil, "", err
		}
		return &k.ID, fmt.Sprintf("已发布为 skill #%d。", k.ID), nil
	default:
		return nil, "已标记为已发布；该类型需要通过对应专用工具继续落地。", nil
	}
}

func ensureTag(tags []string, want string) []string {
	for _, t := range tags {
		if t == want {
			return tags
		}
	}
	return append(tags, want)
}

func renderLearningCandidates(items []*store.LearningCandidate) string {
	if len(items) == 0 {
		return "（没有学习候选）"
	}
	var b strings.Builder
	for _, c := range items {
		fmt.Fprintf(&b, "#%d [%s/%s] %s\n", c.ID, c.Kind, c.Status, c.Title)
		if c.Scope != "" {
			fmt.Fprintf(&b, "  scope: %s\n", c.Scope)
		}
		if len(c.Tags) > 0 {
			fmt.Fprintf(&b, "  tags: %s\n", strings.Join(c.Tags, ", "))
		}
		if c.Confidence > 0 {
			fmt.Fprintf(&b, "  confidence: %.2f\n", c.Confidence)
		}
		fmt.Fprintf(&b, "  %s\n", truncate(c.Content, 280))
	}
	return strings.TrimSpace(b.String())
}

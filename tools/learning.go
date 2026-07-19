package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

func learningTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("propose_learning_candidate", "提交一条待审核的学习候选。适用于普通员工/AI worker/脚本发现可长期复用的信息、规则或流程，但不应直接写成正式规则或 skill 的场景。",
			obj(map[string]any{
				"kind":       p("string", "knowledge/rule/skill/script/profile/summary"),
				"title":      p("string", "候选标题，一句话说清要学习什么"),
				"content":    p("string", "候选正文，必须自包含"),
				"scope":      p("string", "作用域：global（默认）| telegram | api | worker | user:<用户ID>"),
				"tags":       arr("string", "标签（可选）"),
				"confidence": p("number", "置信度 0-1，可选"),
				"evidence":   p("string", "证据摘要：来自哪次对话/任务/文件，为什么值得学习"),
			}, "kind", "title", "content"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Kind       string   `json:"kind"`
					Title      string   `json:"title"`
					Content    string   `json:"content"`
					Scope      string   `json:"scope"`
					Tags       []string `json:"tags"`
					Confidence float32  `json:"confidence"`
					Evidence   string   `json:"evidence"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				kind := strings.TrimSpace(args.Kind)
				if !validLearningKind(kind) {
					return "kind 必须是 knowledge/rule/skill/script/profile/summary。", nil
				}
				title := strings.TrimSpace(args.Title)
				content := strings.TrimSpace(args.Content)
				if title == "" || content == "" {
					return "标题和正文都不能为空。", nil
				}
				scope := normalizeLearningScope(args.Scope)
				if msg := validateSkillScope(scope); msg != "" {
					return msg, nil
				}
				exists, err := d.Store.LearningCandidateExists(ctx, kind, title, store.LearningStatusPending, store.LearningStatusPublished)
				if err != nil {
					return "", err
				}
				if exists {
					return "已有同名学习候选或已发布条目，不重复提交。", nil
				}
				confidence := args.Confidence
				if confidence <= 0 {
					confidence = 0.55
				}
				if confidence > 1 {
					confidence = 1
				}
				ev, _ := json.Marshal(map[string]any{
					"source":   "tool",
					"evidence": strings.TrimSpace(args.Evidence),
				})
				createdBy := u.ID
				c, err := d.Store.CreateLearningCandidate(ctx, store.LearningCandidateInput{
					Kind: kind, Scope: scope, Title: title, Content: content,
					Tags: normalizeLearningTags(args.Tags, scope), Evidence: ev,
					Confidence: confidence, Status: store.LearningStatusPending,
					SourceType: "tool", CreatedBy: &createdBy,
				})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("已提交学习候选（%s），等待超管审核后生效。", internalRef("候选", c.ID)), nil
			}),

		tool("list_learning_candidates", "查看系统自动归纳出来的学习候选（知识/规则/skill/脚本/画像/总结）。用于审核 AI 是否学对了、合并前先看证据。",
			obj(map[string]any{
				"status": enumP("状态筛选，可选", store.LearningStatusPending, store.LearningStatusPublished, store.LearningStatusRejected),
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

func validLearningKind(kind string) bool {
	switch kind {
	case store.LearningKindKnowledge, store.LearningKindRule, store.LearningKindSkill,
		store.LearningKindScript, store.LearningKindProfile, store.LearningKindSummary:
		return true
	default:
		return false
	}
}

func normalizeLearningScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return "global"
	}
	return scope
}

func normalizeLearningTags(tags []string, scope string) []string {
	return textfmt.NormalizeScopeTags(tags, scope)
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
		return &k.ID, fmt.Sprintf("已发布为知识（%s）。", internalRef("知识", k.ID)), nil
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
		return &k.ID, fmt.Sprintf("已发布为行为规则（%s）。", internalRef("规则", k.ID)), nil
	case store.LearningKindSkill:
		k, err := d.saveSkill(ctx, title, content, tags, u.ID)
		if err != nil {
			return nil, "", err
		}
		return &k.ID, fmt.Sprintf("已发布为 skill（%s）。", internalRef("skill", k.ID)), nil
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
		fmt.Fprintf(&b, "%s [%s/%s] %s\n", internalRef("候选", c.ID), c.Kind, c.Status, c.Title)
		if c.Scope != "" {
			fmt.Fprintf(&b, "  scope: %s\n", c.Scope)
		}
		if c.SourceType != "" || c.SourceRef != "" {
			fmt.Fprintf(&b, "  source: %s %s\n", c.SourceType, c.SourceRef)
		}
		if len(c.Tags) > 0 {
			fmt.Fprintf(&b, "  tags: %s\n", strings.Join(c.Tags, ", "))
		}
		if c.Confidence > 0 {
			fmt.Fprintf(&b, "  confidence: %.2f\n", c.Confidence)
		}
		if c.ValueScore > 0 {
			fmt.Fprintf(&b, "  value_score: %.2f\n", c.ValueScore)
		}
		if c.DuplicateOf != nil {
			fmt.Fprintf(&b, "  duplicate_of: %s\n", internalRef("候选", *c.DuplicateOf))
		}
		if c.ConflictWith != nil {
			fmt.Fprintf(&b, "  conflict_with: %s\n", internalRef("候选", *c.ConflictWith))
		}
		if strings.TrimSpace(c.ReviewNote) != "" {
			fmt.Fprintf(&b, "  review_note: %s\n", c.ReviewNote)
		}
		if ev := renderLearningEvidence(c.Evidence); ev != "" {
			fmt.Fprintf(&b, "  evidence: %s\n", ev)
		}
		fmt.Fprintf(&b, "  %s\n", truncate(c.Content, 280))
	}
	return strings.TrimSpace(b.String())
}

func renderLearningEvidence(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "{}" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return truncate(string(raw), 180)
	}
	for _, key := range []string{"evidence", "user_text", "assistant_text"} {
		if v, ok := obj[key].(string); ok && strings.TrimSpace(v) != "" {
			return truncate(strings.TrimSpace(v), 180)
		}
	}
	return ""
}

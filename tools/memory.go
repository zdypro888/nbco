package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

const historySearchLimit = 8

// memoryTools 情景记忆与成本：search_history 人人可用（只搜自己名下会话）；
// ai_usage_stats 限超管（注册表兜底）。
func memoryTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("search_history", "跨会话检索你和当前用户的历史对话（语义+关键词）。用户问「之前/上次聊过什么」「我们定过什么」而摘要里没有时先查这里。只能搜到当前用户自己的会话。",
			obj(map[string]any{"query": p("string", "要找的话题（自然语言）")}, "query"),
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
				var ms []store.ChatMessage
				var err error
				if d.Knowledge != nil {
					ms, err = d.Knowledge.SearchHistory(ctx, u.ID, q, historySearchLimit)
				} else {
					ms, err = d.Store.SearchMessagesOfUser(ctx, u.ID, q, historySearchLimit)
				}
				if err != nil {
					return "", err
				}
				if len(ms) == 0 {
					return "（历史对话里没有找到相关内容）", nil
				}
				var b strings.Builder
				b.WriteString("历史对话片段（按相关度）：\n")
				for _, m := range ms {
					who := "用户"
					content := m.Content
					if m.Role == string(ai.RoleAssistant) {
						who = "AI"
						content = textfmt.StripHistoryMetadata(content)
					}
					fmt.Fprintf(&b, "- [%s·%s] %s\n", fmtTime(m.CreatedAt, d.TZ), who, truncate(content, 300))
				}
				return b.String(), nil
			}),

		tool("ai_usage_stats", "查看 AI 用量统计：今天/7天/30天 token 总量与调用次数，7天内按人排行，以及按战略目标的执行成本。"+
			"注意：目标维度只反映 AI 员工执行成本（worker 调用模型），不含对话/催办/周报等系统轮次，也未归因的目标为空——评估目标 ROI 时以 worker 执行成本为准。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				now := time.Now()
				var b strings.Builder
				for _, r := range []struct {
					label string
					since time.Time
				}{
					{"今天", now.Truncate(24 * time.Hour)},
					{"近7天", now.AddDate(0, 0, -7)},
					{"近30天", now.AddDate(0, 0, -30)},
				} {
					t, err := d.Store.AIUsageSince(ctx, r.since)
					if err != nil {
						return "", err
					}
					fmt.Fprintf(&b, "%s：%d 次调用，输入 %d / 输出 %d tokens\n", r.label, t.Calls, t.InputTokens, t.OutputTokens)
				}
				rows, err := d.Store.AIUsageByUserSince(ctx, now.AddDate(0, 0, -7))
				if err != nil {
					return "", err
				}
				if len(rows) > 0 {
					b.WriteString("\n近7天按人（含 AI 员工）：\n")
					for _, r := range rows {
						fmt.Fprintf(&b, "- %s：%d 次，输入 %d / 输出 %d\n", r.Name, r.Calls, r.InputTokens, r.OutputTokens)
					}
				}
				goals, err := d.Store.AIUsageByGoalSince(ctx, now.AddDate(0, 0, -30))
				if err != nil {
					return "", err
				}
				if len(goals) > 0 {
					b.WriteString("\n近30天按战略目标（仅 worker 执行成本）：\n")
					for _, g := range goals {
						fmt.Fprintf(&b, "- %s（%s）：%d 次，输入 %d / 输出 %d\n", g.Title, internalRef("目标", g.GoalID), g.Calls, g.InputTokens, g.OutputTokens)
					}
				}
				return b.String(), nil
			}),
	}
}

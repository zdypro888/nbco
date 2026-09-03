package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

type interactionChannelKey struct{}

// WithInteractionChannel binds tools to the current transport scope. It lets a
// generic tool resolve "this group" structurally without interpreting wording.
func WithInteractionChannel(ctx context.Context, channel string) context.Context {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return ctx
	}
	return context.WithValue(ctx, interactionChannelKey{}, channel)
}

func interactionChannel(ctx context.Context) string {
	channel, _ := ctx.Value(interactionChannelKey{}).(string)
	return strings.TrimSpace(channel)
}

func userCanAccessFile(ctx context.Context, d Deps, u *store.User, fileID int64) (bool, error) {
	if d.Store == nil || u == nil {
		return false, nil
	}
	return d.Store.UserCanAccessFileInConversation(ctx, u.ID, u.IsSuperadmin, fileID, interactionChannel(ctx))
}

// contextTools exposes one broad retrieval primitive. Eino decides when and
// why to search; this layer only applies channel isolation and row-level ACLs.
func contextTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		immediateTool(tool("search_context", "在当前权限和当前会话范围内检索相关事实与历史。私聊/API 会跨结构化业务数据、工作事实和当前用户历史做语义+词法检索；群聊只检索当前这个群的共享记录，绝不带出私聊。适合对象说法变化、跨来源调查和继续之前讨论；只读。",
			obj(map[string]any{
				"query": p("string", "要查找的主题、事实或上下文（自然语言）"),
				"limit": p("integer", "最多返回条数，默认12，最大30"),
			}, "query"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if d.Store == nil {
					return "当前入口没有数据库连接，无法检索上下文。", nil
				}
				var args struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				query := strings.TrimSpace(args.Query)
				if query == "" {
					return "查询不能为空。", nil
				}
				limit := args.Limit
				if limit <= 0 || limit > 30 {
					limit = 12
				}
				channel := interactionChannel(ctx)
				if store.IsGroupChannel(channel) {
					var messages []store.ChatMessage
					var err error
					if d.Knowledge != nil {
						messages, err = d.Knowledge.SearchGroupHistory(ctx, channel, query, limit)
					} else {
						messages, err = d.Store.SearchMessagesOfChannel(ctx, channel, query, limit)
					}
					if err != nil {
						return "", err
					}
					if len(messages) == 0 {
						return "当前群的共享记录里没有找到相关内容。", nil
					}
					var b strings.Builder
					b.WriteString("当前群相关记录（按相关度；历史答复不证明当前状态）：\n")
					for _, message := range messages {
						role := "用户"
						content := message.Content
						if message.Role == string(ai.RoleAssistant) {
							role = "AI"
							content = textfmt.StripHistoryMetadata(content)
						}
						fmt.Fprintf(&b, "- [%s·%s] %s\n", fmtTime(message.EventAt(), d.TZ), role, truncate(content, 360))
					}
					return b.String(), nil
				}

				// Keep the source set broad here. The semantic planner expands query
				// wording, while PostgreSQL remains the final authorization boundary.
				plan := planSemanticSearch(ctx, d, u, query, nil)
				rows, err := queryAllVisibleData(ctx, d, u, query, plan, limit, 0)
				if err != nil {
					return "", err
				}
				if len(rows) == 0 {
					return "当前权限可见的事实与历史里没有找到相关内容。", nil
				}
				return renderCrossSourceRows(plan, rows), nil
			})),
	}
}

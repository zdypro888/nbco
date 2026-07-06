package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

const telegramGroupToolLimit = 50

// telegramGroupTools 管理 Telegram 群这个外部实体，和用户/worker 一样走 list/get/update。
func telegramGroupTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("list_telegram_groups", "列出 bot 已记录的 Telegram 群。用户问“你进群了吗/公司群收到了吗/有哪些群/监听状态”时先调用。结果里的 group_ref 只给后续工具使用，回复用户时不要展示内部引用或 chat ID。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				groups, err := d.Store.ListTelegramGroupStates(ctx, telegramGroupToolLimit)
				if err != nil {
					return "", err
				}
				if len(groups) == 0 {
					return "当前没有记录到 Telegram 群。", nil
				}
				var b strings.Builder
				b.WriteString("Telegram 群列表：\n")
				for _, g := range groups {
					fmt.Fprintf(&b, "- %s：%s，%s，最近更新 %s；group_ref=%s（内部引用，勿展示给用户）\n",
						telegramGroupTitle(g), telegramGroupStatusText(g), telegramGroupListenText(g),
						fmtTime(g.UpdatedAt, d.TZ), telegramGroupRef(g.ChatID))
				}
				return b.String(), nil
			}),

		tool("get_telegram_group", "查看一个 Telegram 群的接入状态。group 可填群名、群名片段或 list_telegram_groups 返回的 group_ref。回答具体群状态前调用。",
			obj(map[string]any{
				"group": p("string", "群名、群名片段或 group_ref"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group string `json:"group"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil {
					return "", err
				}
				if g == nil {
					return msg, nil
				}
				return renderTelegramGroup(*g, d.TZ), nil
			}),

		tool("set_telegram_group_listen", "开启或关闭 Telegram 群监听。仅超管可见；group 可填群名、群名片段或 group_ref。开启后普通群消息会进入群共享上下文但不主动插话。",
			obj(map[string]any{
				"group":  p("string", "群名、群名片段或 group_ref"),
				"listen": p("boolean", "true 开启监听，false 关闭监听"),
			}, "group", "listen"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group  string `json:"group"`
					Listen bool   `json:"listen"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil {
					return "", err
				}
				if g == nil {
					return msg, nil
				}
				value := ""
				if args.Listen {
					value = "1"
				}
				if err := d.Store.SetKV(ctx, store.TelegramGroupListenKey(g.ChatID), value); err != nil {
					return "", err
				}
				g.Listen = args.Listen
				g.UpdatedAt = time.Now()
				if err := d.Store.SaveTelegramGroupState(ctx, *g); err != nil {
					return "", err
				}
				return fmt.Sprintf("已更新 %s：%s。", telegramGroupTitle(*g), telegramGroupListenText(*g)), nil
			}),
	}
}

func resolveTelegramGroup(ctx context.Context, d Deps, selector string) (*store.TelegramGroupState, string, error) {
	selector = strings.TrimSpace(selector)
	groups, err := d.Store.ListTelegramGroupStates(ctx, telegramGroupToolLimit)
	if err != nil {
		return nil, "", err
	}
	if len(groups) == 0 {
		return nil, "当前没有记录到 Telegram 群。", nil
	}
	if selector == "" && len(groups) == 1 {
		return &groups[0], "", nil
	}
	if chatID, ok := parseTelegramGroupRef(selector); ok {
		for i := range groups {
			if groups[i].ChatID == chatID {
				return &groups[i], "", nil
			}
		}
		return nil, "没有找到这个 Telegram 群。", nil
	}
	lower := strings.ToLower(selector)
	exact := []store.TelegramGroupState{}
	fuzzy := []store.TelegramGroupState{}
	for _, g := range groups {
		title := strings.ToLower(strings.TrimSpace(g.Title))
		switch {
		case title == lower:
			exact = append(exact, g)
		case selector != "" && strings.Contains(title, lower):
			fuzzy = append(fuzzy, g)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = fuzzy
	}
	switch len(matches) {
	case 0:
		return nil, "没有找到匹配的 Telegram 群。先调用 list_telegram_groups 查看可用群。", nil
	case 1:
		return &matches[0], "", nil
	default:
		var b strings.Builder
		b.WriteString("匹配到多个 Telegram 群，请用更完整的群名或 group_ref：\n")
		for _, g := range matches {
			fmt.Fprintf(&b, "- %s；group_ref=%s（内部引用，勿展示给用户）\n", telegramGroupTitle(g), telegramGroupRef(g.ChatID))
		}
		return nil, b.String(), nil
	}
}

func parseTelegramGroupRef(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "telegram:group:")
	s = strings.TrimPrefix(s, "group:")
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

func telegramGroupRef(chatID int64) string {
	return fmt.Sprintf("telegram:group:%d", chatID)
}

func telegramGroupTitle(g store.TelegramGroupState) string {
	if title := strings.TrimSpace(g.Title); title != "" {
		return title
	}
	return "未命名群"
}

func telegramGroupStatusText(g store.TelegramGroupState) string {
	switch g.Status {
	case "member", "administrator", "creator", "owner", "restricted":
		return "已加入"
	case "left", "kicked":
		return "已离开"
	default:
		if g.Status == "" {
			return "状态未知"
		}
		return g.Status
	}
}

func telegramGroupListenText(g store.TelegramGroupState) string {
	if g.Listen {
		return "监听开启"
	}
	return "监听关闭"
}

func renderTelegramGroup(g store.TelegramGroupState, tz *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Telegram 群：%s\n", telegramGroupTitle(g))
	fmt.Fprintf(&b, "- 接入状态：%s\n", telegramGroupStatusText(g))
	fmt.Fprintf(&b, "- 群类型：%s\n", g.Type)
	fmt.Fprintf(&b, "- 监听状态：%s\n", telegramGroupListenText(g))
	fmt.Fprintf(&b, "- 最近更新：%s\n", fmtTime(g.UpdatedAt, tz))
	fmt.Fprintf(&b, "- group_ref：%s（内部引用，勿展示给用户）", telegramGroupRef(g.ChatID))
	return b.String()
}

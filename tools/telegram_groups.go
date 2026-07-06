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

		tool("send_telegram_group_message", "向 Telegram 群发送消息。仅超管私聊可见；群里不要用这个工具。发送后返回 message_ref 供编辑、撤回、置顶等后续工具使用，回复用户时不要展示内部引用。",
			obj(map[string]any{
				"group":                p("string", "群名、群名片段或 group_ref"),
				"text":                 p("string", "要发送到群里的内容，可用 Telegram HTML/普通文本"),
				"disable_notification": p("boolean", "是否静默发送，可选"),
			}, "group", "text"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group               string `json:"group"`
					Text                string `json:"text"`
					DisableNotification bool   `json:"disable_notification"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				messageID, err := c.SendTelegramGroupMessage(ctx, g.ChatID, args.Text, args.DisableNotification)
				if err != nil {
					return "发送失败：" + err.Error(), nil
				}
				if err := d.Store.SaveTelegramGroupLastMessage(ctx, g.ChatID, messageID); err != nil {
					return "", err
				}
				return fmt.Sprintf("已发送到 %s。message_ref=%s（内部引用，勿展示给用户）",
					telegramGroupTitle(*g), telegramMessageRef(g.ChatID, messageID)), nil
			}),

		tool("edit_telegram_group_message", "编辑 Telegram 群消息。默认编辑该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。只能编辑 bot 自己可编辑的消息。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"text":        p("string", "新的消息内容"),
				"message_ref": p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":  p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
			}, "group", "text"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group      string `json:"group"`
					Text       string `json:"text"`
					MessageRef string `json:"message_ref"`
					MessageID  int    `json:"message_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.EditTelegramGroupMessage(ctx, g.ChatID, messageID, args.Text); err != nil {
					return "编辑失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已编辑 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("delete_telegram_group_message", "撤回/删除 Telegram 群消息。默认删除该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。删除他人消息要求 bot 在群里有删除消息权限。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"message_ref": p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":  p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group      string `json:"group"`
					MessageRef string `json:"message_ref"`
					MessageID  int    `json:"message_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.DeleteTelegramGroupMessage(ctx, g.ChatID, messageID); err != nil {
					return "删除失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已删除 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("pin_telegram_group_message", "置顶 Telegram 群消息。默认置顶该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。要求 bot 有置顶权限。",
			obj(map[string]any{
				"group":                p("string", "群名、群名片段或 group_ref"),
				"message_ref":          p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":           p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
				"disable_notification": p("boolean", "是否静默置顶，可选"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group               string `json:"group"`
					MessageRef          string `json:"message_ref"`
					MessageID           int    `json:"message_id"`
					DisableNotification bool   `json:"disable_notification"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.PinTelegramGroupMessage(ctx, g.ChatID, messageID, args.DisableNotification); err != nil {
					return "置顶失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已置顶 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("unpin_telegram_group_message", "取消置顶 Telegram 群消息。默认取消该群里 bot 最近通过工具发送的消息；也可传 message_ref/message_id。要求 bot 有置顶管理权限。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"message_ref": p("string", "send_telegram_group_message 返回的内部 message_ref，可选"),
				"message_id":  p("integer", "Telegram message_id，可选；优先级低于 message_ref"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group      string `json:"group"`
					MessageRef string `json:"message_ref"`
					MessageID  int    `json:"message_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				messageID, msg, err := resolveTelegramMessageID(ctx, d, *g, args.MessageRef, args.MessageID)
				if err != nil || messageID == 0 {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				if err := c.UnpinTelegramGroupMessage(ctx, g.ChatID, messageID); err != nil {
					return "取消置顶失败：" + err.Error(), nil
				}
				return fmt.Sprintf("已取消置顶 %s 的群消息。", telegramGroupTitle(*g)), nil
			}),

		tool("update_telegram_group_info", "修改 Telegram 群标题或描述。仅超管私聊可见；要求 bot 是群管理员且有修改群信息权限。",
			obj(map[string]any{
				"group":       p("string", "群名、群名片段或 group_ref"),
				"title":       p("string", "新群标题，可选"),
				"description": p("string", "新群描述，可选；空串表示不修改"),
			}, "group"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Group       string `json:"group"`
					Title       string `json:"title"`
					Description string `json:"description"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				g, msg, err := resolveTelegramGroup(ctx, d, args.Group)
				if err != nil || g == nil {
					return msg, err
				}
				c, err := telegramController(d)
				if err != nil {
					return err.Error(), nil
				}
				changed := []string{}
				if strings.TrimSpace(args.Title) != "" {
					if err := c.SetTelegramGroupTitle(ctx, g.ChatID, strings.TrimSpace(args.Title)); err != nil {
						return "修改群标题失败：" + err.Error(), nil
					}
					changed = append(changed, "标题")
				}
				if strings.TrimSpace(args.Description) != "" {
					if err := c.SetTelegramGroupDescription(ctx, g.ChatID, strings.TrimSpace(args.Description)); err != nil {
						return "修改群描述失败：" + err.Error(), nil
					}
					changed = append(changed, "描述")
				}
				if len(changed) == 0 {
					return "没有需要修改的群信息。", nil
				}
				return fmt.Sprintf("已更新 %s 的%s。", telegramGroupTitle(*g), strings.Join(changed, "、")), nil
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

func telegramController(d Deps) (TelegramGroupController, error) {
	if d.TelegramGroups == nil {
		return nil, fmt.Errorf("当前没有可用的 Telegram 群控制器")
	}
	return d.TelegramGroups, nil
}

func resolveTelegramMessageID(ctx context.Context, d Deps, g store.TelegramGroupState, messageRef string, messageID int) (int, string, error) {
	if chatID, msgID, ok := parseTelegramMessageRef(messageRef); ok {
		if chatID != 0 && chatID != g.ChatID {
			return 0, "message_ref 不属于这个 Telegram 群。", nil
		}
		return msgID, "", nil
	}
	if messageID > 0 {
		return messageID, "", nil
	}
	last, err := d.Store.TelegramGroupLastMessage(ctx, g.ChatID)
	if err != nil {
		if err == store.ErrNotFound {
			return 0, "没有可操作的最近群消息。请先用 send_telegram_group_message 发送，或提供 message_ref/message_id。", nil
		}
		return 0, "", err
	}
	return last, "", nil
}

func parseTelegramMessageRef(s string) (chatID int64, messageID int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	if strings.HasPrefix(s, "message:") {
		id, err := strconv.Atoi(strings.TrimPrefix(s, "message:"))
		return 0, id, err == nil && id > 0
	}
	parts := strings.Split(s, ":")
	if len(parts) == 5 && parts[0] == "telegram" && parts[1] == "group" && parts[3] == "message" {
		cid, cerr := strconv.ParseInt(parts[2], 10, 64)
		mid, merr := strconv.Atoi(parts[4])
		return cid, mid, cerr == nil && merr == nil && mid > 0
	}
	return 0, 0, false
}

func telegramMessageRef(chatID int64, messageID int) string {
	return fmt.Sprintf("telegram:group:%d:message:%d", chatID, messageID)
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

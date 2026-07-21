package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	ihtml "github.com/zdypro888/ihtml"

	"github.com/zdypro888/nbco/ai"
)

func ihtmlAgentTools(svc ihtml.ScopedService, apis ...ihtml.APISpec) []ai.Tool {
	return []ai.Tool{
		ihtmlTool("ui_list_host_apis",
			"列出宿主登记、会自动携带当前用户身份的同源 HTTP API。构建需要实时业务数据的页面前调用；GET 使用 ihtml.http(path, options) 或 ihtml.http.get(path, options)，其他方法使用 ihtml.http.post/put(path, body, options) 或 ihtml.http.del(path, options)。",
			ai.ToolEffectRead, false, objectSchema(nil),
			func(context.Context, json.RawMessage) (string, error) {
				return marshalToolResult(apis)
			}),

		ihtmlTool("ui_list_state",
			"分页读取当前动态工作台的页面、UI Item 元数据和最近浏览器错误。修改界面前先调用；源码按需用 ui_get_item 分块读取，避免把整站代码塞进上下文。",
			ai.ToolEffectRead, false,
			objectSchema(map[string]any{
				"offset": property("integer", "Item 分页起点，默认 0"),
				"limit":  property("integer", "本页 Item 数，默认 20，最大 50"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Offset int `json:"offset"`
					Limit  int `json:"limit"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil {
					return "参数格式错误。", nil
				}
				items, err := svc.ListItems(ctx)
				if err != nil {
					return "", err
				}
				pages, err := svc.ListPages(ctx)
				if err != nil {
					return "", err
				}
				errs, err := svc.PageErrors(ctx)
				if err != nil {
					return "", err
				}
				if args.Offset < 0 {
					return "offset 不能小于 0。", nil
				}
				if args.Limit <= 0 {
					args.Limit = 20
				}
				if args.Limit > 50 {
					args.Limit = 50
				}
				if args.Offset > len(items) {
					args.Offset = len(items)
				}
				end := min(args.Offset+args.Limit, len(items))
				type itemView struct {
					ID         string            `json:"id"`
					Type       ihtml.ItemType    `json:"type"`
					Title      string            `json:"title,omitempty"`
					Page       string            `json:"page,omitempty"`
					Order      int               `json:"order"`
					Meta       map[string]string `json:"meta,omitempty"`
					ContentLen int               `json:"content_bytes"`
				}
				views := make([]itemView, 0, end-args.Offset)
				for _, item := range items[args.Offset:end] {
					view := itemView{ID: item.ID, Type: item.Type, Title: item.Title, Page: item.Page,
						Order: item.Order, Meta: item.Meta, ContentLen: len(item.Content)}
					views = append(views, view)
				}
				if len(errs) > 20 {
					errs = errs[:20]
				}
				return marshalToolResult(map[string]any{
					"item_count": len(items), "offset": args.Offset, "next_offset": end,
					"truncated": end < len(items), "pages": pages, "items": views, "recent_errors": errs,
				})
			}),

		ihtmlTool("ui_get_item", "按稳定 Item ID 和字符偏移分块读取现有 UI 源码，适合局部修改前精确检查；按 next_offset 继续读取直到 truncated=false。",
			ai.ToolEffectRead, false,
			objectSchema(map[string]any{
				"id":        property("string", "Item ID"),
				"offset":    property("integer", "源码字符起点，默认 0"),
				"max_chars": property("integer", "最多返回字符数，默认 8000，最大 10000"),
			}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID       string `json:"id"`
					Offset   int    `json:"offset"`
					MaxChars int    `json:"max_chars"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil || strings.TrimSpace(args.ID) == "" {
					return "id 必填。", nil
				}
				if args.Offset < 0 {
					return "offset 不能小于 0。", nil
				}
				if args.MaxChars <= 0 {
					args.MaxChars = 8000
				}
				if args.MaxChars > 10000 {
					args.MaxChars = 10000
				}
				items, err := svc.ListItems(ctx)
				if err != nil {
					return "", err
				}
				for _, item := range items {
					if item.ID == args.ID {
						runes := []rune(item.Content)
						if args.Offset > len(runes) {
							args.Offset = len(runes)
						}
						end := min(args.Offset+args.MaxChars, len(runes))
						return marshalToolResult(map[string]any{
							"id": item.ID, "type": item.Type, "title": item.Title, "page": item.Page,
							"order": item.Order, "meta": item.Meta, "content": string(runes[args.Offset:end]),
							"content_chars": len(runes), "offset": args.Offset, "next_offset": end,
							"truncated": end < len(runes), "updated_at": item.UpdatedAt,
						})
					}
				}
				return "Item 不存在。", nil
			}),

		ihtmlTool("ui_apply_items",
			"批量新增或按稳定 ID 覆盖 HTML/JS/CSS Item。它会保存可回滚修订并通过 SSE 实时上屏；只在用户明确要求创建或修改界面时调用。",
			ai.ToolEffectWrite, false,
			objectSchema(map[string]any{
				"items": map[string]any{"type": "array", "description": "要新增或覆盖的 Item", "items": ihtmlItemSchema()},
				"note":  property("string", "简短说明这次界面变更的目的"),
			}, "items"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Items []ihtml.Item `json:"items"`
					Note  string       `json:"note"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil {
					return "参数格式错误。", nil
				}
				if len(args.Items) == 0 {
					return "items 不能为空。", nil
				}
				if err := svc.PutItems(ctx, args.Items, "nbco-agent", strings.TrimSpace(args.Note)); err != nil {
					return "", err
				}
				ids := make([]string, 0, len(args.Items))
				for _, item := range args.Items {
					ids = append(ids, item.ID)
				}
				return marshalToolResult(map[string]any{"ok": true, "updated_ids": ids})
			}),

		ihtmlTool("ui_delete_items", "按稳定 ID 删除 UI Item；删除前自动留修订快照，可回滚。需要下一轮用户确认。",
			ai.ToolEffectWrite, true,
			objectSchema(map[string]any{
				"ids":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Item ID 列表"},
				"note": property("string", "删除原因"),
			}, "ids"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					IDs  []string `json:"ids"`
					Note string   `json:"note"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil || len(args.IDs) == 0 {
					return "ids 必填。", nil
				}
				deleted, err := svc.DeleteItems(ctx, args.IDs, "nbco-agent", strings.TrimSpace(args.Note))
				if err != nil {
					return "", err
				}
				return marshalToolResult(map[string]any{"ok": true, "deleted_ids": deleted})
			}),

		ihtmlTool("ui_set_page", "新增或更新动态工作台页面注册项；页面是菜单和 Item.page 的稳定容器。",
			ai.ToolEffectWrite, false,
			objectSchema(map[string]any{"page": ihtmlPageSchema()}, "page"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Page ihtml.Page `json:"page"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil {
					return "参数格式错误。", nil
				}
				if err := svc.SetPage(ctx, args.Page); err != nil {
					return "", err
				}
				return marshalToolResult(map[string]any{"ok": true, "page": args.Page.Name})
			}),

		ihtmlTool("ui_delete_page", "删除一个页面及其所属 Item；删除前自动留修订快照，需要下一轮用户确认。",
			ai.ToolEffectWrite, true,
			objectSchema(map[string]any{
				"name": property("string", "页面稳定名称"),
			}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil || strings.TrimSpace(args.Name) == "" {
					return "name 必填。", nil
				}
				deleted, err := svc.DeletePage(ctx, args.Name, "nbco-agent")
				if err != nil {
					return "", err
				}
				return marshalToolResult(map[string]any{"ok": true, "deleted_item_ids": deleted})
			}),

		ihtmlTool("ui_list_revisions", "列出动态工作台自动保存的历史快照，用于审计和选择回滚点。",
			ai.ToolEffectRead, false, objectSchema(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				revisions, err := svc.ListRevisions(ctx)
				if err != nil {
					return "", err
				}
				return marshalToolResult(revisions)
			}),

		ihtmlTool("ui_rollback", "把动态工作台整体恢复到指定修订；回滚前仍会保留当前快照，需要下一轮用户确认。",
			ai.ToolEffectWrite, true,
			objectSchema(map[string]any{"revision_id": property("string", "ui_list_revisions 返回的修订 ID")}, "revision_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					RevisionID string `json:"revision_id"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil || strings.TrimSpace(args.RevisionID) == "" {
					return "revision_id 必填。", nil
				}
				if err := svc.Rollback(ctx, args.RevisionID, "nbco-agent"); err != nil {
					return "", err
				}
				return marshalToolResult(map[string]any{"ok": true, "revision_id": args.RevisionID})
			}),
	}
}

func ihtmlTool(name, description, effect string, approval bool, schema map[string]any,
	handler func(context.Context, json.RawMessage) (string, error)) ai.Tool {
	return ai.Tool{
		Name: name, Description: description, Domain: "ui", Effect: effect,
		GroupSensitive: true, ApprovalRequired: approval,
		InputSchema: schema, Handler: handler,
	}
}

func ihtmlItemSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":      property("string", "稳定语义 ID；覆盖同一功能时复用原 ID"),
			"type":    map[string]any{"type": "string", "enum": []string{"html", "css", "js"}},
			"title":   property("string", "人类可读用途"),
			"page":    property("string", "所属页面名；空表示全局"),
			"order":   property("integer", "应用顺序，小值优先"),
			"content": property("string", "HTML、CSS 或 JavaScript 源码"),
			"meta": map[string]any{
				"type": "object", "description": "可选运行提示，如 slot 或 module",
				"additionalProperties": map[string]any{"type": "string"},
			},
		},
		"required": []string{"id", "type", "content"},
	}
}

func ihtmlPageSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        property("string", "稳定路由名，小写字母数字及 -_"),
			"title":       property("string", "菜单标题"),
			"icon":        property("string", "可选短图标文本"),
			"order":       property("integer", "菜单顺序"),
			"template_id": property("string", "可选布局模板 ID"),
		},
		"required": []string{"name", "title"},
	}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func property(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func defaultObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func marshalToolResult(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("编码 UI 工具结果: %w", err)
	}
	return string(raw), nil
}

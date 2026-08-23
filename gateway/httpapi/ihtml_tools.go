package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	ihtml "github.com/zdypro888/ihtml"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/interaction"
)

type ihtmlAgentToolOptions struct {
	APIs          []ihtml.APISpec
	PublicBaseURL string
}

type ihtmlPageInspection struct {
	Registered    bool              `json:"registered"`
	Page          *ihtml.Page       `json:"page,omitempty"`
	ItemCount     int               `json:"item_count"`
	ItemIDs       []string          `json:"item_ids"`
	ContentBytes  int               `json:"content_bytes"`
	LastUpdatedAt *time.Time        `json:"last_updated_at,omitempty"`
	RecentErrors  []ihtml.PageError `json:"recent_errors"`
	WorkspaceURL  string            `json:"workspace_url"`
}

func ihtmlWorkspaceURL(publicBaseURL, page string) string {
	query := url.Values{"view": []string{"workspace"}}
	if ihtml.ValidatePageName(page) == nil {
		query.Set("workspace_page", page)
	}
	path := "/?" + query.Encode()
	base := strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if base == "" {
		return path
	}
	return base + path
}

func inspectIHTMLPage(ctx context.Context, svc ihtml.ScopedService, publicBaseURL, name string) (ihtmlPageInspection, error) {
	result := ihtmlPageInspection{WorkspaceURL: ihtmlWorkspaceURL(publicBaseURL, name), ItemIDs: []string{}, RecentErrors: []ihtml.PageError{}}
	pages, err := svc.ListPages(ctx)
	if err != nil {
		return result, err
	}
	for i := range pages {
		if pages[i].Name == name {
			page := pages[i]
			result.Registered = true
			result.Page = &page
			break
		}
	}
	items, err := svc.ListItems(ctx)
	if err != nil {
		return result, err
	}
	itemUpdatedAt := make(map[string]time.Time)
	var latest time.Time
	for _, item := range items {
		if item.Page != name {
			continue
		}
		result.ItemCount++
		result.ItemIDs = append(result.ItemIDs, item.ID)
		result.ContentBytes += len(item.Content)
		itemUpdatedAt[item.ID] = item.UpdatedAt
		if item.UpdatedAt.After(latest) {
			latest = item.UpdatedAt
		}
	}
	if !latest.IsZero() {
		result.LastUpdatedAt = &latest
	}
	slices.Sort(result.ItemIDs)
	errs, err := svc.PageErrors(ctx)
	if err != nil {
		return result, err
	}
	for _, pageErr := range errs {
		updatedAt, ok := itemUpdatedAt[pageErr.ItemID]
		if !ok || pageErr.Time.Before(updatedAt) {
			continue
		}
		result.RecentErrors = append(result.RecentErrors, pageErr)
		if len(result.RecentErrors) == 20 {
			break
		}
	}
	return result, nil
}

func jsonValueShape(raw json.RawMessage) (kind string, entries int) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "invalid", 0
	}
	switch typed := value.(type) {
	case []any:
		return "array", len(typed)
	case map[string]any:
		return "object", len(typed)
	case nil:
		return "null", 0
	case string:
		return "string", 0
	case bool:
		return "boolean", 0
	default:
		return "number", 0
	}
}

func ihtmlAgentTools(svc ihtml.ScopedService, options ihtmlAgentToolOptions) []ai.Tool {
	apis := options.APIs
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
				type pageView struct {
					ihtml.Page
					WorkspaceURL string `json:"workspace_url"`
				}
				pageViews := make([]pageView, 0, len(pages))
				for _, page := range pages {
					pageViews = append(pageViews, pageView{Page: page, WorkspaceURL: ihtmlWorkspaceURL(options.PublicBaseURL, page.Name)})
				}
				return marshalToolResult(map[string]any{
					"item_count": len(items), "offset": args.Offset, "next_offset": end,
					"truncated": end < len(items), "pages": pageViews, "items": views, "recent_errors": errs,
				})
			}),

		ihtmlTool("ui_get_data", "读取动态工作台的结构化 JSON 数据。数据与 HTML/CSS/JS 分离，页面运行时可用 ihtml.kv.get(key) 读取。",
			ai.ToolEffectRead, false,
			objectSchema(map[string]any{"key": property("string", "稳定数据键，字母数字及 ._-")}, "key"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Key string `json:"key"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil || strings.TrimSpace(args.Key) == "" {
					return "key 必填。", nil
				}
				key := strings.TrimSpace(args.Key)
				var (
					value    json.RawMessage
					revision string
					err      error
				)
				if versioned, ok := svc.(ihtml.ScopedVersionedKVService); ok {
					value, revision, err = versioned.KVGetWithRevision(ctx, key)
				} else {
					value, err = svc.KVGet(ctx, key)
				}
				if errors.Is(err, ihtml.ErrNotFound) {
					return marshalToolResult(map[string]any{"exists": false, "key": key})
				}
				if err != nil {
					return "", err
				}
				kind, entries := jsonValueShape(value)
				return marshalToolResult(map[string]any{
					"exists": true, "key": key, "value": value, "bytes": len(value),
					"kind": kind, "entries": entries, "revision": revision,
				})
			}),

		ihtmlTool("ui_put_data", "写入动态工作台使用的结构化 JSON 数据。数据量较大或需要反复更新时，先存数据，再让页面通过 ihtml.kv.get(key) 加载，避免把业务记录硬编码进页面源码。结果中的 committed 表示写入已提交；verified=false 时只需重新读取核对，不要盲目重写。",
			ai.ToolEffectWrite, false,
			objectSchema(map[string]any{
				"key":               property("string", "稳定数据键，字母数字及 ._-"),
				"value_json":        property("string", "完整且有效的 JSON 文本"),
				"expected_revision": property("string", "可选：ui_get_data 返回的版本；提供时执行原子条件写"),
			}, "key", "value_json"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Key              string `json:"key"`
					ValueJSON        string `json:"value_json"`
					ExpectedRevision string `json:"expected_revision"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil {
					return "参数格式错误。", nil
				}
				key := strings.TrimSpace(args.Key)
				value := json.RawMessage(strings.TrimSpace(args.ValueJSON))
				if key == "" || len(value) == 0 || !json.Valid(value) {
					return "key 必填，value_json 必须是有效 JSON。", nil
				}
				revision := ""
				if expected := strings.TrimSpace(args.ExpectedRevision); expected != "" {
					versioned, ok := svc.(ihtml.ScopedVersionedKVService)
					if !ok {
						return "当前存储不支持条件写。", nil
					}
					var err error
					revision, err = versioned.KVCompareAndSet(ctx, key, value, expected)
					if err != nil {
						return "", err
					}
				} else {
					if err := svc.KVSet(ctx, key, value); err != nil {
						return "", err
					}
				}
				verified := false
				verificationError := ""
				persisted := value
				if versioned, ok := svc.(ihtml.ScopedVersionedKVService); ok {
					current, currentRevision, verifyErr := versioned.KVGetWithRevision(ctx, key)
					if verifyErr != nil {
						verificationError = verifyErr.Error()
					} else {
						persisted, revision = current, currentRevision
						verified = bytes.Equal(current, value)
						if !verified {
							verificationError = "写入后该数据已被另一轮更新"
						}
					}
				} else {
					current, verifyErr := svc.KVGet(ctx, key)
					if verifyErr != nil {
						verificationError = verifyErr.Error()
					} else {
						persisted = current
						verified = bytes.Equal(current, value)
						if !verified {
							verificationError = "写入后该数据已被另一轮更新"
						}
					}
				}
				kind, entries := jsonValueShape(persisted)
				return marshalToolResult(map[string]any{
					"ok": true, "committed": true, "verified": verified,
					"verification_error": verificationError, "key": key,
					"bytes": len(persisted), "kind": kind, "entries": entries, "revision": revision,
				})
			}),

		ihtmlTool("ui_delete_data", "删除动态工作台的一项结构化数据；调用前确认没有页面仍依赖该 key。需要下一轮用户确认。",
			ai.ToolEffectWrite, true,
			objectSchema(map[string]any{"key": property("string", "要删除的数据键")}, "key"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Key string `json:"key"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil || strings.TrimSpace(args.Key) == "" {
					return "key 必填。", nil
				}
				key := strings.TrimSpace(args.Key)
				if err := svc.KVDelete(ctx, key); err != nil {
					return "", err
				}
				return marshalToolResult(map[string]any{"ok": true, "key": key})
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
			"批量新增或按稳定 ID 局部覆盖 HTML/JS/CSS Item。它会保存可回滚修订并通过 SSE 实时上屏；创建或整体更新一个页面时改用 ui_publish_page，避免页面注册和内容分步提交。",
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
				pageURLs := make(map[string]string)
				for _, item := range args.Items {
					ids = append(ids, item.ID)
					if item.Page != "" {
						pageURLs[item.Page] = ihtmlWorkspaceURL(options.PublicBaseURL, item.Page)
					}
				}
				return marshalToolResult(map[string]any{"ok": true, "updated_ids": ids, "workspace_urls": pageURLs})
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

		ihtmlPageTool("ui_publish_page", "原子发布一个完整页面：一次提交同时新增或更新页面注册项，并完整替换该页所有 Item；全局和其他页面不受影响。创建页面、生成报表或整体更新页面时优先使用。结果中的 committed 表示发布已提交；verified=false 时只调用 ui_inspect_page 复核，不要盲目重复发布。",
			ai.ToolEffectWrite, false,
			objectSchema(map[string]any{
				"page":  ihtmlPageSchema(),
				"items": map[string]any{"type": "array", "description": "该页完整 Item 集合；每项 page 必须等于 page.name", "items": ihtmlItemSchema()},
				"note":  property("string", "简短说明这次完整页面发布的目的"),
			}, "page", "items"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Page  ihtml.Page   `json:"page"`
					Items []ihtml.Item `json:"items"`
					Note  string       `json:"note"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil {
					return "参数格式错误。", nil
				}
				if len(args.Items) == 0 {
					return "items 不能为空。", nil
				}
				publisher, ok := svc.(ihtml.ScopedPagePublicationService)
				if !ok {
					return "当前 ihtml 版本不支持原子页面发布。", nil
				}
				if err := publisher.PublishPage(ctx, args.Page, args.Items, "nbco-agent", strings.TrimSpace(args.Note)); err != nil {
					return "", err
				}
				inspection, err := inspectIHTMLPage(ctx, svc, options.PublicBaseURL, args.Page.Name)
				if err != nil {
					return marshalToolResult(map[string]any{
						"ok": true, "committed": true, "verified": false,
						"page": args.Page.Name, "workspace_url": ihtmlWorkspaceURL(options.PublicBaseURL, args.Page.Name),
						"verification_error": err.Error(),
					})
				}
				return marshalToolResult(map[string]any{
					"ok": true, "committed": true, "verified": true,
					"page": inspection.Page, "registered": inspection.Registered,
					"item_count": inspection.ItemCount, "item_ids": inspection.ItemIDs,
					"content_bytes": inspection.ContentBytes, "last_updated_at": inspection.LastUpdatedAt,
					"recent_errors": inspection.RecentErrors, "workspace_url": inspection.WorkspaceURL,
				})
			}),

		ihtmlPageTool("ui_inspect_page", "核对一个页面持久化后的注册项、Item 数量与 ID、源码总字节、最近运行错误和可直达地址。发布后调用它确认实际结果；它不虚构浏览器渲染状态。",
			ai.ToolEffectRead, false,
			objectSchema(map[string]any{"name": property("string", "页面稳定名称")}, "name"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(defaultObject(raw), &args); err != nil || ihtml.ValidatePageName(strings.TrimSpace(args.Name)) != nil {
					return "name 必须是有效页面名。", nil
				}
				inspection, err := inspectIHTMLPage(ctx, svc, options.PublicBaseURL, strings.TrimSpace(args.Name))
				if err != nil {
					return "", err
				}
				return marshalToolResult(inspection)
			}),

		ihtmlPageTool("ui_set_page", "仅新增或更新动态工作台页面的注册元数据（标题、图标、排序、模板）；不会写入页面内容。创建或整体更新页面应使用 ui_publish_page。",
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
				return marshalToolResult(map[string]any{
					"ok": true, "page": args.Page.Name,
					"workspace_url": ihtmlWorkspaceURL(options.PublicBaseURL, args.Page.Name),
				})
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

func ihtmlPageTool(name, description, effect string, approval bool, schema map[string]any,
	handler func(context.Context, json.RawMessage) (string, error)) ai.Tool {
	tool := ihtmlTool(name, description, effect, approval, schema, handler)
	tool.PresentResult = ihtmlPageActions
	return tool
}

func ihtmlPageActions(result string) []interaction.Action {
	var payload struct {
		Registered    *bool             `json:"registered"`
		WorkspaceURL  string            `json:"workspace_url"`
		WorkspaceURLs map[string]string `json:"workspace_urls"`
		Page          json.RawMessage   `json:"page"`
	}
	if json.Unmarshal([]byte(result), &payload) != nil || payload.Registered != nil && !*payload.Registered {
		return nil
	}
	title := "页面"
	if len(payload.Page) > 0 {
		var page struct {
			Name  string `json:"name"`
			Title string `json:"title"`
		}
		if json.Unmarshal(payload.Page, &page) == nil {
			title = strings.TrimSpace(page.Title)
			if title == "" {
				title = strings.TrimSpace(page.Name)
			}
		} else {
			var name string
			if json.Unmarshal(payload.Page, &name) == nil && strings.TrimSpace(name) != "" {
				title = strings.TrimSpace(name)
			}
		}
	}
	actions := make([]interaction.Action, 0, 1+len(payload.WorkspaceURLs))
	if payload.WorkspaceURL != "" {
		actions = append(actions, interaction.Action{
			Kind: interaction.ActionOpenWebApp, Label: "打开" + title, URL: payload.WorkspaceURL,
		})
	}
	pages := make([]string, 0, len(payload.WorkspaceURLs))
	for page := range payload.WorkspaceURLs {
		pages = append(pages, page)
	}
	slices.Sort(pages)
	for _, page := range pages {
		actions = append(actions, interaction.Action{
			Kind: interaction.ActionOpenWebApp, Label: "打开" + page, URL: payload.WorkspaceURLs[page],
		})
	}
	return interaction.Normalize(actions, 4)
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
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", fmt.Errorf("编码 UI 工具结果: %w", err)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

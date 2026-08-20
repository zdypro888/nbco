package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

func fileTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("search_workspace", "由 AI 规划查询词，跨任务、文件和项目检索当前用户可访问的候选对象，不要求先猜类型或内部ID。适合名称片段、错别字、上下文指代和修改/删除前消歧；工具只返回候选，最终对象仍由主 Agent 判断。",
			obj(map[string]any{
				"query": p("string", "名称或名称片段"),
				"limit": p("integer", "最多返回条数，默认 12，最多 30"),
			}, "query"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Query) == "" {
					return "query 不能为空。", nil
				}
				if args.Limit <= 0 || args.Limit > 30 {
					args.Limit = 12
				}
				plan := planSemanticSearch(ctx, d, u, args.Query, []string{"task", "file", "project"})
				items, err := d.Store.WorkspaceCandidates(ctx, u.ID, u.IsSuperadmin, store.WorkspaceCandidateFilter{
					Terms: plan.Terms, Kinds: plan.Kinds, Limit: args.Limit,
				})
				if err != nil {
					return "", err
				}
				fallback := false
				if len(items) == 0 && len(plan.Terms) > 0 {
					items, err = d.Store.WorkspaceCandidates(ctx, u.ID, u.IsSuperadmin, store.WorkspaceCandidateFilter{
						Limit: args.Limit,
					})
					if err != nil {
						return "", err
					}
					fallback = true
				}
				return renderWorkspaceResources(items, d.TZ, plan, fallback), nil
			}),

		tool("list_recent_files", "查看当前用户最近的文件接收队列，并解析对近期文件的上下文指代。返回成功保存的系统 file_id 和未入库文件的失败原因；只有 status=saved 的文件才能派 worker 分析。",
			obj(map[string]any{
				"limit":       p("integer", "返回条数，默认 10，最多 50"),
				"since_hours": p("integer", "只看最近多少小时，默认 24"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Limit      int `json:"limit"`
					SinceHours int `json:"since_hours"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.SinceHours <= 0 || args.SinceHours > 24*30 {
					args.SinceHours = 24
				}
				fs, err := d.Store.RecentFilesByUser(ctx, u.ID, args.Limit, time.Now().Add(-time.Duration(args.SinceHours)*time.Hour))
				if err != nil {
					return "", err
				}
				intakes, err := d.Store.RecentFileIntakesByUser(ctx, u.ID, args.Limit, time.Now().Add(-time.Duration(args.SinceHours)*time.Hour))
				if err != nil {
					return "", err
				}
				return renderFileQueue(fs, intakes, d.TZ), nil
			}),

		tool("send_file", "把系统文件库里的文件发送给用户。适合把 worker 产物、整理好的表格/文档发回 Telegram；发送前先确认 file_id 是系统文件 ID。发给别人需要 send_msg 权限。",
			obj(map[string]any{
				"user_id": p("integer", "收件人用户ID；省略或 0 表示发给当前用户"),
				"file_id": p("integer", "系统文件ID"),
				"caption": p("string", "文件说明，可选"),
			}, "file_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID  int64  `json:"user_id"`
					FileID  int64  `json:"file_id"`
					Caption string `json:"caption"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if args.FileID <= 0 {
					return "file_id 必须是系统文件 ID。", nil
				}
				targetID := args.UserID
				if targetID == 0 {
					targetID = u.ID
				}
				if targetID != u.ID && !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActSendMsg, targetID) {
						return "你没有向该用户发送文件的 send_msg 权限。", nil
					}
				}
				if _, err := mustUser(ctx, d.Store, targetID); err != nil {
					return err.Error(), nil
				}
				ok, err := d.Store.UserCanAccessFile(ctx, u.ID, u.IsSuperadmin, args.FileID)
				if err != nil {
					return "", err
				}
				if !ok {
					return "你无权访问这个文件。", nil
				}
				fn, ok := d.Notifier.(notify.FileNotifier)
				if !ok || fn == nil {
					return "当前通知通道不支持发送文件。", nil
				}
				delivery, err := notify.SendFileForToolInvocation(ctx, d.Store, fn, "send-file", targetID, args.FileID, args.Caption)
				if err != nil || !delivery.Delivered {
					if err == nil {
						err = fmt.Errorf("投递结果不确定，系统未自动重发")
					}
					return "发送文件失败：" + err.Error(), nil
				}
				if targetID == u.ID {
					return "已发送文件给你。", nil
				}
				return "已发送文件。", nil
			}),

		tool("delete_file", "删除系统文件库中的文件记录。调用前用 search_workspace 或 list_recent_files 确认系统 file_id；普通用户只能删除自己上传的文件，超级管理员可删除任意未被任务附件/产物引用的文件。",
			obj(map[string]any{
				"file_id": p("integer", "search_workspace 或 list_recent_files 返回的系统文件ID"),
			}, "file_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					FileID int64 `json:"file_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				f, err := d.Store.FileByID(ctx, args.FileID)
				if errors.Is(err, store.ErrNotFound) {
					return "文件不存在或已经删除。", nil
				}
				if err != nil {
					return "", err
				}
				if !u.IsSuperadmin && (f.CreatedBy == nil || *f.CreatedBy != u.ID) {
					return "你只能删除自己上传的文件。", nil
				}
				if err := d.Store.DeleteUnreferencedFile(ctx, f.ID); err != nil {
					if errors.Is(err, store.ErrConflict) {
						return "该文件仍被任务附件或任务产物引用，不能直接删除。", nil
					}
					if errors.Is(err, store.ErrNotFound) {
						return "文件不存在或已经删除。", nil
					}
					return "", err
				}
				return fmt.Sprintf("已删除文件「%s」。", f.OriginalName), nil
			}),
	}
}

func renderWorkspaceResources(items []store.WorkspaceResource, tz *time.Location, plan semanticSearchPlan, fallback bool) string {
	if len(items) == 0 {
		return "（没有匹配的现存任务、文件或项目）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "AI 查询计划：terms=%q kinds=%q recent=%t。\n", plan.Terms, plan.Kinds, plan.Recent)
	if fallback {
		b.WriteString("字面候选为空，已移除查询词和类型限制，以下是按时间返回的可访问候选；请由主 Agent 结合上下文判断，不要直接猜。\n")
	}
	b.WriteString("工作区匹配结果：\n")
	for _, item := range items {
		fmt.Fprintf(&b, "- resource_ref=%s:%d；类型=%s；名称=%s；状态=%s；创建时间=%s\n",
			item.Kind, item.ID, item.Kind, item.Name, item.State, fmtTime(item.CreatedAt, tz))
	}
	b.WriteString("resource_ref 仅供后续工具定位对象；不要把内部引用主动展示给用户。")
	return strings.TrimSpace(b.String())
}

func renderFileQueue(fs []store.File, intakes []store.FileIntake, tz *time.Location) string {
	var b strings.Builder
	if len(fs) > 0 {
		b.WriteString(renderFileList(fs, tz))
	}
	failed := make([]store.FileIntake, 0, len(intakes))
	for _, in := range intakes {
		if in.Status != store.FileIntakeSaved {
			failed = append(failed, in)
		}
	}
	if len(failed) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("未进入系统的文件接收记录：\n")
		for _, in := range failed {
			fmt.Fprintf(&b, "- %s（%s，%s）status=%s；原因=%s；时间=%s\n",
				in.OriginalName, formatBytes(in.SizeBytes), in.MIMEType, in.Status,
				fileIntakeReason(in), fmtTime(in.CreatedAt, tz))
		}
		b.WriteString("这些记录没有系统 file_id，不能读取或分析；不得声称已经收到文件内容。Telegram 失败消息会提供真实的“打开文件中心”按钮；不要编造 /upload 等命令或链接。")
	}
	if b.Len() == 0 {
		return "（最近没有文件接收记录）"
	}
	return strings.TrimSpace(b.String())
}

func fileIntakeReason(in store.FileIntake) string {
	if reason := strings.TrimSpace(in.ErrorMessage); reason != "" {
		return reason
	}
	if in.Status == store.FileIntakePending {
		return "仍在接收中，尚无可用文件内容"
	}
	return "没有可用文件内容"
}

func renderFileList(fs []store.File, tz *time.Location) string {
	if len(fs) == 0 {
		return "（最近没有上传文件）"
	}
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "- %s：%s（%s，%s，%s）\n", internalRef("文件", f.ID), f.OriginalName, formatBytes(f.SizeBytes), f.MIMEType, fmtTime(f.CreatedAt, tz))
	}
	return strings.TrimSpace(b.String())
}

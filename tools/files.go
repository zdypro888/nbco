package tools

import (
	"context"
	"encoding/json"
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
		tool("list_recent_files", "查看当前用户最近的文件接收队列，包括成功保存的系统 file_id 和未入库文件的失败原因。用户说“刚才那个文件/这些资料/附件”时先调用；只有 status=saved 的文件才能派 worker 分析。",
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

		tool("send_file", "把 nbco 文件库里的文件发送给用户。适合把 worker 产物、整理好的表格/文档发回 Telegram；发送前先确认 file_id 是系统文件 ID。发给别人需要 send_msg 权限。",
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
				if err := fn.SendFile(ctx, targetID, args.FileID, args.Caption); err != nil {
					return "", err
				}
				if targetID == u.ID {
					return "已发送文件给你。", nil
				}
				return "已发送文件。", nil
			}),
	}
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
		b.WriteString("未进入 nbco 的文件接收记录：\n")
		for _, in := range failed {
			fmt.Fprintf(&b, "- %s（%s，%s）status=%s；原因=%s；时间=%s\n",
				in.OriginalName, formatBytes(in.SizeBytes), in.MIMEType, in.Status,
				fileIntakeReason(in), fmtTime(in.CreatedAt, tz))
		}
		b.WriteString("这些记录没有系统 file_id，不能读取或分析；不得声称已经收到文件内容。Telegram 失败消息会提供真实的“打开 nbco 文件中心”按钮；不要编造 /upload 等命令或链接。")
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

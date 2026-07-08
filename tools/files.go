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
		tool("list_recent_files", "查看当前用户最近上传到 nbco 的文件队列。用户说“刚才那两个文件/这些资料/附件”时先用它确认 file_id；需要读文件内容时再决定是否派 worker。",
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
				return renderFileList(fs, d.TZ), nil
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

func renderFileList(fs []store.File, tz *time.Location) string {
	if len(fs) == 0 {
		return "（最近没有上传文件）"
	}
	var b strings.Builder
	for _, f := range fs {
		fmt.Fprintf(&b, "- #%d %s（%s，%s，%s）\n", f.ID, f.OriginalName, formatBytes(f.SizeBytes), f.MIMEType, fmtTime(f.CreatedAt, tz))
	}
	return strings.TrimSpace(b.String())
}

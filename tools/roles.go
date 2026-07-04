package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

// roleTools 角色/Skill。列出与激活对所有人开放；增删改超管专用（在 adminTools 里）。
func roleTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("list_roles", "查看所有可激活的 AI 角色/Skill。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				roles, err := d.Store.ListRoles(ctx)
				if err != nil {
					return "", err
				}
				if len(roles) == 0 {
					return "（无角色）", nil
				}
				var b strings.Builder
				for _, r := range roles {
					fmt.Fprintf(&b, "- %s：%s\n", r.Name, r.TriggerDesc)
				}
				return b.String(), nil
			}),

		tool("activate_role", "激活一个角色/Skill（AI 将按该角色思维工作，下轮对话生效）。传空名字表示取消激活。",
			obj(map[string]any{"name": p("string", "角色名；空串取消激活")}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name string `json:"name"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Name) == "" {
					if err := d.Store.DeactivateRole(ctx, u.ID); err != nil {
						return "", err
					}
					return "已取消角色激活。", nil
				}
				if err := d.Store.ActivateRole(ctx, u.ID, args.Name); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Sprintf("角色 %q 不存在。", args.Name), nil
					}
					return "", err
				}
				return fmt.Sprintf("已激活角色「%s」，下轮对话生效。", args.Name), nil
			}),

		tool("generate_api_token", "生成我的 API Token（用于 HTTP API/MCP 接入），会替换旧 token。明文仅返回一次。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				plain, err := d.Store.IssueAPIToken(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return "已生成 API Token（请妥善保存，仅显示一次）：\n" + plain, nil
			}),

		tool("revoke_api_token", "撤销我的 API Token。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				if err := d.Store.RevokeAPIToken(ctx, u.ID); err != nil {
					return "", err
				}
				return "已撤销。", nil
			}),
	}
}

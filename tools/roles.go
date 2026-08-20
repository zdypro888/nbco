package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

const generatedAPITokenPrefix = "已生成 Access Token（请妥善保存，仅显示一次）：\n"

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

		tool("get_api_token_status", "查看我是否已有控制中心/API Access Token。只返回是否存在和创建时间；明文不可查询，因为系统只保存哈希。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				st, err := d.Store.APITokenStatus(ctx, u.ID)
				if err != nil {
					return "", err
				}
				if !st.Exists {
					return "当前没有控制中心/API Access Token。系统无法查询旧 token 明文；如需登录控制中心，请使用 generate_api_token 换发一个新的。", nil
				}
				return fmt.Sprintf("当前已有控制中心/API Access Token，创建时间：%s。\n明文不可查询（系统只保存哈希）；如果忘记了，只能用 generate_api_token 换发新 token，旧 token 会立即失效。", fmtTime(st.CreatedAt, d.TZ)), nil
			}),

		recoverableResultToolWithFinalize(tool("generate_api_token", "换发我的控制中心/API Access Token（用于 HTTP API/MCP/控制中心登录），会立即替换旧 token。不能用于查询旧 token；明文仅返回一次，格式不要臆测。",
			obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				if toolInvocationRequestKey(ctx, u.ID, "generate_api_token") == "" {
					plain, err := d.Store.IssueAPIToken(ctx, u.ID)
					if err != nil {
						return "", err
					}
					return generatedAPITokenPrefix + plain, nil
				}
				if _, err := d.Store.BeginAPITokenRotation(ctx, u.ID, 10*time.Minute); err != nil {
					return "", err
				}
				rotation, err := d.Store.ConfirmAPITokenRotation(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return generatedAPITokenPrefix + rotation.Candidate, nil
			}), func(ctx context.Context, _ json.RawMessage) (string, bool, error) {
			rotation, err := d.Store.ConfirmAPITokenRotation(ctx, u.ID)
			if errors.Is(err, store.ErrNotFound) {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
			return generatedAPITokenPrefix + rotation.Candidate, true, nil
		}, func(ctx context.Context, _ json.RawMessage, result string) error {
			candidate := strings.TrimSpace(strings.TrimPrefix(result, generatedAPITokenPrefix))
			if candidate == "" || candidate == strings.TrimSpace(result) {
				return nil
			}
			return d.Store.AcknowledgeAPITokenRotation(ctx, u.ID, candidate)
		}),

		tool("revoke_api_token", "撤销我的 Access Token。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				if err := d.Store.RevokeAPIToken(ctx, u.ID); err != nil {
					return "", err
				}
				return "已撤销。", nil
			}),
	}
}

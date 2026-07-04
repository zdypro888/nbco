package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/perm"
	"github.com/zdypro888/nbco/internal/store"
)

// profileTools 自己的信息与画像。
func profileTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("get_my_profile", "查询自己的基本信息（不含画像）。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				me, err := d.Store.UserByID(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return renderUser(me), nil
			}),

		tool("update_my_profile", "更新自己的基本信息。传字段名到值的映射；值为空串表示清除该字段。",
			obj(map[string]any{
				"name":   p("string", "新名字（可选）"),
				"fields": map[string]any{"type": "object", "description": "动态字段名→值", "additionalProperties": map[string]any{"type": "string"}},
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name   string            `json:"name"`
					Fields map[string]string `json:"fields"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				defined, err := d.Store.ListInfoFields(ctx)
				if err != nil {
					return "", err
				}
				allowed := map[string]bool{}
				for _, f := range defined {
					allowed[f] = true
				}
				for k := range args.Fields {
					if !allowed[k] {
						return fmt.Sprintf("字段 %q 未定义。可用字段: %s", k, strings.Join(defined, ", ")), nil
					}
				}
				if args.Name != "" {
					if err := d.Store.UpdateUserName(ctx, u.ID, args.Name); err != nil {
						return "", err
					}
				}
				if len(args.Fields) > 0 {
					if err := d.Store.UpdateUserInfo(ctx, u.ID, args.Fields); err != nil {
						return "", err
					}
				}
				return "已更新。", nil
			}),

		tool("get_my_infos", "查看自己的自我介绍列表。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				ps, err := d.Store.ProfilesBy(ctx, u.ID, u.ID)
				if err != nil {
					return "", err
				}
				return renderProfiles(ps), nil
			}),

		tool("save_my_infos", "保存自己的自我介绍列表（整体替换）。",
			obj(map[string]any{"infos": arr("string", "自我介绍条目")}, "infos"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Infos []string `json:"infos"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if err := d.Store.ReplaceProfiles(ctx, u.ID, u.ID, args.Infos); err != nil {
					return "", err
				}
				return fmt.Sprintf("已保存 %d 条自我介绍。", len(args.Infos)), nil
			}),

		tool("view_user_infos", "查看某用户的画像（自我介绍+他人评价）。只返回你有权查看的部分。",
			obj(map[string]any{"user_id": p("integer", "目标用户ID")}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64 `json:"user_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				subject, err := d.Store.UserByID(ctx, args.UserID)
				if err != nil {
					return fmt.Sprintf("用户 %d 不存在", args.UserID), nil
				}
				all, err := d.Store.ProfilesOn(ctx, subject.ID)
				if err != nil {
					return "", err
				}
				viewerActive, err := d.Store.PermsOf(ctx, u.ID)
				if err != nil {
					return "", err
				}
				subjectPassive, err := d.Store.PassivePermsToward(ctx, subject.ID)
				if err != nil {
					return "", err
				}
				var visible []store.Profile
				for _, pr := range all {
					if perm.CanViewProfile(u.ID, subject.ID, pr.AuthorID, u.IsSuperadmin, viewerActive, subjectPassive) {
						visible = append(visible, pr)
					}
				}
				if len(visible) == 0 {
					return "没有你可见的画像。", nil
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s（ID %d）的画像：\n", subject.Name, subject.ID)
				for _, pr := range visible {
					who := fmt.Sprintf("作者 %d", pr.AuthorID)
					if pr.AuthorID == subject.ID {
						who = "自我介绍"
					}
					fmt.Fprintf(&b, "- [%s] %s\n", who, pr.Content)
				}
				return b.String(), nil
			}),

		tool("get_my_infos_on_user", "查看我对某用户写的画像列表。",
			obj(map[string]any{"user_id": p("integer", "目标用户ID")}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64 `json:"user_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				ps, err := d.Store.ProfilesBy(ctx, args.UserID, u.ID)
				if err != nil {
					return "", err
				}
				return renderProfiles(ps), nil
			}),

		tool("save_infos_on_user", "保存我对某用户的画像列表（整体替换）。需要 write_profile 主动权限。",
			obj(map[string]any{
				"user_id": p("integer", "目标用户ID"),
				"infos":   arr("string", "画像条目"),
			}, "user_id", "infos"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64    `json:"user_id"`
					Infos  []string `json:"infos"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if _, err := mustUser(ctx, d.Store, args.UserID); err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if !perm.CheckActive(grants, perm.ActWriteProfile, args.UserID) {
						return "你没有对该用户的 write_profile 权限。", nil
					}
				}
				if err := d.Store.ReplaceProfiles(ctx, args.UserID, u.ID, args.Infos); err != nil {
					return "", err
				}
				return fmt.Sprintf("已保存 %d 条画像。", len(args.Infos)), nil
			}),
	}
}

func renderUser(u *store.User) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ID: %d\n名字: %s\n状态: %s\n", u.ID, u.Name, u.Status)
	if u.IsSuperadmin {
		b.WriteString("身份: 超级管理员\n")
	}
	// 字段按名排序，输出稳定（利于模型缓存与人眼比对）。
	keys := make([]string, 0, len(u.Info))
	for k := range u.Info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, u.Info[k])
	}
	return b.String()
}

func renderProfiles(ps []store.Profile) string {
	if len(ps) == 0 {
		return "（空）"
	}
	var b strings.Builder
	for i, pr := range ps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, pr.Content)
	}
	return b.String()
}

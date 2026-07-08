package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
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

		tool("update_my_profile", "更新自己的基本信息。传字段名到值的映射；值为空串/null/无表示清除该字段；常见别名会自动归一。",
			obj(map[string]any{
				"name":   p("string", "新名字（可选）"),
				"fields": infoFieldsSchema("动态字段名→值"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name   string             `json:"name"`
					Fields map[string]*string `json:"fields"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				defined, err := d.Store.ListInfoFields(ctx)
				if err != nil {
					return "", err
				}
				fields, msg := normalizeInfoFieldsPtr(args.Fields, defined)
				if msg != "" {
					return msg, nil
				}
				if args.Name != "" {
					if err := d.Store.UpdateUserName(ctx, u.ID, args.Name); err != nil {
						return "", err
					}
				}
				if len(fields) > 0 {
					if err := d.Store.UpdateUserInfo(ctx, u.ID, fields); err != nil {
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
					return "目标用户不存在。", nil
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
				fmt.Fprintf(&b, "%s 的画像：\n", subject.Name)
				for _, pr := range visible {
					who := "他人评价"
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
	fmt.Fprintf(&b, "名字: %s\n状态: %s\n", u.Name, renderUserStatus(u.Status))
	if u.IsSuperadmin {
		b.WriteString("身份: 超级管理员\n")
	} else if u.IsWorker {
		b.WriteString("身份: AI worker\n")
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

func renderUserStatus(status string) string {
	switch status {
	case store.UserActive:
		return "正常"
	case store.UserDisabled:
		return "已停用"
	default:
		return status
	}
}

type infoFieldLookup struct {
	exact map[string]string
	lower map[string]string
	alias map[string]string
}

func newInfoFieldLookup(defined []string) infoFieldLookup {
	l := infoFieldLookup{
		exact: map[string]string{},
		lower: map[string]string{},
		alias: map[string]string{},
	}
	for _, f := range defined {
		name := strings.TrimSpace(f)
		if name == "" {
			continue
		}
		l.exact[name] = name
		l.lower[strings.ToLower(name)] = name
	}
	for alias, canonical := range map[string]string{
		"外号":       "昵称",
		"昵称":       "昵称",
		"nick":     "昵称",
		"nickname": "昵称",
		"岗位":       "职位",
		"职务":       "职位",
		"职位":       "职位",
		"position": "职位",
		"title":    "职位",
		"角色":       "role",
		"role":     "role",
		"邮箱":       "邮箱",
		"email":    "邮箱",
		"mail":     "邮箱",
		"手机":       "手机",
		"手机号":      "手机",
		"电话":       "手机",
		"phone":    "手机",
		"真实姓名":     "真实姓名",
		"真名":       "真实姓名",
		"姓名":       "真实姓名",
	} {
		if canon, ok := l.exact[canonical]; ok {
			l.alias[strings.ToLower(alias)] = canon
		}
	}
	return l
}

func (l infoFieldLookup) canonical(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if v, ok := l.exact[name]; ok {
		return v, true
	}
	if v, ok := l.lower[strings.ToLower(name)]; ok {
		return v, true
	}
	if v, ok := l.alias[strings.ToLower(name)]; ok {
		return v, true
	}
	return "", false
}

func normalizeInfoFields(fields map[string]string, defined []string) (map[string]string, string) {
	if len(fields) == 0 {
		return nil, ""
	}
	lookup := newInfoFieldLookup(defined)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]string{}
	for _, rawKey := range keys {
		field, ok := lookup.canonical(rawKey)
		if !ok {
			return nil, fmt.Sprintf("字段 %q 未定义。可用字段: %s", rawKey, strings.Join(defined, ", "))
		}
		value := normalizeInfoValue(field, fields[rawKey], lookup)
		out[field] = value
	}
	return out, ""
}

func infoFieldsSchema(description string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          description,
		"additionalProperties": map[string]any{"type": "string", "description": "字段值；传空串、null、无、删除等表示清除"},
	}
}

func normalizeInfoFieldsPtr(fields map[string]*string, defined []string) (map[string]string, string) {
	plain := make(map[string]string, len(fields))
	for k, v := range fields {
		if v == nil {
			plain[k] = ""
			continue
		}
		plain[k] = *v
	}
	return normalizeInfoFields(plain, defined)
}

func normalizeInfoValue(field, value string, lookup infoFieldLookup) string {
	value = strings.TrimSpace(value)
	if prefix, rest, ok := splitInfoFieldPrefix(value); ok {
		if canonical, found := lookup.canonical(prefix); found && canonical == field {
			value = strings.TrimSpace(rest)
		}
	}
	if isNullishInfoValue(value) {
		return ""
	}
	return value
}

func splitInfoFieldPrefix(value string) (string, string, bool) {
	for _, sep := range []string{"：", ":"} {
		if i := strings.Index(value, sep); i > 0 {
			return strings.TrimSpace(value[:i]), strings.TrimSpace(value[i+len(sep):]), true
		}
	}
	return "", "", false
}

func isNullishInfoValue(value string) bool {
	v := strings.Trim(strings.ToLower(strings.TrimSpace(value)), `"'`)
	switch v {
	case "", "null", "nil", "none", "n/a", "na", "无", "没有", "清空", "删除", "-":
		return true
	default:
		return false
	}
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

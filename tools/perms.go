package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

// permTools 权限查询与授予。
func permTools(d Deps, u *store.User) []ai.Tool {
	ts := []ai.Tool{
		immediateTool(tool("list_permission_actions", "查看可授予的底层能力、作用域和兼容别名。角色只是工作方式；实际授权由这里的 action、稳定目标 ID 和委托来源共同决定。", obj(nil),
			func(context.Context, json.RawMessage) (string, error) {
				var b strings.Builder
				b.WriteString("可授予能力\n")
				for _, action := range perm.ActiveActionDefinitions() {
					target := "目标=用户ID或_all"
					switch action.Scope {
					case perm.ScopeGlobal:
						target = "目标=_all（资源所有权由执行工具实时校验）"
					case perm.ScopeTelegramGroup:
						target = "目标=group_ref或_all"
					}
					aliases := ""
					if len(action.Aliases) > 0 {
						aliases = "；别名=" + strings.Join(action.Aliases, ",")
					}
					fmt.Fprintf(&b, "- %s：%s；%s%s\n", action.Name, action.Description, target, aliases)
				}
				return strings.TrimSpace(b.String()), nil
			})),

		tool("list_my_active_perms", "查看我能对谁做什么（我的主动权限）。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				grants, err := d.Store.PermsOf(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return renderGrants(grants, store.KindActive), nil
			}),

		tool("list_my_passive_perms", "查看谁能对我做什么（我的被动权限）。", obj(nil),
			func(ctx context.Context, _ json.RawMessage) (string, error) {
				grants, err := d.Store.PermsOf(ctx, u.ID)
				if err != nil {
					return "", err
				}
				return renderGrants(grants, store.KindPassive), nil
			}),

		tool("grant_my_passive_perm", "允许某人查看我身上的画像。action 格式：view_profile:<作者用户ID> 或 view_profile:_all。",
			obj(map[string]any{
				"action":  p("string", "view_profile:<作者ID> 或 view_profile:_all"),
				"subject": p("string", "允许谁看：用户ID 或 _all"),
			}, "action", "subject"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Action  string `json:"action"`
					Subject string `json:"subject"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !perm.ValidPassiveAction(args.Action) {
					return "action 格式不合法。", nil
				}
				if message := validatePassiveAuthor(ctx, d.Store, args.Action); message != "" {
					return message, nil
				}
				key, id, isAll, err := parseTarget(args.Subject)
				if err != nil {
					return err.Error(), nil
				}
				if !isAll {
					if _, err := mustUser(ctx, d.Store, id); err != nil {
						return err.Error(), nil
					}
				}
				if err := d.Store.GrantPerm(ctx, store.Grant{
					Kind: store.KindPassive, UserID: u.ID, Action: args.Action, Target: key, GrantedBy: u.ID,
				}); err != nil {
					if message, handled := permissionGrantError(err); handled {
						return message, nil
					}
					return "", err
				}
				return "已授权。", nil
			}),

		tool("revoke_my_passive_perm", "撤销某人查看我身上画像的权限。",
			obj(map[string]any{
				"action":  p("string", "view_profile:<作者ID> 或 view_profile:_all"),
				"subject": p("string", "用户ID 或 _all"),
			}, "action", "subject"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Action  string `json:"action"`
					Subject string `json:"subject"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !perm.ValidPassiveAction(args.Action) {
					return "action 格式不合法。", nil
				}
				key, _, _, err := parseTarget(args.Subject)
				if err != nil {
					return err.Error(), nil
				}
				if err := d.Store.RevokePerm(ctx, u.ID, store.KindPassive, u.ID, args.Action, key); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该授权不存在。", nil
					}
					if errors.Is(err, store.ErrForbidden) {
						return "撤销未执行：你当前没有管理该成员/能力范围的有效权限，或该授权来源属于同级或更高层。", nil
					}
					return "", err
				}
				return "已撤销。", nil
			}),
	}

	// 主动权限授予：超管任意授；普通用户按转授规则（拥有且不超范围）。
	ts = append(ts,
		tool("grant_active_perm", "给某人授予主动权限。action: "+strings.Join(activeActionList(), "/")+"。用户作用域使用稳定用户ID或_all；Telegram 群管理使用 group_ref 或_all；系统/自有资源能力使用_all。超管可建立授权根；普通用户必须先有对被授权人的 manage_perm，且只能沿自己的有效授权链转授不超过自身范围的能力。",
			obj(map[string]any{
				"user_id": p("integer", "被授权的用户ID"),
				"action":  p("string", "动作；邀请员工权限可填 invite_employee（会自动转成 generate_key）"),
				"target":  p("string", "作用目标：用户ID、Telegram group_ref 或_all，以 action 作用域为准"),
			}, "user_id", "action", "target"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64  `json:"user_id"`
					Action string `json:"action"`
					Target string `json:"target"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				args.Action = normalizeActiveAction(args.Action)
				if !perm.ValidActiveAction(args.Action) {
					return "action 不合法。可用: " + strings.Join(activeActionList(), ", "), nil
				}
				targetUser, err := mustUser(ctx, d.Store, args.UserID)
				if err != nil {
					return err.Error(), nil
				}
				key, message := normalizeActivePermissionTarget(ctx, d, args.Action, args.Target, true)
				if message != "" {
					return message, nil
				}
				if !u.IsSuperadmin {
					granter, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if msg := canManagePermTarget(u, targetUser, granter); msg != "" {
						return msg, nil
					}
					if !perm.CanGrantActive(granter, args.Action, key) {
						return "你只能转授自己拥有、且范围不超过自己的权限。", nil
					}
				}
				if err := d.Store.GrantPerm(ctx, store.Grant{
					Kind: store.KindActive, UserID: args.UserID, Action: args.Action, Target: key, GrantedBy: u.ID,
				}); err != nil {
					if message, handled := permissionGrantError(err); handled {
						return message, nil
					}
					return "", err
				}
				return "已授权。", nil
			}),

		tool("revoke_active_perm", "撤销某人的主动权限及其失去全部有效上游后的后续转授。超管可撤全部来源；普通用户只能撤自己或自己有权管理的下级签发的来源，不能覆盖同级或更高层授权；其他独立授权链继续有效。",
			obj(map[string]any{
				"user_id": p("integer", "用户ID"),
				"action":  p("string", "动作；邀请员工权限可填 invite_employee（会自动转成 generate_key）"),
				"target":  p("string", "作用目标：用户ID、Telegram group_ref 或_all，以 action 作用域为准"),
			}, "user_id", "action", "target"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64  `json:"user_id"`
					Action string `json:"action"`
					Target string `json:"target"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				args.Action = normalizeActiveAction(args.Action)
				if !perm.ValidActiveAction(args.Action) {
					return "action 不合法。可用: " + strings.Join(activeActionList(), ", "), nil
				}
				targetUser, err := mustUser(ctx, d.Store, args.UserID)
				if err != nil {
					return err.Error(), nil
				}
				key, message := normalizeActivePermissionTarget(ctx, d, args.Action, args.Target, false)
				if message != "" {
					return message, nil
				}
				if !u.IsSuperadmin {
					granter, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if msg := canManagePermTarget(u, targetUser, granter); msg != "" {
						return msg, nil
					}
					if !perm.CanGrantActive(granter, args.Action, key) {
						return "你只能撤销自己有权转授范围内的权限。", nil
					}
				}
				if err := d.Store.RevokePerm(ctx, u.ID, store.KindActive, args.UserID, args.Action, key); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该授权不存在。", nil
					}
					if errors.Is(err, store.ErrForbidden) {
						return "撤销未执行：你当前没有管理该成员/能力范围的有效权限，或该授权来源属于同级或更高层。", nil
					}
					return "", err
				}
				return "已撤销。", nil
			}),

		tool("grant_passive_perm", "给某用户添加被动权限（允许 subject 看其身上某作者的画像）。普通用户需要对该用户有 manage_perm；非超管不能管理超级管理员。",
			obj(map[string]any{
				"user_id": p("integer", "被动权限挂在谁身上"),
				"action":  p("string", "view_profile:<作者ID> 或 view_profile:_all"),
				"subject": p("string", "允许谁看：用户ID 或 _all"),
			}, "user_id", "action", "subject"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID  int64  `json:"user_id"`
					Action  string `json:"action"`
					Subject string `json:"subject"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !perm.ValidPassiveAction(args.Action) {
					return "action 格式不合法。", nil
				}
				if message := validatePassiveAuthor(ctx, d.Store, args.Action); message != "" {
					return message, nil
				}
				targetUser, err := mustUser(ctx, d.Store, args.UserID)
				if err != nil {
					return err.Error(), nil
				}
				key, _, _, err := parseTarget(args.Subject)
				if err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if msg := canManagePermTarget(u, targetUser, grants); msg != "" {
						return msg, nil
					}
				}
				if err := d.Store.GrantPerm(ctx, store.Grant{
					Kind: store.KindPassive, UserID: args.UserID, Action: args.Action, Target: key, GrantedBy: u.ID,
				}); err != nil {
					if message, handled := permissionGrantError(err); handled {
						return message, nil
					}
					return "", err
				}
				return "已授权。", nil
			}),

		tool("revoke_passive_perm", "撤销某用户的被动权限。普通用户需要对该用户有 manage_perm；非超管不能管理超级管理员。",
			obj(map[string]any{
				"user_id": p("integer", "被动权限挂在谁身上"),
				"action":  p("string", "view_profile:<作者ID> 或 view_profile:_all"),
				"subject": p("string", "用户ID 或 _all"),
			}, "user_id", "action", "subject"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID  int64  `json:"user_id"`
					Action  string `json:"action"`
					Subject string `json:"subject"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if !perm.ValidPassiveAction(args.Action) {
					return "action 格式不合法。", nil
				}
				targetUser, err := mustUser(ctx, d.Store, args.UserID)
				if err != nil {
					return err.Error(), nil
				}
				key, _, _, err := parseTarget(args.Subject)
				if err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if msg := canManagePermTarget(u, targetUser, grants); msg != "" {
						return msg, nil
					}
				}
				if err := d.Store.RevokePerm(ctx, u.ID, store.KindPassive, args.UserID, args.Action, key); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "该授权不存在。", nil
					}
					if errors.Is(err, store.ErrForbidden) {
						return "撤销未执行：你当前没有管理该成员的有效权限，或该授权来源属于同级或更高层。", nil
					}
					return "", err
				}
				return "已撤销。", nil
			}),

		tool("view_user_perms", "查看某用户的所有权限。需要超管或对其 manage_perm 权限。",
			obj(map[string]any{"user_id": p("integer", "用户ID")}, "user_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					UserID int64 `json:"user_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				targetUser, err := mustUser(ctx, d.Store, args.UserID)
				if err != nil {
					return err.Error(), nil
				}
				if !u.IsSuperadmin {
					grants, err := d.Store.PermsOf(ctx, u.ID)
					if err != nil {
						return "", err
					}
					if msg := canManagePermTarget(u, targetUser, grants); msg != "" {
						return msg, nil
					}
				}
				grants, err := d.Store.PermsOf(ctx, args.UserID)
				if err != nil {
					return "", err
				}
				return "主动权限：\n" + renderGrants(grants, store.KindActive) +
					"\n被动权限：\n" + renderGrants(grants, store.KindPassive), nil
			}),
	)
	return ts
}

func activeActionList() []string {
	definitions := perm.ActiveActionDefinitions()
	out := make([]string, 0, len(definitions))
	for _, action := range definitions {
		item := action.Name
		if len(action.Aliases) > 0 {
			item += "(" + strings.Join(action.Aliases, ",") + ")"
		}
		out = append(out, item)
	}
	return out
}

func normalizeActiveAction(action string) string {
	return perm.NormalizeActiveAction(action)
}

func activeTargetError(action string) string {
	definition, ok := perm.ActiveActionDefinition(action)
	if !ok {
		return "action 不合法。"
	}
	switch definition.Scope {
	case perm.ScopeGlobal:
		return action + " 是系统/自有资源能力，target 必须是 _all；具体资源归属由执行工具实时校验。"
	case perm.ScopeTelegramGroup:
		return action + " 的 target 必须是 list_telegram_groups 返回的 group_ref 或 _all。"
	}
	return "target 必须是有效用户ID或 _all。"
}

func normalizeActivePermissionTarget(ctx context.Context, d Deps, action, raw string, requireExisting bool) (string, string) {
	definition, ok := perm.ActiveActionDefinition(action)
	if !ok {
		return "", "action 不合法。"
	}
	raw = strings.TrimSpace(raw)
	switch definition.Scope {
	case perm.ScopeUser:
		key, id, all, err := parseTarget(raw)
		if err != nil {
			return "", err.Error()
		}
		if !all {
			if _, err := mustUser(ctx, d.Store, id); err != nil {
				return "", err.Error()
			}
		}
		return key, ""
	case perm.ScopeTelegramGroup:
		if raw == store.TargetAll {
			return raw, ""
		}
		chatID, ok := parseTelegramGroupRef(raw)
		if !ok {
			return "", activeTargetError(action)
		}
		key := telegramGroupRef(chatID)
		if requireExisting {
			if _, err := d.Store.TelegramGroupState(ctx, chatID); err != nil {
				return "", "目标 Telegram 群不存在或尚未接入。"
			}
		}
		return key, ""
	default:
		if !perm.ValidActiveTarget(action, raw) {
			return "", activeTargetError(action)
		}
		return raw, ""
	}
}

func permissionGrantError(err error) (string, bool) {
	switch {
	case errors.Is(err, store.ErrConflict):
		return "这条授权边已存在。若同一能力来自另一位授权者，系统会保留为独立来源。", true
	case errors.Is(err, store.ErrForbidden):
		return "授权未生效：授予者当前没有可追溯且范围足够的上游权限。请重新查看有效权限后再操作。", true
	default:
		return "", false
	}
}

func validatePassiveAuthor(ctx context.Context, s *store.Store, action string) string {
	authorID, all, ok := perm.ParsePassiveProfileAuthor(action)
	if !ok {
		return "action 格式不合法。"
	}
	if all {
		return ""
	}
	if _, err := mustUser(ctx, s, authorID); err != nil {
		return "画像作者用户不存在或已停用。"
	}
	return ""
}

func canManagePermTarget(actor, target *store.User, actorGrants []store.Grant) string {
	if actor.IsSuperadmin {
		return ""
	}
	if target.IsSuperadmin {
		return "不能管理超级管理员的权限。"
	}
	if !perm.CheckActive(actorGrants, perm.ActManagePerm, target.ID) {
		return fmt.Sprintf("你没有对 %s 的 manage_perm 权限。", target.Name)
	}
	return ""
}

func renderGrants(grants []store.Grant, kind string) string {
	var b strings.Builder
	for _, g := range grants {
		if g.Kind != kind {
			continue
		}
		fmt.Fprintf(&b, "- %s → %s（授予者内部编号 %s）\n", g.Action, renderPermTarget(g.Target), strconv.FormatInt(g.GrantedBy, 10))
	}
	if b.Len() == 0 {
		return "（无）"
	}
	return b.String()
}

func renderPermTarget(target string) string {
	if target == store.TargetAll {
		return "全部目标"
	}
	if _, ok := parseTelegramGroupRef(target); ok {
		return "Telegram 群 " + target
	}
	if _, err := strconv.ParseInt(target, 10, 64); err == nil {
		return "用户内部编号 " + target
	}
	return target
}

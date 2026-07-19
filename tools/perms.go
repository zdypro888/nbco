package tools

import (
	"context"
	"encoding/json"
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
					if err == store.ErrConflict {
						return "该授权已存在。", nil
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
				key, _, _, err := parseTarget(args.Subject)
				if err != nil {
					return err.Error(), nil
				}
				if err := d.Store.RevokePerm(ctx, store.KindPassive, u.ID, args.Action, key); err != nil {
					if err == store.ErrNotFound {
						return "该授权不存在。", nil
					}
					return "", err
				}
				return "已撤销。", nil
			}),
	}

	// 主动权限授予：超管任意授；普通用户按转授规则（拥有且不超范围）。
	ts = append(ts,
		tool("grant_active_perm", "给某人授予主动权限。action: "+strings.Join(activeActionList(), "/")+"。邀请员工能力的规范 action 是 generate_key，兼容别名 invite_employee。超管可任意授；普通用户必须先有对被授权人的 manage_perm，且只能转授自己拥有、范围不超过自己的权限。",
			obj(map[string]any{
				"user_id": p("integer", "被授权的用户ID"),
				"action":  p("string", "动作；邀请员工权限可填 invite_employee（会自动转成 generate_key）"),
				"target":  p("string", "作用目标：用户ID 或 _all"),
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
				key, id, isAll, err := parseTarget(args.Target)
				if err != nil {
					return err.Error(), nil
				}
				if !isAll {
					if _, err := mustUser(ctx, d.Store, id); err != nil {
						return err.Error(), nil
					}
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
					if err == store.ErrConflict {
						return "该授权已存在。", nil
					}
					return "", err
				}
				return "已授权。", nil
			}),

		tool("revoke_active_perm", "撤销某人的主动权限。邀请员工权限可填 invite_employee 或 generate_key。超管任意撤；普通用户必须先有对该用户的 manage_perm，且只能撤销自己有权转授范围内的权限。",
			obj(map[string]any{
				"user_id": p("integer", "用户ID"),
				"action":  p("string", "动作；邀请员工权限可填 invite_employee（会自动转成 generate_key）"),
				"target":  p("string", "作用目标：用户ID 或 _all"),
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
				key, _, _, err := parseTarget(args.Target)
				if err != nil {
					return err.Error(), nil
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
				if err := d.Store.RevokePerm(ctx, store.KindActive, args.UserID, args.Action, key); err != nil {
					if err == store.ErrNotFound {
						return "该授权不存在。", nil
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
					if err == store.ErrConflict {
						return "该授权已存在。", nil
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
				if err := d.Store.RevokePerm(ctx, store.KindPassive, args.UserID, args.Action, key); err != nil {
					if err == store.ErrNotFound {
						return "该授权不存在。", nil
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
	return []string{
		perm.ActWriteProfile, perm.ActViewSelfIntro, perm.ActManagePerm,
		perm.ActGenerateKey + "(invite_employee)", perm.ActSendMsg, perm.ActCreateProject, perm.ActEditInfo,
		perm.ActManageTGGroup, perm.ActManageWorker,
	}
}

func normalizeActiveAction(action string) string {
	switch strings.TrimSpace(action) {
	case "invite_employee":
		return perm.ActGenerateKey
	default:
		return strings.TrimSpace(action)
	}
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
		return "全体成员"
	}
	if _, err := strconv.ParseInt(target, 10, 64); err == nil {
		return "用户内部编号 " + target
	}
	return target
}

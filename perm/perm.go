// Package perm 是双维度权限模型的纯判定逻辑（无 IO，可单测）。
//
// 主动权限（active）：存在操作者身上 —— 我能对谁做什么。
// 被动权限（passive）：存在被操作者身上 —— 谁能对我做什么。
// 超管旁路由调用方在入口处理（IsSuperadmin 直接放行），本包只做规则判定。
//
// 数据形态沿用 store.Grant：
//   - active:  UserID=操作者, Action=动作；用户作用域的 Target 是目标用户ID
//     文本或 "_all"，系统/自有资源作用域只能是 "_all"
//   - passive: UserID=被操作者, Action="view_profile:<作者ID>" 或 "view_profile:_all",
//     Target=查看者ID文本或 "_all"
package perm

import (
	"slices"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/store"
)

// 主动权限动作。
const (
	ActWriteProfile  = "write_profile"   // 给他人写画像
	ActViewSelfIntro = "view_self_intro" // 看他人自我介绍
	ActManagePerm    = "manage_perm"     // 管理他人被动权限
	ActGenerateKey   = "generate_key"    // 邀请真人员工
	ActSendMsg       = "send_msg"        // 向他人发消息
	ActCreateProject = "create_project"  // 创建项目/给他人派任务
	ActEditInfo      = "edit_info"       // 改他人基本信息
	ActManageTGGroup = "manage_telegram_group"
	// ActManageMandatorySchedule permits making notifications non-optional for
	// the covered recipients. Sending a message alone must not imply this power.
	ActManageMandatorySchedule = "manage_mandatory_schedule"
	// ActManageWorker 创建/管理 AI worker（绑定码、命令、停用）。管理对象是
	// 自己名下的 worker 而非其他用户，授予时目标通常用 _all。
	ActManageWorker = "manage_worker"
)

type ActiveScope string

const (
	// ScopeUser means Target is a stable user ID or _all.
	ScopeUser ActiveScope = "user"
	// ScopeGlobal means the capability governs the actor's own/system resource
	// domain and therefore only accepts _all. Resource ownership is checked by
	// the target tool at execution time.
	ScopeGlobal ActiveScope = "global"
	// ScopeTelegramGroup accepts one stable telegram:group:<chat-id> reference
	// or _all. Group names are display labels and never authorization keys.
	ScopeTelegramGroup ActiveScope = "telegram_group"
)

// ActiveAction defines the stable vocabulary used by storage, tool assembly
// and the AI-facing permission tools. Roles may bundle these capabilities but
// never replace target-level authorization.
type ActiveAction struct {
	Name        string
	Description string
	Scope       ActiveScope
	Aliases     []string
}

var activeActions = []ActiveAction{
	{Name: ActWriteProfile, Description: "给目标成员写画像", Scope: ScopeUser},
	{Name: ActViewSelfIntro, Description: "查看目标成员的自我介绍", Scope: ScopeUser},
	{Name: ActManagePerm, Description: "管理目标成员的权限", Scope: ScopeUser},
	{Name: ActGenerateKey, Description: "签发真人员工一次性邀请", Scope: ScopeGlobal, Aliases: []string{"invite_employee"}},
	{Name: ActSendMsg, Description: "向目标成员发送消息", Scope: ScopeUser},
	{Name: ActCreateProject, Description: "创建项目并给目标成员安排工作", Scope: ScopeUser},
	{Name: ActEditInfo, Description: "修改目标成员的基本信息", Scope: ScopeUser},
	{Name: ActManageTGGroup, Description: "管理指定或全部已接入的 Telegram 群", Scope: ScopeTelegramGroup},
	{Name: ActManageMandatorySchedule, Description: "为目标成员设置不可退订日程", Scope: ScopeUser},
	{Name: ActManageWorker, Description: "创建和调度自己名下的 AI Worker", Scope: ScopeGlobal},
}

// ActiveActionDefinitions returns a detached copy so callers cannot mutate
// the process-wide authorization vocabulary.
func ActiveActionDefinitions() []ActiveAction {
	out := make([]ActiveAction, len(activeActions))
	for i, action := range activeActions {
		out[i] = action
		out[i].Aliases = slices.Clone(action.Aliases)
	}
	return out
}

// NormalizeActiveAction maps accepted aliases to the stored canonical name.
func NormalizeActiveAction(action string) string {
	action = strings.TrimSpace(action)
	for _, definition := range activeActions {
		if action == definition.Name || slices.Contains(definition.Aliases, action) {
			return definition.Name
		}
	}
	return action
}

func activeActionDefinition(action string) (ActiveAction, bool) {
	action = NormalizeActiveAction(action)
	for _, definition := range activeActions {
		if definition.Name == action {
			return definition, true
		}
	}
	return ActiveAction{}, false
}

// ActiveActionDefinition returns detached metadata for one canonical action.
func ActiveActionDefinition(action string) (ActiveAction, bool) {
	definition, ok := activeActionDefinition(action)
	if !ok {
		return ActiveAction{}, false
	}
	definition.Aliases = slices.Clone(definition.Aliases)
	return definition, true
}

const viewProfilePrefix = "view_profile:"

// ValidActiveAction checks the canonical active capability name.
func ValidActiveAction(action string) bool {
	definition, ok := activeActionDefinition(action)
	return ok && definition.Name == strings.TrimSpace(action)
}

// ValidActiveTarget checks that the target shape matches the capability's
// declared scope. User-scoped actions accept a stable numeric ID or _all;
// global/owned-resource capabilities accept only _all.
func ValidActiveTarget(action, target string) bool {
	definition, ok := activeActionDefinition(action)
	if !ok {
		return false
	}
	target = strings.TrimSpace(target)
	switch definition.Scope {
	case ScopeGlobal:
		return target == store.TargetAll
	case ScopeTelegramGroup:
		if target == store.TargetAll {
			return true
		}
		value, ok := strings.CutPrefix(target, "telegram:group:")
		if !ok {
			return false
		}
		id, err := strconv.ParseInt(value, 10, 64)
		return err == nil && id != 0
	}
	if target == store.TargetAll {
		return true
	}
	value, err := strconv.ParseInt(target, 10, 64)
	return err == nil && value > 0
}

// ParsePassiveProfileAuthor parses view_profile:_all or a stable positive
// author user ID. The caller may additionally verify that the user still
// exists when creating a new disclosure grant.
func ParsePassiveProfileAuthor(action string) (id int64, all, ok bool) {
	suffix, ok := strings.CutPrefix(action, viewProfilePrefix)
	if !ok {
		return 0, false, false
	}
	if suffix == store.TargetAll {
		return 0, true, true
	}
	id, err := strconv.ParseInt(suffix, 10, 64)
	return id, false, err == nil && id > 0
}

// ValidPassiveAction validates the stored passive action shape.
func ValidPassiveAction(action string) bool {
	_, _, ok := ParsePassiveProfileAuthor(action)
	return ok
}

// CheckActive 判定 grants（操作者的授权集合）是否允许对 targetID 执行 action。
func CheckActive(grants []store.Grant, action string, targetID int64) bool {
	return CheckActiveTarget(grants, action, strconv.FormatInt(targetID, 10))
}

// CheckActiveTarget checks an already canonicalized user/resource target.
func CheckActiveTarget(grants []store.Grant, action, target string) bool {
	for _, g := range grants {
		if g.Kind != store.KindActive || g.Action != action {
			continue
		}
		if g.Target == target || g.Target == store.TargetAll {
			return true
		}
	}
	return false
}

// CheckPassive 判定 grants（被操作者身上的被动授权集合）是否允许 subjectID 执行 action。
func CheckPassive(grants []store.Grant, action string, subjectID int64) bool {
	key := strconv.FormatInt(subjectID, 10)
	for _, g := range grants {
		if g.Kind != store.KindPassive || g.Action != action {
			continue
		}
		if g.Target == key || g.Target == store.TargetAll {
			return true
		}
	}
	return false
}

// CanViewProfile 判定 viewer 能否看到 subject 身上 author 写的画像。
//
//	viewerActive：viewer 的主动授权集合；subjectPassive：subject 身上的被动授权集合。
func CanViewProfile(viewerID, subjectID, authorID int64, viewerIsSuper bool,
	viewerActive, subjectPassive []store.Grant) bool {
	if viewerIsSuper {
		return true
	}
	// 作者本人能看自己写的。
	if viewerID == authorID {
		return true
	}
	// 自我介绍：viewer 需要对 subject 的 view_self_intro 主动权限。
	if authorID == subjectID {
		return CheckActive(viewerActive, ActViewSelfIntro, subjectID)
	}
	// 他人写的画像：subject 的被动授权允许 viewer 看该作者（或全部作者）。
	author := strconv.FormatInt(authorID, 10)
	if CheckPassive(subjectPassive, viewProfilePrefix+author, viewerID) {
		return true
	}
	return CheckPassive(subjectPassive, viewProfilePrefix+store.TargetAll, viewerID)
}

// CanGrantActive 转授校验：非超管只能转授自己拥有、且范围不超过自己的主动权限。
//
//	granter 持有 action:_all → 可授任意目标；
//	granter 仅持有 action:<t> → 只能授完全相同的目标 <t>（不能授 _all，也不能授别的目标）。
func CanGrantActive(granterGrants []store.Grant, action, targetKey string) bool {
	if !ValidActiveAction(action) || !ValidActiveTarget(action, targetKey) {
		return false
	}
	hasAll := false
	exact := map[string]bool{}
	for _, g := range granterGrants {
		if g.Kind != store.KindActive || g.Action != action {
			continue
		}
		if g.Target == store.TargetAll {
			hasAll = true
		} else {
			exact[g.Target] = true
		}
	}
	if hasAll {
		return true
	}
	if targetKey == store.TargetAll {
		return false
	}
	return exact[targetKey]
}

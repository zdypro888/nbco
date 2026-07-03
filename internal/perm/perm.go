// Package perm 是双维度权限模型的纯判定逻辑（无 IO，可单测）。
//
// 主动权限（active）：存在操作者身上 —— 我能对谁做什么。
// 被动权限（passive）：存在被操作者身上 —— 谁能对我做什么。
// 超管旁路由调用方在入口处理（IsSuperadmin 直接放行），本包只做规则判定。
//
// 数据形态沿用 store.Grant：
//   - active:  UserID=操作者, Action=动作, Target=目标用户ID文本或 "_all"
//   - passive: UserID=被操作者, Action="view_profile:<作者ID>" 或 "view_profile:_all",
//     Target=查看者ID文本或 "_all"
package perm

import (
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/internal/store"
)

// 主动权限动作。
const (
	ActWriteProfile  = "write_profile"   // 给他人写画像
	ActViewSelfIntro = "view_self_intro" // 看他人自我介绍
	ActManagePerm    = "manage_perm"     // 管理他人被动权限
	ActGenerateKey   = "generate_key"    // 生成绑定 Key
	ActSendMsg       = "send_msg"        // 向他人发消息
	ActCreateProject = "create_project"  // 创建项目/给他人派任务
	ActEditInfo      = "edit_info"       // 改他人基本信息
)

// ActiveActions 合法主动动作集合。
var ActiveActions = map[string]bool{
	ActWriteProfile:  true,
	ActViewSelfIntro: true,
	ActManagePerm:    true,
	ActGenerateKey:   true,
	ActSendMsg:       true,
	ActCreateProject: true,
	ActEditInfo:      true,
}

const viewProfilePrefix = "view_profile:"

// ValidActiveAction 校验主动动作。
func ValidActiveAction(action string) bool { return ActiveActions[action] }

// ValidPassiveAction 校验被动动作格式：view_profile:_all 或 view_profile:<数字>。
func ValidPassiveAction(action string) bool {
	suffix, ok := strings.CutPrefix(action, viewProfilePrefix)
	if !ok {
		return false
	}
	if suffix == store.TargetAll {
		return true
	}
	_, err := strconv.ParseInt(suffix, 10, 64)
	return err == nil && suffix != ""
}

// CheckActive 判定 grants（操作者的授权集合）是否允许对 targetID 执行 action。
func CheckActive(grants []store.Grant, action string, targetID int64) bool {
	key := strconv.FormatInt(targetID, 10)
	for _, g := range grants {
		if g.Kind != store.KindActive || g.Action != action {
			continue
		}
		if g.Target == key || g.Target == store.TargetAll {
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
	if !ValidActiveAction(action) {
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

// ViewIntroTargets 计算某人 view_self_intro 的可见范围，用于派任务时的权限继承：
// 返回 ("_all", nil) 或 (非通配) 目标 ID 列表。
func ViewIntroTargets(grants []store.Grant) (all bool, targets []int64) {
	for _, g := range grants {
		if g.Kind != store.KindActive || g.Action != ActViewSelfIntro {
			continue
		}
		if g.Target == store.TargetAll {
			return true, nil
		}
		if id, err := strconv.ParseInt(g.Target, 10, 64); err == nil {
			targets = append(targets, id)
		}
	}
	return false, targets
}

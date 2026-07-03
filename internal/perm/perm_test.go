package perm

import (
	"testing"

	"github.com/zdypro888/nbco/internal/store"
)

func active(action, target string) store.Grant {
	return store.Grant{Kind: store.KindActive, Action: action, Target: target}
}

func passive(action, target string) store.Grant {
	return store.Grant{Kind: store.KindPassive, Action: action, Target: target}
}

func TestValidPassiveAction(t *testing.T) {
	cases := map[string]bool{
		"view_profile:_all":  true,
		"view_profile:42":    true,
		"view_profile:":      false,
		"view_profile:abc":   false,
		"view_profile:_al":   false,
		"write_profile":      false,
		"view_profile:-1":    true, // 负数虽无意义但格式合法；授权时上层还会校验用户存在
		"xview_profile:_all": false,
	}
	for action, want := range cases {
		if got := ValidPassiveAction(action); got != want {
			t.Errorf("ValidPassiveAction(%q) = %v, want %v", action, got, want)
		}
	}
}

func TestCheckActive(t *testing.T) {
	grants := []store.Grant{
		active(ActWriteProfile, "7"),
		active(ActViewSelfIntro, store.TargetAll),
		passive("view_profile:_all", "9"), // 混入被动记录不应影响主动判定
	}
	if !CheckActive(grants, ActWriteProfile, 7) {
		t.Error("精确目标应命中")
	}
	if CheckActive(grants, ActWriteProfile, 8) {
		t.Error("非授权目标不应命中")
	}
	if !CheckActive(grants, ActViewSelfIntro, 12345) {
		t.Error("_all 应命中任意目标")
	}
	if CheckActive(grants, ActManagePerm, 7) {
		t.Error("未授权动作不应命中")
	}
	if CheckActive(nil, ActWriteProfile, 7) {
		t.Error("空授权集不应命中")
	}
}

func TestCheckPassive(t *testing.T) {
	grants := []store.Grant{
		passive("view_profile:3", "5"),
		passive("view_profile:_all", store.TargetAll),
		active(ActWriteProfile, "5"), // 混入主动记录不应影响被动判定
	}
	if !CheckPassive(grants, "view_profile:3", 5) {
		t.Error("精确 subject 应命中")
	}
	if CheckPassive(grants, "view_profile:3", 6) {
		t.Error("非授权 subject 不应命中")
	}
	if !CheckPassive(grants, "view_profile:_all", 999) {
		t.Error("target=_all 应允许任意 subject")
	}
}

func TestCanViewProfile(t *testing.T) {
	const (
		viewer  = int64(1)
		subject = int64(2)
		author  = int64(3)
	)
	t.Run("超管全可见", func(t *testing.T) {
		if !CanViewProfile(viewer, subject, author, true, nil, nil) {
			t.Fail()
		}
	})
	t.Run("作者看自己写的", func(t *testing.T) {
		if !CanViewProfile(author, subject, author, false, nil, nil) {
			t.Fail()
		}
	})
	t.Run("自我介绍需要view_self_intro", func(t *testing.T) {
		va := []store.Grant{active(ActViewSelfIntro, "2")}
		if !CanViewProfile(viewer, subject, subject, false, va, nil) {
			t.Error("有 view_self_intro:2 应可见")
		}
		if CanViewProfile(viewer, subject, subject, false, nil, nil) {
			t.Error("无权限不应可见")
		}
	})
	t.Run("他人画像走被动权限", func(t *testing.T) {
		sp := []store.Grant{passive("view_profile:3", "1")}
		if !CanViewProfile(viewer, subject, author, false, nil, sp) {
			t.Error("精确作者授权应可见")
		}
		sp2 := []store.Grant{passive("view_profile:_all", "1")}
		if !CanViewProfile(viewer, subject, author, false, nil, sp2) {
			t.Error("作者通配授权应可见")
		}
		sp3 := []store.Grant{passive("view_profile:4", "1")}
		if CanViewProfile(viewer, subject, author, false, nil, sp3) {
			t.Error("别的作者的授权不应放行")
		}
		sp4 := []store.Grant{passive("view_profile:3", "8")}
		if CanViewProfile(viewer, subject, author, false, nil, sp4) {
			t.Error("授权给别人的不应放行")
		}
	})
	t.Run("被动通配subject", func(t *testing.T) {
		sp := []store.Grant{passive("view_profile:3", store.TargetAll)}
		if !CanViewProfile(viewer, subject, author, false, nil, sp) {
			t.Error("subject=_all 应放行任意查看者")
		}
	})
}

func TestCanGrantActive(t *testing.T) {
	t.Run("持有_all可授任意", func(t *testing.T) {
		g := []store.Grant{active(ActWriteProfile, store.TargetAll)}
		if !CanGrantActive(g, ActWriteProfile, "7") {
			t.Error("应可授精确目标")
		}
		if !CanGrantActive(g, ActWriteProfile, store.TargetAll) {
			t.Error("应可授 _all")
		}
	})
	t.Run("持有精确目标只能授同目标", func(t *testing.T) {
		g := []store.Grant{active(ActWriteProfile, "7")}
		if !CanGrantActive(g, ActWriteProfile, "7") {
			t.Error("同目标应可授")
		}
		if CanGrantActive(g, ActWriteProfile, "8") {
			t.Error("越界目标不应可授")
		}
		if CanGrantActive(g, ActWriteProfile, store.TargetAll) {
			t.Error("不应能放大到 _all")
		}
	})
	t.Run("未持有不可授", func(t *testing.T) {
		if CanGrantActive(nil, ActWriteProfile, "7") {
			t.Fail()
		}
	})
	t.Run("非法动作不可授", func(t *testing.T) {
		g := []store.Grant{active("bogus", store.TargetAll)}
		if CanGrantActive(g, "bogus", "7") {
			t.Fail()
		}
	})
	t.Run("动作之间不串权", func(t *testing.T) {
		g := []store.Grant{active(ActWriteProfile, store.TargetAll)}
		if CanGrantActive(g, ActManagePerm, "7") {
			t.Fail()
		}
	})
}

func TestViewIntroTargets(t *testing.T) {
	all, _ := ViewIntroTargets([]store.Grant{active(ActViewSelfIntro, store.TargetAll)})
	if !all {
		t.Error("_all 应返回 all=true")
	}
	all, targets := ViewIntroTargets([]store.Grant{
		active(ActViewSelfIntro, "3"),
		active(ActViewSelfIntro, "5"),
		active(ActWriteProfile, "9"), // 无关动作
	})
	if all {
		t.Error("无通配不应返回 all")
	}
	if len(targets) != 2 || targets[0] != 3 || targets[1] != 5 {
		t.Errorf("targets = %v, want [3 5]", targets)
	}
}

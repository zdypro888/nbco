package tools

import (
	"strings"
	"testing"

	"github.com/zdypro888/nbco/store"
)

func TestRenderUserShowsEmployeeIDButNotTelegramID(t *testing.T) {
	got := renderUser(&store.User{
		ID:           42,
		Name:         "PRO",
		Status:       store.UserActive,
		IsSuperadmin: true,
		Info:         map[string]string{"职位": "董事长"},
	})
	for _, bad := range []string{"TG ID", "Telegram ID", "active"} {
		if strings.Contains(got, bad) {
			t.Fatalf("renderUser 不应暴露外部/原始字段 %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"员工ID: 42", "名字: PRO", "状态: 正常", "身份: 超级管理员", "职位: 董事长"} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderUser 缺 %q:\n%s", want, got)
		}
	}
}

func TestRenderUserDirectoryIsTelegramFriendlyAndSeparatesWorkers(t *testing.T) {
	ownerID := int64(1)
	users := []*store.User{
		{ID: ownerID, Name: "PRO", Status: store.UserActive, IsSuperadmin: true},
		{ID: 2, Name: "UTM", Status: store.UserActive, IsWorker: true},
		{ID: 3, Name: "黄桑", Status: store.UserActive},
		{ID: 4, Name: "JA", Status: store.UserDisabled},
	}
	got := renderUserDirectory(users, ownerID, map[int64]userDirectoryStats{
		2: {SelfIntro: 0, PeerReview: 0},
		3: {SelfIntro: 6, PeerReview: 1},
		4: {},
	})
	for _, want := range []string{
		"[工具引用·工作内存]",
		`user_id=2 name="UTM" kind=worker status=active`,
		`user_id=3 name="黄桑" kind=human status=active`,
		"[用户可见目录]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("目录缺工具引用 %q:\n%s", want, got)
		}
	}
	visible := got
	if idx := strings.Index(visible, "[用户可见目录]"); idx >= 0 {
		visible = visible[idx+len("[用户可见目录]"):]
	}
	for _, bad := range []string{"user_id", "tg_id", "Telegram ID", "<table", "|---"} {
		if strings.Contains(visible, bad) {
			t.Fatalf("用户可见目录不应泄露工具字段或表格标记 %q:\n%s", bad, visible)
		}
	}
	for _, want := range []string{
		"真人员工（2 位）",
		"- 员工ID 3｜黄桑（正常）｜画像：6 条｜评价：1 条",
		"- 员工ID 4｜JA（已停用）｜画像：暂无可见｜评价：暂无可见",
		"AI worker（1 个，虚拟成员，不计入真人员工）",
		"- 员工ID 2｜UTM（正常，AI worker）｜画像：暂无可见｜评价：暂无可见",
		"统计：真人员工 2 位，AI worker 1 个。",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("目录缺 %q:\n%s", want, got)
		}
	}
}

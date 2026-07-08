package tools

import (
	"strings"
	"testing"

	"github.com/zdypro888/nbco/store"
)

func TestRenderUserHidesInternalIDs(t *testing.T) {
	got := renderUser(&store.User{
		ID:           42,
		Name:         "PRO",
		Status:       store.UserActive,
		IsSuperadmin: true,
		Info:         map[string]string{"职位": "董事长"},
	})
	for _, bad := range []string{"ID", "42", "TG ID", "Telegram ID", "active"} {
		if strings.Contains(got, bad) {
			t.Fatalf("renderUser 泄露内部字段 %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"名字: PRO", "状态: 正常", "身份: 超级管理员", "职位: 董事长"} {
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
	for _, bad := range []string{"#2", "#3", "#4", "ID", "user_id", "active", "<table", "|---"} {
		if strings.Contains(got, bad) {
			t.Fatalf("目录不应泄露内部编号/原始状态/表格标记 %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{
		"真人员工（2 位）",
		"- 黄桑（正常）｜画像：6 条｜评价：1 条",
		"- JA（已停用）｜画像：暂无可见｜评价：暂无可见",
		"AI worker（1 个，虚拟成员，不计入真人员工）",
		"- UTM（正常，AI worker）｜画像：暂无可见｜评价：暂无可见",
		"统计：真人员工 2 位，AI worker 1 个。",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("目录缺 %q:\n%s", want, got)
		}
	}
}

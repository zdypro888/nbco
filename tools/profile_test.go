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

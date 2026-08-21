package tools

import (
	"encoding/json"
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
	}, 4, 0, 50)
	var result struct {
		Users []struct {
			UserID              int64  `json:"user_id"`
			Name                string `json:"name"`
			Kind                string `json:"kind"`
			Status              string `json:"status"`
			IsCurrent           bool   `json:"is_current"`
			VisibleProfileCount int    `json:"visible_profile_count"`
			VisibleReviewCount  int    `json:"visible_review_count"`
		} `json:"users"`
		Total       int  `json:"total"`
		HasMore     bool `json:"has_more"`
		HumanCount  int  `json:"page_human_count"`
		WorkerCount int  `json:"page_worker_count"`
	}
	if err := json.Unmarshal([]byte(got), &result); err != nil {
		t.Fatalf("directory is not structured JSON: %v\n%s", err, got)
	}
	if result.Total != 4 || result.HasMore || result.HumanCount != 3 || result.WorkerCount != 1 || len(result.Users) != 4 {
		t.Fatalf("directory counts = humans:%d workers:%d users:%d", result.HumanCount, result.WorkerCount, len(result.Users))
	}
	byID := make(map[int64]struct {
		name, kind, status string
		profiles, reviews  int
	})
	for _, item := range result.Users {
		byID[item.UserID] = struct {
			name, kind, status string
			profiles, reviews  int
		}{item.Name, item.Kind, item.Status, item.VisibleProfileCount, item.VisibleReviewCount}
	}
	if got := byID[2]; got.name != "UTM" || got.kind != "worker" || got.status != store.UserActive {
		t.Fatalf("worker entry = %+v", got)
	}
	if got := byID[3]; got.name != "黄桑" || got.kind != "human" || got.profiles != 6 || got.reviews != 1 {
		t.Fatalf("human entry = %+v", got)
	}
	if !result.Users[0].IsCurrent || result.Users[0].UserID != ownerID {
		t.Fatalf("current user must remain in complete directory: %+v", result.Users[0])
	}
}

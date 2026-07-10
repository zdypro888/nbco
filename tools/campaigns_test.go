package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestRenderDataCampaignListKeepsEvidenceScopesSeparate(t *testing.T) {
	empty := renderDataCampaignList(nil)
	for _, want := range []string{"未建立专项追踪", "不能据此判断", "如本轮可用系统活动账本", "证据不足"} {
		if !strings.Contains(empty, want) {
			t.Fatalf("empty campaign result missing %q: %s", want, empty)
		}
	}

	got := renderDataCampaignList([]store.DataCollectionCampaignView{{
		DataCollectionCampaign: store.DataCollectionCampaign{ID: 3, Title: "完善档案", Status: store.DataCampaignActive, CreatedBy: 1},
		CreatorName:            "PRO",
		Total:                  24,
		Completed:              7,
		Pending:                17,
		Notified:               18,
	}})
	for _, want := range []string{"完成 7/24", "待补 17", "已通知 18", "待补只表示字段仍缺失", "不会自动重复提醒"} {
		if !strings.Contains(got, want) {
			t.Fatalf("campaign list missing %q:\n%s", want, got)
		}
	}
}

func TestRenderDataCampaignDetailDistinguishesNotificationFromProgress(t *testing.T) {
	now := time.Now()
	campaign := &store.DataCollectionCampaign{
		ID: 3, Title: "完善档案", Status: store.DataCampaignActive,
		RequiredFields: []string{"手机", "职位"},
	}
	got := renderDataCampaignDetail(campaign, []store.DataCollectionCampaignTarget{
		{UserID: 2, UserName: "Alice", Status: store.DataCampaignTargetPending, MissingFields: []string{"职位"}, LastNotifiedAt: &now},
		{UserID: 3, UserName: "Bob", Status: store.DataCampaignTargetPending, MissingFields: []string{"手机", "职位"}},
		{UserID: 4, UserName: "Carol", Status: store.DataCampaignTargetCompleted, CompletedAt: &now},
	})
	for _, want := range []string{"完成：1/3", "待补：2", "已通知：1", "Alice｜待补：职位｜已通知", "Bob｜待补：手机、职位｜未通知", "不代表成员正在处理"} {
		if !strings.Contains(got, want) {
			t.Fatalf("campaign detail missing %q:\n%s", want, got)
		}
	}
}

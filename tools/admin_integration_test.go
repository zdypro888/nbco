package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestCompanyOverviewExpandsOnlyStructuredWorkEvidence(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	owner := mkToolsUser(t, s, "overview-owner", true)
	now := time.Now().UTC()
	for _, in := range []store.WorkEvidenceInput{
		{
			SourceType: "telegram", SourceKey: "overview-raw", Kind: store.WorkEvidenceCommunication,
			Status: store.WorkEvidenceObserved, Title: "成员", Content: "不应出现在公司概览的原始消息",
			ActorUserID: &owner.ID, EventAt: now, CreatedBy: &owner.ID,
		},
		{
			SourceType: "telegram_group_digest", SourceKey: "overview-summary", Kind: store.WorkEvidenceSummary,
			Status: store.WorkEvidenceActive, Title: "项目摘要", Content: "可用于主动报告的结构化结论",
			ActorUserID: &owner.ID, EventAt: now.Add(time.Second), CreatedBy: &owner.ID,
		},
	} {
		if _, err := s.UpsertWorkEvidence(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	overview, err := CompanyOverview(ctx, s, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(overview, "群聊消息 1") || !strings.Contains(overview, "可用于主动报告的结构化结论") {
		t.Fatalf("overview missing aggregate or structured evidence: %s", overview)
	}
	if strings.Contains(overview, "不应出现在公司概览的原始消息") {
		t.Fatalf("overview leaked raw communication: %s", overview)
	}
}

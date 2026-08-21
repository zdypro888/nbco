package tools

import (
	"context"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestGenericKnowledgeUpdateCannotBypassSkillContract(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	admin := mkToolsUser(t, s, "knowledge-admin", true)
	skill, err := s.CreateSkill(ctx, "发布流程", store.NewSkillContent(
		"发布前", "验证后发布", "运行测试，再执行部署", "不得跳过验证",
	), []string{"scope:global"}, admin.ID)
	if err != nil {
		t.Fatal(err)
	}

	result := callToolByName(t, knowledgeTools(Deps{Store: s, TZ: time.UTC}, admin), "update_knowledge", map[string]any{
		"id": skill.ID, "content": "任意展示文本",
	})
	if !ToolResultRejected(result) {
		t.Fatalf("generic skill update result = %q, want structured rejection", result)
	}
	after, err := s.KnowledgeByID(ctx, skill.ID)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := store.DecodeSkillContent(after.Content)
	if err != nil {
		t.Fatalf("persisted skill contract corrupted: %v", err)
	}
	if decoded.Procedure != "运行测试，再执行部署" {
		t.Fatalf("skill procedure changed through generic tool: %+v", decoded)
	}
}

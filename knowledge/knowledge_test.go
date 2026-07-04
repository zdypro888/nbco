package knowledge

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/store"
)

func TestMerge(t *testing.T) {
	k := func(id int64) *store.Knowledge { return &store.Knowledge{ID: id} }
	out := merge([]*store.Knowledge{k(1), k(2)}, []*store.Knowledge{k(2), k(3)}, 3)
	if len(out) != 3 || out[0].ID != 1 || out[1].ID != 2 || out[2].ID != 3 {
		t.Fatalf("语义在前、去重、补足: %+v", out)
	}
	if l := merge([]*store.Knowledge{k(1), k(2), k(3)}, nil, 2); len(l) != 2 {
		t.Fatalf("应截到 limit: %d", len(l))
	}
}

// bagEmbedder：固定词表的词袋向量，让相似文本向量相近（可测语义排序）。
type bagEmbedder struct{ vocab []string }

func (b bagEmbedder) Model() string { return "bag" }
func (b bagEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, len(b.vocab))
		for j, w := range b.vocab {
			v[j] = float32(strings.Count(t, w))
		}
		out[i] = v
	}
	return out, nil
}

func TestSemanticSearchRanks(t *testing.T) {
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 NBCO_TEST_PG_DSN")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Pool().Exec(ctx, `TRUNCATE knowledge RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "u", true, store.Identity{Provider: "test", ExternalID: "kb"})
	if err != nil {
		t.Fatal(err)
	}

	emb := bagEmbedder{vocab: []string{"数据库", "备份", "前端", "样式", "部署"}}
	svc := New(s, emb)
	// 存三条，都会被 embed。
	dbK, _ := svc.Save(ctx, "数据库备份", "每天凌晨备份数据库到对象存储", nil, u.ID)
	_, _ = svc.Save(ctx, "前端样式规范", "统一使用设计系统的样式变量", nil, u.ID)
	_, _ = svc.Save(ctx, "部署脚本", "一键部署到生产", nil, u.ID)

	// 语义查询「数据库怎么备份」——即便措辞不同，词袋相似度也把 dbK 排首位。
	res, err := svc.Search(ctx, "数据库 备份 怎么做", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].ID != dbK.ID {
		t.Fatalf("语义检索应把数据库备份排首位: %+v", res)
	}
}

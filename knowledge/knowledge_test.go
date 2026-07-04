package knowledge

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/store"
)

const testDBLockKey = 7767002

func openKnowledgeTestStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 NBCO_TEST_PG_DSN")
	}
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, testDBLockKey); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, testDBLockKey)
		conn.Release()
	})
	if _, err := s.Pool().Exec(ctx, `TRUNCATE users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

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
	s, ctx := openKnowledgeTestStore(t)
	u, err := s.CreateUser(ctx, "u", true, store.Identity{Provider: "test", ExternalID: "kb"})
	if err != nil {
		t.Fatal(err)
	}

	emb := bagEmbedder{vocab: []string{"数据库", "备份", "前端", "样式", "部署"}}
	svc := New(s, emb)
	// 存三条（embed 现在异步 fire-and-forget）。
	dbK, err := svc.Save(ctx, "数据库备份", "每天凌晨备份数据库到对象存储", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, "前端样式规范", "统一使用设计系统的样式变量", nil, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, "部署脚本", "一键部署到生产", nil, u.ID); err != nil {
		t.Fatal(err)
	}
	// 用 Backfill（同步）确保三条都嵌入，消除异步竞态。
	for {
		_, embedded, err := svc.Backfill(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if embedded == 0 {
			break
		}
	}

	// 语义查询「数据库怎么备份」——即便措辞不同，词袋相似度也把 dbK 排首位。
	res, err := svc.Search(ctx, "数据库 备份 怎么做", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].ID != dbK.ID {
		t.Fatalf("语义检索应把数据库备份排首位: %+v", res)
	}
}

// failEmbedder：始终失败，验证 Backfill 探测失败即停、不死循环空转。
type failEmbedder struct{}

func (failEmbedder) Model() string { return "fail" }
func (failEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, context.DeadlineExceeded
}

func TestBackfillStopsWhenServiceDown(t *testing.T) {
	s, ctx := openKnowledgeTestStore(t)
	u, err := s.CreateUser(ctx, "u", true, store.Identity{Provider: "test", ExternalID: "kbfail"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateKnowledge(ctx, "标题", "正文", nil, u.ID); err != nil {
		t.Fatal(err)
	}

	// 服务失败：探测失败→返回 error、零尝试零成功（驱动据此停手，不空转）。
	attempted, embedded, err := New(s, failEmbedder{}).Backfill(ctx, 10)
	if err == nil {
		t.Fatal("服务失败应返回 error 让驱动停手")
	}
	if attempted != 0 || embedded != 0 {
		t.Fatalf("探测失败不应尝试嵌入: attempted=%d embedded=%d", attempted, embedded)
	}
	// nil embedder：安全空返回。
	if a, e, err := New(s, nil).Backfill(ctx, 10); a != 0 || e != 0 || err != nil {
		t.Fatalf("nil embedder Backfill = %d %d %v", a, e, err)
	}
}

package knowledge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	if len(out) != 3 || out[0].ID != 2 || out[1].ID != 1 || out[2].ID != 3 {
		t.Fatalf("RRF 应优先语义和词法共同命中的条目: %+v", out)
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
	var cursor int64
	for {
		res, err := svc.Backfill(ctx, 10, cursor)
		if err != nil {
			t.Fatal(err)
		}
		cursor = res.LastID
		if !res.HasMore {
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

type fixedEmbedder struct{}

func (fixedEmbedder) Model() string { return "fixed" }
func (fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

type countingQueryEmbedder struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	fail    bool
}

func (e *countingQueryEmbedder) Model() string { return "counting" }

func (e *countingQueryEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	if e.fail {
		return nil, fmt.Errorf("temporary embedding failure")
	}
	if e.entered != nil {
		e.once.Do(func() { close(e.entered) })
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return [][]float32{{1, float32(len(strings.Join(texts, "")))}}, nil
}

func TestQueryVectorDeduplicatesAndCaches(t *testing.T) {
	emb := &countingQueryEmbedder{entered: make(chan struct{}), release: make(chan struct{})}
	svc := New(nil, emb)
	const callers = 12
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.queryVector(context.Background(), "同一轮输入")
			errs <- err
		}()
	}
	select {
	case <-emb.entered:
	case <-time.After(time.Second):
		t.Fatal("embedding request did not start")
	}
	close(emb.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := emb.calls.Load(); got != 1 {
		t.Fatalf("concurrent identical queries called embedder %d times", got)
	}
	if _, err := svc.queryVector(context.Background(), "同一轮输入"); err != nil {
		t.Fatal(err)
	}
	if got := emb.calls.Load(); got != 1 {
		t.Fatalf("cached query called embedder again: %d", got)
	}
}

func TestQueryVectorFailureCooldown(t *testing.T) {
	emb := &countingQueryEmbedder{fail: true}
	svc := New(nil, emb)
	if _, err := svc.queryVector(context.Background(), "first"); err == nil {
		t.Fatal("expected first embedding failure")
	}
	if _, err := svc.queryVector(context.Background(), "second"); err == nil {
		t.Fatal("expected cooldown failure")
	}
	if got := emb.calls.Load(); got != 1 {
		t.Fatalf("cooldown should suppress repeated requests, calls=%d", got)
	}
}

func TestScopedSemanticSearch(t *testing.T) {
	s, ctx := openKnowledgeTestStore(t)
	worker, err := s.CreateUser(ctx, "worker", true, store.Identity{Provider: "test", ExternalID: "scoped-worker"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "other", true, store.Identity{Provider: "test", ExternalID: "scoped-other"})
	if err != nil {
		t.Fatal(err)
	}
	own, err := s.CreateKnowledge(ctx, "alpha", "beta", []string{"project:7"}, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateKnowledge(ctx, "gamma", "delta", []string{"project:8"}, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []*store.Knowledge{own, foreign} {
		if err := s.SetKnowledgeEmbedding(ctx, k.ID, "fixed:2", []float32{1, 0}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(s, fixedEmbedder{})
	byAuthor, err := svc.SearchByAuthor(ctx, worker.ID, "needle", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(byAuthor) != 1 || byAuthor[0].ID != own.ID {
		t.Fatalf("作者 scoped 语义应只召回自己的经验: %+v", byAuthor)
	}
	byTag, err := svc.SearchByTag(ctx, "project:7", "needle", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(byTag) != 1 || byTag[0].ID != own.ID {
		t.Fatalf("项目 scoped 语义应只召回本项目经验: %+v", byTag)
	}
}

// failEmbedder：始终失败，验证 Backfill 探测失败即停、不死循环空转。
type failEmbedder struct{}

func (failEmbedder) Model() string { return "fail" }
func (failEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, context.DeadlineExceeded
}

type selectiveEmbedder struct{}

func (selectiveEmbedder) Model() string { return "selective" }
func (selectiveEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if strings.Contains(text, "BAD") {
			return nil, fmt.Errorf("bad content")
		}
		out[i] = []float32{1, float32(len(text))}
	}
	return out, nil
}

func TestBackfillSkipsFailedRowsWithoutStarvingLaterKnowledge(t *testing.T) {
	s, ctx := openKnowledgeTestStore(t)
	u, err := s.CreateUser(ctx, "u", true, store.Identity{Provider: "test", ExternalID: "kbskip"})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := s.CreateKnowledge(ctx, "BAD", "不能向量化", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	good, err := s.CreateKnowledge(ctx, "GOOD", "可以向量化", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}

	svc := New(s, selectiveEmbedder{})
	res, err := svc.Backfill(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted != 1 || res.Embedded != 0 || res.LastID != bad.ID || !res.HasMore {
		t.Fatalf("bad batch = %+v, badID=%d", res, bad.ID)
	}
	res, err = svc.Backfill(ctx, 1, res.LastID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted != 1 || res.Embedded != 1 || res.LastID != good.ID {
		t.Fatalf("good batch = %+v, goodID=%d", res, good.ID)
	}
	need, err := s.KnowledgeNeedingEmbedding(ctx, "selective:2", 10)
	if err != nil {
		t.Fatal(err)
	}
	var stillNeedsBad, stillNeedsGood bool
	for _, k := range need {
		stillNeedsBad = stillNeedsBad || k.ID == bad.ID
		stillNeedsGood = stillNeedsGood || k.ID == good.ID
	}
	if !stillNeedsBad || stillNeedsGood {
		t.Fatalf("失败行应保留待重试，后续成功行不应再待回填: need=%+v", need)
	}
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
	res, err := New(s, failEmbedder{}).Backfill(ctx, 10, 0)
	if err == nil {
		t.Fatal("服务失败应返回 error 让驱动停手")
	}
	if res.Attempted != 0 || res.Embedded != 0 {
		t.Fatalf("探测失败不应尝试嵌入: %+v", res)
	}
	// nil embedder：安全空返回。
	if res, err := New(s, nil).Backfill(ctx, 10, 0); res.Attempted != 0 || res.Embedded != 0 || err != nil {
		t.Fatalf("nil embedder Backfill = %+v %v", res, err)
	}
}

func TestRuleApplies(t *testing.T) {
	cases := []struct {
		tags    []string
		channel string
		userID  int64
		want    bool
	}{
		{nil, "telegram", 1, true},                                        // 无 scope 标签 = 全局
		{[]string{"misc"}, "api", 1, true},                                // 非 scope 标签不参与判定
		{[]string{"scope:global"}, "worker", 1, true},                     // 显式全局
		{[]string{"scope:telegram"}, "telegram", 1, true},                 // 渠道精确命中
		{[]string{"scope:telegram"}, "telegram:group:42", 1, true},        // 派生渠道命中
		{[]string{"scope:telegram"}, "api", 1, false},                     // 渠道不匹配
		{[]string{"scope:worker"}, "worker", 5, true},                     // worker 场景
		{[]string{"scope:worker"}, "telegram", 5, false},                  // worker 规则不进聊天
		{[]string{"scope:user:7"}, "api", 7, true},                        // 用户命中
		{[]string{"scope:user:7"}, "api", 8, false},                       // 用户不匹配
		{[]string{"scope:user:7", "scope:telegram"}, "telegram", 8, true}, // 多 scope 任一命中
	}
	for i, c := range cases {
		if got := RuleApplies(c.tags, c.channel, c.userID); got != c.want {
			t.Errorf("case %d: RuleApplies(%v, %q, %d) = %v, want %v", i, c.tags, c.channel, c.userID, got, c.want)
		}
	}
}

func TestRulesSemanticSearch(t *testing.T) {
	s, ctx := openKnowledgeTestStore(t)
	u, err := s.CreateUser(ctx, "u", true, store.Identity{Provider: "test", ExternalID: "rules"})
	if err != nil {
		t.Fatal(err)
	}
	emb := bagEmbedder{vocab: []string{"凭据", "泄露", "周报", "格式", "部署", "群", "邀请", "员工"}}
	svc := New(s, emb)

	if _, err := svc.SaveRule(ctx, "凭据保密", "不得泄露凭据", []string{"scope:global"}, u.ID, true); err != nil {
		t.Fatal(err)
	}
	weekly, err := svc.SaveRule(ctx, "周报格式", "周报格式用列表", []string{"scope:telegram"}, u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, "部署知识", "部署走一键脚本", nil, u.ID); err != nil {
		t.Fatal(err)
	}
	groupSkill, err := svc.SaveSkill(ctx, "群邀请流程", "触发条件：群里有人要加入\n摘要：先识别身份再邀请员工\n执行方法：查群成员，确认真人员工后生成邀请", []string{"scope:telegram"}, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	for {
		res, err := svc.Backfill(ctx, 10, cursor)
		if err != nil {
			t.Fatal(err)
		}
		cursor = res.LastID
		if !res.HasMore {
			break
		}
	}

	// 规则检索：语义命中动态规则；常驻规则与普通知识都不出现。
	res, err := svc.SearchRules(ctx, "这周的周报什么格式", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ID != weekly.ID {
		t.Fatalf("SearchRules 应只回动态规则: %+v", res)
	}
	res, err = svc.SearchRules(ctx, "数据库备份策略", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("无关查询不应仅因 topN 注入规则: %+v", res)
	}
	skills, err := svc.SearchSkills(ctx, "群里有人说要邀请员工", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].ID != groupSkill.ID {
		t.Fatalf("SearchSkills 应召回 skill 且不混规则/知识: %+v", skills)
	}
}

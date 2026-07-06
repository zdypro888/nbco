package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Knowledge 知识条目：对话与任务中沉淀的可复用结论（决策、流程、方案、约定）。
// 全员可读写（公司共享资产）；删除限作者与超管（在工具层校验）。
// kind=policy 的条目是行为规则（Policy Memory）：pinned 的每轮常驻系统提示，
// 其余按语义相关度动态注入；作用域用 tags 表达（scope:global/telegram/worker/user:<id>）。
type Knowledge struct {
	ID        int64
	Title     string
	Content   string
	Tags      []string
	AuthorID  int64
	Kind      string
	Pinned    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Knowledge.Kind 取值。
const (
	KnowledgeKindFact   = "fact"   // 普通知识（默认）
	KnowledgeKindPolicy = "policy" // 行为规则
)

const knowledgeCols = `id, title, content, tags, author_id, kind, pinned, created_at, updated_at`

func scanKnowledge(row interface{ Scan(...any) error }) (*Knowledge, error) {
	var k Knowledge
	if err := row.Scan(&k.ID, &k.Title, &k.Content, &k.Tags, &k.AuthorID, &k.Kind, &k.Pinned, &k.CreatedAt, &k.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &k, nil
}

// CreateKnowledge 存一条知识。
func (s *Store) CreateKnowledge(ctx context.Context, title, content string, tags []string, authorID int64) (*Knowledge, error) {
	if tags == nil {
		tags = []string{}
	}
	return scanKnowledge(s.pool.QueryRow(ctx,
		`INSERT INTO knowledge (title, content, tags, author_id) VALUES ($1, $2, $3, $4)
		 RETURNING `+knowledgeCols, title, content, tags, authorID))
}

// CreateRule 存一条行为规则（kind=policy）。pinned 规则每轮常驻系统提示。
func (s *Store) CreateRule(ctx context.Context, title, content string, tags []string, authorID int64, pinned bool) (*Knowledge, error) {
	if tags == nil {
		tags = []string{}
	}
	return scanKnowledge(s.pool.QueryRow(ctx,
		`INSERT INTO knowledge (title, content, tags, author_id, kind, pinned) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+knowledgeCols, title, content, tags, authorID, KnowledgeKindPolicy, pinned))
}

// PinnedRules 全部常驻规则（按创建序）。
func (s *Store) PinnedRules(ctx context.Context) ([]*Knowledge, error) {
	return s.queryKnowledge(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge WHERE kind = $1 AND pinned ORDER BY id`, KnowledgeKindPolicy)
}

// ListRules 全部规则，常驻在前、新的在前。
func (s *Store) ListRules(ctx context.Context, limit int) ([]*Knowledge, error) {
	return s.queryKnowledge(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge WHERE kind = $1 ORDER BY pinned DESC, id DESC LIMIT $2`,
		KnowledgeKindPolicy, limit)
}

// SetRulePinned 调整规则常驻与否。目标非规则返回 ErrNotFound。
func (s *Store) SetRulePinned(ctx context.Context, id int64, pinned bool) error {
	return s.execOne(ctx,
		`UPDATE knowledge SET pinned = $2, updated_at = now() WHERE id = $1 AND kind = $3`,
		id, pinned, KnowledgeKindPolicy)
}

// SearchRules 词法检索非常驻规则（常驻的已在系统提示里，不参与动态召回）。
func (s *Store) SearchRules(ctx context.Context, query string, limit int) ([]*Knowledge, error) {
	return s.searchKnowledge(ctx, query, limit, "kind = $1 AND NOT pinned", []any{KnowledgeKindPolicy})
}

// EmbeddedRules 取非常驻规则的已嵌入向量（语义候选）。
func (s *Store) EmbeddedRules(ctx context.Context, model string) ([]KnowledgeVec, error) {
	return s.embeddedKnowledge(ctx,
		`embed_model = $1 AND embedding IS NOT NULL AND kind = $2 AND NOT pinned`, model, KnowledgeKindPolicy)
}

// UpdateKnowledge 更新知识条目（nil 字段不动；tags 传 nil 不动，空切片清空）。
// 标题或正文变更时清空 embed_model：向量按「标题×2+正文」构造，只改标题也会
// 让旧向量失真；清标签后即时 Reembed 或启动回填都会重新嵌入，不清则永不刷新。
func (s *Store) UpdateKnowledge(ctx context.Context, id int64, title, content *string, tags []string) (*Knowledge, error) {
	return scanKnowledge(s.pool.QueryRow(ctx,
		`UPDATE knowledge SET
		   title = COALESCE($2, title),
		   content = COALESCE($3, content),
		   tags = COALESCE($4, tags),
		   embed_model = CASE WHEN $2 IS NOT NULL OR $3 IS NOT NULL THEN '' ELSE embed_model END,
		   updated_at = now()
		 WHERE id = $1 RETURNING `+knowledgeCols, id, title, content, tags))
}

// KnowledgeByID 取单条知识。
func (s *Store) KnowledgeByID(ctx context.Context, id int64) (*Knowledge, error) {
	return scanKnowledge(s.pool.QueryRow(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge WHERE id = $1`, id))
}

// DeleteKnowledge 删除知识条目。
func (s *Store) DeleteKnowledge(ctx context.Context, id int64) error {
	return s.execOne(ctx, `DELETE FROM knowledge WHERE id = $1`, id)
}

// SearchKnowledge 词法检索：把 query 切成词，任一词命中标题/正文，或整串命中 tag，
// 按「命中词数」优先、其次新旧排序。比单串 ILIKE 更能召回多关键词查询。
func (s *Store) SearchKnowledge(ctx context.Context, query string, limit int) ([]*Knowledge, error) {
	return s.searchKnowledge(ctx, query, limit, "kind = $1", []any{KnowledgeKindFact})
}

// SearchKnowledgeByAuthor 按作者优先检索知识，用于 worker 个人经验。
func (s *Store) SearchKnowledgeByAuthor(ctx context.Context, authorID int64, query string, limit int) ([]*Knowledge, error) {
	return s.searchKnowledge(ctx, query, limit, "kind = $1 AND author_id = $2", []any{KnowledgeKindFact, authorID})
}

// SearchKnowledgeByTag 按标签优先检索知识，用于项目/worker 等维度经验。
func (s *Store) SearchKnowledgeByTag(ctx context.Context, tag, query string, limit int) ([]*Knowledge, error) {
	return s.searchKnowledge(ctx, query, limit, "kind = $1 AND $2 = ANY(tags)", []any{KnowledgeKindFact, tag})
}

func (s *Store) searchKnowledge(ctx context.Context, query string, limit int, scope string, scopeArgs []any) ([]*Knowledge, error) {
	terms := searchTerms(query)
	if len(terms) == 0 {
		if scope == "" {
			return s.RecentKnowledge(ctx, limit)
		}
		args := append([]any{}, scopeArgs...)
		args = append(args, limit)
		return s.queryKnowledge(ctx,
			`SELECT `+knowledgeCols+` FROM knowledge WHERE `+scope+` ORDER BY id DESC LIMIT $`+fmt.Sprint(len(args)), args...)
	}
	// 为每个词构造一个 ILIKE 条件，score = 命中词数。
	var conds []string
	args := append([]any{}, scopeArgs...)
	args = append(args, query) // exactArg = 整串（tag 精确匹配）
	exactArg := fmt.Sprintf("$%d", len(args))
	scoreParts := []string{}
	for _, t := range terms {
		args = append(args, "%"+escapeLike(t)+"%")
		p := fmt.Sprintf("$%d", len(args))
		conds = append(conds, fmt.Sprintf("(title ILIKE %s OR content ILIKE %s)", p, p))
		scoreParts = append(scoreParts, fmt.Sprintf("(CASE WHEN title ILIKE %s OR content ILIKE %s THEN 1 ELSE 0 END)", p, p))
	}
	args = append(args, limit)
	where := fmt.Sprintf("(%s = ANY(tags) OR %s)", exactArg, strings.Join(conds, " OR "))
	if scope != "" {
		where = scope + " AND " + where
	}
	sql := fmt.Sprintf(
		`SELECT `+knowledgeCols+` FROM knowledge
		 WHERE %s
		 ORDER BY (CASE WHEN %s = ANY(tags) THEN 100 ELSE 0 END) + %s DESC, id DESC
		 LIMIT $%d`,
		where, exactArg, strings.Join(scoreParts, " + "), len(args))
	return s.queryKnowledge(ctx, sql, args...)
}

// searchTerms 把查询切成检索词：按空白与常见标点分词，去重去空、限长。
// 中文没有空格时整串作为一个词（词法层面够用，语义召回靠 embedding）。
func searchTerms(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ',' || r == '，' ||
			r == '、' || r == ';' || r == '；' || r == '/' || r == '|'
	})
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) >= 12 {
			break
		}
	}
	return out
}

// SetKnowledgeEmbedding 落一条知识的 embedding 向量与产出它的模型名。
func (s *Store) SetKnowledgeEmbedding(ctx context.Context, id int64, model string, vec []float32) error {
	return s.execOne(ctx,
		`UPDATE knowledge SET embedding = $2, embed_model = $3 WHERE id = $1`, id, vec, model)
}

// KnowledgeVec 一条知识的 id + 向量（语义检索候选）。
type KnowledgeVec struct {
	ID        int64
	Embedding []float32
}

// EmbeddedKnowledge 取所有已用指定模型嵌入的知识向量（供应用层 cosine 排序）。
// nbco 规模（一家公司的知识库）下全量加载 + 暴力点积足够，无需向量索引扩展。
func (s *Store) EmbeddedKnowledge(ctx context.Context, model string) ([]KnowledgeVec, error) {
	return s.embeddedKnowledge(ctx, `embed_model = $1 AND embedding IS NOT NULL AND kind = $2`, model, KnowledgeKindFact)
}

// EmbeddedKnowledgeByAuthor 取某作者已嵌入的知识向量。
func (s *Store) EmbeddedKnowledgeByAuthor(ctx context.Context, model string, authorID int64) ([]KnowledgeVec, error) {
	return s.embeddedKnowledge(ctx, `embed_model = $1 AND embedding IS NOT NULL AND kind = $2 AND author_id = $3`, model, KnowledgeKindFact, authorID)
}

// EmbeddedKnowledgeByTag 取带指定标签的已嵌入知识向量。
func (s *Store) EmbeddedKnowledgeByTag(ctx context.Context, model, tag string) ([]KnowledgeVec, error) {
	return s.embeddedKnowledge(ctx, `embed_model = $1 AND embedding IS NOT NULL AND kind = $2 AND $3 = ANY(tags)`, model, KnowledgeKindFact, tag)
}

func (s *Store) embeddedKnowledge(ctx context.Context, where string, args ...any) ([]KnowledgeVec, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, embedding FROM knowledge WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KnowledgeVec
	for rows.Next() {
		var kv KnowledgeVec
		if err := rows.Scan(&kv.ID, &kv.Embedding); err != nil {
			return nil, err
		}
		out = append(out, kv)
	}
	return out, rows.Err()
}

// KnowledgeByIDs 按 id 批量取知识，保持传入顺序（语义排序后回取详情）。
func (s *Store) KnowledgeByIDs(ctx context.Context, ids []int64) ([]*Knowledge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ks, err := s.queryKnowledge(ctx, `SELECT `+knowledgeCols+` FROM knowledge WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*Knowledge, len(ks))
	for _, k := range ks {
		byID[k.ID] = k
	}
	out := make([]*Knowledge, 0, len(ids))
	for _, id := range ids {
		if k := byID[id]; k != nil {
			out = append(out, k)
		}
	}
	return out, nil
}

// KnowledgeNeedingEmbedding 取尚未按当前模型嵌入的知识（回填用）。
func (s *Store) KnowledgeNeedingEmbedding(ctx context.Context, model string, limit int) ([]*Knowledge, error) {
	return s.KnowledgeNeedingEmbeddingAfter(ctx, model, 0, limit)
}

// KnowledgeNeedingEmbeddingAfter 从 id 游标之后取尚未按当前模型嵌入的知识。
// 回填驱动用游标顺序扫描整库，避免某个失败行长期挡住它后面的知识。
func (s *Store) KnowledgeNeedingEmbeddingAfter(ctx context.Context, model string, afterID int64, limit int) ([]*Knowledge, error) {
	return s.queryKnowledge(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge WHERE embed_model <> $1 AND id > $2 ORDER BY id LIMIT $3`, model, afterID, limit)
}

// RecentKnowledge 最近的知识条目。
func (s *Store) RecentKnowledge(ctx context.Context, limit int) ([]*Knowledge, error) {
	return s.queryKnowledge(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge WHERE kind = $1 ORDER BY id DESC LIMIT $2`, KnowledgeKindFact, limit)
}

func (s *Store) queryKnowledge(ctx context.Context, sql string, args ...any) ([]*Knowledge, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ks []*Knowledge
	for rows.Next() {
		k, err := scanKnowledge(rows)
		if err != nil {
			return nil, err
		}
		ks = append(ks, k)
	}
	return ks, rows.Err()
}

// escapeLike 转义 LIKE 通配符，让用户输入按字面匹配。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

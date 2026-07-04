package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Knowledge 知识条目：对话与任务中沉淀的可复用结论（决策、流程、方案、约定）。
// 全员可读写（公司共享资产）；删除限作者与超管（在工具层校验）。
type Knowledge struct {
	ID        int64
	Title     string
	Content   string
	Tags      []string
	AuthorID  int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

const knowledgeCols = `id, title, content, tags, author_id, created_at, updated_at`

func scanKnowledge(row interface{ Scan(...any) error }) (*Knowledge, error) {
	var k Knowledge
	if err := row.Scan(&k.ID, &k.Title, &k.Content, &k.Tags, &k.AuthorID, &k.CreatedAt, &k.UpdatedAt); err != nil {
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

// UpdateKnowledge 更新知识条目（nil 字段不动；tags 传 nil 不动，空切片清空）。
func (s *Store) UpdateKnowledge(ctx context.Context, id int64, title, content *string, tags []string) (*Knowledge, error) {
	return scanKnowledge(s.pool.QueryRow(ctx,
		`UPDATE knowledge SET
		   title = COALESCE($2, title),
		   content = COALESCE($3, content),
		   tags = COALESCE($4, tags),
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
	terms := searchTerms(query)
	if len(terms) == 0 {
		return s.RecentKnowledge(ctx, limit)
	}
	// 为每个词构造一个 ILIKE 条件，score = 命中词数。
	var conds []string
	args := []any{query} // $1 = 整串（tag 精确匹配）
	scoreParts := []string{}
	for _, t := range terms {
		args = append(args, "%"+escapeLike(t)+"%")
		p := fmt.Sprintf("$%d", len(args))
		conds = append(conds, fmt.Sprintf("(title ILIKE %s OR content ILIKE %s)", p, p))
		scoreParts = append(scoreParts, fmt.Sprintf("(CASE WHEN title ILIKE %s OR content ILIKE %s THEN 1 ELSE 0 END)", p, p))
	}
	args = append(args, limit)
	sql := fmt.Sprintf(
		`SELECT `+knowledgeCols+` FROM knowledge
		 WHERE $1 = ANY(tags) OR %s
		 ORDER BY (CASE WHEN $1 = ANY(tags) THEN 100 ELSE 0 END) + %s DESC, id DESC
		 LIMIT $%d`,
		strings.Join(conds, " OR "), strings.Join(scoreParts, " + "), len(args))
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
	rows, err := s.pool.Query(ctx,
		`SELECT id, embedding FROM knowledge WHERE embed_model = $1 AND embedding IS NOT NULL`, model)
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
	return s.queryKnowledge(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge WHERE embed_model <> $1 ORDER BY id LIMIT $2`, model, limit)
}

// RecentKnowledge 最近的知识条目。
func (s *Store) RecentKnowledge(ctx context.Context, limit int) ([]*Knowledge, error) {
	return s.queryKnowledge(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge ORDER BY id DESC LIMIT $1`, limit)
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

package store

import (
	"context"
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

// SearchKnowledge 按关键词检索：标题/正文模糊匹配，或 tag 精确匹配。新的在前。
func (s *Store) SearchKnowledge(ctx context.Context, query string, limit int) ([]*Knowledge, error) {
	like := "%" + escapeLike(query) + "%"
	return s.queryKnowledge(ctx,
		`SELECT `+knowledgeCols+` FROM knowledge
		 WHERE title ILIKE $1 OR content ILIKE $1 OR $2 = ANY(tags)
		 ORDER BY id DESC LIMIT $3`, like, query, limit)
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

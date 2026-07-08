package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	LearningKindKnowledge = "knowledge"
	LearningKindRule      = "rule"
	LearningKindSkill     = "skill"
	LearningKindScript    = "script"
	LearningKindProfile   = "profile"
	LearningKindSummary   = "summary"

	LearningStatusPending   = "pending"
	LearningStatusPublished = "published"
	LearningStatusRejected  = "rejected"
)

// LearningCandidate is a proposed durable memory item extracted from chats,
// worker results, group discussions, or reflection jobs. It keeps evidence so
// the system can learn without turning every model guess into hidden doctrine.
type LearningCandidate struct {
	ID                   int64
	Kind                 string
	Scope                string
	Title                string
	Content              string
	Tags                 []string
	Evidence             json.RawMessage
	Confidence           float32
	Status               string
	SourceType           string
	SourceRef            string
	CreatedBy            *int64
	ReviewedBy           *int64
	ReviewedAt           *time.Time
	PublishedKnowledgeID *int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type LearningCandidateInput struct {
	Kind       string
	Scope      string
	Title      string
	Content    string
	Tags       []string
	Evidence   json.RawMessage
	Confidence float32
	Status     string
	SourceType string
	SourceRef  string
	CreatedBy  *int64
}

const learningCandidateCols = `id, kind, scope, title, content, tags, evidence, confidence, status, source_type, source_ref, created_by, reviewed_by, reviewed_at, published_knowledge_id, created_at, updated_at`

func scanLearningCandidate(row interface{ Scan(...any) error }) (*LearningCandidate, error) {
	var c LearningCandidate
	if err := row.Scan(&c.ID, &c.Kind, &c.Scope, &c.Title, &c.Content, &c.Tags, &c.Evidence,
		&c.Confidence, &c.Status, &c.SourceType, &c.SourceRef, &c.CreatedBy, &c.ReviewedBy,
		&c.ReviewedAt, &c.PublishedKnowledgeID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &c, nil
}

func (s *Store) CreateLearningCandidate(ctx context.Context, in LearningCandidateInput) (*LearningCandidate, error) {
	if len(in.Evidence) == 0 {
		in.Evidence = json.RawMessage(`{}`)
	}
	if in.Scope == "" {
		in.Scope = "global"
	}
	if len(in.Tags) == 0 { // tags 列 NOT NULL：nil/空 → 空数组，避免插入 NULL 违反约束
		in.Tags = []string{}
	}
	if in.Status == "" {
		in.Status = LearningStatusPending
	}
	return scanLearningCandidate(s.pool.QueryRow(ctx,
		`INSERT INTO learning_candidates
		   (kind, scope, title, content, tags, evidence, confidence, status, source_type, source_ref, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING `+learningCandidateCols,
		in.Kind, in.Scope, in.Title, in.Content, in.Tags, in.Evidence, in.Confidence,
		in.Status, in.SourceType, in.SourceRef, in.CreatedBy))
}

func (s *Store) LearningCandidateByID(ctx context.Context, id int64) (*LearningCandidate, error) {
	return scanLearningCandidate(s.pool.QueryRow(ctx,
		`SELECT `+learningCandidateCols+` FROM learning_candidates WHERE id = $1`, id))
}

func (s *Store) ListLearningCandidates(ctx context.Context, status, kind string, limit int) ([]*LearningCandidate, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	sql := `SELECT ` + learningCandidateCols + ` FROM learning_candidates WHERE TRUE`
	args := []any{}
	if status != "" {
		args = append(args, status)
		sql += ` AND status = $` + fmt.Sprint(len(args))
	}
	if kind != "" {
		args = append(args, kind)
		sql += ` AND kind = $` + fmt.Sprint(len(args))
	}
	args = append(args, limit)
	sql += ` ORDER BY id DESC LIMIT $` + fmt.Sprint(len(args))
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LearningCandidate
	for rows.Next() {
		c, err := scanLearningCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) LearningCandidateExists(ctx context.Context, kind, title string, statuses ...string) (bool, error) {
	if kind == "" || title == "" {
		return false, nil
	}
	sql := `SELECT EXISTS(SELECT 1 FROM learning_candidates WHERE kind = $1 AND lower(trim(title)) = lower(trim($2))`
	args := []any{kind, title}
	if len(statuses) > 0 {
		args = append(args, statuses)
		sql += ` AND status = ANY($3)`
	}
	sql += `)`
	var ok bool
	if err := s.pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

func (s *Store) MarkLearningCandidatePublished(ctx context.Context, id, reviewerID int64, knowledgeID *int64) error {
	return s.execOne(ctx,
		`UPDATE learning_candidates
		    SET status = $2, reviewed_by = $3, reviewed_at = now(), published_knowledge_id = $4, updated_at = now()
		  WHERE id = $1`,
		id, LearningStatusPublished, reviewerID, knowledgeID)
}

func (s *Store) RejectLearningCandidate(ctx context.Context, id, reviewerID int64) error {
	return s.execOne(ctx,
		`UPDATE learning_candidates
		    SET status = $2, reviewed_by = $3, reviewed_at = now(), updated_at = now()
		  WHERE id = $1`,
		id, LearningStatusRejected, reviewerID)
}

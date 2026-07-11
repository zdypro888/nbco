package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
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

	// LearningDuplicateThreshold is intentionally below exact-text territory:
	// the miner commonly paraphrases the same durable rule across conversations.
	LearningDuplicateThreshold = 0.60
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
	DuplicateOf          *int64
	ConflictWith         *int64
	ValueScore           float32
	ReviewNote           string
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

const learningCandidateCols = `id, kind, scope, title, content, tags, evidence, confidence, status, source_type, source_ref, created_by, reviewed_by, reviewed_at, published_knowledge_id, duplicate_of, conflict_with, value_score, review_note, created_at, updated_at`

func scanLearningCandidate(row interface{ Scan(...any) error }) (*LearningCandidate, error) {
	var c LearningCandidate
	if err := row.Scan(&c.ID, &c.Kind, &c.Scope, &c.Title, &c.Content, &c.Tags, &c.Evidence,
		&c.Confidence, &c.Status, &c.SourceType, &c.SourceRef, &c.CreatedBy, &c.ReviewedBy,
		&c.ReviewedAt, &c.PublishedKnowledgeID, &c.DuplicateOf, &c.ConflictWith,
		&c.ValueScore, &c.ReviewNote, &c.CreatedAt, &c.UpdatedAt); err != nil {
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

// ScoreLearningCandidates gives pending candidates a deterministic governance
// score and links obvious duplicates/conflicts. It is deliberately conservative:
// AI can add nuance in review_note, but the store pass must be stable and cheap.
func (s *Store) ScoreLearningCandidates(ctx context.Context, limit int) (int, error) {
	// 上限 200：冲突检测对每个候选最多再扫 200 条同类（learningConflictID），
	// 外层候选数也限 200，把单次评分的最坏比较次数钳在 200×200=4万，而非无界。
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+learningCandidateCols+`
		   FROM learning_candidates
		  WHERE status = $1
		  ORDER BY id DESC LIMIT $2`, LearningStatusPending, limit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var items []*LearningCandidate
	for rows.Next() {
		c, err := scanLearningCandidate(rows)
		if err != nil {
			return 0, err
		}
		items = append(items, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	updated := 0
	for _, c := range items {
		score := learningValueScore(c)
		dupe, similarity, err := s.learningDuplicateID(ctx, c)
		if err != nil {
			return updated, err
		}
		conflict, err := s.learningConflictID(ctx, c)
		if err != nil {
			return updated, err
		}
		note := ""
		if conflict != nil {
			// A polarity conflict can also have very high lexical similarity. It
			// must remain reviewable instead of being auto-rejected as a duplicate.
			dupe = nil
			note = "疑似冲突：与候选 " + fmt.Sprint(*conflict) + " 的规则/结论方向可能相反。"
			score *= 0.8
		} else if dupe != nil {
			note = fmt.Sprintf("疑似重复：与候选 %d 的语义相似度 %.2f。", *dupe, similarity)
			score *= 0.3
		}
		status := LearningStatusPending
		if dupe != nil && similarity >= 0.9 {
			status = LearningStatusRejected
			note += " 高置信重复，已自动归档。"
		}
		tag, err := s.pool.Exec(ctx,
			`UPDATE learning_candidates
			    SET duplicate_of = $2, conflict_with = $3, value_score = $4, review_note = $5,
			        status = $7, reviewed_at = CASE WHEN $7 = 'rejected' THEN now() ELSE reviewed_at END,
			        updated_at = now()
			  WHERE id = $1 AND status = $6
			    AND (duplicate_of IS DISTINCT FROM $2 OR conflict_with IS DISTINCT FROM $3
			      OR value_score IS DISTINCT FROM $4 OR review_note IS DISTINCT FROM $5 OR status IS DISTINCT FROM $7)`,
			c.ID, dupe, conflict, score, note, LearningStatusPending, status)
		if err != nil {
			return updated, err
		}
		if tag.RowsAffected() > 0 {
			updated++
		}
	}
	return updated, nil
}

func (s *Store) learningDuplicateID(ctx context.Context, c *LearningCandidate) (*int64, float64, error) {
	if c == nil || strings.TrimSpace(c.Title) == "" {
		return nil, 0, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, title, content FROM learning_candidates
		  WHERE id <> $1 AND kind = $2 AND status = ANY($3)
		  ORDER BY CASE WHEN id < $1 THEN 0 ELSE 1 END, id DESC
		  LIMIT 200`, c.ID, c.Kind, []string{LearningStatusPending, LearningStatusPublished})
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var bestID int64
	best := 0.0
	for rows.Next() {
		var id int64
		var title, content string
		if err := rows.Scan(&id, &title, &content); err != nil {
			return nil, 0, err
		}
		sim := LearningTextSimilarity(c.Title, c.Content, title, content)
		if sim > best {
			best, bestID = sim, id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if bestID == 0 || best < LearningDuplicateThreshold {
		return nil, best, nil
	}
	return &bestID, best, nil
}

// SimilarLearningCandidateExists checks semantic-ish text overlap against a
// bounded recent set. It catches paraphrased candidates without another model
// call and is deterministic enough for retries.
func (s *Store) SimilarLearningCandidateExists(ctx context.Context, kind, title, content string, threshold float64, statuses ...string) (bool, error) {
	if threshold <= 0 {
		threshold = LearningDuplicateThreshold
	}
	if len(statuses) == 0 {
		statuses = []string{LearningStatusPending, LearningStatusPublished}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT title, content FROM learning_candidates
		  WHERE kind = $1 AND status = ANY($2) ORDER BY id DESC LIMIT 200`, kind, statuses)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var otherTitle, otherContent string
		if err := rows.Scan(&otherTitle, &otherContent); err != nil {
			return false, err
		}
		if LearningTextsConflict(kind, title, content, otherTitle, otherContent) {
			continue
		}
		if LearningTextSimilarity(title, content, otherTitle, otherContent) >= threshold {
			return true, nil
		}
	}
	return false, rows.Err()
}

func LearningTextSimilarity(titleA, contentA, titleB, contentB string) float64 {
	titleScore := learningShingleDice(learningShingles(titleA), learningShingles(titleB))
	contentScore := learningShingleDice(learningShingles(contentA), learningShingles(contentB))
	combinedScore := learningShingleDice(
		learningShingles(titleA+" "+contentA),
		learningShingles(titleB+" "+contentB),
	)
	// Two views cover both good miner titles and differently titled but strongly
	// overlapping content. Dice is less punitive than Jaccard when one version
	// adds clarifying detail.
	return max(combinedScore, 0.65*titleScore+0.35*contentScore, 0.25*titleScore+0.75*contentScore)
}

func learningShingleDice(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if _, ok := b[token]; ok {
			intersection++
		}
	}
	return float64(2*intersection) / float64(len(a)+len(b))
}

func normalizedLearningTitle(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(title))), " ")
}

func learningShingles(text string) map[string]struct{} {
	var normalized []rune
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized = append(normalized, r)
		} else if len(normalized) > 0 && normalized[len(normalized)-1] != ' ' {
			normalized = append(normalized, ' ')
		}
	}
	clean := strings.TrimSpace(string(normalized))
	out := map[string]struct{}{}
	for _, word := range strings.Fields(clean) {
		out["w:"+word] = struct{}{}
	}
	compact := []rune(strings.ReplaceAll(clean, " ", ""))
	if len(compact) < 3 {
		if len(compact) > 0 {
			out["r:"+string(compact)] = struct{}{}
		}
		return out
	}
	for i := 0; i+3 <= len(compact); i++ {
		out["r:"+string(compact[i:i+3])] = struct{}{}
	}
	return out
}

func (s *Store) learningConflictID(ctx context.Context, c *LearningCandidate) (*int64, error) {
	if c == nil || (c.Kind != LearningKindRule && c.Kind != LearningKindSkill) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+learningCandidateCols+`
		   FROM learning_candidates
		  WHERE id <> $1 AND kind = $2 AND status = ANY($3)
		  ORDER BY id DESC LIMIT 200`,
		c.ID, c.Kind, []string{LearningStatusPending, LearningStatusPublished})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		other, err := scanLearningCandidate(rows)
		if err != nil {
			return nil, err
		}
		if likelyLearningConflict(c, other) {
			id := other.ID
			return &id, nil
		}
	}
	return nil, rows.Err()
}

func learningValueScore(c *LearningCandidate) float32 {
	score := c.Confidence
	if score <= 0 {
		score = 0.4
	}
	if len(c.Content) > 80 {
		score += 0.15
	}
	if len(c.Tags) > 0 {
		score += 0.1
	}
	if len(c.Evidence) > 2 && string(c.Evidence) != "{}" {
		score += 0.2
	}
	if c.Kind == LearningKindRule || c.Kind == LearningKindSkill {
		score += 0.1
	}
	if score > 1 {
		return 1
	}
	return score
}

func likelyLearningConflict(a, b *LearningCandidate) bool {
	if a == nil || b == nil {
		return false
	}
	if a.Kind != LearningKindRule && a.Kind != LearningKindSkill {
		return false
	}
	if !learningSubjectOverlaps(a.Title, a.Content, b.Title, b.Content) {
		return false
	}
	return learningPolarity(a.Title+"\n"+a.Content)*learningPolarity(b.Title+"\n"+b.Content) == -1
}

// LearningTextsConflict exposes the deterministic governance guard used both
// while mining and during periodic candidate scoring.
func LearningTextsConflict(kind, titleA, contentA, titleB, contentB string) bool {
	return likelyLearningConflict(
		&LearningCandidate{Kind: kind, Title: titleA, Content: contentA},
		&LearningCandidate{Kind: kind, Title: titleB, Content: contentB},
	)
}

func learningSubjectOverlaps(titleA, contentA, titleB, contentB string) bool {
	if a, b := normalizedLearningTitle(titleA), normalizedLearningTitle(titleB); a != "" && a == b {
		return true
	}
	titleScore := learningShingleDice(learningShingles(titleA), learningShingles(titleB))
	combinedScore := learningShingleDice(
		learningShingles(stripLearningPolarity(titleA+" "+contentA)),
		learningShingles(stripLearningPolarity(titleB+" "+contentB)),
	)
	return titleScore >= 0.45 || combinedScore >= 0.50
}

func learningPolarity(text string) int {
	text = strings.ToLower(text)
	if containsAny(text, "不要", "禁止", "不能", "不得", "不允许", "关闭", "禁用", "disable", "never", "must not", "do not", "don't") {
		return -1
	}
	if containsAny(text, "必须", "应当", "允许", "可以", "启用", "开启", "默认", "enable", "always", "must", "should") {
		return 1
	}
	return 0
}

func stripLearningPolarity(text string) string {
	text = strings.ToLower(text)
	for _, term := range []string{
		"不要", "禁止", "不能", "不得", "不允许", "关闭", "禁用", "disable", "never", "must not", "do not", "don't",
		"必须", "应当", "允许", "可以", "启用", "开启", "默认", "enable", "always", "must", "should",
	} {
		text = strings.ReplaceAll(text, term, " ")
	}
	return text
}

func containsAny(s string, terms ...string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

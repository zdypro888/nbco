package store

import (
	"context"
	"crypto/sha256"
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

	// MemoryClass is orthogonal to Kind: it records which subsystem owns the
	// truth. Only durable candidates may enter semantic memory automatically.
	LearningMemoryUnclassified = "unclassified"
	LearningMemoryDurable      = "durable"
	LearningMemoryCanonical    = "canonical"
	LearningMemoryTransient    = "transient"

	// LearningDuplicateThreshold only admits related text into Agent review. It
	// never authorizes an automatic duplicate or conflict verdict.
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
	MemoryClass          string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type LearningCandidateInput struct {
	Kind        string
	Scope       string
	Title       string
	Content     string
	Tags        []string
	Evidence    json.RawMessage
	Confidence  float32
	Status      string
	SourceType  string
	SourceRef   string
	CreatedBy   *int64
	MemoryClass string
}

// LearningContextAsset is a rule or skill that actually participated in a
// recent turn. Memory mining uses it to understand what a later correction is
// referring to; it is context, never user evidence.
type LearningContextAsset struct {
	ID      int64
	Kind    string
	Title   string
	Content string
	Tags    []string
	Phase   string
}

type LearningTurnContext struct {
	Messages []ChatMessage
	Assets   []LearningContextAsset
}

// LearningContextBeforeMessage returns a bounded conversational neighborhood
// and the memory assets used by the immediately preceding turns. This closes
// the reference gap for feedback such as "the previous result was wrong"
// without promoting prior assistant text to evidence.
func (s *Store) LearningContextBeforeMessage(
	ctx context.Context,
	sessionID, beforeMessageID int64,
	messageLimit, turnLimit int,
) (*LearningTurnContext, error) {
	if sessionID <= 0 || beforeMessageID <= 0 {
		return &LearningTurnContext{}, nil
	}
	if messageLimit <= 0 || messageLimit > 12 {
		messageLimit = 6
	}
	if turnLimit <= 0 || turnLimit > 4 {
		turnLimit = 2
	}
	messages, err := s.MessagesBefore(ctx, sessionID, 0, beforeMessageID, messageLimit)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`WITH recent_turns AS (
		   SELECT id, assistant_message_id
		     FROM conversation_turns
		    WHERE session_id = $1 AND status = 'completed'
		      AND assistant_message_id IS NOT NULL AND assistant_message_id < $2
		    ORDER BY assistant_message_id DESC
		    LIMIT $3
		 )
		 SELECT k.id, k.kind, k.title, k.content, k.tags, u.phase
		   FROM recent_turns t
		   JOIN conversation_asset_usages u ON u.conversation_turn_id = t.id
		   JOIN knowledge k ON k.id = u.knowledge_id
		  WHERE k.active
		  ORDER BY t.assistant_message_id DESC,
		           CASE u.phase WHEN 'loaded' THEN 0 WHEN 'injected' THEN 1 ELSE 2 END,
		           k.id DESC
		  LIMIT 16`, sessionID, beforeMessageID, turnLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := make([]LearningContextAsset, 0, 16)
	seen := make(map[int64]bool, 16)
	for rows.Next() {
		var asset LearningContextAsset
		if err := rows.Scan(&asset.ID, &asset.Kind, &asset.Title, &asset.Content, &asset.Tags, &asset.Phase); err != nil {
			return nil, wrapErr(err)
		}
		if seen[asset.ID] {
			continue
		}
		seen[asset.ID] = true
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr(err)
	}
	return &LearningTurnContext{Messages: messages, Assets: assets}, nil
}

const learningCandidateCols = `id, kind, scope, title, content, tags, evidence, confidence, status, source_type, source_ref, created_by, reviewed_by, reviewed_at, published_knowledge_id, duplicate_of, conflict_with, value_score, review_note, memory_class, created_at, updated_at`

func scanLearningCandidate(row interface{ Scan(...any) error }) (*LearningCandidate, error) {
	var c LearningCandidate
	if err := row.Scan(&c.ID, &c.Kind, &c.Scope, &c.Title, &c.Content, &c.Tags, &c.Evidence,
		&c.Confidence, &c.Status, &c.SourceType, &c.SourceRef, &c.CreatedBy, &c.ReviewedBy,
		&c.ReviewedAt, &c.PublishedKnowledgeID, &c.DuplicateOf, &c.ConflictWith,
		&c.ValueScore, &c.ReviewNote, &c.MemoryClass, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &c, nil
}

func (s *Store) CreateLearningCandidate(ctx context.Context, in LearningCandidateInput) (*LearningCandidate, error) {
	if len(in.Evidence) == 0 {
		in.Evidence = json.RawMessage(`{}`)
	}
	in.Scope = strings.TrimSpace(in.Scope)
	if in.Scope == "" {
		in.Scope = "global"
	}
	if len(in.Tags) == 0 { // tags 列 NOT NULL：nil/空 → 空数组，避免插入 NULL 违反约束
		in.Tags = []string{}
	}
	if in.Status == "" {
		in.Status = LearningStatusPending
	}
	if in.Kind == LearningKindSkill {
		content, err := DecodeSkillContent(in.Content)
		if err != nil {
			return nil, err
		}
		canonical, err := EncodeSkillContent(content)
		if err != nil {
			return nil, err
		}
		in.Content = canonical
	}
	in.MemoryClass = NormalizeLearningMemoryClass(in.Kind, in.MemoryClass)
	contentIdentity := learningContentIdentity(in.Title, in.Content)
	return scanLearningCandidate(s.pool.QueryRow(ctx,
		`INSERT INTO learning_candidates
		   (kind, scope, title, content, tags, evidence, confidence, status, source_type, source_ref, created_by, memory_class, content_identity)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 RETURNING `+learningCandidateCols,
		in.Kind, in.Scope, in.Title, in.Content, in.Tags, in.Evidence, in.Confidence,
		in.Status, in.SourceType, in.SourceRef, in.CreatedBy, in.MemoryClass, contentIdentity))
}

func learningContentIdentity(title, content string) string {
	sum := sha256.Sum256([]byte(normalizeLearningText(title) + "\x00" + normalizeLearningText(content)))
	return fmt.Sprintf("%x", sum[:])
}

func NormalizeLearningMemoryClass(kind, memoryClass string) string {
	switch strings.TrimSpace(memoryClass) {
	case LearningMemoryDurable, LearningMemoryCanonical, LearningMemoryTransient, LearningMemoryUnclassified:
		return strings.TrimSpace(memoryClass)
	}
	switch strings.TrimSpace(kind) {
	case LearningKindRule, LearningKindSkill, LearningKindScript:
		return LearningMemoryDurable
	case LearningKindProfile:
		return LearningMemoryCanonical
	case LearningKindSummary:
		return LearningMemoryTransient
	default:
		return LearningMemoryUnclassified
	}
}

func (s *Store) LearningCandidateByID(ctx context.Context, id int64) (*LearningCandidate, error) {
	return scanLearningCandidate(s.pool.QueryRow(ctx,
		`SELECT `+learningCandidateCols+` FROM learning_candidates WHERE id = $1`, id))
}

func (s *Store) LearningCandidatesByIDs(ctx context.Context, ids []int64) ([]*LearningCandidate, error) {
	ids = normalizeSnapshotIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+learningCandidateCols+` FROM learning_candidates
		  WHERE id = ANY($1) ORDER BY array_position($1::bigint[], id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*LearningCandidate, 0, len(ids))
	for rows.Next() {
		c, err := scanLearningCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LearningCandidateIDsForGovernance returns the complete oldest-first input
// boundary for one governance cycle. The scheduler persists this list before
// scoring or reviewing it, so later candidates cannot enter an open cycle and
// an old backlog cannot be starved by newer rows.
func (s *Store) LearningCandidateIDsForGovernance(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM learning_candidates
		  WHERE status=$1 AND memory_class=$2 AND kind=ANY($3)
		  ORDER BY id`,
		LearningStatusPending, LearningMemoryDurable,
		[]string{LearningKindKnowledge, LearningKindRule, LearningKindSkill})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, wrapErr(err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) SetLearningCandidateMemoryClass(ctx context.Context, id, reviewerID int64, memoryClass string) error {
	memoryClass = NormalizeLearningMemoryClass("", memoryClass)
	if memoryClass == LearningMemoryUnclassified {
		return fmt.Errorf("memory class must be durable, canonical, or transient")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	candidate, err := scanLearningCandidate(tx.QueryRow(ctx,
		`SELECT `+learningCandidateCols+` FROM learning_candidates WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if candidate.Status == LearningStatusRejected ||
		(candidate.Status == LearningStatusPublished && memoryClass == LearningMemoryDurable) {
		return ErrNotFound
	}
	if candidate.Status == LearningStatusPublished && candidate.PublishedKnowledgeID != nil {
		note := "reclassified as " + memoryClass + "; removed from semantic memory"
		if err := snapshotKnowledgeRow(ctx, tx, *candidate.PublishedKnowledgeID, &reviewerID, note); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE knowledge SET active=false, embed_model='', embedding=NULL, updated_at=now()
			  WHERE id=$1`, *candidate.PublishedKnowledgeID); err != nil {
			return err
		}
	}
	status := candidate.Status
	note := candidate.ReviewNote
	if memoryClass != LearningMemoryDurable {
		status = LearningStatusRejected
		note = "归档：信息权威归属为 " + memoryClass + "，不进入语义记忆。"
	}
	if _, err := tx.Exec(ctx,
		`UPDATE learning_candidates
		    SET memory_class=$2, status=$3, reviewed_by=$4, reviewed_at=now(),
		        review_note=$5, updated_at=now()
		  WHERE id=$1`, id, memoryClass, status, reviewerID, note); err != nil {
		return wrapErr(err)
	}
	return tx.Commit(ctx)
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

func (s *Store) MarkLearningCandidatePublished(ctx context.Context, id, reviewerID int64, knowledgeID *int64) error {
	return s.execOne(ctx,
		`UPDATE learning_candidates
		    SET status = $2, reviewed_by = $3, reviewed_at = now(), published_knowledge_id = $4, updated_at = now()
		  WHERE id = $1 AND status = $5`,
		id, LearningStatusPublished, reviewerID, knowledgeID, LearningStatusPending)
}

func (s *Store) RejectLearningCandidate(ctx context.Context, id, reviewerID int64) error {
	return s.execOne(ctx,
		`UPDATE learning_candidates
		    SET status = $2, reviewed_by = $3, reviewed_at = now(), updated_at = now()
		  WHERE id = $1 AND status = $4`,
		id, LearningStatusRejected, reviewerID, LearningStatusPending)
}

// ScoreLearningCandidates gives pending candidates a deterministic governance
// score and links exact duplicates. Text similarity is only a retrieval hint for
// Agent review; the store never infers semantic agreement or conflict.
func (s *Store) ScoreLearningCandidates(ctx context.Context, limit int) (int, error) {
	// 上限 200：相关候选检测对每个候选最多再扫 200 条同类，
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
	return s.scoreLearningCandidates(ctx, items)
}

// ScoreLearningCandidatesByIDs scores one immutable scheduler batch. It does
// not discover rows itself, which preserves the cycle snapshot boundary.
func (s *Store) ScoreLearningCandidatesByIDs(ctx context.Context, ids []int64) (int, error) {
	items, err := s.LearningCandidatesByIDs(ctx, ids)
	if err != nil {
		return 0, err
	}
	return s.scoreLearningCandidates(ctx, items)
}

func (s *Store) scoreLearningCandidates(ctx context.Context, items []*LearningCandidate) (int, error) {
	updated := 0
	for _, c := range items {
		if c == nil || c.Status != LearningStatusPending {
			continue
		}
		score := learningValueScore(c)
		related, similarity, equivalent, err := s.learningDuplicateID(ctx, c)
		if err != nil {
			return updated, err
		}
		var dupe *int64
		note := ""
		if equivalent && related != nil {
			dupe = related
			note = fmt.Sprintf("精确重复：与候选 %d 的规范化标题和正文相同。", *dupe)
			score *= 0.3
		} else if related != nil {
			note = fmt.Sprintf("相关候选：与候选 %d 的文本相似度 %.2f；重复或冲突由 Agent 结合语义审核。", *related, similarity)
		}
		status := LearningStatusPending
		if dupe != nil {
			status = LearningStatusRejected
			note += " 已自动归档。"
		}
		tag, err := s.pool.Exec(ctx,
			`UPDATE learning_candidates
			    SET duplicate_of = $2, conflict_with = $3, value_score = $4, review_note = $5,
			        status = $7, reviewed_at = CASE WHEN $7 = 'rejected' THEN now() ELSE reviewed_at END,
			        updated_at = now()
			  WHERE id = $1 AND status = $6
			    AND (duplicate_of IS DISTINCT FROM $2 OR conflict_with IS DISTINCT FROM $3
			      OR value_score IS DISTINCT FROM $4 OR review_note IS DISTINCT FROM $5 OR status IS DISTINCT FROM $7)`,
			c.ID, dupe, nil, score, note, LearningStatusPending, status)
		if err != nil {
			return updated, err
		}
		if tag.RowsAffected() > 0 {
			updated++
		}
	}
	return updated, nil
}

func (s *Store) learningDuplicateID(ctx context.Context, c *LearningCandidate) (*int64, float64, bool, error) {
	if c == nil || strings.TrimSpace(c.Title) == "" {
		return nil, 0, false, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, title, content FROM learning_candidates
		  WHERE id <> $1 AND kind = $2 AND status = ANY($3) AND memory_class = $4 AND scope = $5
		  ORDER BY CASE WHEN id < $1 THEN 0 ELSE 1 END, id DESC
		  LIMIT 200`, c.ID, c.Kind, []string{LearningStatusPending, LearningStatusPublished}, c.MemoryClass, c.Scope)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	var bestID int64
	best := 0.0
	for rows.Next() {
		var id int64
		var title, content string
		if err := rows.Scan(&id, &title, &content); err != nil {
			return nil, 0, false, err
		}
		if LearningTextsEquivalent(c.Title, c.Content, title, content) {
			return &id, 1, true, nil
		}
		sim := LearningTextSimilarity(c.Title, c.Content, title, content)
		if sim > best {
			best, bestID = sim, id
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	if bestID == 0 || best < LearningDuplicateThreshold {
		return nil, best, false, nil
	}
	return &bestID, best, false, nil
}

// EquivalentLearningCandidateExists checks exact normalized identity. Semantic
// similarity is intentionally not a write gate because similar rules may have
// opposite meaning.
func (s *Store) EquivalentLearningCandidateExists(ctx context.Context, kind, scope, title, content string, statuses ...string) (bool, error) {
	return s.equivalentLearningCandidateExists(ctx, kind, "", scope, title, content, statuses...)
}

func (s *Store) EquivalentLearningCandidateExistsInClass(ctx context.Context, kind, memoryClass, scope, title, content string, statuses ...string) (bool, error) {
	return s.equivalentLearningCandidateExists(ctx, kind, NormalizeLearningMemoryClass(kind, memoryClass), scope, title, content, statuses...)
}

func (s *Store) equivalentLearningCandidateExists(ctx context.Context, kind, memoryClass, scope, title, content string, statuses ...string) (bool, error) {
	if len(statuses) == 0 {
		statuses = []string{LearningStatusPending, LearningStatusPublished}
	}
	query := `SELECT title, content FROM learning_candidates WHERE kind = $1 AND status = ANY($2)`
	args := []any{kind, statuses}
	if memoryClass != "" {
		query += ` AND memory_class = $3`
		args = append(args, memoryClass)
	}
	args = append(args, strings.TrimSpace(scope))
	query += ` AND scope = $` + fmt.Sprint(len(args))
	query += ` ORDER BY id DESC`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var otherTitle, otherContent string
		if err := rows.Scan(&otherTitle, &otherContent); err != nil {
			return false, err
		}
		if LearningTextsEquivalent(title, content, otherTitle, otherContent) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// LearningTextsEquivalent is the only automatic duplicate verdict. It ignores
// case and whitespace but does not guess the meaning of natural language.
func LearningTextsEquivalent(titleA, contentA, titleB, contentB string) bool {
	return normalizeLearningText(titleA) == normalizeLearningText(titleB) &&
		normalizeLearningText(contentA) == normalizeLearningText(contentB)
}

func normalizeLearningText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
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

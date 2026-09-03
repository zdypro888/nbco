package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	WorkEvidenceSourceConversationFact      = "conversation_fact"
	WorkEvidenceSourceTelegramGroupAnalysis = "telegram_group_analysis"

	WorkEvidenceCommunication = "communication"
	WorkEvidenceSummary       = "summary"
	WorkEvidenceUpdate        = "update"
	WorkEvidenceDecision      = "decision"
	WorkEvidenceRisk          = "risk"
	WorkEvidenceDeliverable   = "deliverable"

	WorkEvidenceObserved   = "observed"
	WorkEvidenceActive     = "active"
	WorkEvidenceResolved   = "resolved"
	WorkEvidenceSuperseded = "superseded"
	WorkEvidenceIgnored    = "ignored"
)

// ConversationFactSourceKey gives the Agent tool and the asynchronous memory
// miner the same idempotency identity. If both recognize the same fact from one
// user message, they refresh one projection instead of creating parallel facts.
func ConversationFactSourceKey(messageID, actorID int64, evidence string) string {
	identity := strings.Join(strings.Fields(evidence), " ")
	sum := sha256.Sum256([]byte(identity))
	if messageID > 0 {
		return fmt.Sprintf("message:%d:%x", messageID, sum[:16])
	}
	return fmt.Sprintf("actor:%d:%x", actorID, sum[:16])
}

type WorkEvidence struct {
	ID              int64           `json:"id"`
	SourceType      string          `json:"source_type"`
	SourceKey       string          `json:"source_key"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	Title           string          `json:"title"`
	Content         string          `json:"content"`
	ActorUserID     *int64          `json:"actor_user_id,omitempty"`
	ActorName       string          `json:"actor_name,omitempty"`
	ProjectID       *int64          `json:"project_id,omitempty"`
	ProjectName     string          `json:"project_name,omitempty"`
	TaskID          *int64          `json:"task_id,omitempty"`
	WorkerRunID     *int64          `json:"worker_run_id,omitempty"`
	SourceMessageID *int64          `json:"source_message_id,omitempty"`
	Confidence      float32         `json:"confidence"`
	EventAt         time.Time       `json:"event_at"`
	Metadata        json.RawMessage `json:"metadata"`
	CreatedBy       *int64          `json:"created_by,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type WorkEvidenceInput struct {
	SourceType      string
	SourceKey       string
	Kind            string
	Status          string
	Title           string
	Content         string
	ActorUserID     *int64
	ProjectID       *int64
	TaskID          *int64
	WorkerRunID     *int64
	SourceMessageID *int64
	Confidence      float32
	EventAt         time.Time
	Metadata        json.RawMessage
	CreatedBy       *int64
}

const workEvidenceCols = `e.id, e.source_type, e.source_key, e.kind, e.status, e.title, e.content,
e.actor_user_id, COALESCE(u.name, ''), e.project_id, COALESCE(p.name, ''), e.task_id,
e.worker_run_id, e.source_message_id, e.confidence, e.event_at, e.metadata, e.created_by,
e.created_at, e.updated_at`

type workEvidenceQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanWorkEvidence(row interface{ Scan(...any) error }) (*WorkEvidence, error) {
	var evidence WorkEvidence
	if err := row.Scan(&evidence.ID, &evidence.SourceType, &evidence.SourceKey, &evidence.Kind,
		&evidence.Status, &evidence.Title, &evidence.Content, &evidence.ActorUserID,
		&evidence.ActorName, &evidence.ProjectID, &evidence.ProjectName, &evidence.TaskID,
		&evidence.WorkerRunID, &evidence.SourceMessageID, &evidence.Confidence,
		&evidence.EventAt, &evidence.Metadata, &evidence.CreatedBy, &evidence.CreatedAt,
		&evidence.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &evidence, nil
}

func normalizeWorkEvidence(in *WorkEvidenceInput) bool {
	if in == nil {
		return false
	}
	in.SourceType = strings.TrimSpace(in.SourceType)
	in.SourceKey = strings.TrimSpace(in.SourceKey)
	in.Kind = strings.TrimSpace(in.Kind)
	in.Status = strings.TrimSpace(in.Status)
	in.Title = strings.TrimSpace(in.Title)
	in.Content = strings.TrimSpace(in.Content)
	if in.SourceType == "" || in.SourceKey == "" || in.Content == "" {
		return false
	}
	if in.Kind == "" {
		in.Kind = WorkEvidenceCommunication
	}
	if in.Status == "" {
		in.Status = WorkEvidenceObserved
	}
	if in.Confidence <= 0 {
		in.Confidence = 1
	}
	if in.Confidence > 1 {
		in.Confidence = 1
	}
	if in.EventAt.IsZero() {
		in.EventAt = time.Now().UTC()
	}
	if len(in.Metadata) == 0 || !json.Valid(in.Metadata) {
		in.Metadata = json.RawMessage(`{}`)
	}
	return true
}

// UpsertWorkEvidence preserves one canonical projection per provider source.
// A webhook retry or regenerated digest refreshes the projection instead of
// creating duplicate operational facts.
func (s *Store) UpsertWorkEvidence(ctx context.Context, in WorkEvidenceInput) (*WorkEvidence, error) {
	return upsertWorkEvidence(ctx, s.pool, in)
}

func upsertWorkEvidence(ctx context.Context, queryer workEvidenceQueryer, in WorkEvidenceInput) (*WorkEvidence, error) {
	if !normalizeWorkEvidence(&in) {
		return nil, ErrConflict
	}
	return scanWorkEvidence(queryer.QueryRow(ctx,
		`WITH upserted AS (
		 INSERT INTO work_evidence (
		   source_type, source_key, kind, status, title, content, actor_user_id,
		   project_id, task_id, worker_run_id, source_message_id, confidence,
		   event_at, metadata, created_by
		 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		 ON CONFLICT (source_type, source_key) DO UPDATE SET
		   kind = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence THEN EXCLUDED.kind ELSE work_evidence.kind END,
		   status = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence THEN EXCLUDED.status ELSE work_evidence.status END,
		   title = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence THEN EXCLUDED.title ELSE work_evidence.title END,
		   content = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence THEN EXCLUDED.content ELSE work_evidence.content END,
		   actor_user_id = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence
		     THEN COALESCE(EXCLUDED.actor_user_id, work_evidence.actor_user_id)
		     ELSE COALESCE(work_evidence.actor_user_id, EXCLUDED.actor_user_id) END,
		   project_id = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence
		     THEN COALESCE(EXCLUDED.project_id, work_evidence.project_id)
		     ELSE COALESCE(work_evidence.project_id, EXCLUDED.project_id) END,
		   task_id = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence
		     THEN COALESCE(EXCLUDED.task_id, work_evidence.task_id)
		     ELSE COALESCE(work_evidence.task_id, EXCLUDED.task_id) END,
		   worker_run_id = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence
		     THEN COALESCE(EXCLUDED.worker_run_id, work_evidence.worker_run_id)
		     ELSE COALESCE(work_evidence.worker_run_id, EXCLUDED.worker_run_id) END,
		   source_message_id = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence
		     THEN COALESCE(EXCLUDED.source_message_id, work_evidence.source_message_id)
		     ELSE COALESCE(work_evidence.source_message_id, EXCLUDED.source_message_id) END,
		   confidence = GREATEST(EXCLUDED.confidence, work_evidence.confidence),
		   event_at = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence THEN EXCLUDED.event_at ELSE work_evidence.event_at END,
		   metadata = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence THEN EXCLUDED.metadata ELSE work_evidence.metadata END,
		   created_by = CASE WHEN EXCLUDED.confidence >= work_evidence.confidence
		     THEN COALESCE(EXCLUDED.created_by, work_evidence.created_by)
		     ELSE COALESCE(work_evidence.created_by, EXCLUDED.created_by) END,
		   updated_at = now()
		 RETURNING *
		) SELECT `+workEvidenceCols+`
		    FROM upserted e
		    LEFT JOIN users u ON u.id = e.actor_user_id
		    LEFT JOIN projects p ON p.id = e.project_id`,
		in.SourceType, in.SourceKey, in.Kind, in.Status, in.Title, in.Content,
		in.ActorUserID, in.ProjectID, in.TaskID, in.WorkerRunID, in.SourceMessageID,
		in.Confidence, in.EventAt, in.Metadata, in.CreatedBy))
}

type WorkEvidenceStats struct {
	ObservedMessages int64      `json:"observed_messages"`
	StructuredItems  int64      `json:"structured_items"`
	Actors           int64      `json:"actors"`
	Projects         int64      `json:"projects"`
	LatestAt         *time.Time `json:"latest_at,omitempty"`
}

func (s *Store) WorkEvidenceStatsSince(ctx context.Context, since time.Time) (*WorkEvidenceStats, error) {
	var stats WorkEvidenceStats
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE kind = 'communication'),
		        count(*) FILTER (WHERE status IN ('observed','active') AND kind <> 'communication'),
		        count(DISTINCT actor_user_id) FILTER (WHERE actor_user_id IS NOT NULL),
		        count(DISTINCT project_id) FILTER (WHERE project_id IS NOT NULL),
		        max(event_at)
		   FROM work_evidence WHERE event_at >= $1`, since).
		Scan(&stats.ObservedMessages, &stats.StructuredItems, &stats.Actors, &stats.Projects, &stats.LatestAt)
	return &stats, wrapErr(err)
}

func (s *Store) RecentWorkEvidence(ctx context.Context, since time.Time, limit int) ([]*WorkEvidence, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+workEvidenceCols+`
		   FROM work_evidence e
		   LEFT JOIN users u ON u.id = e.actor_user_id
		   LEFT JOIN projects p ON p.id = e.project_id
		  WHERE e.event_at >= $1
		  ORDER BY e.event_at DESC, e.id DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*WorkEvidence, 0, limit)
	for rows.Next() {
		item, err := scanWorkEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// RecentStructuredWorkEvidence returns report-ready operational facts. Raw
// communication remains searchable evidence, but must not be copied into
// proactive reports without an explicit summarization step.
func (s *Store) RecentStructuredWorkEvidence(ctx context.Context, since time.Time, limit int) ([]*WorkEvidence, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+workEvidenceCols+`
		   FROM work_evidence e
		   LEFT JOIN users u ON u.id = e.actor_user_id
		   LEFT JOIN projects p ON p.id = e.project_id
		  WHERE e.event_at >= $1
		    AND e.kind <> $2
		    AND e.status IN ($3, $4)
		  ORDER BY e.event_at DESC, e.id DESC LIMIT $5`,
		since, WorkEvidenceCommunication, WorkEvidenceObserved, WorkEvidenceActive, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*WorkEvidence, 0, limit)
	for rows.Next() {
		item, err := scanWorkEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

const (
	MaterialReceived   = "received"
	MaterialQueued     = "queued"
	MaterialProcessing = "processing"
	MaterialNeedsInput = "needs_input"
	MaterialCompleted  = "completed"
	MaterialIgnored    = "ignored"
)

type MaterialCaseFile struct {
	ID           int64  `json:"id"`
	OriginalName string `json:"original_name"`
	MIMEType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
}

type MaterialCase struct {
	ID          int64              `json:"id"`
	OwnerID     int64              `json:"owner_id"`
	OwnerName   string             `json:"owner_name"`
	Source      string             `json:"source"`
	SourceRef   string             `json:"source_ref"`
	Title       string             `json:"title"`
	Instruction string             `json:"instruction"`
	Status      string             `json:"status"`
	TaskID      *int64             `json:"task_id,omitempty"`
	WorkerRunID *int64             `json:"worker_run_id,omitempty"`
	LastError   string             `json:"last_error,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
	Files       []MaterialCaseFile `json:"files"`
}

func queueMaterialFilesTx(ctx context.Context, tx pgx.Tx, ownerID int64, fileScope string, fileIDs []int64, taskID int64, title, instruction string) error {
	fileIDs = uniquePositiveIDs(fileIDs)
	if ownerID <= 0 || taskID <= 0 || len(fileIDs) == 0 {
		return ErrConflict
	}
	fileScope = strings.TrimSpace(fileScope)
	if !IsGroupChannel(fileScope) {
		fileScope = ""
	}
	rows, err := tx.Query(ctx,
		`SELECT id FROM files
		  WHERE id = ANY($2::bigint[]) AND (created_by = $1 OR ($3 <> '' AND source = $3))
		  ORDER BY id FOR UPDATE`, ownerID, fileIDs, fileScope)
	if err != nil {
		return err
	}
	locked := 0
	for rows.Next() {
		locked++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if locked != len(fileIDs) {
		return ErrNotFound
	}
	var alreadyActive bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM material_cases c
		   JOIN material_case_files cf ON cf.case_id = c.id
		   WHERE c.source = 'workflow' AND c.status IN ('queued','processing','needs_input')
		     AND cf.file_id = ANY($1::bigint[])
		 )`, fileIDs).Scan(&alreadyActive); err != nil {
		return err
	}
	if alreadyActive {
		return ErrConflict
	}
	var runID *int64
	var latestRun int64
	if err := tx.QueryRow(ctx,
		`SELECT id FROM worker_runs WHERE task_id = $1 ORDER BY id DESC LIMIT 1`, taskID).Scan(&latestRun); err == nil {
		runID = &latestRun
	}
	// Move only the selected files out of passive cases. A Telegram album may be
	// processed in parts, so unselected siblings must remain visibly pending.
	if _, err := tx.Exec(ctx,
		`DELETE FROM material_case_files cf
		  USING material_cases c
		  WHERE c.id = cf.case_id AND (c.owner_id = $1 OR ($3 <> '' AND c.source = $3))
		    AND c.source <> 'workflow'
		    AND c.status IN ('received','needs_input') AND cf.file_id = ANY($2)`, ownerID, fileIDs, fileScope); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE material_cases c SET status = 'completed', completed_at = now(), updated_at = now()
		  WHERE (c.owner_id = $1 OR ($2 <> '' AND c.source = $2)) AND c.source <> 'workflow'
		    AND c.status IN ('received','needs_input')
		    AND NOT EXISTS (SELECT 1 FROM material_case_files cf WHERE cf.case_id = c.id)`, ownerID, fileScope); err != nil {
		return err
	}
	var caseID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO material_cases (owner_id, source, source_ref, title, instruction, status, task_id, worker_run_id, created_by)
		 VALUES ($1,'workflow',$2,$3,$4,'queued',$5,$6,$1)
		 ON CONFLICT (owner_id, source, source_ref) DO UPDATE SET
		   instruction = EXCLUDED.instruction, status = 'queued', task_id = EXCLUDED.task_id,
		   worker_run_id = EXCLUDED.worker_run_id, last_error = '', completed_at = NULL, updated_at = now()
		 RETURNING id`, ownerID, fmt.Sprintf("task:%d", taskID), strings.TrimSpace(title),
		strings.TrimSpace(instruction), taskID, runID).Scan(&caseID); err != nil {
		return err
	}
	for _, fileID := range fileIDs {
		tag, err := tx.Exec(ctx,
			`INSERT INTO material_case_files (case_id, file_id)
			 SELECT $1, id FROM files
			  WHERE id = $2 AND (created_by = $3 OR ($4 <> '' AND source = $4))
			 ON CONFLICT DO NOTHING`, caseID, fileID, ownerID, fileScope)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM material_case_files WHERE case_id = $1 AND file_id = $2)`,
				caseID, fileID).Scan(&exists); err != nil || !exists {
				return ErrNotFound
			}
		}
	}
	return nil
}

func (s *Store) SetMaterialTaskState(ctx context.Context, taskID int64, status, detail string) error {
	status, detail, ok := normalizeMaterialTaskState(taskID, status, detail)
	if !ok {
		return ErrConflict
	}
	completed := status == MaterialCompleted
	_, err := s.pool.Exec(ctx,
		`UPDATE material_cases SET status = $2, last_error = $3,
		 completed_at = CASE WHEN $4 THEN now() ELSE NULL END, updated_at = now()
		 WHERE task_id = $1 AND status NOT IN ('completed','ignored')`,
		taskID, status, strings.TrimSpace(detail), completed)
	return wrapErr(err)
}

func setMaterialTaskStateTx(ctx context.Context, tx pgx.Tx, taskID int64, status, detail string) error {
	status, detail, ok := normalizeMaterialTaskState(taskID, status, detail)
	if !ok {
		return ErrConflict
	}
	completed := status == MaterialCompleted
	_, err := tx.Exec(ctx,
		`UPDATE material_cases SET status = $2, last_error = $3,
		 completed_at = CASE WHEN $4 THEN now() ELSE NULL END, updated_at = now()
		 WHERE task_id = $1 AND status NOT IN ('completed','ignored')`,
		taskID, status, detail, completed)
	return wrapErr(err)
}

func normalizeMaterialTaskState(taskID int64, status, detail string) (string, string, bool) {
	status = strings.TrimSpace(status)
	switch status {
	case MaterialReceived, MaterialQueued, MaterialProcessing, MaterialNeedsInput, MaterialCompleted, MaterialIgnored:
		return status, strings.TrimSpace(detail), taskID > 0
	default:
		return "", "", false
	}
}

func (s *Store) MaterialCases(ctx context.Context, ownerID int64, includeClosed bool, limit int) ([]*MaterialCase, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `WITH selected AS (
		 SELECT c.id FROM material_cases c WHERE ($1 = 0 OR c.owner_id = $1)`
	if !includeClosed {
		query += ` AND c.status NOT IN ('completed','ignored')`
	}
	query += ` ORDER BY c.id DESC LIMIT $2
	) SELECT c.id, c.owner_id, u.name, c.source, c.source_ref, c.title, c.instruction,
	         c.status, c.task_id, c.worker_run_id, c.last_error, c.created_at, c.updated_at,
	         c.completed_at, f.id, COALESCE(f.original_name,''), COALESCE(f.mime_type,''), COALESCE(f.size_bytes,0)
	    FROM selected s JOIN material_cases c ON c.id = s.id
	    JOIN users u ON u.id = c.owner_id
	    LEFT JOIN material_case_files cf ON cf.case_id = c.id
	    LEFT JOIN files f ON f.id = cf.file_id
	   ORDER BY c.id DESC, f.id`
	rows, err := s.pool.Query(ctx, query, ownerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]*MaterialCase{}
	var out []*MaterialCase
	for rows.Next() {
		var item MaterialCase
		var file MaterialCaseFile
		var fileID *int64
		if err := rows.Scan(&item.ID, &item.OwnerID, &item.OwnerName, &item.Source, &item.SourceRef,
			&item.Title, &item.Instruction, &item.Status, &item.TaskID, &item.WorkerRunID,
			&item.LastError, &item.CreatedAt, &item.UpdatedAt, &item.CompletedAt, &fileID,
			&file.OriginalName, &file.MIMEType, &file.SizeBytes); err != nil {
			return nil, err
		}
		current := byID[item.ID]
		if current == nil {
			item.Files = []MaterialCaseFile{}
			current = &item
			byID[item.ID] = current
			out = append(out, current)
		}
		if fileID != nil {
			file.ID = *fileID
			current.Files = append(current.Files, file)
		}
	}
	return out, rows.Err()
}

type MaterialCaseStats struct {
	Received   int64 `json:"received"`
	Queued     int64 `json:"queued"`
	Processing int64 `json:"processing"`
	NeedsInput int64 `json:"needs_input"`
	Completed  int64 `json:"completed"`
}

func (s *Store) MaterialCaseStats(ctx context.Context, ownerID int64) (*MaterialCaseStats, error) {
	var stats MaterialCaseStats
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status = 'received'),
		        count(*) FILTER (WHERE status = 'queued'),
		        count(*) FILTER (WHERE status = 'processing'),
		        count(*) FILTER (WHERE status = 'needs_input'),
		        count(*) FILTER (WHERE status = 'completed')
		   FROM material_cases WHERE ($1 = 0 OR owner_id = $1)`, ownerID).
		Scan(&stats.Received, &stats.Queued, &stats.Processing, &stats.NeedsInput, &stats.Completed)
	return &stats, wrapErr(err)
}

type ConversationAssetUsage struct {
	KnowledgeID int64
	Phase       string
	TurnOutcome string
}

const (
	AssetPhaseInjected  = "injected"
	AssetPhaseCandidate = "candidate"
	AssetPhaseLoaded    = "loaded"

	AssetOutcomeCompleted       = "completed"
	AssetOutcomeActionSucceeded = "action_succeeded"
	AssetOutcomePartial         = "partial"
	AssetOutcomeFailed          = "failed"
)

func (s *Store) RecordConversationAssetUsages(ctx context.Context, turnID int64, usages []ConversationAssetUsage) error {
	if turnID <= 0 || len(usages) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := recordConversationAssetUsagesTx(ctx, tx, turnID, usages); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func recordConversationAssetUsagesTx(ctx context.Context, tx pgx.Tx, turnID int64, usages []ConversationAssetUsage) error {
	seen := make(map[string]bool, len(usages))
	for _, usage := range usages {
		if usage.KnowledgeID <= 0 || !validAssetPhase(usage.Phase) {
			continue
		}
		if !validAssetOutcome(usage.TurnOutcome) {
			usage.TurnOutcome = AssetOutcomeCompleted
		}
		key := fmt.Sprintf("%d:%s", usage.KnowledgeID, usage.Phase)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := tx.Exec(ctx,
			`INSERT INTO conversation_asset_usages (conversation_turn_id, knowledge_id, phase, turn_outcome)
			 SELECT $1, id, $3, $4 FROM knowledge WHERE id = $2
			 ON CONFLICT (conversation_turn_id, knowledge_id, phase) DO UPDATE
			 SET turn_outcome = EXCLUDED.turn_outcome`,
			turnID, usage.KnowledgeID, usage.Phase, usage.TurnOutcome); err != nil {
			return err
		}
	}
	return nil
}

func validAssetPhase(phase string) bool {
	return phase == AssetPhaseInjected || phase == AssetPhaseCandidate || phase == AssetPhaseLoaded
}

func validAssetOutcome(outcome string) bool {
	switch outcome {
	case AssetOutcomeCompleted, AssetOutcomeActionSucceeded, AssetOutcomePartial, AssetOutcomeFailed:
		return true
	default:
		return false
	}
}

type AssetUsageStats struct {
	Injected        int64 `json:"injected"`
	Candidates      int64 `json:"candidates"`
	Loaded          int64 `json:"loaded"`
	DistinctAssets  int64 `json:"distinct_assets"`
	Completed       int64 `json:"completed"`
	ActionSucceeded int64 `json:"action_succeeded"`
	Partial         int64 `json:"partial"`
	Failed          int64 `json:"failed"`
}

func (s *Store) AssetUsageStatsSince(ctx context.Context, since time.Time) (*AssetUsageStats, error) {
	var stats AssetUsageStats
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE phase = 'injected'),
		        count(*) FILTER (WHERE phase = 'candidate'),
		        count(*) FILTER (WHERE phase = 'loaded'),
		        count(DISTINCT knowledge_id),
		        count(DISTINCT conversation_turn_id) FILTER (WHERE turn_outcome = 'completed'),
		        count(DISTINCT conversation_turn_id) FILTER (WHERE turn_outcome = 'action_succeeded'),
		        count(DISTINCT conversation_turn_id) FILTER (WHERE turn_outcome = 'partial'),
		        count(DISTINCT conversation_turn_id) FILTER (WHERE turn_outcome = 'failed')
		   FROM conversation_asset_usages WHERE created_at >= $1`, since).
		Scan(&stats.Injected, &stats.Candidates, &stats.Loaded, &stats.DistinctAssets,
			&stats.Completed, &stats.ActionSucceeded, &stats.Partial, &stats.Failed)
	return &stats, wrapErr(err)
}

type AssetEffectiveness struct {
	KnowledgeID     int64      `json:"knowledge_id"`
	Title           string     `json:"title"`
	Kind            string     `json:"kind"`
	Injected        int64      `json:"injected"`
	Candidates      int64      `json:"candidates"`
	Loaded          int64      `json:"loaded"`
	Completed       int64      `json:"completed"`
	ActionSucceeded int64      `json:"action_succeeded"`
	Partial         int64      `json:"partial"`
	Failed          int64      `json:"failed"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
}

func (s *Store) ListAssetEffectivenessSince(ctx context.Context, since time.Time, limit int) ([]AssetEffectiveness, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT k.id, k.title, k.kind,
		        count(*) FILTER (WHERE u.phase = 'injected'),
		        count(*) FILTER (WHERE u.phase = 'candidate'),
		        count(*) FILTER (WHERE u.phase = 'loaded'),
		        count(DISTINCT u.conversation_turn_id) FILTER (WHERE u.turn_outcome = 'completed'),
		        count(DISTINCT u.conversation_turn_id) FILTER (WHERE u.turn_outcome = 'action_succeeded'),
		        count(DISTINCT u.conversation_turn_id) FILTER (WHERE u.turn_outcome = 'partial'),
		        count(DISTINCT u.conversation_turn_id) FILTER (WHERE u.turn_outcome = 'failed'),
		        max(u.created_at)
		   FROM conversation_asset_usages u
		   JOIN knowledge k ON k.id = u.knowledge_id
		  WHERE u.created_at >= $1
		  GROUP BY k.id, k.title, k.kind
		  ORDER BY max(u.created_at) DESC, k.id DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer rows.Close()
	out := make([]AssetEffectiveness, 0, limit)
	for rows.Next() {
		var item AssetEffectiveness
		if err := rows.Scan(&item.KnowledgeID, &item.Title, &item.Kind, &item.Injected,
			&item.Candidates, &item.Loaded, &item.Completed, &item.ActionSucceeded,
			&item.Partial, &item.Failed, &item.LastUsedAt); err != nil {
			return nil, wrapErr(err)
		}
		out = append(out, item)
	}
	return out, wrapErr(rows.Err())
}

type ProductHealthStats struct {
	LearningPending           int64 `json:"learning_pending"`
	LearningConflicts         int64 `json:"learning_conflicts"`
	DeliveryFailures24H       int64 `json:"delivery_failures_24h"`
	NotificationFailures24H   int64 `json:"notification_failures_24h"`
	NotificationUncertain     int64 `json:"notification_uncertain"`
	ExternalActionFailures    int64 `json:"external_action_failures_24h"`
	ExternalActionUncertain   int64 `json:"external_action_uncertain"`
	DomainOutboxFailures      int64 `json:"domain_outbox_failures_24h"`
	DomainOutboxBacklog       int64 `json:"domain_outbox_backlog"`
	TelegramInboundFailures   int64 `json:"telegram_inbound_failures_24h"`
	TelegramInboundBacklog    int64 `json:"telegram_inbound_backlog"`
	TelegramDeliveryFailures  int64 `json:"telegram_delivery_failures_24h"`
	TelegramDeliveryUncertain int64 `json:"telegram_delivery_uncertain"`
	WorkerLLMFailures         int64 `json:"worker_llm_failures_24h"`
	WorkerLLMUncertain        int64 `json:"worker_llm_uncertain"`
	ToolFailures24H           int64 `json:"tool_failures_24h"`
	ActionFailures24H         int64 `json:"action_failures_24h"`
	ConversationFailures24H   int64 `json:"conversation_failures_24h"`
	WorkerNeedsInput          int64 `json:"worker_needs_input"`
	WorkerRetrying            int64 `json:"worker_retrying"`
}

func (s *Store) ProductHealthStats(ctx context.Context, since time.Time) (*ProductHealthStats, error) {
	var stats ProductHealthStats
	err := s.pool.QueryRow(ctx,
		`SELECT
		   (SELECT count(*) FROM learning_candidates WHERE status = 'pending'),
		   (SELECT count(*) FROM learning_candidates WHERE status = 'pending' AND conflict_with IS NOT NULL),
		   (SELECT count(*) FROM schedule_deliveries WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM notification_deliveries WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM notification_deliveries WHERE status = 'started' AND updated_at < now() - interval '2 minutes'),
		   (SELECT count(*) FROM external_action_receipts WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM external_action_receipts WHERE status = 'started' AND updated_at < now() - interval '2 minutes'),
		   (SELECT count(*) FROM domain_outbox_events WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM domain_outbox_events
		      WHERE (status = 'pending' AND available_at < now() - interval '2 minutes')
		         OR (status = 'processing' AND claimed_at < now() - interval '2 minutes')),
		   (SELECT count(*) FROM telegram_inbound_updates WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM telegram_inbound_updates
		      WHERE (status = 'pending' AND available_at < now() - interval '2 minutes')
		         OR (status = 'processing' AND claimed_at < now() - interval '2 minutes')),
		   (SELECT count(*) FROM telegram_delivery_parts WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM telegram_delivery_parts WHERE status = 'started' AND updated_at < now() - interval '2 minutes'),
		   (SELECT count(*) FROM worker_llm_calls WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM worker_llm_calls WHERE status = 'started' AND updated_at < now() - interval '7 minutes'),
		   (SELECT count(*) FROM audit_log WHERE NOT ok AND created_at >= $1),
		   (SELECT count(*) FROM action_turns WHERE outcome = 'tool_handler_error' AND created_at >= $1),
		   (SELECT count(*) FROM conversation_turns WHERE status = 'failed' AND updated_at >= $1),
		   (SELECT count(*) FROM worker_runs WHERE status = 'awaiting_input'),
		   (SELECT count(*) FROM worker_runs WHERE status = 'retry_wait')`, since).
		Scan(&stats.LearningPending, &stats.LearningConflicts, &stats.DeliveryFailures24H,
			&stats.NotificationFailures24H, &stats.NotificationUncertain,
			&stats.ExternalActionFailures, &stats.ExternalActionUncertain,
			&stats.DomainOutboxFailures, &stats.DomainOutboxBacklog,
			&stats.TelegramInboundFailures, &stats.TelegramInboundBacklog,
			&stats.TelegramDeliveryFailures, &stats.TelegramDeliveryUncertain,
			&stats.WorkerLLMFailures, &stats.WorkerLLMUncertain,
			&stats.ToolFailures24H, &stats.ActionFailures24H, &stats.ConversationFailures24H,
			&stats.WorkerNeedsInput, &stats.WorkerRetrying)
	return &stats, wrapErr(err)
}

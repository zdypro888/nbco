package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const fileIndexMaxAttempts = 8

// File 是 nbco 管理的文件实体。storage_path 是相对文件存储根目录的路径。
type File struct {
	ID           int64
	Source       string
	OriginalName string
	MIMEType     string
	SizeBytes    int64
	SHA256       string
	StoragePath  string
	CreatedBy    *int64
	CreatedAt    time.Time
}

const (
	FileIntakePending = "pending"
	FileIntakeSaved   = "saved"
	FileIntakeFailed  = "failed"
)

// FileIntake records every external file handoff, including failures that did
// not produce a File. This is the source of truth for “did you receive it?”.
type FileIntake struct {
	ID           int64
	UserID       int64
	Source       string
	ExternalRef  string
	OriginalName string
	MIMEType     string
	SizeBytes    int64
	Status       string
	ErrorCode    string
	ErrorMessage string
	FileID       *int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Artifact 是 worker 或用户提交到任务上的产物文件。
type Artifact struct {
	ID        int64
	TaskID    int64
	File      File
	ClaimID   string
	CreatedBy int64
	Caption   string
	CreatedAt time.Time
}

// FileContentIndexJob is one durably claimed file awaiting deterministic text
// extraction. ClaimToken prevents a stale process from completing a lease that
// another process reclaimed after a restart.
type FileContentIndexJob struct {
	File
	Attempts          int
	ClaimToken        string
	ExtractorRevision string
}

type FileVectorIndexJob struct {
	File
	Chunks      []FileTextChunk
	Attempts    int
	ClaimToken  string
	VectorModel string
}

// FileTextChunk is an authoritative searchable fragment. Qdrant stores only
// its stable ID and vector; content remains permission-protected in PostgreSQL.
type FileTextChunk struct {
	ID         int64
	FileID     int64
	ChunkIndex int
	Content    string
	CreatedAt  time.Time
}

type FileContentIndexStats struct {
	Total            int64 `json:"total"`
	Pending          int64 `json:"pending"`
	Processing       int64 `json:"processing"`
	Indexed          int64 `json:"indexed"`
	Empty            int64 `json:"empty"`
	Unsupported      int64 `json:"unsupported"`
	Failed           int64 `json:"failed"`
	Truncated        int64 `json:"truncated"`
	Chunks           int64 `json:"chunks"`
	VectorPending    int64 `json:"vector_pending"`
	VectorProcessing int64 `json:"vector_processing"`
	VectorIndexed    int64 `json:"vector_indexed"`
	VectorFailed     int64 `json:"vector_failed"`
}

func scanFile(row interface{ Scan(...any) error }) (*File, error) {
	var f File
	if err := row.Scan(&f.ID, &f.Source, &f.OriginalName, &f.MIMEType, &f.SizeBytes,
		&f.SHA256, &f.StoragePath, &f.CreatedBy, &f.CreatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &f, nil
}

// CreateFile 记录一个已落到文件存储里的文件。
func (s *Store) CreateFile(ctx context.Context, f *File) (*File, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := createFileTx(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

func createFileTx(ctx context.Context, tx pgx.Tx, f *File) (*File, error) {
	created, err := scanFile(tx.QueryRow(ctx,
		`INSERT INTO files (source, original_name, mime_type, size_bytes, sha256, storage_path, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, source, original_name, mime_type, size_bytes, sha256, storage_path, created_by, created_at`,
		f.Source, f.OriginalName, f.MIMEType, f.SizeBytes, f.SHA256, f.StoragePath, f.CreatedBy))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO file_content_indexes (file_id) VALUES ($1)`, created.ID); err != nil {
		return nil, err
	}
	return created, nil
}

// CreateFileWithMaterialCase publishes one API upload, its indexing job and
// its awaiting-instruction material projection atomically.
func (s *Store) CreateFileWithMaterialCase(ctx context.Context, f *File, sourceRef string) (*File, error) {
	if f == nil || f.CreatedBy == nil || *f.CreatedBy <= 0 {
		return nil, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := createFileTx(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	sourceRef = strings.TrimSpace(sourceRef)
	if sourceRef == "" {
		sourceRef = fmt.Sprintf("file:%d", created.ID)
	}
	var caseID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO material_cases (owner_id, source, source_ref, title, created_by)
		 VALUES ($1,$2,$3,$4,$1)
		 ON CONFLICT (owner_id, source, source_ref) DO UPDATE SET
		   status = 'received', title = EXCLUDED.title, task_id = NULL,
		   worker_run_id = NULL, last_error = '', completed_at = NULL, updated_at = now()
		 RETURNING id`, *f.CreatedBy, f.Source, sourceRef, strings.TrimSpace(f.OriginalName)).Scan(&caseID); err != nil {
		return nil, wrapErr(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO material_case_files (case_id, file_id) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, caseID, created.ID); err != nil {
		return nil, wrapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// ClaimFilesForContentIndex discovers new immutable files and claims a bounded
// batch. Failed jobs retry with backoff; abandoned processing leases are
// reclaimable so a service restart cannot strand data forever.
func (s *Store) ClaimFilesForContentIndex(ctx context.Context, limit int, extractorRevision string) ([]FileContentIndexJob, error) {
	if limit <= 0 || limit > 16 {
		limit = 2
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	extractorRevision = strings.TrimSpace(extractorRevision)
	if _, err := tx.Exec(ctx,
		`UPDATE file_content_indexes
		    SET status = 'failed',
		        last_error = CASE WHEN last_error = '' THEN '索引进程中断且重试次数耗尽' ELSE last_error END,
		        claim_token = '', claimed_at = NULL, updated_at = now()
		  WHERE status = 'processing' AND claimed_at < now() - interval '15 minutes'
		    AND attempts >= $1 AND extractor_revision = $2`, fileIndexMaxAttempts, extractorRevision); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT f.id, f.source, f.original_name, f.mime_type, f.size_bytes,
		        f.sha256, f.storage_path, f.created_by, f.created_at,
		        i.attempts, i.extractor_revision
		   FROM file_content_indexes i JOIN files f ON f.id = i.file_id
			  WHERE ((i.status IN ('pending', 'failed') AND i.available_at <= now()
			          AND (i.attempts < $3 OR i.extractor_revision <> $2))
			     OR (i.status = 'unsupported' AND i.extractor_revision <> $2)
			     OR (i.status = 'processing' AND i.claimed_at < now() - interval '15 minutes'
			         AND (i.attempts < $3 OR i.extractor_revision <> $2)))
		  ORDER BY i.file_id
		  FOR UPDATE OF i SKIP LOCKED
				  LIMIT $1`, limit, extractorRevision, fileIndexMaxAttempts)
	if err != nil {
		return nil, err
	}
	var jobs []FileContentIndexJob
	for rows.Next() {
		var job FileContentIndexJob
		var previousRevision string
		if err := rows.Scan(&job.ID, &job.Source, &job.OriginalName, &job.MIMEType,
			&job.SizeBytes, &job.SHA256, &job.StoragePath, &job.CreatedBy, &job.CreatedAt,
			&job.Attempts, &previousRevision); err != nil {
			rows.Close()
			return nil, err
		}
		job.ClaimToken = rand.Text()
		job.ExtractorRevision = extractorRevision
		if previousRevision != extractorRevision {
			job.Attempts = 0
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range jobs {
		if _, err := tx.Exec(ctx,
			`UPDATE file_content_indexes
				    SET status = 'processing', attempts = $3,
				        claim_token = $2, claimed_at = now(), extractor_revision = $4, updated_at = now()
				  WHERE file_id = $1`, jobs[i].ID, jobs[i].ClaimToken, jobs[i].Attempts+1, extractorRevision); err != nil {
			return nil, err
		}
		jobs[i].Attempts++
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}

// CompleteFileContentIndex atomically replaces extracted chunks and completes
// the matching lease. Empty is a successful terminal state, distinct from an
// extractor failure or unsupported binary format.
func (s *Store) CompleteFileContentIndex(ctx context.Context, job FileContentIndexJob, extractor string, contents []string, truncated bool) ([]FileTextChunk, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked int64
	if err := tx.QueryRow(ctx,
		`SELECT file_id FROM file_content_indexes
		  WHERE file_id = $1 AND status = 'processing' AND claim_token = $2
		  FOR UPDATE`, job.ID, job.ClaimToken).Scan(&locked); err != nil {
		return nil, wrapErr(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM file_text_chunks WHERE file_id = $1`, job.ID); err != nil {
		return nil, err
	}
	chunks := make([]FileTextChunk, 0, len(contents))
	for i, content := range contents {
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		var chunk FileTextChunk
		if err := tx.QueryRow(ctx,
			`INSERT INTO file_text_chunks (file_id, chunk_index, content)
			 VALUES ($1, $2, $3)
			 RETURNING id, file_id, chunk_index, content, created_at`,
			job.ID, i, content).Scan(&chunk.ID, &chunk.FileID, &chunk.ChunkIndex, &chunk.Content, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	status := "indexed"
	if len(chunks) == 0 {
		status = "empty"
	}
	if _, err := tx.Exec(ctx,
		`UPDATE file_content_indexes
		    SET status = $3, extractor = $4, extractor_revision = $7,
		        chunk_count = $5, truncated = $6,
		        last_error = '', claim_token = '', claimed_at = NULL,
		        indexed_at = now(),
		        vector_status = CASE WHEN $5 = 0 THEN 'indexed' ELSE 'pending' END,
		        vector_error = '', vector_attempts = 0, vector_claim_token = '', vector_claimed_at = NULL,
		        vector_available_at = now(), vector_indexed_at = CASE WHEN $5 = 0 THEN now() ELSE NULL END,
		        updated_at = now()
		  WHERE file_id = $1 AND claim_token = $2`,
		job.ID, job.ClaimToken, status, strings.TrimSpace(extractor), len(chunks), truncated, job.ExtractorRevision); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return chunks, nil
}

// FailFileContentIndex records a bounded retry or a terminal unsupported
// result. Metadata remains searchable even when no deterministic text
// extractor is available.
func (s *Store) FailFileContentIndex(ctx context.Context, job FileContentIndexJob, cause error, retry bool) error {
	status := "unsupported"
	availableAt := time.Now()
	if retry {
		status = "failed"
		if job.Attempts < fileIndexMaxAttempts {
			shift := min(job.Attempts, 6)
			availableAt = availableAt.Add(time.Minute * time.Duration(1<<shift))
		}
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 2000 {
		message = message[:2000]
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE file_content_indexes
		    SET status = $3, last_error = $4, extractor_revision = $6,
		        claim_token = '', claimed_at = NULL,
		        vector_status = CASE WHEN $3 = 'unsupported' THEN 'unavailable' ELSE vector_status END,
		        vector_error = CASE WHEN $3 = 'unsupported' THEN $4 ELSE vector_error END,
		        available_at = $5, updated_at = now()
		  WHERE file_id = $1 AND status = 'processing' AND claim_token = $2`,
		job.ID, job.ClaimToken, status, message, availableAt, job.ExtractorRevision)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ClaimFilesForVectorIndex(ctx context.Context, limit int, model string) ([]FileVectorIndexJob, error) {
	if limit <= 0 || limit > 16 {
		limit = 2
	}
	model = strings.TrimSpace(model)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE file_content_indexes
		    SET vector_status = 'failed',
		        vector_error = CASE WHEN vector_error = '' THEN '向量索引进程中断且重试次数耗尽' ELSE vector_error END,
		        vector_claim_token = '', vector_claimed_at = NULL, updated_at = now()
		  WHERE vector_status = 'processing' AND vector_claimed_at < now() - interval '15 minutes'
		    AND vector_attempts >= $1 AND vector_model = $2`, fileIndexMaxAttempts, model); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT f.id, f.source, f.original_name, f.mime_type, f.size_bytes,
		        f.sha256, f.storage_path, f.created_by, f.created_at,
		        i.vector_attempts, i.vector_model
		   FROM file_content_indexes i JOIN files f ON f.id = i.file_id
		  WHERE i.status = 'indexed' AND i.chunk_count > 0 AND (
			        (i.vector_status IN ('pending', 'failed', 'unavailable') AND i.vector_available_at <= now()
			         AND (i.vector_attempts < $3 OR i.vector_model <> $2))
			     OR (i.vector_status = 'indexed' AND i.vector_model <> $2)
			     OR (i.vector_status = 'processing' AND i.vector_claimed_at < now() - interval '15 minutes'
			         AND (i.vector_attempts < $3 OR i.vector_model <> $2)))
			  ORDER BY i.file_id FOR UPDATE OF i SKIP LOCKED LIMIT $1`, limit, model, fileIndexMaxAttempts)
	if err != nil {
		return nil, err
	}
	var jobs []FileVectorIndexJob
	for rows.Next() {
		var job FileVectorIndexJob
		var previousModel string
		if err := rows.Scan(&job.ID, &job.Source, &job.OriginalName, &job.MIMEType, &job.SizeBytes,
			&job.SHA256, &job.StoragePath, &job.CreatedBy, &job.CreatedAt, &job.Attempts, &previousModel); err != nil {
			rows.Close()
			return nil, err
		}
		if previousModel != model {
			job.Attempts = 0
		}
		job.ClaimToken = rand.Text()
		job.VectorModel = model
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range jobs {
		if _, err := tx.Exec(ctx,
			`UPDATE file_content_indexes SET vector_status = 'processing', vector_attempts = $3,
			 vector_claim_token = $2, vector_claimed_at = now(), vector_model = $4, updated_at = now()
			 WHERE file_id = $1`, jobs[i].ID, jobs[i].ClaimToken, jobs[i].Attempts+1, model); err != nil {
			return nil, err
		}
		jobs[i].Attempts++
		chunkRows, err := tx.Query(ctx,
			`SELECT id, file_id, chunk_index, content, created_at FROM file_text_chunks
			  WHERE file_id = $1 ORDER BY chunk_index`, jobs[i].ID)
		if err != nil {
			return nil, err
		}
		for chunkRows.Next() {
			var chunk FileTextChunk
			if err := chunkRows.Scan(&chunk.ID, &chunk.FileID, &chunk.ChunkIndex, &chunk.Content, &chunk.CreatedAt); err != nil {
				chunkRows.Close()
				return nil, err
			}
			jobs[i].Chunks = append(jobs[i].Chunks, chunk)
		}
		if err := chunkRows.Err(); err != nil {
			chunkRows.Close()
			return nil, err
		}
		chunkRows.Close()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) CompleteFileVectorIndex(ctx context.Context, job FileVectorIndexJob) error {
	return s.execOne(ctx,
		`UPDATE file_content_indexes SET vector_status = 'indexed', vector_error = '',
		 vector_claim_token = '', vector_claimed_at = NULL, vector_indexed_at = now(), updated_at = now()
		 WHERE file_id = $1 AND vector_status = 'processing' AND vector_claim_token = $2`,
		job.ID, job.ClaimToken)
}

func (s *Store) FailFileVectorIndex(ctx context.Context, job FileVectorIndexJob, cause error) error {
	availableAt := time.Now().Add(time.Minute * time.Duration(1<<min(job.Attempts, 6)))
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 2000 {
		message = message[:2000]
	}
	return s.execOne(ctx,
		`UPDATE file_content_indexes SET vector_status = 'failed', vector_error = $3,
		 vector_claim_token = '', vector_claimed_at = NULL, vector_available_at = $4, updated_at = now()
		 WHERE file_id = $1 AND vector_status = 'processing' AND vector_claim_token = $2`,
		job.ID, job.ClaimToken, message, availableAt)
}

func (s *Store) FileTextChunksByFile(ctx context.Context, fileID int64) ([]FileTextChunk, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, file_id, chunk_index, content, created_at
		   FROM file_text_chunks WHERE file_id = $1 ORDER BY chunk_index`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []FileTextChunk
	for rows.Next() {
		var chunk FileTextChunk
		if err := rows.Scan(&chunk.ID, &chunk.FileID, &chunk.ChunkIndex, &chunk.Content, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return chunks, rows.Err()
}

// FileTextChunkIDs returns the authoritative ID set used to remove Qdrant
// points left behind after a file or a replaced extraction is deleted.
func (s *Store) FileTextChunkIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM file_text_chunks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) FileContentIndexStats(ctx context.Context) (FileContentIndexStats, error) {
	var stats FileContentIndexStats
	err := s.pool.QueryRow(ctx,
		`SELECT count(f.id),
		        count(*) FILTER (WHERE i.file_id IS NULL OR i.status = 'pending'),
		        count(*) FILTER (WHERE i.status = 'processing'),
		        count(*) FILTER (WHERE i.status = 'indexed'),
		        count(*) FILTER (WHERE i.status = 'empty'),
		        count(*) FILTER (WHERE i.status = 'unsupported'),
		        count(*) FILTER (WHERE i.status = 'failed'),
		        count(*) FILTER (WHERE i.truncated),
		        COALESCE(sum(i.chunk_count), 0),
		        count(*) FILTER (WHERE i.status = 'indexed' AND i.vector_status IN ('pending', 'unavailable')),
		        count(*) FILTER (WHERE i.status = 'indexed' AND i.vector_status = 'processing'),
		        count(*) FILTER (WHERE i.status = 'indexed' AND i.vector_status = 'indexed'),
		        count(*) FILTER (WHERE i.status = 'indexed' AND i.vector_status = 'failed')
		   FROM files f LEFT JOIN file_content_indexes i ON i.file_id = f.id`).
		Scan(&stats.Total, &stats.Pending, &stats.Processing, &stats.Indexed,
			&stats.Empty, &stats.Unsupported, &stats.Failed, &stats.Truncated, &stats.Chunks,
			&stats.VectorPending, &stats.VectorProcessing, &stats.VectorIndexed, &stats.VectorFailed)
	return stats, err
}

// FileByID 取文件元数据。
func (s *Store) FileByID(ctx context.Context, id int64) (*File, error) {
	return scanFile(s.pool.QueryRow(ctx,
		`SELECT id, source, original_name, mime_type, size_bytes, sha256, storage_path, created_by, created_at
		 FROM files WHERE id = $1`, id))
}

// RecentFilesByUser returns recently uploaded files for a user. It is the input
// buffer for "I just uploaded two files, now do X" style workflows.
func (s *Store) RecentFilesByUser(ctx context.Context, userID int64, limit int, since time.Time) ([]File, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, source, original_name, mime_type, size_bytes, sha256, storage_path, created_by, created_at
		 FROM files
		 WHERE created_by = $1 AND created_at >= $2
		 ORDER BY id DESC LIMIT $3`, userID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func scanFileIntake(row interface{ Scan(...any) error }) (*FileIntake, error) {
	var in FileIntake
	if err := row.Scan(&in.ID, &in.UserID, &in.Source, &in.ExternalRef, &in.OriginalName,
		&in.MIMEType, &in.SizeBytes, &in.Status, &in.ErrorCode, &in.ErrorMessage,
		&in.FileID, &in.CreatedAt, &in.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &in, nil
}

const fileIntakeCols = `id, user_id, source, external_ref, original_name, mime_type, size_bytes, status, error_code, error_message, file_id, created_at, updated_at`

func (s *Store) CreateFileIntake(ctx context.Context, in FileIntake) (*FileIntake, error) {
	in.Source = strings.TrimSpace(in.Source)
	in.ExternalRef = strings.TrimSpace(in.ExternalRef)
	return scanFileIntake(s.pool.QueryRow(ctx,
		`INSERT INTO file_intakes (user_id, source, external_ref, original_name, mime_type, size_bytes, canonical)
		 VALUES ($1, $2, $3, $4, $5, $6, true)
		 ON CONFLICT (user_id, source, external_ref)
		   WHERE canonical AND external_ref <> ''
		 DO UPDATE SET original_name = EXCLUDED.original_name,
		               mime_type = EXCLUDED.mime_type,
		               size_bytes = EXCLUDED.size_bytes,
		               status = CASE WHEN file_intakes.status = 'saved' THEN 'saved' ELSE 'pending' END,
		               error_code = CASE WHEN file_intakes.status = 'saved' THEN file_intakes.error_code ELSE '' END,
		               error_message = CASE WHEN file_intakes.status = 'saved' THEN file_intakes.error_message ELSE '' END,
		               updated_at = now()
		 RETURNING `+fileIntakeCols,
		in.UserID, in.Source, in.ExternalRef, in.OriginalName, in.MIMEType, in.SizeBytes))
}

// CompleteFileIntake publishes an intake exactly once. Concurrent webhook
// attempts return the already-published canonical file ID so their duplicate
// file row can be discarded safely by the caller. materialRef groups multiple
// provider messages (for example one Telegram album) into one material case.
func (s *Store) CompleteFileIntake(ctx context.Context, intakeID, fileID int64, materialRef string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var userID int64
	var source, sourceRef, name, status string
	var savedFileID *int64
	if err := tx.QueryRow(ctx,
		`SELECT user_id, source, external_ref, original_name, status, file_id
		   FROM file_intakes WHERE id = $1 AND canonical FOR UPDATE`, intakeID).
		Scan(&userID, &source, &sourceRef, &name, &status, &savedFileID); err != nil {
		return 0, wrapErr(err)
	}
	if status == "saved" && savedFileID != nil {
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return *savedFileID, nil
	}
	var ownsFile bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM files WHERE id = $1 AND created_by = $2)`, fileID, userID).Scan(&ownsFile); err != nil {
		return 0, wrapErr(err)
	}
	if !ownsFile {
		return 0, ErrNotFound
	}
	if _, err := tx.Exec(ctx,
		`UPDATE file_intakes
		    SET status = 'saved', file_id = $2, error_code = '', error_message = '', updated_at = now()
		  WHERE id = $1`, intakeID, fileID); err != nil {
		return 0, wrapErr(err)
	}
	// Telegram may deliver several files in one message. Keep receipt identity at
	// file granularity while grouping the material work item by message ID.
	caseRef := strings.TrimSpace(materialRef)
	if caseRef == "" {
		caseRef = strings.TrimSpace(sourceRef)
	}
	if source == "telegram" {
		if messageRef, _, ok := strings.Cut(caseRef, ":"); ok && messageRef != "" && !strings.HasPrefix(caseRef, "media-group:") {
			caseRef = messageRef
		}
	}
	if caseRef == "" {
		caseRef = fmt.Sprintf("intake:%d", intakeID)
	}
	var caseID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO material_cases (owner_id, source, source_ref, title, created_by)
		 VALUES ($1,$2,$3,$4,$1)
		 ON CONFLICT (owner_id, source, source_ref) DO UPDATE SET
		   status = 'received', title = EXCLUDED.title, task_id = NULL,
		   worker_run_id = NULL, last_error = '', completed_at = NULL, updated_at = now()
		 RETURNING id`, userID, source, caseRef, strings.TrimSpace(name)).Scan(&caseID); err != nil {
		return 0, wrapErr(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO material_case_files (case_id, file_id) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, caseID, fileID); err != nil {
		return 0, wrapErr(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return fileID, nil
}

func (s *Store) FailFileIntake(ctx context.Context, intakeID int64, code, message string) error {
	return s.execOne(ctx,
		`UPDATE file_intakes
		 SET status = 'failed', error_code = $2, error_message = $3, updated_at = now()
		 WHERE id = $1 AND status = 'pending'`, intakeID, code, message)
}

func (s *Store) RecentFileIntakesByUser(ctx context.Context, userID int64, limit int, since time.Time) ([]FileIntake, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+fileIntakeCols+` FROM file_intakes
		 WHERE user_id = $1 AND canonical AND created_at >= $2
		 ORDER BY id DESC LIMIT $3`, userID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]FileIntake, 0, limit)
	for rows.Next() {
		in, err := scanFileIntake(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *in)
	}
	return out, rows.Err()
}

// FileStoragePaths 返回 files 表仍引用的内容寻址相对路径，用于物理 blob GC。
func (s *Store) FileStoragePaths(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT storage_path FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths[p] = true
	}
	return paths, rows.Err()
}

// AddTaskAttachmentFile 把文件挂到任务附件上。
func (s *Store) AddTaskAttachmentFile(ctx context.Context, taskID, fileID int64, caption string) error {
	_, err := s.AddTaskAttachmentFileOnce(ctx, taskID, fileID, caption)
	return err
}

// CreateTaskWithFileAttachments publishes a task and all of its file inputs in
// one transaction. Workers poll independently, so a task must never become
// claimable before the attachments required to execute it are visible.
func (s *Store) CreateTaskWithFileAttachments(ctx context.Context, task *Task, fileIDs []int64, caption string) (*Task, error) {
	return s.createTaskWithFileAttachments(ctx, task, fileIDs, caption, nil, nil)
}

func (s *Store) CreateTaskWithFileAttachmentsAndWorkerRun(ctx context.Context, task *Task, fileIDs []int64, caption string, spec WorkerRunSpec) (*Task, error) {
	return s.createTaskWithFileAttachments(ctx, task, fileIDs, caption, &spec, nil)
}

type MaterialTaskSpec struct {
	OwnerID     int64
	Title       string
	Instruction string
}

// CreateMaterialTaskWithWorkerRun atomically publishes the task, its worker
// run, file attachments, and the material lifecycle projection.
func (s *Store) CreateMaterialTaskWithWorkerRun(ctx context.Context, task *Task, fileIDs []int64, caption string, spec WorkerRunSpec, material MaterialTaskSpec) (*Task, error) {
	return s.createTaskWithFileAttachments(ctx, task, fileIDs, caption, &spec, &material)
}

func (s *Store) createTaskWithFileAttachments(ctx context.Context, task *Task, fileIDs []int64, caption string, spec *WorkerRunSpec, material *MaterialTaskSpec) (*Task, error) {
	for _, fileID := range fileIDs {
		if fileID <= 0 {
			return nil, ErrNotFound
		}
	}
	fileIDs = uniquePositiveIDs(fileIDs)
	if len(fileIDs) > 0 {
		if spec == nil {
			spec = &WorkerRunSpec{}
		}
		spec.FileIDs = append([]int64(nil), fileIDs...)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := createTaskTx(ctx, tx, task, spec)
	if err != nil {
		return nil, err
	}
	for _, fileID := range fileIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_attachments (task_id, kind, file_ref, caption, file_id)
			 VALUES ($1, 'file', $2, $3, $4) ON CONFLICT DO NOTHING`,
			created.ID, fmt.Sprint(fileID), caption, fileID); err != nil {
			return nil, wrapErr(err)
		}
	}
	if material != nil {
		if err := queueMaterialFilesTx(ctx, tx, material.OwnerID, fileIDs, created.ID,
			material.Title, material.Instruction); err != nil {
			return nil, err
		}
	}
	return created, tx.Commit(ctx)
}

// AddTaskAttachmentFileOnce returns true only when a new relationship was
// created. The database unique index makes this safe across concurrent turns.
func (s *Store) AddTaskAttachmentFileOnce(ctx context.Context, taskID, fileID int64, caption string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := lockTaskForInputChangeTx(ctx, tx, taskID)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO task_attachments (task_id, kind, file_ref, caption, file_id)
		 VALUES ($1, 'file', $2, $3, $4)
		 ON CONFLICT DO NOTHING`,
		taskID, fmt.Sprint(fileID), caption, fileID)
	if err != nil {
		return false, wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if _, err := reviseTaskForInputChangeTx(ctx, tx, current); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// TaskFileAttachments 返回任务上的真实文件附件；旧 file_ref-only 附件不在这里返回。
func (s *Store) TaskFileAttachments(ctx context.Context, taskID int64) ([]File, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT f.id, f.source, f.original_name, f.mime_type, f.size_bytes, f.sha256, f.storage_path, f.created_by, f.created_at
		 FROM task_attachments a
		 JOIN files f ON f.id = a.file_id
		 WHERE a.task_id = $1
		 ORDER BY a.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// WorkerCanSubmitArtifact validates the exact active execution lease before a
// multipart body is accepted.
func (s *Store) WorkerCanSubmitArtifact(ctx context.Context, runID, workerID int64, claimID string) (bool, error) {
	if claimID == "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM worker_runs
		   WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3)`,
		runID, workerID, claimID).Scan(&ok)
	return ok, err
}

// DeleteOrphanFileRow 删除一条没有任何附件/产物引用的 files 行（清理「落盘后 claim
// 恰失效」的极窄竞态残留）。单条原子语句、只作用于自己的 fileID，并发安全。
// **只删 DB 行、不碰内容寻址 blob**——同内容的其它上传可能正共享该 blob，物理回收
// 交给离线 GC，避免删掉别人还在引用的物理文件。
func (s *Store) DeleteOrphanFileRow(ctx context.Context, fileID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := deleteUnreferencedFileRowTx(ctx, tx, fileID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteUnreferencedFile deletes file metadata only when no task attachment or
// artifact still references it. Content-addressed blobs are reclaimed by GC so
// another file row sharing the same bytes remains safe.
func (s *Store) DeleteUnreferencedFile(ctx context.Context, fileID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	deleted, err := deleteUnreferencedFileRowTx(ctx, tx, fileID)
	if err != nil {
		return err
	}
	if deleted {
		return tx.Commit(ctx)
	}
	if err := tx.Rollback(ctx); err != nil {
		return err
	}
	if _, err := s.FileByID(ctx, fileID); err != nil {
		return err
	}
	return ErrConflict
}

func deleteUnreferencedFileRowTx(ctx context.Context, tx pgx.Tx, fileID int64) (bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT case_id FROM material_case_files WHERE file_id = $1`, fileID)
	if err != nil {
		return false, wrapErr(err)
	}
	var caseIDs []int64
	for rows.Next() {
		var caseID int64
		if err := rows.Scan(&caseID); err != nil {
			rows.Close()
			return false, wrapErr(err)
		}
		caseIDs = append(caseIDs, caseID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, wrapErr(err)
	}
	rows.Close()
	tag, err := tx.Exec(ctx,
		`DELETE FROM files WHERE id = $1
		   AND NOT EXISTS(SELECT 1 FROM task_attachments WHERE file_id = $1)
		   AND NOT EXISTS(SELECT 1 FROM task_artifacts WHERE file_id = $1)
		   AND NOT EXISTS(SELECT 1 FROM worker_run_files WHERE file_id = $1)`, fileID)
	if err != nil {
		return false, wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if len(caseIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`DELETE FROM material_cases c
			  WHERE c.id = ANY($1::bigint[])
			    AND NOT EXISTS (SELECT 1 FROM material_case_files cf WHERE cf.case_id = c.id)`, caseIDs); err != nil {
			return false, wrapErr(err)
		}
	}
	return true, nil
}

// AddWorkerArtifact records the file against the run. Linked business tasks
// also receive the artifact projection used by review and subsequent rework.
func (s *Store) AddWorkerArtifact(ctx context.Context, runID, workerID int64, claimID string, fileID int64, requestID, caption string) (canonicalFileID int64, inserted bool, err error) {
	caption = strings.TrimSpace(caption)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	taskID, err := lockWorkerRunTx(ctx, tx, runID, workerID)
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE worker_runs SET claimed_at = now(), updated_at = now()
		 WHERE id = $1 AND worker_id = $2 AND status = 'claimed' AND claim_id = $3
		`, runID, workerID, claimID); err != nil {
		return 0, false, wrapErr(err)
	}
	attemptTag, err := tx.Exec(ctx,
		`UPDATE worker_run_attempts SET heartbeat_at = now(), updated_at = now()
		 WHERE run_id = $1 AND worker_id = $2 AND claim_id = $3 AND status = 'claimed'`, runID, workerID, claimID)
	if err != nil {
		return 0, false, wrapErr(err)
	}
	if attemptTag.RowsAffected() == 0 {
		return 0, false, ErrNotFound
	}
	requestID = strings.TrimSpace(requestID)
	var incomingSHA string
	var incomingSize int64
	if err := tx.QueryRow(ctx,
		`SELECT sha256, size_bytes FROM files WHERE id=$1 AND created_by=$2`, fileID, workerID).
		Scan(&incomingSHA, &incomingSize); err != nil {
		return 0, false, wrapErr(err)
	}
	if requestID != "" {
		var existingID, existingSize int64
		var existingSHA, existingCaption string
		err := tx.QueryRow(ctx,
			`SELECT rf.file_id, f.sha256, f.size_bytes, rf.caption
			   FROM worker_run_files rf JOIN files f ON f.id=rf.file_id
			  WHERE rf.run_id=$1 AND rf.request_id=$2`, runID, requestID).
			Scan(&existingID, &existingSHA, &existingSize, &existingCaption)
		switch {
		case err == nil:
			if existingSHA != incomingSHA || existingSize != incomingSize || existingCaption != caption {
				return 0, false, ErrConflict
			}
			if err := tx.Commit(ctx); err != nil {
				return 0, false, err
			}
			return existingID, false, nil
		case wrapErr(err) != ErrNotFound:
			return 0, false, wrapErr(err)
		}
	}
	fileTag, err := tx.Exec(ctx,
		`INSERT INTO worker_run_files (run_id, file_id, role, caption, created_by, request_id)
		 VALUES ($1,$2,'artifact',$3,$4,$5) ON CONFLICT DO NOTHING`,
		runID, fileID, caption, workerID, requestID)
	if err != nil {
		return 0, false, wrapErr(err)
	}
	inserted = fileTag.RowsAffected() == 1
	canonicalFileID = fileID
	if !inserted && requestID != "" {
		var existingID, existingSize int64
		var existingSHA, existingCaption string
		err := tx.QueryRow(ctx,
			`SELECT rf.file_id, f.sha256, f.size_bytes, rf.caption
			   FROM worker_run_files rf JOIN files f ON f.id=rf.file_id
			  WHERE rf.run_id=$1 AND rf.request_id=$2`, runID, requestID).
			Scan(&existingID, &existingSHA, &existingSize, &existingCaption)
		if err == nil {
			if existingSHA != incomingSHA || existingSize != incomingSize || existingCaption != caption {
				return 0, false, ErrConflict
			}
			canonicalFileID = existingID
		} else if wrapErr(err) != ErrNotFound {
			return 0, false, wrapErr(err)
		}
	}
	if taskID != nil && inserted {
		if _, err := tx.Exec(ctx,
			`INSERT INTO task_artifacts (task_id, file_id, claim_id, created_by, caption)
			 VALUES ($1,$2,$3,$4,$5)`, *taskID, fileID, claimID, workerID, caption); err != nil {
			return 0, false, wrapErr(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return canonicalFileID, inserted, nil
}

// TaskArtifacts 返回任务产物。
func (s *Store) TaskArtifacts(ctx context.Context, taskID int64) ([]Artifact, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id, a.task_id, a.claim_id, a.created_by, a.caption, a.created_at,
		        f.id, f.source, f.original_name, f.mime_type, f.size_bytes, f.sha256, f.storage_path, f.created_by, f.created_at
		 FROM task_artifacts a
		 JOIN files f ON f.id = a.file_id
		 WHERE a.task_id = $1
		 ORDER BY a.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.ID, &a.TaskID, &a.ClaimID, &a.CreatedBy, &a.Caption, &a.CreatedAt,
			&a.File.ID, &a.File.Source, &a.File.OriginalName, &a.File.MIMEType, &a.File.SizeBytes,
			&a.File.SHA256, &a.File.StoragePath, &a.File.CreatedBy, &a.File.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UserCanAccessFile 判断普通用户是否能下载文件。
func (s *Store) UserCanAccessFile(ctx context.Context, userID int64, superadmin bool, fileID int64) (bool, error) {
	if superadmin {
		return true, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM files WHERE id = $1 AND created_by = $2
		    UNION ALL
		    SELECT 1 FROM task_attachments a JOIN tasks t ON t.id = a.task_id
		      WHERE a.file_id = $1 AND (
		        t.assigner_id = $2 OR t.assignee_id = $2 OR EXISTS (
		          SELECT 1 FROM task_participants tp WHERE tp.task_id = t.id AND tp.user_id = $2
		        )
		      )
		    UNION ALL
		    SELECT 1 FROM task_artifacts a JOIN tasks t ON t.id = a.task_id
		      WHERE a.file_id = $1 AND (
		        t.assigner_id = $2 OR t.assignee_id = $2 OR EXISTS (
		          SELECT 1 FROM task_participants tp WHERE tp.task_id = t.id AND tp.user_id = $2
		        )
		      )
		    UNION ALL
		    SELECT 1 FROM worker_run_files rf JOIN worker_runs r ON r.id = rf.run_id
		      WHERE rf.file_id = $1 AND (r.requested_by = $2 OR r.worker_id = $2)
		)`, fileID, userID).Scan(&ok)
	return ok, err
}

// WorkerCanDownloadFile accepts run-specific input/artifacts and, for linked
// runs, the task's original inputs and prior rework artifacts.
func (s *Store) WorkerCanDownloadFile(ctx context.Context, runID, workerID int64, claimID string, fileID int64) (bool, error) {
	if claimID == "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM worker_run_files rf JOIN worker_runs r ON r.id = rf.run_id
		    WHERE rf.run_id = $1 AND rf.file_id = $4
		      AND r.worker_id = $2 AND r.status = 'claimed' AND r.claim_id = $3
		    UNION ALL
		    SELECT 1 FROM worker_runs r JOIN task_attachments a ON a.task_id = r.task_id
		    WHERE r.id = $1 AND a.file_id = $4
		      AND r.worker_id = $2 AND r.status = 'claimed' AND r.claim_id = $3
		    UNION ALL
		    SELECT 1 FROM worker_runs r JOIN task_artifacts a ON a.task_id = r.task_id
		    WHERE r.id = $1 AND a.file_id = $4
		      AND r.worker_id = $2 AND r.status = 'claimed' AND r.claim_id = $3
		)`, runID, workerID, claimID, fileID).Scan(&ok)
	return ok, err
}

// WorkerRunFiles returns direct run inputs or previously uploaded artifacts.
func (s *Store) WorkerRunFiles(ctx context.Context, runID int64, role string) ([]File, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT f.id, f.source, f.original_name, f.mime_type, f.size_bytes, f.sha256,
		        f.storage_path, f.created_by, f.created_at
		 FROM worker_run_files rf JOIN files f ON f.id = rf.file_id
		 WHERE rf.run_id = $1 AND ($2 = '' OR rf.role = $2)
		 ORDER BY rf.id`, runID, strings.TrimSpace(role))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		file, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *file)
	}
	return out, rows.Err()
}

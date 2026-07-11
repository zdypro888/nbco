package store

import (
	"context"
	"fmt"
	"time"
)

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
	return scanFile(s.pool.QueryRow(ctx,
		`INSERT INTO files (source, original_name, mime_type, size_bytes, sha256, storage_path, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, source, original_name, mime_type, size_bytes, sha256, storage_path, created_by, created_at`,
		f.Source, f.OriginalName, f.MIMEType, f.SizeBytes, f.SHA256, f.StoragePath, f.CreatedBy))
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
	return scanFileIntake(s.pool.QueryRow(ctx,
		`INSERT INTO file_intakes (user_id, source, external_ref, original_name, mime_type, size_bytes)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING `+fileIntakeCols,
		in.UserID, in.Source, in.ExternalRef, in.OriginalName, in.MIMEType, in.SizeBytes))
}

func (s *Store) CompleteFileIntake(ctx context.Context, intakeID, fileID int64) error {
	return s.execOne(ctx,
		`UPDATE file_intakes
		 SET status = 'saved', file_id = $2, error_code = '', error_message = '', updated_at = now()
		 WHERE id = $1 AND status = 'pending'`, intakeID, fileID)
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
		 WHERE user_id = $1 AND created_at >= $2
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

// AddTaskAttachmentFileOnce returns true only when a new relationship was
// created. The database unique index makes this safe across concurrent turns.
func (s *Store) AddTaskAttachmentFileOnce(ctx context.Context, taskID, fileID int64, caption string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO task_attachments (task_id, kind, file_ref, caption, file_id)
		 VALUES ($1, 'file', $2, $3, $4)
		 ON CONFLICT DO NOTHING`,
		taskID, fmt.Sprint(fileID), caption, fileID)
	if err != nil {
		return false, wrapErr(err)
	}
	return tag.RowsAffected() == 1, nil
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

// WorkerCanSubmitArtifact 预校验：任务仍是该 worker 手上、in_progress、claim 匹配时
// 才允许上传产物。用于「落盘前」拦截过期/伪造 claim，避免写孤儿 blob。
func (s *Store) WorkerCanSubmitArtifact(ctx context.Context, taskID, workerID int64, claimID string) (bool, error) {
	if claimID == "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM tasks
		   WHERE id = $1 AND assignee_id = $2 AND status = 'in_progress' AND worker_claim_id = $3)`,
		taskID, workerID, claimID).Scan(&ok)
	return ok, err
}

// DeleteOrphanFileRow 删除一条没有任何附件/产物引用的 files 行（清理「落盘后 claim
// 恰失效」的极窄竞态残留）。单条原子语句、只作用于自己的 fileID，并发安全。
// **只删 DB 行、不碰内容寻址 blob**——同内容的其它上传可能正共享该 blob，物理回收
// 交给离线 GC，避免删掉别人还在引用的物理文件。
func (s *Store) DeleteOrphanFileRow(ctx context.Context, fileID int64) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM files WHERE id = $1
		   AND NOT EXISTS(SELECT 1 FROM task_attachments WHERE file_id = $1)
		   AND NOT EXISTS(SELECT 1 FROM task_artifacts WHERE file_id = $1)`, fileID)
	return err
}

// DeleteUnreferencedFile deletes file metadata only when no task attachment or
// artifact still references it. Content-addressed blobs are reclaimed by GC so
// another file row sharing the same bytes remains safe.
func (s *Store) DeleteUnreferencedFile(ctx context.Context, fileID int64) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM files WHERE id = $1
		   AND NOT EXISTS(SELECT 1 FROM task_attachments WHERE file_id = $1)
		   AND NOT EXISTS(SELECT 1 FROM task_artifacts WHERE file_id = $1)`, fileID)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := s.FileByID(ctx, fileID); err != nil {
		return err
	}
	return ErrConflict
}

// AddWorkerArtifact 仅当 worker 仍持有同一 claim 时，把文件登记为任务产物。
func (s *Store) AddWorkerArtifact(ctx context.Context, taskID, workerID int64, claimID string, fileID int64, caption string) error {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO task_artifacts (task_id, file_id, claim_id, created_by, caption)
		 SELECT id, $4, $3, $2, $5 FROM tasks
		 WHERE id = $1 AND assignee_id = $2 AND status = 'in_progress' AND worker_claim_id = $3`,
		taskID, workerID, claimID, fileID, caption)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
		)`, fileID, userID).Scan(&ok)
	return ok, err
}

// WorkerCanDownloadFile 判断 worker 是否能用当前 claim 下载某个任务输入文件。
// 输入包括原始任务附件，以及返工时上一轮提交过的产物。
func (s *Store) WorkerCanDownloadFile(ctx context.Context, taskID, workerID int64, claimID string, fileID int64) (bool, error) {
	if claimID == "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM task_attachments a JOIN tasks t ON t.id = a.task_id
		    WHERE a.task_id = $1 AND a.file_id = $4
		      AND t.assignee_id = $2 AND t.status = 'in_progress' AND t.worker_claim_id = $3
		    UNION ALL
		    SELECT 1 FROM task_artifacts a JOIN tasks t ON t.id = a.task_id
		    WHERE a.task_id = $1 AND a.file_id = $4
		      AND t.assignee_id = $2 AND t.status = 'in_progress' AND t.worker_claim_id = $3
		)`, taskID, workerID, claimID, fileID).Scan(&ok)
	return ok, err
}

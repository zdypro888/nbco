package documentindex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/semantic"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/vectorstore"
)

const (
	defaultInterval       = 15 * time.Second
	extractTimeout        = 5 * time.Minute
	vectorCleanupInterval = time.Hour
	vectorCleanupRetry    = 10 * time.Minute
)

type Service struct {
	store             *store.Store
	semantic          *semantic.Service
	root              string
	nextVectorCleanup time.Time
}

func New(s *store.Store, semanticService *semantic.Service, root string) *Service {
	return &Service{store: s, semantic: semanticService, root: strings.TrimSpace(root)}
}

func (s *Service) Enabled() bool {
	return s != nil && s.store != nil && s.root != ""
}

// Run immediately catches up existing files, then continuously indexes newly
// uploaded files. PostgreSQL leases make the loop restart-safe.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if !s.Enabled() {
		return
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	for ctx.Err() == nil {
		s.runOnceSafely(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) runOnceSafely(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("文件正文索引 panic 已恢复", "panic", recovered)
		}
	}()
	revision := extractorCapabilityRevision(ctx)
	jobs, err := s.store.ClaimFilesForContentIndex(ctx, 2, revision)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("领取文件正文索引任务失败", "err", err)
		}
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		s.process(ctx, job)
	}
	s.processVectorQueue(ctx)
	s.cleanupVectorIndexIfDue(ctx)
}

func (s *Service) process(ctx context.Context, job store.FileContentIndexJob) {
	extractCtx, cancel := context.WithTimeout(ctx, extractTimeout)
	result, err := extract(extractCtx, s.root, job.File)
	cancel()
	if err != nil {
		retry := !isTerminalExtractionError(err) && ctx.Err() == nil
		if markErr := s.store.FailFileContentIndex(ctx, job, err, retry); markErr != nil && ctx.Err() == nil {
			slog.Warn("记录文件正文索引失败状态失败", "file_id", job.ID, "err", markErr)
		}
		level := slog.LevelWarn
		if !retry {
			level = slog.LevelInfo
		}
		slog.Log(ctx, level, "文件正文无法建立索引", "file_id", job.ID,
			"name", job.OriginalName, "retry", retry, "err", err)
		return
	}
	chunks, err := s.store.CompleteFileContentIndex(ctx, job, result.Extractor, splitText(result.Text), result.Truncated)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("保存文件正文分块失败", "file_id", job.ID, "err", err)
		}
		return
	}
	if len(chunks) == 0 {
		slog.Debug("文件没有可索引文本", "file_id", job.ID, "name", job.OriginalName)
		return
	}
	slog.Info("文件正文提取完成", "file_id", job.ID, "name", job.OriginalName,
		"chunks", len(chunks), "extractor", result.Extractor, "truncated", result.Truncated)
}

func (s *Service) processVectorQueue(ctx context.Context) {
	if s.semantic == nil || !s.semantic.Enabled() {
		return
	}
	model, _, err := s.semantic.CurrentModel(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("文件向量索引等待 embedding/Qdrant 恢复", "err", err)
		}
		return
	}
	jobs, err := s.store.ClaimFilesForVectorIndex(ctx, 2, model)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("领取文件向量索引任务失败", "err", err)
		}
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		s.processVector(ctx, job)
	}
}

func (s *Service) processVector(ctx context.Context, job store.FileVectorIndexJob) {
	docs := make([]semantic.Document, 0, len(job.Chunks))
	for _, chunk := range job.Chunks {
		docs = append(docs, semantic.Document{
			Ref: vectorstore.Ref{Source: semantic.SourceFileChunk, EntityID: strconv.FormatInt(chunk.ID, 10)},
			Content: fmt.Sprintf("original_name: %s\nmime_type: %s\ncontent: %s",
				strings.TrimSpace(job.OriginalName), strings.TrimSpace(job.MIMEType), strings.TrimSpace(chunk.Content)),
			Payload: map[string]any{vectorstore.PayloadKind: semantic.SourceFileChunk},
		})
	}
	indexCtx, cancel := context.WithTimeout(ctx, extractTimeout)
	report, err := s.semantic.UpsertDocumentsDetailed(indexCtx, docs)
	cancel()
	if err == nil && len(report.Failed) > 0 {
		err = fmt.Errorf("%d/%d 个文件分块无法嵌入", len(report.Failed), len(docs))
	}
	if err != nil {
		if markErr := s.store.FailFileVectorIndex(ctx, job, err); markErr != nil && ctx.Err() == nil {
			slog.Warn("记录文件向量失败状态失败", "file_id", job.ID, "err", markErr)
		}
		slog.Warn("文件正文向量写入失败，保留 PostgreSQL 正文并稍后重试", "file_id", job.ID, "err", err)
		return
	}
	if err := s.store.CompleteFileVectorIndex(ctx, job); err != nil {
		slog.Warn("记录文件向量完成状态失败", "file_id", job.ID, "err", err)
		return
	}
	slog.Info("文件正文向量索引完成", "file_id", job.ID, "name", job.OriginalName,
		"chunks", len(job.Chunks), "vectors", report.Indexed, "model", job.VectorModel)
}

func (s *Service) cleanupVectorIndexIfDue(ctx context.Context) {
	if s.semantic == nil || !s.semantic.Enabled() || time.Now().Before(s.nextVectorCleanup) {
		return
	}
	ids, err := s.store.FileTextChunkIDs(ctx)
	if err != nil {
		s.nextVectorCleanup = time.Now().Add(vectorCleanupRetry)
		slog.Warn("读取文件分块索引清单失败", "err", err)
		return
	}
	valid := make(map[string]bool, len(ids))
	for _, id := range ids {
		ref := vectorstore.Ref{Source: semantic.SourceFileChunk, EntityID: strconv.FormatInt(id, 10)}
		valid[ref.Key()] = true
	}
	if _, err := s.semantic.DeleteMissing(ctx, semantic.SourceFileChunk, valid); err != nil {
		s.nextVectorCleanup = time.Now().Add(vectorCleanupRetry)
		slog.Warn("清理文件分块孤儿向量失败", "err", err)
		return
	}
	s.nextVectorCleanup = time.Now().Add(vectorCleanupInterval)
}

func isUnsupported(err error) bool {
	return errors.Is(err, ErrUnsupported)
}

func isTerminalExtractionError(err error) bool {
	return errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnsafeInput)
}

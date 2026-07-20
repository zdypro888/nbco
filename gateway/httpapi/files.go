package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/safefs"
	"github.com/zdypro888/nbco/store"
)

const (
	maxUploadBytes     = 200 << 20
	maxMultipartMemory = 8 << 20
	fileGCEvery        = 6 * time.Hour
	fileGCGrace        = 24 * time.Hour
)

type fileJSON struct {
	ID           int64  `json:"id"`
	OriginalName string `json:"original_name"`
	MIMEType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	CreatedAt    string `json:"created_at,omitempty"`
	DownloadURL  string `json:"download_url,omitempty"`
}

type fileIntakeJSON struct {
	ID           int64  `json:"id"`
	OriginalName string `json:"original_name"`
	MIMEType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	Status       string `json:"status"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	CreatedAt    string `json:"created_at"`
}

func toFileJSON(f store.File, downloadURL string) fileJSON {
	return fileJSON{
		ID: f.ID, OriginalName: f.OriginalName, MIMEType: f.MIMEType,
		SizeBytes: f.SizeBytes, SHA256: f.SHA256, CreatedAt: f.CreatedAt.Format(time.RFC3339),
		DownloadURL: downloadURL,
	}
}

func parseID(v string) (int64, error) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("非法 ID")
	}
	return id, nil
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit 必须是正整数"})
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	sinceHours := 24 * 7
	if raw := strings.TrimSpace(r.URL.Query().Get("since_hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since_hours 必须是正整数"})
			return
		}
		if n > 24*90 {
			n = 24 * 90
		}
		sinceHours = n
	}
	files, err := s.store.RecentFilesByUser(r.Context(), u.ID, limit, time.Now().Add(-time.Duration(sinceHours)*time.Hour))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取文件失败"})
		return
	}
	out := make([]fileJSON, 0, len(files))
	for _, f := range files {
		out = append(out, toFileJSON(f, "/api/files/"+strconv.FormatInt(f.ID, 10)))
	}
	intakes, err := s.store.RecentFileIntakesByUser(r.Context(), u.ID, limit, time.Now().Add(-time.Duration(sinceHours)*time.Hour))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取文件接收记录失败"})
		return
	}
	intakeOut := make([]fileIntakeJSON, 0, len(intakes))
	for _, in := range intakes {
		if in.Status == store.FileIntakeSaved {
			continue
		}
		intakeOut = append(intakeOut, fileIntakeJSON{
			ID: in.ID, OriginalName: in.OriginalName, MIMEType: in.MIMEType,
			SizeBytes: in.SizeBytes, Status: in.Status, ErrorCode: in.ErrorCode,
			ErrorMessage: in.ErrorMessage, CreatedAt: in.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out, "intakes": intakeOut})
}

func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	f, err := s.saveMultipartFile(w, r, u.ID, "api")
	if err != nil {
		slog.Warn("文件上传失败", "user", u.ID, "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": toFileJSON(*f, "/api/files/"+strconv.FormatInt(f.ID, 10))})
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ok, err := s.store.UserCanAccessFile(r.Context(), u.ID, u.IsSuperadmin, id)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权访问文件"})
		return
	}
	s.serveFile(w, r, id)
}

func (s *Server) handleAttachFile(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	taskID, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	t, err := s.store.TaskByID(r.Context(), taskID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "任务不存在"})
		return
	}
	access, err := s.store.TaskAccessForUser(r.Context(), t, u.ID, u.IsSuperadmin)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "权限查询失败"})
		return
	}
	if !access.CanContribute && !access.CanManage {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权操作该任务"})
		return
	}
	var req struct {
		FileID  int64  `json:"file_id"`
		Caption string `json:"caption"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.FileID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_id 必填"})
		return
	}
	ok, err := s.store.UserCanAccessFile(r.Context(), u.ID, u.IsSuperadmin, req.FileID)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权访问文件"})
		return
	}
	inserted, err := s.store.AddTaskAttachmentFileOnce(r.Context(), taskID, req.FileID, req.Caption)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "挂载失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "attached": inserted})
}

func (s *Server) handleWorkerDownloadFile(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	candidate := r.URL.Query().Get("run_id")
	if strings.TrimSpace(candidate) == "" {
		candidate = r.URL.Query().Get("task_id") // rolling-upgrade compatibility
	}
	runID, err := parseID(candidate)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id 必填"})
		return
	}
	claimID := strings.TrimSpace(r.URL.Query().Get("claim_id"))
	if claimID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "claim_id 必填"})
		return
	}
	runID, err = s.store.ResolveWorkerRunID(r.Context(), runID, u.ID, claimID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权访问文件"})
		return
	}
	ok, err := s.store.WorkerCanDownloadFile(r.Context(), runID, u.ID, claimID, id)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权访问文件"})
		return
	}
	s.serveFile(w, r, id)
}

func (s *Server) handleWorkerArtifact(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	// run_id/claim_id 走 query：在解析（会把 200MB spool 到临时盘的）文件体【之前】
	// 就校验 claim。未授权/过期直接 409 返回，不消费文件体——既杜绝孤儿 blob，也
	// 堵死「拿 Worker Access Token 反复传 200MB 把临时盘写爆」。
	candidate := r.URL.Query().Get("run_id")
	if strings.TrimSpace(candidate) == "" {
		candidate = r.URL.Query().Get("task_id") // rolling-upgrade compatibility
	}
	runID, perr := parseID(candidate)
	if perr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id 必填"})
		return
	}
	claimID := strings.TrimSpace(r.URL.Query().Get("claim_id"))
	if claimID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "claim_id 必填"})
		return
	}
	runID, cerr := s.store.ResolveWorkerRunID(r.Context(), runID, u.ID, claimID)
	if cerr != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
		return
	}
	ok, cerr := s.store.WorkerCanSubmitArtifact(r.Context(), runID, u.ID, claimID)
	if cerr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "校验失败"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许上传产物"})
		return
	}
	// 授权通过才落盘。
	f, err := s.saveMultipartFile(w, r, u.ID, "worker")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.AddWorkerArtifact(r.Context(), runID, u.ID, claimID, f.ID, r.URL.Query().Get("caption")); err != nil {
		// 预校验已过、此处失败=落盘期间 claim 恰好失效的极窄竞态。只删孤儿 files 行、
		// 不碰内容寻址 blob（blob 可能被并发的同内容上传复用，物理回收交离线 GC）。
		_ = s.store.DeleteOrphanFileRow(r.Context(), f.ID)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许上传产物"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "产物登记失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": toFileJSON(*f, "")})
}

// saveMultipartFile 解析上传并落盘（内容寻址）。调用方须在调用【之前】完成授权
// 校验（worker 产物端点用 query 参数预校验），避免给未授权请求 spool 文件体。
func (s *Server) saveMultipartFile(w http.ResponseWriter, r *http.Request, userID int64, source string) (*store.File, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxMultipartMemory); err != nil {
		return nil, fmt.Errorf("解析上传失败")
	}
	// multipart 解析会把大文件缓冲到临时文件，请求结束务必清理。
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	src, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("file 必填")
	}
	defer src.Close()

	if err := safefs.EnsurePrivateDir(s.fileStorePath); err != nil {
		return nil, fmt.Errorf("创建文件存储目录失败")
	}
	tmp, err := os.CreateTemp(s.fileStorePath, ".upload-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败")
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(src, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("保存上传失败")
	}
	sum := hex.EncodeToString(h.Sum(nil))
	rel := filepath.Join(sum[:2], sum)
	dst := filepath.Join(s.fileStorePath, rel)
	if err := safefs.EnsurePrivateDir(filepath.Dir(dst)); err != nil {
		return nil, fmt.Errorf("创建存储目录失败")
	}
	moved, err := safefs.InstallContentFile(tmpName, dst, sum)
	if err != nil {
		return nil, fmt.Errorf("落盘失败")
	}
	if moved {
		tmpName = ""
	}
	uid := userID
	mimeType := hdr.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(hdr.Filename))
	}
	return s.store.CreateFile(r.Context(), &store.File{
		Source: source, OriginalName: filepath.Base(hdr.Filename), MIMEType: mimeType,
		SizeBytes: n, SHA256: sum, StoragePath: rel, CreatedBy: &uid,
	})
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, id int64) {
	f, err := s.store.FileByID(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件不存在"})
		return
	}
	fp, err := safefs.OpenRegular(s.fileStorePath, f.StoragePath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "文件不存在"})
		return
	}
	defer fp.Close()
	if f.MIMEType != "" {
		w.Header().Set("Content-Type", f.MIMEType)
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.OriginalName))
	http.ServeContent(w, r, f.OriginalName, f.CreatedAt, fp)
}

func (s *Server) runFileGC(ctx context.Context) {
	s.runFileGCPass(ctx)
	t := time.NewTicker(fileGCEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runFileGCPass(ctx)
		}
	}
}

func (s *Server) runFileGCPass(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("文件存储 GC panic 已恢复；后续周期仍会继续", "panic", r)
		}
	}()
	s.gcFileStore(ctx)
}

func (s *Server) gcFileStore(ctx context.Context) {
	if err := s.collectOrphanFiles(ctx, fileGCGrace); err != nil {
		slog.Warn("文件存储 GC 失败", "err", err)
	}
}

func (s *Server) collectOrphanFiles(ctx context.Context, grace time.Duration) error {
	live, err := s.store.FileStoragePaths(ctx)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(s.fileStorePath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer rootFS.Close()
	cutoff := time.Now().Add(-grace)
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.Base(path), ".upload-") || !live[rel] {
			if err := rootFS.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	})
}

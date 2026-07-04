package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	"github.com/zdypro888/nbco/store"
)

const maxUploadBytes = 200 << 20

type fileJSON struct {
	ID           int64  `json:"id"`
	OriginalName string `json:"original_name"`
	MIMEType     string `json:"mime_type"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	DownloadURL  string `json:"download_url,omitempty"`
}

func toFileJSON(f store.File, downloadURL string) fileJSON {
	return fileJSON{
		ID: f.ID, OriginalName: f.OriginalName, MIMEType: f.MIMEType,
		SizeBytes: f.SizeBytes, SHA256: f.SHA256, DownloadURL: downloadURL,
	}
}

func parseID(v string) (int64, error) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("非法 ID")
	}
	return id, nil
}

// errArtifactRejected：worker 产物的 claim 预校验未通过（落盘前拦截）。
var errArtifactRejected = errors.New("artifact rejected")

func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	f, err := s.saveMultipartFile(w, r, u.ID, "api", nil)
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
	if !u.IsSuperadmin && t.AssignerID != u.ID && t.AssigneeID != u.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权操作该任务"})
		return
	}
	var req struct {
		FileID  int64  `json:"file_id"`
		Caption string `json:"caption"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FileID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file_id 必填"})
		return
	}
	ok, err := s.store.UserCanAccessFile(r.Context(), u.ID, u.IsSuperadmin, req.FileID)
	if err != nil || !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权访问文件"})
		return
	}
	if err := s.store.AddTaskAttachmentFile(r.Context(), taskID, req.FileID, req.Caption); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "挂载失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
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
	ok, err := s.store.WorkerCanAccessFile(r.Context(), u.ID, id)
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
	var taskID int64
	var claimID string
	// 授权校验（validate）在落盘之前跑：claim 无效/过期就不写 blob、不建 files 行，
	// 杜绝孤儿文件，也堵死「拿 worker token 反复传 200MB 撑爆磁盘」。
	f, err := s.saveMultipartFile(w, r, u.ID, "worker", func() error {
		var perr error
		if taskID, perr = parseID(r.FormValue("task_id")); perr != nil {
			return fmt.Errorf("task_id 必填")
		}
		if claimID = strings.TrimSpace(r.FormValue("claim_id")); claimID == "" {
			return fmt.Errorf("claim_id 必填")
		}
		ok, cerr := s.store.WorkerCanSubmitArtifact(r.Context(), taskID, u.ID, claimID)
		if cerr != nil {
			return fmt.Errorf("校验失败")
		}
		if !ok {
			return errArtifactRejected
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errArtifactRejected) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许上传产物"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.AddWorkerArtifact(r.Context(), taskID, u.ID, claimID, f.ID, r.FormValue("caption")); err != nil {
		// 预校验已过，此处失败=落盘期间 claim 恰好失效的极窄竞态：清理刚建的孤儿。
		if path, derr := s.store.DeleteFileIfUnreferenced(r.Context(), f.ID); derr == nil && path != "" {
			if fp, ferr := s.filePath(path); ferr == nil {
				_ = os.Remove(fp)
			}
		}
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许上传产物"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "产物登记失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": toFileJSON(*f, "")})
}

// saveMultipartFile 解析上传并落盘（内容寻址）。validate（可为 nil）在解析表单
// 之后、落盘之前执行——授权校验放这里，不过就不写任何 blob/files 行。
func (s *Server) saveMultipartFile(w http.ResponseWriter, r *http.Request, userID int64, source string, validate func() error) (*store.File, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		return nil, fmt.Errorf("解析上传失败")
	}
	// multipart 解析会把大文件缓冲到临时文件，请求结束务必清理。
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	src, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("file 必填")
	}
	defer src.Close()

	if err := os.MkdirAll(s.fileStorePath, 0o755); err != nil {
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
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, fmt.Errorf("创建存储目录失败")
	}
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(tmpName, dst); err != nil {
			return nil, fmt.Errorf("落盘失败")
		}
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
	path, err := s.filePath(f.StoragePath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "文件路径非法"})
		return
	}
	fp, err := os.Open(path)
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

func (s *Server) filePath(rel string) (string, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("bad path")
	}
	root, err := filepath.Abs(s.fileStorePath)
	if err != nil {
		return "", err
	}
	full, err := filepath.Abs(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("bad path")
	}
	return full, nil
}

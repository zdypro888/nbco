package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var workerDownloadNames = map[string]bool{
	"nbco-worker-darwin-arm64":             true,
	"nbco-worker-linux-amd64":              true,
	"nbco-worker-linux-arm64":              true,
	"nbco-worker-windows-amd64.exe":        true,
	"nbco-worker-darwin-arm64.sha256":      true,
	"nbco-worker-linux-amd64.sha256":       true,
	"nbco-worker-linux-arm64.sha256":       true,
	"nbco-worker-windows-amd64.exe.sha256": true,
}

func (s *Server) handleWorkerDownloadBinary(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if !workerDownloadNames[name] {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker 发行文件不存在"})
		return
	}
	root := strings.TrimSpace(s.downloadPath)
	if root == "" {
		root = "downloads"
	}
	full, err := safeJoin(root, "worker", name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker 发行文件不存在"})
		return
	}
	if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("worker 发行文件未部署：%s", name),
		})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取发行文件失败"})
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if strings.HasSuffix(name, ".sha256") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeFile(w, r, full)
}

func safeJoin(root string, parts ...string) (string, error) {
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(append([]string{base}, parts...)...)
	full, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	if full != base && !strings.HasPrefix(full, base+string(filepath.Separator)) {
		return "", fmt.Errorf("bad path")
	}
	return full, nil
}

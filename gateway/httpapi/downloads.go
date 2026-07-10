package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/zdypro888/nbco/safefs"
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
	f, err := safefs.OpenRegular(root, "worker/"+name)
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("worker 发行文件未部署：%s", name),
		})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取发行文件失败"})
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker 发行文件不存在"})
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if strings.HasSuffix(name, ".sha256") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/store"
)

const bootstrapMaxBody = 16 << 10

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, bootstrapMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 必填"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 必填"})
		return
	}
	ident := store.Identity{
		Provider:   "api",
		ExternalID: "bootstrap:" + strconv.FormatInt(time.Now().UnixNano(), 10),
		ChatRef:    "api:" + name,
	}
	u, token, err := s.store.BootstrapSuperadminWithAPIToken(r.Context(), name, ident)
	if errors.Is(err, store.ErrConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "系统已有活跃超级管理员"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "初始化失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": map[string]any{
			"id": u.ID, "name": u.Name, "is_superadmin": u.IsSuperadmin,
		},
		"token": token,
		"usage": fmt.Sprintf("Authorization: Bearer %s", token),
	})
}

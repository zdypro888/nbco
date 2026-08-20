package httpapi

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/zdypro888/nbco/store"
)

const bootstrapMaxBody = 16 << 10

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		AccessToken string `json:"access_token"`
	}
	if err := decodeJSONLimit(w, r, &req, bootstrapMaxBody); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 必填"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 必填"})
		return
	}
	requestKey, err := requestIdempotencyKey(r, true)
	if err != nil {
		writeHTTPActionClaimError(w, err, nil)
		return
	}
	accessToken := strings.TrimSpace(req.AccessToken)
	decoded, decodeErr := hex.DecodeString(accessToken)
	if decodeErr != nil || len(decoded) != 24 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "access_token 必须是客户端预生成的 48 位十六进制随机值"})
		return
	}
	externalID := "bootstrap:" + httpActionKey(0, "bootstrap", requestKey)
	ident := store.Identity{
		Provider:   "api",
		ExternalID: externalID,
		ChatRef:    "api:" + externalID,
	}
	u, token, err := s.store.BootstrapSuperadminWithAPITokenCandidate(r.Context(), name, ident, accessToken)
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

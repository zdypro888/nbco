package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/store"
)

const (
	ihtmlTicketVersion = byte(1)
	ihtmlTicketTTL     = 5 * time.Minute
	ihtmlTicketBytes   = 1 + 8 + 8 + 8 // version + user + expiry + nonce
)

type ihtmlTicketManager struct {
	secret [32]byte
	now    func() time.Time
}

func newIHTMLTicketManager() (*ihtmlTicketManager, error) {
	m := &ihtmlTicketManager{now: time.Now}
	if _, err := rand.Read(m.secret[:]); err != nil {
		return nil, fmt.Errorf("生成 ihtml 连接票据密钥: %w", err)
	}
	return m, nil
}

func (m *ihtmlTicketManager) issue(userID int64) (string, time.Time, error) {
	if m == nil || userID <= 0 {
		return "", time.Time{}, errors.New("invalid ihtml ticket subject")
	}
	expires := m.now().UTC().Add(ihtmlTicketTTL)
	payload := make([]byte, ihtmlTicketBytes)
	payload[0] = ihtmlTicketVersion
	binary.BigEndian.PutUint64(payload[1:9], uint64(userID))
	binary.BigEndian.PutUint64(payload[9:17], uint64(expires.Unix()))
	if _, err := rand.Read(payload[17:]); err != nil {
		return "", time.Time{}, fmt.Errorf("生成 ihtml 连接票据随机数: %w", err)
	}
	signature := m.sign(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(signature), expires, nil
}

func (m *ihtmlTicketManager) verify(token string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	payloadPart, signaturePart, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil || len(payload) != ihtmlTicketBytes || payload[0] != ihtmlTicketVersion {
		return 0, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil || !hmac.Equal(signature, m.sign(payload)) {
		return 0, false
	}
	userID := int64(binary.BigEndian.Uint64(payload[1:9]))
	expires := int64(binary.BigEndian.Uint64(payload[9:17]))
	now := m.now().UTC().Unix()
	// A bounded future window rejects tokens forged from malformed payloads even
	// if a future implementation accidentally reuses a signing key.
	if userID <= 0 || expires <= now || expires > now+int64((ihtmlTicketTTL+time.Minute)/time.Second) {
		return 0, false
	}
	return userID, true
}

func (m *ihtmlTicketManager) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, m.secret[:])
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func (s *Server) handleIHTMLTicket(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if s.ihtmlTickets == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "动态工作台未启用"})
		return
	}
	token, expires, err := s.ihtmlTickets.issue(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "签发工作台连接票据失败"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expires.Format(time.RFC3339),
	})
}

func (s *Server) ihtmlUserFromTicket(r *http.Request, allowQuery bool) (*store.User, error) {
	if s == nil || s.ihtmlTickets == nil || r == nil {
		return nil, store.ErrNotFound
	}
	raw := strings.TrimSpace(r.Header.Get("X-Ihtml-User"))
	if raw == "" && allowQuery {
		raw = strings.TrimSpace(r.URL.Query().Get("auth"))
	}
	userID, ok := s.ihtmlTickets.verify(raw)
	if !ok {
		return nil, store.ErrNotFound
	}
	u, err := s.store.UserByID(r.Context(), userID)
	if err != nil || u.Status != store.UserActive {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (s *Server) resolveIHTMLUser(r *http.Request) (string, error) {
	u, err := s.ihtmlUserFromTicket(r, true)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(u.ID, 10), nil
}

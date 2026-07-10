package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const telegramInitDataMaxAge = 24 * time.Hour

// validateTelegramInitData authenticates a Telegram Mini App launch according
// to the WebApp initData HMAC protocol and returns the Telegram user ID.
func validateTelegramInitData(raw, botToken string, now time.Time, maxAge time.Duration) (int64, bool) {
	raw = strings.TrimSpace(raw)
	botToken = strings.TrimSpace(botToken)
	if raw == "" || botToken == "" {
		return 0, false
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return 0, false
	}
	hashes, ok := values["hash"]
	if !ok || len(hashes) != 1 {
		return 0, false
	}
	provided, err := hex.DecodeString(hashes[0])
	if err != nil || len(provided) != sha256.Size {
		return 0, false
	}
	delete(values, "hash")
	keys := make([]string, 0, len(values))
	for key, items := range values {
		if key == "" || len(items) != 1 {
			return 0, false
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values.Get(key))
	}
	secretMAC := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secretMAC.Write([]byte(botToken))
	checkMAC := hmac.New(sha256.New, secretMAC.Sum(nil))
	_, _ = checkMAC.Write([]byte(strings.Join(lines, "\n")))
	if !hmac.Equal(provided, checkMAC.Sum(nil)) {
		return 0, false
	}
	authDate, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil || authDate <= 0 {
		return 0, false
	}
	issued := time.Unix(authDate, 0)
	if issued.After(now.Add(5*time.Minute)) || (maxAge > 0 && now.Sub(issued) > maxAge) {
		return 0, false
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(values.Get("user")), &user); err != nil || user.ID <= 0 {
		return 0, false
	}
	return user.ID, true
}

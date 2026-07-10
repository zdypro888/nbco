package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestValidateTelegramInitData(t *testing.T) {
	now := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	token := "123456:test-token"
	values := url.Values{
		"auth_date": {"1783695600"},
		"query_id":  {"query"},
		"user":      {`{"id":6103874246,"first_name":"PRO"}`},
	}
	raw := signedTelegramInitData(values, token)
	if id, ok := validateTelegramInitData(raw, token, now, 24*time.Hour); !ok || id != 6103874246 {
		t.Fatalf("valid initData: id=%d ok=%v", id, ok)
	}
	if _, ok := validateTelegramInitData(raw+"x", token, now, 24*time.Hour); ok {
		t.Fatal("tampered initData should fail")
	}
	if _, ok := validateTelegramInitData(raw, "wrong", now, 24*time.Hour); ok {
		t.Fatal("wrong bot token should fail")
	}
	if _, ok := validateTelegramInitData(raw, token, now.Add(25*time.Hour), 24*time.Hour); ok {
		t.Fatal("expired initData should fail")
	}
}

func signedTelegramInitData(values url.Values, token string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+values.Get(key))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	_, _ = secret.Write([]byte(token))
	check := hmac.New(sha256.New, secret.Sum(nil))
	_, _ = check.Write([]byte(strings.Join(lines, "\n")))
	values.Set("hash", hex.EncodeToString(check.Sum(nil)))
	return values.Encode()
}

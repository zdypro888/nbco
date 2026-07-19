package httpapi

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestIHTMLTicketLifecycle(t *testing.T) {
	manager, err := newIHTMLTicketManager()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	token, expires, err := manager.issue(42)
	if err != nil {
		t.Fatal(err)
	}
	if expires != now.Add(ihtmlTicketTTL) {
		t.Fatalf("expiry = %s", expires)
	}
	if userID, ok := manager.verify(token); !ok || userID != 42 {
		t.Fatalf("fresh ticket = user %d, valid %v", userID, ok)
	}

	manager.now = func() time.Time { return expires }
	if _, ok := manager.verify(token); ok {
		t.Fatal("ticket must be invalid at its expiry boundary")
	}

	manager.now = func() time.Time { return now }
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint64(payload[1:9], 99)
	tampered := base64.RawURLEncoding.EncodeToString(payload) + "." + parts[1]
	if _, ok := manager.verify(tampered); ok {
		t.Fatal("tampered ticket was accepted")
	}
}

func TestIHTMLTicketRejectsUnboundedFuture(t *testing.T) {
	manager, err := newIHTMLTicketManager()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	payload := make([]byte, ihtmlTicketBytes)
	payload[0] = ihtmlTicketVersion
	binary.BigEndian.PutUint64(payload[1:9], 7)
	binary.BigEndian.PutUint64(payload[9:17], uint64(now.Add(24*time.Hour).Unix()))
	token := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(manager.sign(payload))
	if _, ok := manager.verify(token); ok {
		t.Fatal("ticket with an unbounded future expiry was accepted")
	}
}

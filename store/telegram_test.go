package store

import (
	"strings"
	"testing"
)

func TestTelegramGroupStatusActive(t *testing.T) {
	tests := map[string]bool{
		"member":        true,
		"administrator": true,
		"creator":       true,
		"owner":         true,
		"restricted":    true,
		"left":          false,
		"kicked":        false,
		"":              false,
	}
	for status, want := range tests {
		if got := telegramGroupStatusActive(status); got != want {
			t.Errorf("telegramGroupStatusActive(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestTelegramGroupMembershipKeyIsOutsideGroupListingPrefix(t *testing.T) {
	if key := telegramGroupMembershipKey(-1001); strings.HasPrefix(key, KVTelegramGroupPrefix) {
		t.Fatalf("membership key %q must not appear in group-state listings", key)
	}
}

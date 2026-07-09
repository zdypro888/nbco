package store

import "testing"

func TestMissingRequiredFieldsReturnsEmptySlice(t *testing.T) {
	got := missingRequiredFields([]string{"手机"}, map[string]string{"手机": "13800000000"})
	if got == nil {
		t.Fatal("missingRequiredFields must return an empty slice, not nil, so pgx stores '{}' instead of SQL NULL")
	}
	if len(got) != 0 {
		t.Fatalf("expected no missing fields, got %v", got)
	}
}

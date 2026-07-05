package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONLimitsBody(t *testing.T) {
	body := `{"message":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	var dst struct {
		Message string `json:"message"`
	}
	if err := decodeJSON(rec, req, &dst); err == nil {
		t.Fatal("oversized JSON body should fail")
	}
}

package textfmt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	botToken := "1234567890:TESTtelegramBotTokenValueForRedaction"
	workerToken := "0123456789abcdef0123456789abcdef0123456789abcdef"
	apiKey := "sk-test-0123456789abcdef0123456789abcdef"
	apiHash := "0123456789abcdef0123456789abcdef"
	modelKey := "0123456789abcdef0123456789abcdef.fakeModelKey123"
	in := `{"token":"` + workerToken + `","api_hash":"` + apiHash + `","note":"` + botToken + ` ` + apiKey + ` ` + modelKey + `"}`
	out := RedactSecrets(in)
	for _, leak := range []string{botToken, workerToken, apiKey, apiHash, modelKey} {
		if strings.Contains(out, leak) {
			t.Fatalf("secret leaked after redaction: %s in %s", leak, out)
		}
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("redaction marker missing: %s", out)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("redacted JSON should remain valid: %v\n%s", err, out)
	}
	if decoded["token"] != "[redacted]" || decoded["api_hash"] != "[redacted]" {
		t.Fatalf("JSON secret fields not redacted cleanly: %#v", decoded)
	}
}

func TestStripReasoning(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"block", "<think>先想想</think>答案", "答案"},
		{"dangling close", "先想想</think>答案", "答案"},
		{"escaped block", "&lt;think&gt;先想想&lt;/think&gt;答案", "答案"},
		{"escaped dangling close", "先想想&lt;/think&gt;答案", "答案"},
		{"dangling open", "答案\n<think>后面不该显示", "答案"},
		{"plain", "正常答案", "正常答案"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripReasoning(tt.in); got != tt.want {
				t.Fatalf("StripReasoning(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

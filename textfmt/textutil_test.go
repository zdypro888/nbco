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

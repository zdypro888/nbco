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

func TestSanitizeVisibleReplyHidesToolOnlyUserRefs(t *testing.T) {
	in := `[工具引用·工作内存]
- user_id=3 name="黄桑" kind=human status=active
- user_id=4 name="JA" kind=human status=active

[用户可见目录]
真人员工（2 位）：
- 黄桑（正常）
- JA（正常）

我已发送给 黄桑（user_id=3），用户内部编号 4 不展示。`
	out := SanitizeVisibleReply(in)
	for _, bad := range []string{"工具引用", "user_id", "用户内部编号 4"} {
		if strings.Contains(out, bad) {
			t.Fatalf("visible reply leaked %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"真人员工（2 位）", "黄桑", "JA", "我已发送给 黄桑"} {
		if !strings.Contains(out, want) {
			t.Fatalf("visible reply missing %q:\n%s", want, out)
		}
	}
}

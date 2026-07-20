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
	githubToken := "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	privateKey := "-----BEGIN PRIVATE KEY-----\n0123456789abcdef\n-----END PRIVATE KEY-----"
	in := `{"token":"` + workerToken + `","api_hash":"` + apiHash + `","note":"` + botToken + ` ` + apiKey + ` ` + modelKey + ` ` + githubToken + ` ` + privateKey + `"}`
	out := RedactSecrets(in)
	for _, leak := range []string{botToken, workerToken, apiKey, apiHash, modelKey, githubToken, privateKey} {
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

func TestSanitizeVisibleReplyHidesToolProtocolButPreservesAuthorizedIDs(t *testing.T) {
	in := `[工具引用·工作内存]
- user_id=3 name="黄桑" kind=human status=active
- user_id=4 name="JA" kind=human status=active

[用户可见目录]
真人员工（2 位）：
- 黄桑（正常）
- JA（正常）

我已发送给黄桑（user_id=3），员工内部编号 4，TG ID: 6103874246。`
	out := SanitizeVisibleReply(in)
	for _, bad := range []string{"工具引用", `name="黄桑" kind=human`} {
		if strings.Contains(out, bad) {
			t.Fatalf("visible reply leaked %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"真人员工（2 位）", "黄桑", "JA", "user_id=3", "员工内部编号 4", "TG ID: 6103874246"} {
		if !strings.Contains(out, want) {
			t.Fatalf("visible reply missing %q:\n%s", want, out)
		}
	}
}

func TestSanitizeVisibleReplyHidesInternalMarkers(t *testing.T) {
	got := SanitizeVisibleReply("[nbco:tool_budget_exhausted] 请基于已有结果回答。")
	if got != "请基于已有结果回答。" {
		t.Fatalf("internal marker leaked: %q", got)
	}
}

func TestNormalizeEscapedLineBreaks(t *testing.T) {
	t.Run("double escaped layout", func(t *testing.T) {
		in := "💬 来自 黄桑：\n<b>标题</b>\\n\\n• 第一项\\r\\n• 第二项"
		want := "💬 来自 黄桑：\n<b>标题</b>\n\n• 第一项\n• 第二项"
		if got := NormalizeEscapedLineBreaks(in); got != want {
			t.Fatalf("NormalizeEscapedLineBreaks() = %q, want %q", got, want)
		}
	})

	t.Run("single notation remains literal", func(t *testing.T) {
		in := `Go 字符串可用 \n 表示换行`
		if got := NormalizeEscapedLineBreaks(in); got != in {
			t.Fatalf("single notation changed: %q", got)
		}
	})

	t.Run("code regions remain literal", func(t *testing.T) {
		in := "说明\\n\\n`a\\nb`\n<code>c\\nd</code>\n<pre>e\\nf</pre>\n```go\n" + `g := "x\n"` + "\n```"
		got := NormalizeEscapedLineBreaks(in)
		if !strings.HasPrefix(got, "说明\n\n") {
			t.Fatalf("layout was not normalized: %q", got)
		}
		for _, want := range []string{`a\nb`, `c\nd`, `e\nf`, `x\n`} {
			if !strings.Contains(got, want) {
				t.Fatalf("protected code lost %q: %q", want, got)
			}
		}
	})
}

func TestStripHistoryMetadata(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"legacy", "[历史消息时间 2026-07-11 22:19 +08:00 (Asia/Shanghai)] <b>已发送</b>", "<b>已发送</b>"},
		{"repeated legacy", "[历史消息时间 old] [历史消息时间 newer] answer", "answer"},
		{"structured suffix", "answer\n<nbco_history_meta timestamp=\"2026-07-11 22:19:00 +08:00\"/>", "answer"},
		{"escaped structured", "answer &lt;nbco_history_meta timestamp=\"x\"/&gt;", "answer"},
		{"streaming legacy", "[历史消息时间 2026-07", ""},
		{"streaming structured", "answer\n<nbco_history_meta timestamp=\"2026", "answer"},
		{"ordinary brackets", "[历史进度] 正常内容", "[历史进度] 正常内容"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripHistoryMetadata(tt.in); got != tt.want {
				t.Fatalf("StripHistoryMetadata(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

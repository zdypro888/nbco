// Package textfmt 提供跨包共享的纯文本工具：截断、字节量、scope 标签归一。
// 这些函数原先在 store/chat/tools/gateway 各自重复实现，集中到此处消除漂移风险。
package textfmt

import (
	"fmt"
	"regexp"
	"strings"
)

// TruncateRunes 按 rune 数截断（不破坏多字节字符），超出加省略号。
// 原 store/events.go、gateway/httpapi、chat/chat.go(truncateSnippet) 三处字节级重复实现。
func TruncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// FormatBytes 把字节数格式化成人类可读的 1024 进制量级（B/KiB/MiB…）。
// 原 tools/formatBytes、chat/formatBytesForPrompt、telegram/formatTelegramBytes 三处字节级重复实现。
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	units := "KMGTPE"
	for v := n / unit; v >= unit && exp < len(units)-1; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), units[exp])
}

// NormalizeScopeTags 归一标签：前置 scope:<scope>，去空白/去重/跳过已存在的 scope: 前缀标签。
// scope 为空时回退 "global"。原 normalizeSkillTags/normalizeMinedTags/normalizeLearningTags 三处近重复。
func NormalizeScopeTags(tags []string, scope string) []string {
	if scope == "" {
		scope = "global"
	}
	out := []string{"scope:" + scope}
	seen := map[string]bool{out[0]: true}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] || strings.HasPrefix(tag, "scope:") {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	return out
}

var (
	secretJSONAssignmentRe = regexp.MustCompile(`(?i)("?(?:api[_-]?key|api[_-]?hash|access[_-]?token|worker[_-]?access[_-]?token|token|secret|password)"?\s*:\s*")([^"]{8,})(")`)
	secretKVAssignmentRe   = regexp.MustCompile(`(?i)\b((?:api[_-]?key|api[_-]?hash|access[_-]?token|worker[_-]?access[_-]?token|token|secret|password)\s*[:=]\s*["']?)([^"'\s<]{8,})(["']?)`)
	secretPatterns         = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{12,}`),
		regexp.MustCompile(`(?i)\bsk-[a-z0-9][a-z0-9._-]{12,}`),
		regexp.MustCompile(`(?i)\b[0-9]{6,14}:[a-z0-9_-]{20,}\b`),
		regexp.MustCompile(`(?i)\b[a-z0-9]{24,}\.[a-z0-9_-]{12,}\b`),
		regexp.MustCompile(`(?i)\b[a-f0-9]{48,}\b`),
	}
)

// RedactSecrets removes API keys, Telegram bot tokens, worker access tokens,
// and common credential assignments before text is persisted in logs/history.
func RedactSecrets(s string) string {
	s = secretJSONAssignmentRe.ReplaceAllString(s, `$1[redacted]$3`)
	s = secretKVAssignmentRe.ReplaceAllString(s, `$1[redacted]$3`)
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

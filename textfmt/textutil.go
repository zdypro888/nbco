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
	reasoningBlockRe        = regexp.MustCompile(`(?is)<\s*think\b[^>]*>.*?<\s*/\s*think\s*>`)
	reasoningCloseRe        = regexp.MustCompile(`(?is)<\s*/\s*think\s*>`)
	reasoningOpenRe         = regexp.MustCompile(`(?is)<\s*think\b[^>]*>`)
	escapedReasoningBlockRe = regexp.MustCompile(`(?is)&lt;\s*think\b[^&]*?&gt;.*?&lt;\s*/\s*think\s*&gt;`)
	escapedReasoningCloseRe = regexp.MustCompile(`(?is)&lt;\s*/\s*think\s*&gt;`)
	escapedReasoningOpenRe  = regexp.MustCompile(`(?is)&lt;\s*think\b[^&]*?&gt;`)

	toolOnlySectionRe  = regexp.MustCompile(`(?is)(^|\n)\[工具引用[^\n]*\]\s*\n.*?\n\[用户可见目录\]\s*\n?`)
	trailingToolOnlyRe = regexp.MustCompile(`(?is)(^|\n)\[工具引用[^\n]*\]\s*\n.*$`)
	internalMarkerRe   = regexp.MustCompile(`(?i)\[nbco:[a-z0-9_-]+\]\s*`)
	userIDParenRe      = regexp.MustCompile(`(?i)[（(][^（）()\n]*(?:user[_ -]?id|用户\s*id|用户内部编号|成员内部编号|员工内部编号|tg\s*id|telegram\s*id)[^（）()\n]*[）)]`)
	userIDKVRe         = regexp.MustCompile(`(?i)\buser[_ -]?id\s*[:=：]\s*-?\d+\b`)
	userInternalRefRe  = regexp.MustCompile(`(?i)(用户|成员|员工|授予者|创建者|操作者|目标用户)\s*内部编号\s*-?\d+`)
	userIDLabelRe      = regexp.MustCompile(`(?i)(用户|成员|员工|tg|telegram)\s*id\s*[:=：]?\s*-?\d+\b`)
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

// StripReasoning removes model-emitted <think> blocks from user-visible text.
// Some OpenAI-compatible reasoning models put chain-of-thought in Content
// instead of a dedicated reasoning field, sometimes even with a missing opening
// tag during streaming. In that dangling-close case, everything before the last
// </think> is treated as hidden reasoning and dropped.
func StripReasoning(s string) string {
	if s == "" {
		return s
	}
	for {
		next := reasoningBlockRe.ReplaceAllString(s, "")
		next = escapedReasoningBlockRe.ReplaceAllString(next, "")
		if next == s {
			break
		}
		s = next
	}
	if locs := reasoningCloseRe.FindAllStringIndex(s, -1); len(locs) > 0 {
		s = s[locs[len(locs)-1][1]:]
	}
	if locs := escapedReasoningCloseRe.FindAllStringIndex(s, -1); len(locs) > 0 {
		s = s[locs[len(locs)-1][1]:]
	}
	if loc := reasoningOpenRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	if loc := escapedReasoningOpenRe.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	s = reasoningCloseRe.ReplaceAllString(s, "")
	s = escapedReasoningCloseRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// SanitizeVisibleReply removes tool-only references and user identity internals
// from text that is about to be shown to end users. Tool handlers may expose
// user_id in their own outputs so the model can chain calls; this final display
// pass is the deterministic privacy boundary.
func SanitizeVisibleReply(s string) string {
	s = StripReasoning(s)
	s = toolOnlySectionRe.ReplaceAllString(s, "$1")
	s = trailingToolOnlyRe.ReplaceAllString(s, "$1")
	s = internalMarkerRe.ReplaceAllString(s, "")
	s = userIDParenRe.ReplaceAllString(s, "")
	s = userIDKVRe.ReplaceAllString(s, "用户标识")
	s = userInternalRefRe.ReplaceAllString(s, "$1标识")
	s = userIDLabelRe.ReplaceAllString(s, "$1标识")
	return strings.TrimSpace(s)
}

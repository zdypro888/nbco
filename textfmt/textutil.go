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
		regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{12,}`),
		regexp.MustCompile(`(?i)\bsk-[a-z0-9][a-z0-9._-]{12,}`),
		regexp.MustCompile(`(?i)\b(?:gh[pousr]_[a-z0-9]{20,}|github_pat_[a-z0-9_]{20,})\b`),
		regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
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

	toolOnlySectionRe            = regexp.MustCompile(`(?is)(^|\n)\[工具引用[^\n]*\]\s*\n.*?\n\[用户可见目录\]\s*\n?`)
	trailingToolOnlyRe           = regexp.MustCompile(`(?is)(^|\n)\[工具引用[^\n]*\]\s*\n.*$`)
	internalMarkerRe             = regexp.MustCompile(`(?i)\[nbco:[a-z0-9_-]+\]\s*`)
	legacyHistoryTimeRe          = regexp.MustCompile(`(?m)(^|\n)[\t ]*\[历史消息时间[^\r\n]{1,240}\][\t ]*`)
	historyMetaTagRe             = regexp.MustCompile(`(?is)[\t ]*<nbco_history_meta\b[^>]*?/?>[\t ]*`)
	historyMetaDanglingRe        = regexp.MustCompile(`(?is)[\t ]*<nbco_history_meta\b[^>]*$`)
	escapedHistoryMetaTagRe      = regexp.MustCompile(`(?is)[\t ]*&lt;nbco_history_meta\b.*?/?&gt;[\t ]*`)
	escapedHistoryMetaDanglingRe = regexp.MustCompile(`(?is)[\t ]*&lt;nbco_history_meta\b[^&]*$`)
)

// RedactSecrets removes API keys, Telegram bot tokens, worker access tokens,
// and common credential assignments at derived or diagnostic boundaries such
// as logs, audits, semantic indexes, and learning candidates. Do not apply it
// to an authorized canonical record: destructive redaction there breaks later
// Agent turns and cannot be reversed after permissions have been checked.
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

// StripHistoryMetadata removes nbco's internal replay-time protocol from model
// text. Historical timestamps are useful to resolve relative dates, but they
// are not presentation content and must never be persisted or shown. The
// legacy natural-language prefix is removed as well so already polluted history
// cannot teach later turns to repeat it. Dangling forms cover streamed partials.
func StripHistoryMetadata(s string) string {
	if s == "" {
		return s
	}
	for {
		next := legacyHistoryTimeRe.ReplaceAllString(s, "$1")
		next = historyMetaTagRe.ReplaceAllString(next, "")
		next = escapedHistoryMetaTagRe.ReplaceAllString(next, "")
		if next == s {
			break
		}
		s = next
	}
	s = historyMetaDanglingRe.ReplaceAllString(s, "")
	s = escapedHistoryMetaDanglingRe.ReplaceAllString(s, "")
	// A streaming snapshot can end halfway through the old prefix before the
	// closing bracket arrives. Suppress only that unfinished final line.
	lineStart := strings.LastIndexByte(s, '\n') + 1
	lastLine := strings.TrimLeft(s[lineStart:], " \t")
	if strings.HasPrefix(lastLine, "[历史消息时间") && !strings.Contains(lastLine, "]") {
		s = s[:lineStart]
	}
	return strings.TrimSpace(s)
}

// NormalizeEscapedLineBreaks repairs presentation text whose producer escaped
// line breaks twice, leaving visible "\\n" sequences after JSON decoding. It
// only acts when at least two layout escapes occur outside code spans, which
// avoids changing ordinary prose such as "use \\n as the separator". Code and
// preformatted regions keep their literal escape sequences.
func NormalizeEscapedLineBreaks(s string) string {
	if !strings.Contains(s, `\n`) && !strings.Contains(s, `\r`) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	escapedBreaks := 0
	inFence, inInlineCode := false, false
	htmlCodeDepth := 0

	for i := 0; i < len(s); {
		if !inInlineCode && strings.HasPrefix(s[i:], "```") {
			inFence = !inFence
			b.WriteString("```")
			i += 3
			continue
		}
		if !inFence && s[i] == '`' {
			inInlineCode = !inInlineCode
			b.WriteByte(s[i])
			i++
			continue
		}
		if !inFence && !inInlineCode && s[i] == '<' {
			if relEnd := strings.IndexByte(s[i:], '>'); relEnd >= 0 {
				end := i + relEnd
				tag := strings.TrimSpace(s[i+1 : end])
				closing := strings.HasPrefix(tag, "/")
				tag = strings.TrimSpace(strings.TrimPrefix(tag, "/"))
				fields := strings.Fields(tag)
				if len(fields) > 0 {
					name := strings.ToLower(strings.TrimSuffix(fields[0], "/"))
					if name == "code" || name == "pre" {
						if closing {
							htmlCodeDepth = max(0, htmlCodeDepth-1)
						} else if !strings.HasSuffix(tag, "/") {
							htmlCodeDepth++
						}
						b.WriteString(s[i : end+1])
						i = end + 1
						continue
					}
				}
			}
		}

		protected := inFence || inInlineCode || htmlCodeDepth > 0
		standaloneSlash := i == 0 || s[i-1] != '\\'
		if !protected && standaloneSlash && s[i] == '\\' {
			switch {
			case strings.HasPrefix(s[i:], `\r\n`):
				b.WriteByte('\n')
				escapedBreaks++
				i += len(`\r\n`)
				continue
			case strings.HasPrefix(s[i:], `\n`), strings.HasPrefix(s[i:], `\r`):
				b.WriteByte('\n')
				escapedBreaks++
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	if escapedBreaks < 2 {
		return s
	}
	return b.String()
}

// SanitizeVisibleReply removes reasoning and internal presentation protocols
// from text that is about to be shown to end users. Authorization belongs at
// the tool/data boundary; stable business IDs and explicitly requested channel
// IDs must not be regex-rewritten after an authorized tool returned them.
func SanitizeVisibleReply(s string) string {
	s = StripReasoning(s)
	s = StripHistoryMetadata(s)
	s = toolOnlySectionRe.ReplaceAllString(s, "$1")
	s = trailingToolOnlyRe.ReplaceAllString(s, "$1")
	s = internalMarkerRe.ReplaceAllString(s, "")
	s = NormalizeEscapedLineBreaks(s)
	return strings.TrimSpace(s)
}

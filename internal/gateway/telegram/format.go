package telegram

// Markdown → Telegram HTML 兜底转换。
// 系统提示要求模型输出 Telegram HTML 子集，但本地模型（DeepSeek 等）服从度有限，
// 常冒出 Markdown（**加粗**、表格、# 标题）。发送前做保守转换：
// 合法 HTML 标签会保留，其他内容按 Markdown-ish 文本转成 TG HTML。

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

var (
	tgAllowedHTMLTagRe = regexp.MustCompile(`(?i)(</?(b|strong|i|em|u|s|del|code|pre|blockquote)>|</a>|<a\s+href="https?://[^"<>\s]+">)`)

	fencedCodeRe = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n?(.*?)```")
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	boldRe       = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	headerRe     = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*$`)
	bulletRe     = regexp.MustCompile(`(?m)^([ \t]*)[-*][ \t]+`)
	linkRe       = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	tableSepRe   = regexp.MustCompile(`^[|\s:\-]+$`)
)

// toTelegramHTML 把一条待发送文本整理成 Telegram HTML。
func toTelegramHTML(s string) string {
	var earlyStash []string // HTML 标签：转义后先还原，让 Telegram 标签生效。
	earlyPut := func(rendered string) string {
		earlyStash = append(earlyStash, rendered)
		return fmt.Sprintf("\x01%d\x01", len(earlyStash)-1)
	}
	var lateStash []string // 代码块/表格：最后还原，避免内部 Markdown 被误转。
	latePut := func(rendered string) string {
		lateStash = append(lateStash, rendered)
		return fmt.Sprintf("\x02%d\x02", len(lateStash)-1)
	}

	// 先摘出代码块/行内代码，避免内部内容被后续规则误伤。
	s = fencedCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		body := fencedCodeRe.FindStringSubmatch(m)[1]
		return latePut("<pre>" + html.EscapeString(strings.TrimRight(body, "\n")) + "</pre>")
	})
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		body := inlineCodeRe.FindStringSubmatch(m)[1]
		return latePut("<code>" + html.EscapeString(body) + "</code>")
	})

	// 保留 Telegram 支持的 HTML 标签，其他尖括号统一转义。
	s = tgAllowedHTMLTagRe.ReplaceAllStringFunc(s, earlyPut)
	esc := html.EscapeString(s)
	for i, r := range earlyStash {
		esc = strings.Replace(esc, fmt.Sprintf("\x01%d\x01", i), r, 1)
	}

	esc = convertTables(esc, latePut)
	esc = boldRe.ReplaceAllString(esc, "<b>$1</b>")
	esc = headerRe.ReplaceAllString(esc, "<b>$1</b>")
	esc = bulletRe.ReplaceAllString(esc, "$1• ")
	esc = linkRe.ReplaceAllString(esc, `<a href="$2">$1</a>`)

	for i, r := range lateStash {
		esc = strings.Replace(esc, fmt.Sprintf("\x02%d\x02", i), r, 1)
	}
	return esc
}

// convertTables 把连续的 Markdown 表格行（|…|）转成 <pre> 等宽块（去掉分隔行），
// 聊天窗里表格没有好的呈现，等宽对齐是最不难看的退路。
func convertTables(s string, stashPut func(string) string) string {
	lines := strings.Split(s, "\n")
	var out []string
	var table []string
	flush := func() {
		if len(table) == 0 {
			return
		}
		out = append(out, stashPut("<pre>"+strings.Join(table, "\n")+"</pre>"))
		table = nil
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 2 {
			if !tableSepRe.MatchString(t) { // 丢掉 |---|---| 分隔行
				table = append(table, tidyTableRow(t))
			}
			continue
		}
		flush()
		out = append(out, line)
	}
	flush()
	return strings.Join(out, "\n")
}

// tidyTableRow 把 "| a | b |" 收拾成 "a  b"。
func tidyTableRow(row string) string {
	cells := strings.Split(strings.Trim(row, "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return strings.Join(cells, "  ")
}

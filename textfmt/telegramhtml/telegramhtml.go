package telegramhtml

// Markdown -> Telegram HTML fallback conversion.
// Models are instructed to output Telegram's HTML subset, but local or
// OpenAI-compatible models may still emit Markdown. This converter keeps
// supported Telegram HTML tags and conservatively normalizes common Markdown.

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

var (
	tgAllowedHTMLTagRe           = regexp.MustCompile(`(?i)(</?(b|strong|i|em|u|s|del|code|pre|blockquote)>|</a>|<a\s+href="https?://[^"<>\s]+">)`)
	unsupportedPresentationTagRe = regexp.MustCompile(`(?is)</?(?:font|span|div|p|hr|br|h[1-6]|ul|ol|li|section|article|header|footer|main|details|summary)\b[^<>]*>`)
	malformedCloseRe             = regexp.MustCompile(`(?i)</\s*(b|strong|i|em|u|s|del|code|pre|blockquote)\s*([:：，,。；;、])`)

	fencedCodeRe = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n?(.*?)```")
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	boldRe       = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	headerRe     = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*$`)
	bulletRe     = regexp.MustCompile(`(?m)^([ \t]*)[-*][ \t]+`)
	linkRe       = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	tableSepRe   = regexp.MustCompile(`^[|\s:\-]+$`)
	htmlTableRe  = regexp.MustCompile(`(?is)<table\b[^>]*>.*?</table>`)
	htmlRowRe    = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr>`)
	htmlCellRe   = regexp.MustCompile(`(?is)<t[dh]\b[^>]*>(.*?)</t[dh]>`)
	stripTagRe   = regexp.MustCompile(`(?is)<[^>]+>`)
)

// ToHTML converts text into Telegram-compatible HTML.
func ToHTML(s string) string {
	s = repairMalformedClosingTags(s)

	var earlyStash []string // HTML tags restored after escaping.
	earlyPut := func(rendered string) string {
		earlyStash = append(earlyStash, rendered)
		return fmt.Sprintf("\x01%d\x01", len(earlyStash)-1)
	}
	var lateStash []string // Code blocks/tables restored last.
	latePut := func(rendered string) string {
		lateStash = append(lateStash, rendered)
		return fmt.Sprintf("\x02%d\x02", len(lateStash)-1)
	}

	s = fencedCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		body := fencedCodeRe.FindStringSubmatch(m)[1]
		return latePut("<pre>" + html.EscapeString(strings.TrimRight(body, "\n")) + "</pre>")
	})
	s = inlineCodeRe.ReplaceAllStringFunc(s, func(m string) string {
		body := inlineCodeRe.FindStringSubmatch(m)[1]
		return latePut("<code>" + html.EscapeString(body) + "</code>")
	})
	s = convertHTMLTables(s, latePut)

	s = tgAllowedHTMLTagRe.ReplaceAllStringFunc(s, earlyPut)
	// Telegram rejects presentation tags such as <font> and <hr>. Once the
	// supported subset has been stashed, remove unsupported tag syntax while
	// keeping its visible body instead of escaping raw markup into the message.
	s = unsupportedPresentationTagRe.ReplaceAllString(s, "")
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

func repairMalformedClosingTags(s string) string {
	return malformedCloseRe.ReplaceAllString(s, "</$1>")
}

func convertHTMLTables(s string, stashPut func(string) string) string {
	return htmlTableRe.ReplaceAllStringFunc(s, func(table string) string {
		lines := htmlTableLines(table)
		if len(lines) == 0 {
			return table
		}
		return stashPut("<pre>" + html.EscapeString(strings.Join(lines, "\n")) + "</pre>")
	})
}

func htmlTableLines(table string) []string {
	var lines []string
	for _, row := range htmlRowRe.FindAllStringSubmatch(table, -1) {
		if len(row) < 2 {
			continue
		}
		var cells []string
		for _, cell := range htmlCellRe.FindAllStringSubmatch(row[1], -1) {
			if len(cell) < 2 {
				continue
			}
			text := html.UnescapeString(stripTagRe.ReplaceAllString(cell[1], ""))
			text = strings.Join(strings.Fields(text), " ")
			if text != "" {
				cells = append(cells, text)
			}
		}
		if len(cells) > 0 {
			lines = append(lines, strings.Join(cells, "  "))
		}
	}
	return lines
}

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
			if !tableSepRe.MatchString(t) {
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

func tidyTableRow(row string) string {
	cells := strings.Split(strings.Trim(row, "|"), "|")
	for i := range cells {
		cell := strings.TrimSpace(cells[i])
		// A Markdown table is rendered as <pre>, where Telegram does not parse
		// nested Markdown or HTML presentation. Keep only the visible cell text
		// so markers such as **status** never leak to the user.
		cell = stripTagRe.ReplaceAllString(cell, "")
		cell = boldRe.ReplaceAllString(cell, "$1")
		cell = linkRe.ReplaceAllString(cell, "$1 ($2)")
		cells[i] = cell
	}
	return strings.Join(cells, "  ")
}

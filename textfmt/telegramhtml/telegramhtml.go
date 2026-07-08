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
	tgAllowedHTMLTagRe = regexp.MustCompile(`(?i)(</?(b|strong|i|em|u|s|del|code|pre|blockquote)>|</a>|<a\s+href="https?://[^"<>\s]+">)`)

	fencedCodeRe = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n?(.*?)```")
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	boldRe       = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	headerRe     = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*$`)
	bulletRe     = regexp.MustCompile(`(?m)^([ \t]*)[-*][ \t]+`)
	linkRe       = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	tableSepRe   = regexp.MustCompile(`^[|\s:\-]+$`)
)

// ToHTML converts text into Telegram-compatible HTML.
func ToHTML(s string) string {
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
		cells[i] = strings.TrimSpace(cells[i])
	}
	return strings.Join(cells, "  ")
}

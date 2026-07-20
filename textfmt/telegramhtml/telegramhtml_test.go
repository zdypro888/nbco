package telegramhtml

import (
	"strings"
	"testing"
)

func TestToHTMLRemovesUnsupportedTagsButKeepsContent(t *testing.T) {
	got := ToHTML(`<b>报告</b><font color="green">成功</font><hr><script>alert</script>`)
	if !strings.Contains(got, "<b>报告</b>") || !strings.Contains(got, "成功") || !strings.Contains(got, "&lt;script&gt;alert&lt;/script&gt;") {
		t.Fatalf("visible content or supported formatting was lost: %q", got)
	}
	for _, forbidden := range []string{"font", "<hr", "&lt;font", "&lt;hr"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Fatalf("unsupported tag leaked as visible markup %q: %q", forbidden, got)
		}
	}
}

func TestToHTMLDoesNotTreatComparisonsAsTags(t *testing.T) {
	got := ToHTML("2 < 3 && 5 > 4")
	if got != "2 &lt; 3 &amp;&amp; 5 &gt; 4" {
		t.Fatalf("comparison text changed unexpectedly: %q", got)
	}
}

package telegram

import (
	"strings"
	"testing"
)

func TestToTelegramHTMLPassthrough(t *testing.T) {
	in := "✅ <b>完成</b>，详情见 <code>nbco.log</code>"
	if got := toTelegramHTML(in); got != in {
		t.Errorf("已是 TG HTML 应原样放行:\n%q", got)
	}
}

func TestToTelegramHTMLPlainUnchanged(t *testing.T) {
	in := "你好，任务 #12 已经完成。\n• 第一条\n• 第二条"
	if got := toTelegramHTML(in); got != in {
		t.Errorf("普通中文文本不应被改动:\n%q", got)
	}
}

func TestToTelegramHTMLMarkdown(t *testing.T) {
	got := toTelegramHTML("# 标题\n\n**重点**内容，代码 `go build`：\n- 甲\n* 乙\n[官网](https://example.com)")
	for _, want := range []string{
		"<b>标题</b>", "<b>重点</b>", "<code>go build</code>",
		"• 甲", "• 乙", `<a href="https://example.com">官网</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "# ") {
		t.Errorf("Markdown 残留:\n%s", got)
	}
}

func TestToTelegramHTMLTable(t *testing.T) {
	got := toTelegramHTML("概览：\n| 项目 | 内容 |\n|------|------|\n| ID | 1 |\n| 名字 | PRO |\n完")
	if !strings.Contains(got, "<pre>") || strings.Contains(got, "|") {
		t.Errorf("表格应转 <pre> 且去掉竖线:\n%s", got)
	}
	if !strings.Contains(got, "ID  1") || !strings.Contains(got, "名字  PRO") {
		t.Errorf("表格内容丢失:\n%s", got)
	}
	if !strings.Contains(got, "概览：") || !strings.Contains(got, "完") {
		t.Errorf("表格外文本丢失:\n%s", got)
	}
}

func TestToTelegramHTMLCodeBlockProtected(t *testing.T) {
	got := toTelegramHTML("```\n**这里不是加粗** | 也不是表格 |\n```")
	if !strings.Contains(got, "<pre>**这里不是加粗** | 也不是表格 |</pre>") {
		t.Errorf("代码块内部不应被转换:\n%s", got)
	}
}

func TestToTelegramHTMLEscapes(t *testing.T) {
	got := toTelegramHTML("对比：a < b && c > d")
	if !strings.Contains(got, "a &lt; b &amp;&amp; c &gt; d") {
		t.Errorf("特殊字符应转义:\n%s", got)
	}
}

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

func TestToTelegramHTMLRestoresDoubleEscapedLayout(t *testing.T) {
	in := `<b>标题</b>\n\n• 第一项\n• 第二项`
	want := "<b>标题</b>\n\n• 第一项\n• 第二项"
	if got := toTelegramHTML(in); got != want {
		t.Fatalf("double-escaped layout = %q, want %q", got, want)
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

func TestToTelegramHTMLRawHTMLTable(t *testing.T) {
	got := toTelegramHTML("名单：\n<table>\n<tr><th>ID</th><th>姓名</th><th>状态</th></tr>\n<tr><td>#3</td><td>黄桑</td><td>active</td></tr>\n</table>\n完")
	for _, bad := range []string{"<table", "</table", "<tr", "<td", "&lt;table", "&lt;td"} {
		if strings.Contains(got, bad) {
			t.Fatalf("raw HTML table 不应裸露标签 %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"<pre>ID  姓名  状态\n#3  黄桑  active</pre>", "名单：", "完"} {
		if !strings.Contains(got, want) {
			t.Fatalf("raw HTML table 转换缺 %q:\n%s", want, got)
		}
	}
}

func TestToTelegramHTMLMixedHTMLAndMarkdown(t *testing.T) {
	got := toTelegramHTML("<b>概览</b>\n\n**重点**\n| 项目 | 内容 |\n|---|---|\n| ID | 1 |\n<script>x</script>")
	for _, want := range []string{"<b>概览</b>", "<b>重点</b>", "<pre>项目  内容\nID  1</pre>", "&lt;script&gt;x&lt;/script&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "|---|") {
		t.Errorf("混合格式仍有 Markdown 残留:\n%s", got)
	}
}

func TestToTelegramHTMLRepairsMalformedClosingTags(t *testing.T) {
	got := toTelegramHTML("<b>权限管控：</b：设置谁有权修改资料。\n<b>立规矩：</b：定义行为准则。")
	for _, bad := range []string{"</b：", "：："} {
		if strings.Contains(got, bad) {
			t.Fatalf("坏闭合标签未清理 %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"<b>权限管控：</b>设置", "<b>立规矩：</b>定义"} {
		if !strings.Contains(got, want) {
			t.Fatalf("修复结果缺 %q:\n%s", want, got)
		}
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

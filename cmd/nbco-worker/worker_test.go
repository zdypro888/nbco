package main

import (
	"strings"
	"testing"
)

func TestParseTail(t *testing.T) {
	out := "干活的一堆输出……\n" +
		markSummary + "\n新建了登录页，本地验证通过\n" +
		markLessons + "\n登录页要覆盖空态和错误态\n" +
		markEnd + "\n"
	summary, lessons := parseTail(out)
	if summary != "新建了登录页，本地验证通过" {
		t.Errorf("summary = %q", summary)
	}
	if lessons != "登录页要覆盖空态和错误态" {
		t.Errorf("lessons = %q", lessons)
	}
}

func TestParseTailNoLessons(t *testing.T) {
	out := markSummary + "\n改完了\n" + markLessons + "\n无\n" + markEnd
	summary, lessons := parseTail(out)
	if summary != "改完了" || lessons != "" {
		t.Errorf("summary=%q lessons=%q（无/None 应归空）", summary, lessons)
	}
}

func TestParseTailMissing(t *testing.T) {
	if s, l := parseTail("完全没有哨兵的输出"); s != "" || l != "" {
		t.Errorf("无哨兵应返回空: %q %q", s, l)
	}
}

func TestParseTailUsesLastMarkers(t *testing.T) {
	// CLI 可能在解释 prompt 时先复述了标记，只认最后一组。
	out := "我会在最后输出 " + markSummary + " 这样的标记\n" +
		"……真正干活……\n" +
		markSummary + "\n真结论\n" + markLessons + "\n真经验\n" + markEnd
	summary, _ := parseTail(out)
	if summary != "真结论" {
		t.Errorf("应取最后一组标记, got %q", summary)
	}
}

func TestStripANSI(t *testing.T) {
	in := "\x1b[32m✅ 完成\x1b[0m\r\n\x1b[1;34m下一步\x1b[m"
	got := stripANSI(in)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\r") {
		t.Errorf("残留转义: %q", got)
	}
	if !strings.Contains(got, "✅ 完成") || !strings.Contains(got, "下一步") {
		t.Errorf("正文被误删: %q", got)
	}
}

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt(
		&Task{Title: "写登录页", Goal: "让用户能登录", Description: "实现表单", Acceptance: "能提交"},
		[]string{"经验A：先看规范"},
	)
	for _, want := range []string{"写登录页", "让用户能登录", "实现表单", "能提交", "经验A：先看规范", markSummary, markEnd} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺 %q", want)
		}
	}
}

func TestCliArgs(t *testing.T) {
	claude := (&Worker{cfg: Config{Engine: "claude"}}).cliArgs("干活")
	if claude[0] != "-p" || claude[1] != "干活" {
		t.Errorf("claude args = %v", claude)
	}
	codex := (&Worker{cfg: Config{Engine: "codex"}}).cliArgs("干活")
	if codex[0] != "exec" || codex[len(codex)-1] != "干活" {
		t.Errorf("codex args = %v", codex)
	}
}

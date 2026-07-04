package telegram

import (
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestMessageText(t *testing.T) {
	if got := messageText(&models.Message{Text: "  你好  "}); got != "你好" {
		t.Errorf("纯文本 = %q", got)
	}
	if got := messageText(&models.Message{}); got != "" {
		t.Errorf("空消息 = %q", got)
	}
	got := messageText(&models.Message{
		Document: &models.Document{FileID: "doc1", FileName: "报告.pdf"},
		Caption:  "上周的报告",
	})
	if !strings.Contains(got, "报告.pdf") || !strings.Contains(got, "file_id=doc1") || !strings.Contains(got, "上周的报告") {
		t.Errorf("文件消息 = %q", got)
	}
	got = messageText(&models.Message{
		Photo: []models.PhotoSize{{FileID: "small"}, {FileID: "big"}},
	})
	if !strings.Contains(got, "file_id=big") || strings.Contains(got, "small") {
		t.Errorf("图片应取最大尺寸: %q", got)
	}
	got = messageText(&models.Message{Voice: &models.Voice{FileID: "v1"}})
	if !strings.Contains(got, "语音") {
		t.Errorf("语音消息 = %q", got)
	}
}

func TestSplitChunksShort(t *testing.T) {
	chunks := splitChunks("短消息", 10)
	if len(chunks) != 1 || chunks[0] != "短消息" {
		t.Errorf("chunks = %v", chunks)
	}
}

func TestSplitChunksByLine(t *testing.T) {
	// 每行 5 字符，限 12：两行一片。
	text := strings.TrimSuffix(strings.Repeat("aaaaa\n", 5), "\n")
	chunks := splitChunks(text, 12)
	if len(chunks) != 3 {
		t.Fatalf("片数 = %d, %v", len(chunks), chunks)
	}
	for i, c := range chunks {
		if len([]rune(c)) > 12 {
			t.Errorf("第 %d 片超限: %q", i, c)
		}
	}
	if strings.Join(chunks, "\n") != text {
		t.Errorf("拼回不等于原文: %v", chunks)
	}
}

func TestSplitChunksLongLine(t *testing.T) {
	// 单行超限：硬切，多字节字符不能被劈开。
	text := strings.Repeat("汉", 25)
	chunks := splitChunks(text, 10)
	if len(chunks) != 3 {
		t.Fatalf("片数 = %d", len(chunks))
	}
	if strings.Join(chunks, "") != text {
		t.Errorf("拼回不等于原文")
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 10 {
			t.Errorf("第 %d 片 %d 字超限", i, n)
		}
	}
}

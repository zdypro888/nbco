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

func TestCommandOf(t *testing.T) {
	const me = "nbi_jp_bot"
	cases := map[string]string{
		"/listen":            "/listen",
		"/listen@nbi_jp_bot": "/listen",
		"/listen@NBI_JP_BOT": "/listen", // 大小写不敏感
		"/new@nbi_jp_bot 参数": "/new",
		"/new@other_bot":     "", // 发给别的 bot，忽略
		"/listen@other_bot":  "",
		"  /start  ":         "/start",
		"你好 /listen":         "",
		"listen":             "",
		"":                   "",
	}
	for in, want := range cases {
		if got := commandOf(in, me); got != want {
			t.Errorf("commandOf(%q) = %q, want %q", in, got, want)
		}
	}
	// botUsername 未知时保守：只认裸命令，@后缀一律忽略。
	if commandOf("/new", "") != "/new" || commandOf("/new@nbi_jp_bot", "") != "" {
		t.Error("botUsername 未知时应只认裸命令")
	}
}

func TestHasMention(t *testing.T) {
	const me = "nbi_jp_bot"
	yes := []string{"@nbi_jp_bot 帮我看看", "查一下 @NBI_JP_BOT", "开头 @nbi_jp_bot", "尾部说完 @nbi_jp_bot"}
	no := []string{"交给 @nbi_jp_bot_dev 跟进", "@nbi_jp_botx", "没有提及"}
	for _, s := range yes {
		if !hasMention(s, me) {
			t.Errorf("hasMention(%q) 应为 true", s)
		}
	}
	for _, s := range no {
		if hasMention(s, me) {
			t.Errorf("hasMention(%q) 应为 false", s)
		}
	}
}

func TestStripMention(t *testing.T) {
	const me = "nbi_jp_bot"
	if got := stripMention("@nbi_jp_bot 帮我看看进度", me); strings.TrimSpace(got) != "帮我看看进度" {
		t.Errorf("stripMention 结果 = %q", got)
	}
	if got := stripMention("@nbi_jp_bot 转告 @someone_bot", me); !strings.Contains(got, "@someone_bot") {
		t.Errorf("应保留他人句柄: %q", got)
	}
	if got := stripMention("交给 @nbi_jp_bot_dev", me); got != "交给 @nbi_jp_bot_dev" {
		t.Errorf("不应剥相似前缀: %q", got)
	}
}

func TestGroupChannelAndListenKey(t *testing.T) {
	if groupChannel(-100123) != "telegram:group:-100123" {
		t.Errorf("groupChannel = %q", groupChannel(-100123))
	}
	if listenKey(-100123) != "tg_listen:-100123" {
		t.Errorf("listenKey = %q", listenKey(-100123))
	}
}

func TestOnboardingMessages(t *testing.T) {
	help := unboundHelpMessage(false)
	for _, want := range []string{"欢迎来到", "加入方式", "绑定 Key", "查任务", "设置提醒"} {
		if !strings.Contains(help, want) {
			t.Errorf("未绑定帮助缺少 %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "/superadmin") {
		t.Errorf("已有管理员时不应提示 /superadmin:\n%s", help)
	}

	bootstrap := unboundHelpMessage(true)
	if !strings.Contains(bootstrap, "/superadmin") {
		t.Errorf("全新系统应提示 /superadmin:\n%s", bootstrap)
	}

	success := bindSuccessMessage("PRO")
	for _, bad := range []string{"ID ", "用户ID", "TG ID", "Telegram ID"} {
		if strings.Contains(success, bad) {
			t.Errorf("绑定成功文案不应暴露内部身份 %q:\n%s", bad, success)
		}
	}
	if !strings.Contains(success, "欢迎加入") || !strings.Contains(success, "自我介绍") {
		t.Errorf("绑定成功文案信息不足:\n%s", success)
	}
}

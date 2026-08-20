package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/zdypro888/nbco/store"
)

func TestGatewayFormatsTimeInBusinessTimezone(t *testing.T) {
	g := &Gateway{tz: time.FixedZone("CST", 8*60*60)}
	utc := time.Date(2026, 7, 9, 17, 30, 0, 0, time.UTC)
	if got := g.formatTime(utc); got != "2026-07-10 01:30:00 +08:00 (CST)" {
		t.Fatalf("formatted time = %q", got)
	}
}

func TestGatewayTelegramHandshakeRetriesUntilAPIReady(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123:secret/getMe" {
			http.NotFound(w, r)
			return
		}
		attempts++
		if attempts < 3 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"nbco","username":"nbco_bot","can_read_all_group_messages":true}}`)
	}))
	defer srv.Close()

	g, err := New("123:secret", srv.URL, nil, nil, nil, nil, "", "", "", nil, "", "", "Oncoin", time.UTC)
	if err != nil {
		t.Fatalf("New must not depend on API readiness: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !g.waitUntilReady(ctx) || !g.Ready() {
		t.Fatal("gateway did not become ready")
	}
	if attempts != 3 {
		t.Fatalf("getMe attempts = %d, want 3", attempts)
	}
}

func TestOpenTelegramFileSupportsLocalBotAPIAbsolutePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(path, []byte("voice-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{telegramAPIURL: "http://127.0.0.1:8081"}
	r, _, err := g.openTelegramFile(context.Background(), &models.File{FilePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "voice-data" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestMessageText(t *testing.T) {
	g := &Gateway{}
	ctx := context.Background()
	if got := g.messageText(ctx, &models.Message{Text: "  你好  "}); got != "你好" {
		t.Errorf("纯文本 = %q", got)
	}
	if got := g.messageText(ctx, &models.Message{}); got != "" {
		t.Errorf("空消息 = %q", got)
	}
	got := g.messageText(ctx, &models.Message{
		Document: &models.Document{FileID: "doc1", FileName: "报告.pdf"},
		Caption:  "上周的报告",
	})
	if !strings.Contains(got, "报告.pdf") || strings.Contains(got, "doc1") || !strings.Contains(got, "上周的报告") {
		t.Errorf("文件消息 = %q", got)
	}
	got = g.messageText(ctx, &models.Message{
		Photo: []models.PhotoSize{{FileID: "small"}, {FileID: "big"}},
	})
	if got != "[用户发来图片]" || strings.Contains(got, "big") || strings.Contains(got, "small") {
		t.Errorf("图片应取最大尺寸: %q", got)
	}
	got = g.messageText(ctx, &models.Message{Voice: &models.Voice{FileID: "voice-secret"}})
	if strings.Contains(got, "voice-secret") || !strings.Contains(got, "未配置转写服务") {
		t.Errorf("语音占位不应暴露 Telegram file_id: %q", got)
	}
}

func TestTelegramMemorySourceExcludesSyntheticFileContext(t *testing.T) {
	msg := &models.Message{
		Document: &models.Document{FileID: "private", FileName: "report.pdf"},
		Caption:  "分析这份报告",
	}
	augmented := telegramFileContext([]store.File{{ID: 7, OriginalName: "report.pdf"}}, nil, msg.Caption)
	got := telegramMemorySourceText(msg, augmented)
	if got != msg.Caption || strings.Contains(got, "file_id") || strings.Contains(got, "analyze_company_materials") {
		t.Fatalf("memory source = %q", got)
	}
}

func TestTelegramMessageEnvelopeKeepsStableActorAndSourceIdentity(t *testing.T) {
	msg := &models.Message{
		ID: 42, Date: 1_787_116_200,
		Chat:           models.Chat{ID: -10088, Type: models.ChatTypeSupergroup, Title: "项目群"},
		From:           &models.User{ID: 991, FirstName: "Display"},
		ReplyToMessage: &models.Message{ID: 41}, MessageThreadID: 7,
		MediaGroupID: "batch-a", EditDate: 1_787_116_260,
	}
	u := &store.User{ID: 12, Name: "稳定员工"}
	envelope := telegramMessageEnvelope(msg, u)
	if envelope.Provider != Provider || envelope.ExternalChatRef != "-10088" ||
		envelope.ExternalMessageRef != "42" || envelope.ExternalActorRef != "991" ||
		envelope.ActorUserID == nil || *envelope.ActorUserID != u.ID ||
		envelope.ActorDisplayName != u.Name || envelope.ReplyToExternalRef != "41" ||
		envelope.ThreadRef != "7" || envelope.SourceCreatedAt == nil {
		t.Fatalf("envelope = %+v", envelope)
	}
	if !strings.Contains(string(envelope.Metadata), "batch-a") {
		t.Fatalf("metadata = %s", envelope.Metadata)
	}
}

func TestDisplayNameFromMessageWithoutFrom(t *testing.T) {
	got := displayNameFromMessage(&models.Message{
		Chat:       models.Chat{ID: -1, Type: models.ChatTypeSupergroup, Title: "群"},
		SenderChat: &models.Chat{ID: -2, Type: models.ChatTypeChannel, Title: "频道身份"},
	})
	if got != "频道身份" {
		t.Fatalf("displayNameFromMessage = %q", got)
	}
	if got := displayNameFromMessage(&models.Message{}); got != "匿名成员" {
		t.Fatalf("missing sender display = %q", got)
	}
}

func TestGroupMonitorEvaluationDelay(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if got := groupMonitorEvaluationDelay(time.Time{}, now); got != groupMonitorDebounce {
		t.Fatalf("zero batch delay = %v", got)
	}
	if got := groupMonitorEvaluationDelay(now.Add(-time.Minute), now); got != groupMonitorDebounce {
		t.Fatalf("normal debounce = %v", got)
	}
	if got := groupMonitorEvaluationDelay(now.Add(-groupMonitorMaxWait+5*time.Second), now); got != 5*time.Second {
		t.Fatalf("max wait cap = %v", got)
	}
	if got := groupMonitorEvaluationDelay(now.Add(-groupMonitorMaxWait-time.Second), now); got != 0 {
		t.Fatalf("overdue batch should flush immediately, got %v", got)
	}
}

func TestGroupMonitorRetryDelay(t *testing.T) {
	if got := groupMonitorRetryDelay(1); got != groupMonitorDebounce {
		t.Fatalf("first retry = %v", got)
	}
	if got := groupMonitorRetryDelay(4); got != 8*groupMonitorDebounce {
		t.Fatalf("fourth retry = %v", got)
	}
	if got := groupMonitorRetryDelay(99); got != 30*time.Minute {
		t.Fatalf("retry cap = %v", got)
	}
}

func TestGroupMonitorLeaseHonorsOtherLiveInstance(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	mon := &store.TelegramGroupMonitor{
		AnalysisOwner: "another-instance", AnalysisStartedAt: now.Add(-time.Minute), AnalysisThrough: now,
	}
	if !groupMonitorLeaseAlive(mon, now) {
		t.Fatal("a fresh lease owned by another instance must remain authoritative")
	}
	mon.AnalysisStartedAt = now.Add(-groupMonitorAnalysisLease)
	if groupMonitorLeaseAlive(mon, now) {
		t.Fatal("expired lease must be reclaimable")
	}
}

func TestShouldDebouncePlainTextOnly(t *testing.T) {
	g := &Gateway{}
	if !g.shouldDebounce(&models.Message{Text: "第一句"}, "第一句") {
		t.Fatal("plain text should debounce")
	}
	if g.shouldDebounce(&models.Message{Text: "/listen"}, "/listen") {
		t.Fatal("commands should not debounce")
	}
	if g.shouldDebounce(&models.Message{Document: &models.Document{FileID: "f1", FileName: "a.txt"}}, "文件 a.txt") {
		t.Fatal("structured messages should not debounce")
	}
}

func TestImmediateMessageQueuesAfterPendingText(t *testing.T) {
	const key int64 = 42
	g := &Gateway{
		pending: map[int64]*pendingTextMessage{}, dispatchTails: map[int64]chan struct{}{},
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	g.pending[key] = &pendingTextMessage{
		ctx: context.Background(), lockKey: key, msg: &models.Message{Text: "first"},
		texts: []string{"first", "second"}, timer: timer,
	}
	queued := g.queueMessageAfterPendingLocked(context.Background(), key, false, &models.Message{Text: "/new"})
	if len(queued) != 2 {
		t.Fatalf("queued messages = %d", len(queued))
	}
	if queued[0].msg.Text != "first\nsecond" || queued[1].msg.Text != "/new" {
		t.Fatalf("queue order = %q then %q", queued[0].msg.Text, queued[1].msg.Text)
	}
	if queued[1].prev != queued[0].done {
		t.Fatal("immediate message is not chained behind pending text")
	}
	if g.pending[key] != nil {
		t.Fatal("pending text was not atomically detached")
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

func TestSplitChunksAvoidsHTMLTagAndEntityCuts(t *testing.T) {
	htmlText := strings.Repeat("甲", 8) + "<b>bold</b>" + strings.Repeat("乙", 8) + "&amp;" + strings.Repeat("丙", 8)
	chunks := splitChunks(htmlText, 20)
	for _, c := range chunks {
		if strings.Count(c, "<") != strings.Count(c, ">") {
			t.Fatalf("chunk cuts HTML tag: %#v", chunks)
		}
		if strings.Contains(c, "&am") && !strings.Contains(c, "&amp;") {
			t.Fatalf("chunk cuts entity: %#v", chunks)
		}
	}
	if strings.Join(chunks, "") != htmlText {
		t.Fatalf("chunks do not rejoin original: %#v", chunks)
	}
}

func TestSplitChunksBalancesOpenHTMLTags(t *testing.T) {
	htmlText := "<b>" + strings.Repeat("甲", 25) + "</b>"
	chunks := splitChunks(htmlText, 10)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks: %#v", chunks)
	}
	for _, c := range chunks {
		if strings.Count(c, "<b>") != strings.Count(c, "</b>") {
			t.Fatalf("chunk has unbalanced bold tag: %#v", chunks)
		}
	}
	visible := htmlTagTokenRe.ReplaceAllString(strings.Join(chunks, ""), "")
	if visible != strings.Repeat("甲", 25) {
		t.Fatalf("visible text changed: %q chunks=%#v", visible, chunks)
	}
}

func TestTelegramPlainTextCleansMalformedHTML(t *testing.T) {
	got := telegramPlainText("<b>权限管控：</b：设置谁有权修改资料。<table><tr><td>ID</td></tr></table>")
	for _, bad := range []string{"<b>", "</b", "<table", "：："} {
		if strings.Contains(got, bad) {
			t.Fatalf("纯文本兜底仍有坏格式 %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"权限管控：设置", "ID"} {
		if !strings.Contains(got, want) {
			t.Fatalf("纯文本兜底缺 %q:\n%s", want, got)
		}
	}
}

type sendFallbackHTTP struct {
	calls      int
	texts      []string
	parseModes []string
}

func (h *sendFallbackHTTP) Do(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/sendMessage") {
		return streamLoopResp("true"), nil
	}
	h.calls++
	if err := req.ParseMultipartForm(1 << 20); err != nil {
		return nil, err
	}
	h.texts = append(h.texts, req.FormValue("text"))
	h.parseModes = append(h.parseModes, req.FormValue("parse_mode"))
	if h.calls == 1 {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`)),
			Header:     http.Header{},
		}, nil
	}
	return streamLoopResp(`{"message_id":42,"date":0,"chat":{"id":1,"type":"private"}}`), nil
}

func TestSendOneFallbackDoesNotExposeHTMLTags(t *testing.T) {
	h := &sendFallbackHTTP{}
	b, err := bot.New("TESTTOKEN", bot.WithHTTPClient(time.Second, h), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	g := &Gateway{bot: b}
	if err := g.sendOne(context.Background(), 1, "<b>权限管控：</b：设置谁有权修改资料。"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 2 {
		t.Fatalf("sendMessage calls = %d, want 2", h.calls)
	}
	if h.parseModes[0] == "" {
		t.Fatalf("首次发送应带 HTML parse_mode: %#v", h.parseModes)
	}
	if h.parseModes[1] != "" {
		t.Fatalf("兜底发送不应带 parse_mode: %#v", h.parseModes)
	}
	if strings.Contains(h.texts[1], "<b>") || strings.Contains(h.texts[1], "</b") || strings.Contains(h.texts[1], "：：") {
		t.Fatalf("兜底文本不应暴露 HTML 标签或重复冒号:\n%q", h.texts[1])
	}
	if !strings.Contains(h.texts[1], "权限管控：设置") {
		t.Fatalf("兜底文本内容丢失:\n%q", h.texts[1])
	}
}

func TestCommandOf(t *testing.T) {
	const me = "example_bot"
	cases := map[string]string{
		"/listen":             "/listen",
		"/listen@example_bot": "/listen",
		"/listen@EXAMPLE_BOT": "/listen", // 大小写不敏感
		"/new@example_bot 参数": "/new",
		"/start abc":          "/start",
		"/new@other_bot":      "", // 发给别的 bot，忽略
		"/listen@other_bot":   "",
		"  /start  ":          "/start",
		"你好 /listen":          "",
		"listen":              "",
		"":                    "",
	}
	for in, want := range cases {
		if got := commandOf(in, me); got != want {
			t.Errorf("commandOf(%q) = %q, want %q", in, got, want)
		}
	}
	// botUsername 未知时保守：只认裸命令，@后缀一律忽略。
	if commandOf("/new", "") != "/new" || commandOf("/new@example_bot", "") != "" {
		t.Error("botUsername 未知时应只认裸命令")
	}
}

func TestCommandArgs(t *testing.T) {
	cases := map[string]string{
		"/model mlx-community/DeepSeek-V4-Flash":          "mlx-community/DeepSeek-V4-Flash",
		"  /model@example_bot   gpt-4.1-mini  ":           "gpt-4.1-mini",
		"/model":                                          "",
		"/model    ":                                      "",
		"/model mlx-community/DeepSeek-V4-Flash trailing": "mlx-community/DeepSeek-V4-Flash trailing",
	}
	for in, want := range cases {
		if got := commandArgs(in); got != want {
			t.Errorf("commandArgs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidModelName(t *testing.T) {
	valid := []string{"mlx-community/DeepSeek-V4-Flash", "gpt-4.1-mini", "claude-sonnet-4-20250514"}
	for _, m := range valid {
		if !validModelName(m) {
			t.Errorf("validModelName(%q) = false", m)
		}
	}
	invalid := []string{"", "two words", strings.Repeat("x", 161), "bad<model>", "bad&model"}
	for _, m := range invalid {
		if validModelName(m) {
			t.Errorf("validModelName(%q) = true", m)
		}
	}
}

func TestLoadedModelsUsesOllamaCompatiblePS(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ollama/api/ps" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"models":[{"name":"a","model":"mlx-community/DeepSeek-V4-Flash"},{"name":"fallback"},{"model":"mlx-community/Qwen3.6-35B-A3B-8bit"},{"model":"mlx-community/DeepSeek-V4-Flash"}]}`)
	}))
	defer srv.Close()
	g := &Gateway{modelBaseURL: srv.URL + "/v1", modelAPIKey: "secret"}
	got, err := g.loadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	want := []string{"mlx-community/DeepSeek-V4-Flash", "fallback", "mlx-community/Qwen3.6-35B-A3B-8bit"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("loaded models = %#v, want %#v", got, want)
	}
}

func TestLoadedModelsFallsBackToOpenAICatalog(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case "/ollama/api/ps":
			http.NotFound(w, r)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"gpt-5.6-terra"},{"id":"gpt-5.6-luna"},{"id":"gpt-5.6-luna"},{"id":"bad model"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	g := &Gateway{modelBaseURL: srv.URL + "/v1", modelAPIKey: "secret"}
	got, err := g.loadedModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/ollama/api/ps,/v1/models" {
		t.Fatalf("paths = %#v", paths)
	}
	want := []string{"gpt-5.6-luna", "gpt-5.6-terra"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("available models = %#v, want %#v", got, want)
	}
}

func TestLoadedModelsHelpAndMembership(t *testing.T) {
	models := []string{"mlx-community/Qwen3.6-35B-A3B-8bit", "mlx-community/DeepSeek-V4-Flash"}
	if !modelInList("mlx-community/DeepSeek-V4-Flash", models) {
		t.Fatal("loaded model should match exactly")
	}
	if modelInList("mlx-community/deepseek-v4-flash", models) {
		t.Fatal("model match should be exact")
	}
	help := loadedModelsHelp(models)
	for _, want := range []string{"可选模型", "<code>mlx-community/Qwen3.6-35B-A3B-8bit</code>", "/model"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q: %s", want, help)
		}
	}
}

func TestInviteTokenFromText(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef"
	for _, in := range []string{
		key,
		strings.ToUpper(key),
		"/start " + key,
		"/start@example_bot " + key,
	} {
		got, ok := inviteTokenFromText(in, "example_bot")
		if !ok || got != key {
			t.Errorf("inviteTokenFromText(%q) = (%q,%v), want %q,true", in, got, ok, key)
		}
	}
	for _, in := range []string{"/start", "/start nope", "/start@other_bot " + key, "hello " + key} {
		if got, ok := inviteTokenFromText(in, "example_bot"); ok {
			t.Errorf("inviteTokenFromText(%q) = (%q,true), want false", in, got)
		}
	}
}

func TestHasMention(t *testing.T) {
	const me = "example_bot"
	yes := []string{"@example_bot 帮我看看", "查一下 @EXAMPLE_BOT", "开头 @example_bot", "尾部说完 @example_bot"}
	no := []string{"交给 @example_bot_dev 跟进", "@example_botx", "没有提及"}
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

func TestHasTextMention(t *testing.T) {
	entities := []models.MessageEntity{
		{Type: models.MessageEntityTypeTextMention, Offset: 0, Length: 2, User: &models.User{ID: 42}},
	}
	if !hasTextMention(entities, 42) {
		t.Fatal("text_mention should match bot id")
	}
	if hasTextMention(entities, 7) {
		t.Fatal("text_mention should not match other user")
	}
}

func TestStripMention(t *testing.T) {
	const me = "example_bot"
	if got := stripMention("@example_bot 帮我看看进度", me); strings.TrimSpace(got) != "帮我看看进度" {
		t.Errorf("stripMention 结果 = %q", got)
	}
	if got := stripMention("@example_bot 转告 @someone_bot", me); !strings.Contains(got, "@someone_bot") {
		t.Errorf("应保留他人句柄: %q", got)
	}
	if got := stripMention("交给 @example_bot_dev", me); got != "交给 @example_bot_dev" {
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
	help := unboundHelpMessage("Oncoin", false)
	for _, want := range []string{"欢迎来到", "Oncoin", "加入方式", "一次性邀请链接", "邀请码", "查任务", "设置提醒"} {
		if !strings.Contains(help, want) {
			t.Errorf("未绑定帮助缺少 %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "/superadmin") {
		t.Errorf("已有管理员时不应提示 /superadmin:\n%s", help)
	}
	escaped := unboundHelpMessage(`OnCoin & <Ops>`, false)
	if !strings.Contains(escaped, `OnCoin &amp; &lt;Ops&gt;`) || strings.Contains(escaped, `<Ops>`) {
		t.Errorf("未绑定帮助没有安全转义实例品牌:\n%s", escaped)
	}

	bootstrap := unboundHelpMessage("Oncoin", true)
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

func TestTelegramCloudLargeFileFailsBeforeDownload(t *testing.T) {
	g := &Gateway{fileStorePath: t.TempDir()}
	_, err := g.saveTelegramFile(context.Background(), 1, incomingTelegramFile{
		fileID: "opaque", name: "large.zip", sizeBytes: telegramCloudFileDownloadLimit + 1,
	})
	var intakeErr *telegramFileIntakeError
	if !errors.As(err, &intakeErr) || intakeErr.code != "telegram_cloud_limit" {
		t.Fatalf("error = %#v, want telegram_cloud_limit", err)
	}
}

func TestTelegramFileIntakeHelpers(t *testing.T) {
	rawID := "reusable-telegram-file-id"
	ref := telegramFileExternalRef(42, "", rawID)
	if strings.Contains(ref, rawID) || !strings.HasPrefix(ref, "42:") {
		t.Fatalf("external ref must be stable without exposing file_id: %q", ref)
	}
	uniqueRef := telegramFileExternalRef(42, "unique", rawID)
	if uniqueRef != "42:unique" {
		t.Fatalf("unique ref = %q", uniqueRef)
	}
	if got := telegramMaterialSourceRef(&models.Message{ID: 42}); got != "42" {
		t.Fatalf("single-message material ref = %q", got)
	}
	if got := telegramMaterialSourceRef(&models.Message{ID: 42, MediaGroupID: "album-7"}); got != "media-group:album-7" {
		t.Fatalf("album material ref = %q", got)
	}

	savedFile := &store.File{ID: 9, OriginalName: "saved.pdf"}
	results := []incomingTelegramFileResult{
		{input: incomingTelegramFile{name: "saved.pdf"}, file: savedFile},
		{input: incomingTelegramFile{name: "large.zip", sizeBytes: telegramCloudFileDownloadLimit + 1}, errorCode: "telegram_cloud_limit", errorMessage: "too large"},
	}
	saved, failed := splitTelegramFileIntakes(results)
	if len(saved) != 1 || saved[0].ID != savedFile.ID || len(failed) != 1 {
		t.Fatalf("split result: saved=%+v failed=%+v", saved, failed)
	}
	prompt := failedFilesPrompt(failed)
	for _, want := range []string{"large.zip", "接收失败", "不得声称", "不要编造"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("failed prompt missing %q: %s", want, prompt)
		}
	}

	g := &Gateway{publicBaseURL: "https://im.app:8443"}
	if got := g.fileCenterURL(); got != "https://im.app:8443/?view=files" {
		t.Fatalf("file center URL = %q", got)
	}
	g.publicBaseURL = "http://127.0.0.1:8443"
	if got := g.fileCenterURL(); got != "" {
		t.Fatalf("insecure file center URL must not become a Telegram WebApp: %q", got)
	}
}

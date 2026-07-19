package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestParseTelegramGroupRef(t *testing.T) {
	for _, in := range []string{"telegram:group:-100123", "group:-100123", "-100123"} {
		id, ok := parseTelegramGroupRef(in)
		if !ok || id != -100123 {
			t.Fatalf("parseTelegramGroupRef(%q) = %d,%v", in, id, ok)
		}
	}
	if _, ok := parseTelegramGroupRef("示例公司群"); ok {
		t.Fatal("group title should not parse as group ref")
	}
}

func TestRenderTelegramGroupHidesChatIDLabel(t *testing.T) {
	got := renderTelegramGroup(store.TelegramGroupState{
		ChatID:    -100123,
		Title:     "示例公司群",
		Type:      "supergroup",
		Status:    "member",
		Listen:    true,
		UpdatedAt: time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC),
	}, time.UTC)
	for _, want := range []string{"示例公司群", "已加入", "监听开启", "group_ref"} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderTelegramGroup missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "chat_id") || strings.Contains(got, "TG ID") {
		t.Fatalf("renderTelegramGroup should not expose chat id labels:\n%s", got)
	}
}

func TestTelegramGroupMessageRangeAndRender(t *testing.T) {
	tz := time.FixedZone("CST", 8*60*60)
	from, to, msg := telegramGroupMessageRange("2020-07-10", 0, tz)
	if msg != "" || from.In(tz).Format("2006-01-02 15:04") != "2020-07-10 00:00" || to.Sub(from) != 24*time.Hour {
		t.Fatalf("date range = %v %v %q", from, to, msg)
	}
	today := time.Now().In(tz).Format("2006-01-02")
	from, to, msg = telegramGroupMessageRange(today, 0, tz)
	if msg != "" || to.Before(from) || time.Until(to) > time.Second {
		t.Fatalf("today range must end at now: %v %v %q", from, to, msg)
	}
	future := time.Now().In(tz).AddDate(0, 0, 1).Format("2006-01-02")
	if _, _, msg := telegramGroupMessageRange(future, 0, tz); !strings.Contains(msg, "不能晚于") {
		t.Fatalf("future date message = %q", msg)
	}
	if _, _, msg := telegramGroupMessageRange("bad", 0, tz); !strings.Contains(msg, "YYYY-MM-DD") {
		t.Fatalf("invalid date message = %q", msg)
	}
	page := store.ChannelMessagePage{
		Total: 3, NextCursor: 42,
		Messages: []store.ChatMessage{
			{Role: "user", Content: "【Alice】日报完成", CreatedAt: from.Add(10 * time.Hour)},
			{Role: "assistant", Content: "收到", CreatedAt: from.Add(10*time.Hour + time.Minute)},
		},
	}
	got := renderTelegramGroupMessages(store.TelegramGroupState{Title: "项目群"}, page, from, to, tz)
	for _, want := range []string{"项目群", "共 3 条", "仅最新部分", "【Alice】日报完成", "nbco：收到", "next_cursor: 42"} {
		if !strings.Contains(got, want) {
			t.Fatalf("group messages missing %q:\n%s", want, got)
		}
	}
}

func TestTelegramGroupDigestDirectiveUsesMessageFactTool(t *testing.T) {
	got := telegramGroupDigestDirective(store.TelegramGroupState{ChatID: -100123, Title: "项目群"}, "只看风险")
	for _, want := range []string{"list_telegram_group_messages", "telegram:group:-100123", "实际消息", "只看风险"} {
		if !strings.Contains(got, want) {
			t.Fatalf("digest directive missing %q: %s", want, got)
		}
	}
}

func TestTelegramGroupMessageAndDigestToolsIntegration(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	boss := mkToolsUser(t, s, "boss", true)
	group := store.TelegramGroupState{
		ChatID: -100123, Title: "项目群", Type: "supergroup", Status: "member", Listen: false, UpdatedAt: time.Now(),
	}
	if err := s.SaveTelegramGroupState(ctx, group); err != nil {
		t.Fatal(err)
	}
	session, err := s.StartGroupSession(ctx, boss.ID, telegramGroupChannel(group.ChatID), "eino")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, session.ID, "user", "【Alice】今天完成发布"); err != nil {
		t.Fatal(err)
	}

	byName := map[string]func(context.Context, json.RawMessage) (string, error){}
	for _, tool := range telegramGroupTools(Deps{Store: s, TZ: time.UTC}, boss) {
		byName[tool.Name] = tool.Handler
	}
	listOut, err := byName["list_telegram_group_messages"](ctx, json.RawMessage(`{"group":"项目群"}`))
	if err != nil || !strings.Contains(listOut, "今天完成发布") {
		t.Fatalf("list group messages = %q err=%v", listOut, err)
	}
	monitorOut, err := byName["set_telegram_group_monitor"](ctx, json.RawMessage(`{"group":"项目群","enabled":true,"instruction":"按管理价值判断"}`))
	if err != nil || !strings.Contains(monitorOut, "事件监控") || !strings.Contains(monitorOut, "每日摘要仍未开启") {
		t.Fatalf("set monitor = %q err=%v", monitorOut, err)
	}
	storedGroup, err := s.TelegramGroupState(ctx, group.ChatID)
	if err != nil || storedGroup.Listen {
		t.Fatalf("monitor must not change explicit listen setting: group=%+v err=%v", storedGroup, err)
	}

	setOut, err := byName["set_telegram_group_digest"](ctx, json.RawMessage(`{"group":"项目群","enabled":true,"daily_at":"18:30","instruction":"只看风险"}`))
	if err != nil || !strings.Contains(setOut, "每日摘要") || !strings.Contains(setOut, "18:30") {
		t.Fatalf("set digest = %q err=%v", setOut, err)
	}
	sc, err := s.AutomationSchedule(ctx, boss.ID, telegramGroupDigestSourceKind, "-100123")
	if err != nil || sc.SourceKind != telegramGroupDigestSourceKind || !strings.Contains(sc.Message, "list_telegram_group_messages") {
		t.Fatalf("digest schedule = %+v err=%v", sc, err)
	}
	if sc.Title != "项目群 每日摘要" {
		t.Fatalf("digest should have a user-facing title: %+v", sc)
	}
	storedGroup, err = s.TelegramGroupState(ctx, group.ChatID)
	if err != nil || storedGroup.Listen {
		t.Fatalf("digest must not change explicit listen setting: group=%+v err=%v", storedGroup, err)
	}
	firstID := sc.ID
	if _, err := byName["set_telegram_group_digest"](ctx, json.RawMessage(`{"group":"项目群","enabled":true,"daily_at":"19:00"}`)); err != nil {
		t.Fatal(err)
	}
	sc, err = s.AutomationSchedule(ctx, boss.ID, telegramGroupDigestSourceKind, "-100123")
	if err != nil || sc.ID != firstID || sc.DailyAt != "19:00" {
		t.Fatalf("digest update should be idempotent: %+v err=%v", sc, err)
	}
	getOut, err := byName["get_telegram_group"](ctx, json.RawMessage(`{"group":"项目群"}`))
	if err != nil || !strings.Contains(getOut, "每日摘要：已设置") || !strings.Contains(getOut, "群消息内容：本工具未查询") {
		t.Fatalf("get group digest status = %q err=%v", getOut, err)
	}
	if _, err := byName["set_telegram_group_digest"](ctx, json.RawMessage(`{"group":"项目群","enabled":false}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AutomationSchedule(ctx, boss.ID, telegramGroupDigestSourceKind, "-100123"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled digest should not remain active: %v", err)
	}
}

func TestParseTelegramMessageRef(t *testing.T) {
	chatID, messageID, ok := parseTelegramMessageRef("telegram:group:-100123:message:77")
	if !ok || chatID != -100123 || messageID != 77 {
		t.Fatalf("parseTelegramMessageRef full = %d,%d,%v", chatID, messageID, ok)
	}
	chatID, messageID, ok = parseTelegramMessageRef("message:88")
	if !ok || chatID != 0 || messageID != 88 {
		t.Fatalf("parseTelegramMessageRef short = %d,%d,%v", chatID, messageID, ok)
	}
	if _, _, ok := parseTelegramMessageRef("telegram:group:-100123"); ok {
		t.Fatal("group ref should not parse as message ref")
	}
}

func TestTelegramMemberDisplayAndStatus(t *testing.T) {
	got := telegramMemberDisplay(TelegramGroupMember{Name: "曾 子函", Username: "zdypro", Status: "administrator"})
	if got != "曾 子函（@zdypro）" {
		t.Fatalf("telegramMemberDisplay = %q", got)
	}
	if got := telegramMemberStatusText("banned"); got != "已移出" {
		t.Fatalf("telegramMemberStatusText banned = %q", got)
	}
	if got := telegramMemberStatusText(""); got != "状态未知" {
		t.Fatalf("telegramMemberStatusText empty = %q", got)
	}
	cap := telegramBotMemberCapabilityText("administrator")
	for _, want := range []string{"成员变更事件", "查询指定成员", "不能一次性拉取全体普通成员列表"} {
		if !strings.Contains(cap, want) {
			t.Fatalf("telegramBotMemberCapabilityText missing %q: %s", want, cap)
		}
	}
	rights := telegramMemberRightsText([]string{"delete_messages", "pin_messages", "invite_users"})
	for _, want := range []string{"删除消息", "置顶消息", "邀请成员"} {
		if !strings.Contains(rights, want) {
			t.Fatalf("telegramMemberRightsText missing %q: %s", want, rights)
		}
	}
}

func TestMatchSeenTelegramMember(t *testing.T) {
	members := []store.TelegramGroupSeenMember{
		{UserID: 10, Name: "黄桑", Username: "ceo"},
		{UserID: 11, Name: "黄工", Username: "huang_dev"},
	}
	m, ambiguous := matchSeenTelegramMember(members, "@ceo", true)
	if ambiguous || m == nil || m.UserID != 10 {
		t.Fatalf("exact username match = %+v ambiguous=%v", m, ambiguous)
	}
	if m, ambiguous := matchSeenTelegramMember(members, "黄", false); m != nil || !ambiguous {
		t.Fatalf("fuzzy ambiguous = %+v ambiguous=%v", m, ambiguous)
	}
	if m, ambiguous := matchSeenTelegramMember(members, "@", false); m != nil || ambiguous {
		t.Fatalf("empty username should not match = %+v ambiguous=%v", m, ambiguous)
	}
}

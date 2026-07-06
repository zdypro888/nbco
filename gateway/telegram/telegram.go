// Package telegram 是 Telegram 入口网关：接消息 → 编排器；同时实现 notify.Notifier。
// 中枢不感知 Telegram —— 本包是可替换的外设（原则：接口皆可换，中枢不可换）。
package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/store"
)

// Provider 渠道标识（identities.provider / chat_sessions.channel）。
const Provider = "telegram"

// 消息分片上限。Telegram 单条上限 4096 字符；这里留出较大余量给 HTML 分片
// 自动补的重开/闭合标签，避免长格式消息被补标签顶过上限后降级纯文本。
const chunkLimit = 3200

var bindKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
var htmlTagTokenRe = regexp.MustCompile(`(?i)</?(b|strong|i|em|u|s|del|code|pre|blockquote|a)(?:\s+[^>]*)?>`)

// Gateway Telegram 网关。
type Gateway struct {
	bot          *bot.Bot
	store        *store.Store
	orch         *chat.Orchestrator
	bus          *events.Bus // 系统事件总线（可为 nil）：入职等事件交 AI 分析决策
	superadmins  map[int64]bool
	defaultModel string

	mu    sync.Mutex
	locks map[int64]*sync.Mutex // 串行化键：私聊=用户ID（正数），群=chat ID（负数），天然不撞
	self  *models.User          // bot 自身身份（Run 时 GetMe 缓存，@提及与回复检测用）
}

// New 创建网关。
func New(token string, s *store.Store, orch *chat.Orchestrator, bus *events.Bus, superadmins []int64, defaultModel string) (*Gateway, error) {
	g := &Gateway{
		store:        s,
		orch:         orch,
		bus:          bus,
		superadmins:  map[int64]bool{},
		defaultModel: strings.TrimSpace(defaultModel),
		locks:        map[int64]*sync.Mutex{},
	}
	for _, id := range superadmins {
		g.superadmins[id] = true
	}
	b, err := bot.New(token,
		bot.WithDefaultHandler(g.handle),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateMyChatMember,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("初始化 Telegram bot: %w", err)
	}
	g.bot = b
	return g, nil
}

// Run 长轮询直到 ctx 结束。启动时缓存 bot 身份并按作用域注册命令菜单
// （私聊菜单与群菜单各自只列有意义的命令）。
func (g *Gateway) Run(ctx context.Context) {
	if me, err := g.bot.GetMe(ctx); err == nil {
		g.mu.Lock()
		g.self = me
		g.mu.Unlock()
		if strings.TrimSpace(me.Username) != "" {
			if err := g.store.SetKV(ctx, store.KVTelegramBotUsername, me.Username); err != nil {
				slog.Warn("缓存 Telegram bot username 失败", "err", err)
			}
		}
		// 群内纯文本 @提及要送达 bot 必须关闭 privacy mode（BotFather /setprivacy →
		// Disable）。CanReadAllGroupMessages=false 表示仍开着，@提及流程会静默失效。
		if !me.CanReadAllGroupMessages {
			slog.Warn("Telegram bot 处于 privacy mode：群内纯 @提及收不到，只有『回复 bot 消息』能触发；" +
				"如需 @提及生效，请在 @BotFather 用 /setprivacy 对本 bot 选 Disable")
		}
	} else {
		slog.Warn("GetMe 失败，群内 @提及识别不可用", "err", err)
	}
	g.setupCommands(ctx)
	g.bot.Start(ctx)
}

// setupCommands 按作用域注册命令菜单：群命令（如 /listen）只出现在群里，
// 私聊命令只出现在私聊——快捷键只在有意义的场景展示。失败仅记日志。
func (g *Gateway) setupCommands(ctx context.Context) {
	private := []models.BotCommand{
		{Command: "start", Description: "开始使用 / 查看说明"},
		{Command: "new", Description: "开启新会话（清空对话上下文）"},
		{Command: "model", Description: "查看/切换模型（超管私聊）"},
	}
	group := []models.BotCommand{
		{Command: "listen", Description: "开/关本群监听（记录讨论作为上下文，超管专用）"},
		{Command: "new", Description: "重置本群会话（超管专用）"},
	}
	if _, err := g.bot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: private, Scope: &models.BotCommandScopeAllPrivateChats{},
	}); err != nil {
		slog.Warn("注册私聊命令菜单失败", "err", err)
	}
	if _, err := g.bot.SetMyCommands(ctx, &bot.SetMyCommandsParams{
		Commands: group, Scope: &models.BotCommandScopeAllGroupChats{},
	}); err != nil {
		slog.Warn("注册群命令菜单失败", "err", err)
	}
}

// botUsername 缓存的 bot 用户名（未知时空串）。
func (g *Gateway) botUsername() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.self == nil {
		return ""
	}
	return g.self.Username
}

// botID 缓存的 bot 用户 ID（未知时 0）。
func (g *Gateway) botID() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.self == nil {
		return 0
	}
	return g.self.ID
}

// Send 实现 notify.Notifier：按用户查渠道地址投递。
func (g *Gateway) Send(ctx context.Context, userID int64, text string) error {
	ref, err := g.store.ChatRef(ctx, userID, Provider)
	if err != nil {
		return fmt.Errorf("用户 %d 无 Telegram 渠道: %w", userID, err)
	}
	chatID, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return fmt.Errorf("chat_ref 非法: %q", ref)
	}
	return g.sendChunks(ctx, chatID, text)
}

func (g *Gateway) handle(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update.MyChatMember != nil {
		g.handleMyChatMember(ctx, update.MyChatMember)
		return
	}
	msg := update.Message
	if msg == nil {
		return
	}
	if g.handleGroupServiceMessage(ctx, msg) {
		return
	}
	text := messageText(msg)
	isGroup := msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup
	if text == "" {
		return
	}
	if !isGroup && msg.From == nil {
		return
	}
	// 逐 update 起 goroutine 并加锁串行：私聊按用户、群按 chat（群共享会话不并发）。
	// 慢轮次不阻塞其他人/其他群。
	lockKey := userID(msg.From)
	if isGroup {
		lockKey = msg.Chat.ID // 群 chat ID 为负数，与用户 ID 不冲突
	}
	go func() {
		lock := g.userLock(lockKey)
		lock.Lock()
		defer lock.Unlock()
		if isGroup {
			g.processGroup(ctx, msg)
			return
		}
		if msg.Chat.Type != models.ChatTypePrivate {
			return // channel 等场景不处理
		}
		g.process(ctx, msg)
	}()
}

func (g *Gateway) handleMyChatMember(ctx context.Context, upd *models.ChatMemberUpdated) {
	chat := upd.Chat
	isGroup := chat.Type == models.ChatTypeGroup || chat.Type == models.ChatTypeSupergroup
	if !isGroup {
		return
	}
	oldStatus := upd.OldChatMember.Type
	newStatus := upd.NewChatMember.Type
	slog.Info("TG bot 群成员状态变化", "chat", chat.ID, "title", chat.Title,
		"from", upd.From.ID, "old", oldStatus, "new", newStatus)
	if !isActiveChatMember(newStatus) {
		if err := g.store.SetKV(ctx, listenKey(chat.ID), ""); err != nil {
			slog.Warn("关闭群监听失败", "chat", chat.ID, "err", err)
		}
		g.saveGroupState(ctx, chat, string(newStatus), false)
		return
	}
	if err := g.store.SetKV(ctx, listenKey(chat.ID), "1"); err != nil {
		slog.Warn("开启群监听失败", "chat", chat.ID, "err", err)
	}
	g.saveGroupState(ctx, chat, string(newStatus), true)
	// bot 被加入群时不一定能把操作者映射到公司用户；先确保群会话在首次 @ 时可用。
	g.reply(ctx, chat.ID, g.groupReadyMessage())
}

func (g *Gateway) handleGroupServiceMessage(ctx context.Context, msg *models.Message) bool {
	isGroup := msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup
	if !isGroup {
		return false
	}
	for _, u := range msg.NewChatMembers {
		if g.botID() != 0 && u.ID == g.botID() {
			slog.Info("TG bot 被加入群", "chat", msg.Chat.ID, "title", msg.Chat.Title, "by", userID(msg.From))
			if err := g.store.SetKV(ctx, listenKey(msg.Chat.ID), "1"); err != nil {
				slog.Warn("开启群监听失败", "chat", msg.Chat.ID, "err", err)
			}
			g.saveGroupState(ctx, msg.Chat, string(models.ChatMemberTypeMember), true)
			g.reply(ctx, msg.Chat.ID, g.groupReadyMessage())
			return true
		}
	}
	if msg.LeftChatMember != nil && g.botID() != 0 && msg.LeftChatMember.ID == g.botID() {
		slog.Info("TG bot 离开群", "chat", msg.Chat.ID, "title", msg.Chat.Title)
		if err := g.store.SetKV(ctx, listenKey(msg.Chat.ID), ""); err != nil {
			slog.Warn("关闭群监听失败", "chat", msg.Chat.ID, "err", err)
		}
		g.saveGroupState(ctx, msg.Chat, string(models.ChatMemberTypeLeft), false)
		return true
	}
	return false
}

func (g *Gateway) saveGroupState(ctx context.Context, chat models.Chat, status string, listen bool) {
	if err := g.store.SaveTelegramGroupState(ctx, store.TelegramGroupState{
		ChatID: chat.ID,
		Title:  chat.Title,
		Type:   string(chat.Type),
		Status: status,
		Listen: listen,
	}); err != nil {
		slog.Warn("保存 Telegram 群状态失败", "chat", chat.ID, "err", err)
	}
}

func isActiveChatMember(status models.ChatMemberType) bool {
	return status == models.ChatMemberTypeMember ||
		status == models.ChatMemberTypeAdministrator ||
		status == models.ChatMemberTypeOwner ||
		status == models.ChatMemberTypeRestricted
}

func userID(u *models.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

// --- 群聊 ---

// groupChannel 群共享会话的渠道值。
func groupChannel(chatID int64) string { return fmt.Sprintf("telegram:group:%d", chatID) }

// listenKey 群监听开关的 kv 键。
func listenKey(chatID int64) string { return store.TelegramGroupListenKey(chatID) }

// processGroup 群消息：命令 → 显式处理；@提及/回复 bot → 以发言人权限跑群会话；
// 其余消息仅在监听开启时旁听进上下文（不回复）。绝不在群里做绑定引导。
func (g *Gateway) processGroup(ctx context.Context, msg *models.Message) {
	chatID := msg.Chat.ID
	channel := groupChannel(chatID)
	text := messageText(msg)
	tgID := userID(msg.From)
	var u *store.User
	bound := false
	if tgID != 0 {
		var uerr error
		u, uerr = g.store.UserByIdentity(ctx, Provider, strconv.FormatInt(tgID, 10))
		bound = uerr == nil && u.Status == store.UserActive
	}
	cmd := commandOf(text, g.botUsername())
	listenOn := false
	if on, _ := g.store.GetKV(ctx, listenKey(chatID)); on == "1" {
		listenOn = true
	}
	g.saveGroupState(ctx, msg.Chat, string(models.ChatMemberTypeMember), listenOn)

	switch cmd {
	case "/listen":
		if !bound || u == nil || !u.IsSuperadmin {
			g.reply(ctx, chatID, "只有超级管理员能开关群监听。")
			return
		}
		if listenOn {
			if err := g.store.SetKV(ctx, listenKey(chatID), ""); err != nil {
				g.reply(ctx, chatID, "操作失败，请稍后再试。")
				return
			}
			g.saveGroupState(ctx, msg.Chat, string(models.ChatMemberTypeMember), false)
			g.reply(ctx, chatID, "🔇 已关闭本群监听。之后只有 @我 才会参与。")
			return
		}
		if err := g.store.SetKV(ctx, listenKey(chatID), "1"); err != nil {
			g.reply(ctx, chatID, "操作失败，请稍后再试。")
			return
		}
		g.saveGroupState(ctx, msg.Chat, string(models.ChatMemberTypeMember), true)
		_ = g.orch.TouchGroupSession(ctx, u, channel)
		g.reply(ctx, chatID, "🎧 已开启本群监听：我会把群里的讨论记为上下文（不插话），@我 时能接住前文。\n"+
			"注意：若我收不到普通群消息，请在 @BotFather 的 /setprivacy 里选择 Disable。再次 /listen 关闭。")
		return
	case "/new":
		if !bound || u == nil || !u.IsSuperadmin {
			g.reply(ctx, chatID, "只有超级管理员能重置群会话。")
			return
		}
		if err := g.orch.NewGroupSession(ctx, u, channel); err != nil {
			g.reply(ctx, chatID, "重置失败，请稍后再试。")
			return
		}
		g.reply(ctx, chatID, "🆕 本群会话已重置。")
		return
	case "/model":
		g.reply(ctx, chatID, "模型切换属于超级管理员操作，请私聊我使用 /model。")
		return
	}

	mentioned := g.mentioned(msg, text)
	if !mentioned {
		// 旁听：监听开启才记录，谁说的都署名（未绑定用户用 TG 显示名）。
		if listenOn {
			speaker := displayNameFromMessage(msg)
			if bound {
				speaker = u.Name
			}
			g.orch.RecordGroupMessage(ctx, channel, speaker, text)
		}
		return
	}
	slog.Info("TG 群提及", "chat", chatID, "tg_user", tgID, "bound", bound, "sender_chat", senderChatTitle(msg))
	if tgID == 0 {
		g.reply(ctx, chatID, "我收到了，但这条消息没有个人 Telegram 身份（可能是匿名管理员或频道身份发言）。请切回个人身份再 @ 我。")
		return
	}
	if !bound {
		g.reply(ctx, chatID, "你还未加入公司系统，请先私聊我完成绑定（找管理员要员工邀请链接或邀请码），之后就能在群里 @我 了。")
		return
	}
	ask := strings.TrimSpace(stripMention(text, g.botUsername()))
	if ask == "" {
		ask = "（在群里叫了你一声）"
	}
	g.sendTyping(ctx, chatID)
	answer, err := g.orch.HandleGroupMessage(ctx, u, channel, u.Name, ask)
	if err != nil {
		slog.Error("群对话轮次失败", "chat", chatID, "user", u.ID, "err", err)
		g.reply(ctx, chatID, "这轮对话出错了，请重试。")
		return
	}
	g.reply(ctx, chatID, answer)
}

// mentioned 是否点名了 bot：文本里 @用户名（大小写不敏感、需词边界），
// 或回复了 bot 的消息。
func (g *Gateway) mentioned(msg *models.Message, text string) bool {
	if un := g.botUsername(); un != "" && hasMention(text, un) {
		return true
	}
	if id := g.botID(); id != 0 && hasTextMention(msg.Entities, id) {
		return true
	}
	if id := g.botID(); id != 0 && hasTextMention(msg.CaptionEntities, id) {
		return true
	}
	return msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil &&
		g.botID() != 0 && msg.ReplyToMessage.From.ID == g.botID()
}

func hasTextMention(entities []models.MessageEntity, botID int64) bool {
	for _, e := range entities {
		if e.Type == models.MessageEntityTypeTextMention && e.User != nil && e.User.ID == botID {
			return true
		}
	}
	return false
}

// isUsernameByte Telegram 用户名字符集 [A-Za-z0-9_]（全 ASCII）。
func isUsernameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

// hasMention 文本是否包含 @username（大小写不敏感，且要求词边界——
// 后一个字符不是用户名字符，避免 @bot 误匹配 @bot_dev）。
func hasMention(text, username string) bool {
	lower := strings.ToLower(text)
	at := "@" + strings.ToLower(username)
	for i := 0; ; {
		j := strings.Index(lower[i:], at)
		if j < 0 {
			return false
		}
		end := i + j + len(at)
		if end >= len(lower) || !isUsernameByte(lower[end]) {
			return true
		}
		i = end
	}
}

// stripMention 去掉文本里对本 bot 的 @提及（大小写不敏感、词边界），
// 保留其他 @句柄不动。
func stripMention(text, username string) string {
	if username == "" {
		return text
	}
	lower := strings.ToLower(text)
	at := "@" + strings.ToLower(username)
	var b strings.Builder
	for i := 0; i < len(text); {
		if strings.HasPrefix(lower[i:], at) {
			end := i + len(at)
			if end >= len(text) || !isUsernameByte(text[end]) {
				i = end // 跳过本 bot 的提及
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

// commandOf 消息首词的命令形式。群里命令常带 @bot 后缀（/cmd@botname）：
// 只认裸命令或后缀正是本 bot 的命令，发给其他 bot 的（/cmd@other）返回空串。
// botUsername 未知（GetMe 失败）时保守只认裸命令。
func commandOf(text, botUsername string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return ""
	}
	cmd, suffix, hasAt := strings.Cut(fields[0], "@")
	if hasAt && !strings.EqualFold(suffix, botUsername) {
		return "" // 命令是发给别的 bot 的
	}
	return cmd
}

func commandArgs(text string) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 2 {
		return ""
	}
	first := fields[0]
	idx := strings.Index(text, first)
	if idx < 0 {
		return strings.Join(fields[1:], " ")
	}
	return strings.TrimSpace(text[idx+len(first):])
}

// displayName TG 用户显示名（名+姓，否则用户名）。
func displayName(from *models.User) string {
	if from == nil {
		return "未知成员"
	}
	name := strings.TrimSpace(strings.Join([]string{from.FirstName, from.LastName}, " "))
	if name == "" {
		name = from.Username
	}
	if name == "" {
		name = fmt.Sprintf("成员%d", from.ID)
	}
	return name
}

func displayNameFromMessage(msg *models.Message) string {
	if msg == nil {
		return "未知成员"
	}
	if msg.From != nil {
		return displayName(msg.From)
	}
	if msg.SenderChat != nil {
		if name := strings.TrimSpace(msg.SenderChat.Title); name != "" {
			return name
		}
		if name := strings.TrimSpace(msg.SenderChat.Username); name != "" {
			return name
		}
		return fmt.Sprintf("群身份%d", msg.SenderChat.ID)
	}
	return "匿名成员"
}

func senderChatTitle(msg *models.Message) string {
	if msg == nil || msg.SenderChat == nil {
		return ""
	}
	if title := strings.TrimSpace(msg.SenderChat.Title); title != "" {
		return title
	}
	if username := strings.TrimSpace(msg.SenderChat.Username); username != "" {
		return username
	}
	return fmt.Sprintf("%d", msg.SenderChat.ID)
}

func boundStartMessage(name string) string {
	return fmt.Sprintf("👋 你好，<b>%s</b>！\n"+
		"直接说事就行：📋 查任务、✅ 汇报进度、⏰ 设置提醒、📊 看项目进展都可以。\n"+
		"也可以发送 /new 开启新会话。", html.EscapeString(name))
}

func unboundHelpMessage(canBootstrap bool) string {
	var b strings.Builder
	b.WriteString("👋 欢迎来到 <b>NBCO</b>。\n\n")
	b.WriteString("我还没在公司系统里识别到你。加入后，我可以帮你查任务、汇报进度、设置提醒、沉淀个人信息和团队知识。\n\n")
	b.WriteString("<b>加入方式</b>\n")
	b.WriteString("1. 找管理员发送一次性邀请链接。\n")
	b.WriteString("2. 如果拿到的是邀请码，把那串 32 位码直接发给我。\n")
	b.WriteString("3. 绑定成功后，直接说工作事项就行。\n")
	if canBootstrap {
		b.WriteString("\n如果这是全新部署、还没有管理员，请发送 /superadmin 初始化首位超级管理员。")
	}
	return b.String()
}

func bindSuccessMessage(name string) string {
	return fmt.Sprintf("🎉 欢迎加入，<b>%s</b>！\n\n"+
		"你已经完成绑定，可以直接和我说工作事项。\n"+
		"可以试试：\n"+
		"• 查看我的任务\n"+
		"• 设置一个明天上午的提醒\n"+
		"• 记录一下我的岗位和职责\n\n"+
		"先自我介绍一下也可以，我会帮你整理成档案。", html.EscapeString(name))
}

func (g *Gateway) groupReadyMessage() string {
	username := strings.TrimSpace(g.botUsername())
	mention := "我"
	if username != "" {
		mention = "@" + username
	}
	return "👋 我已加入本群，并已开启群监听。\n" +
		fmt.Sprintf("在群里回复我的消息，或直接 %s 叫我，我会按发言人的公司权限处理。\n", html.EscapeString(mention)) +
		"涉及邀请、授权、Token、模型切换等高危操作，请私聊我完成。"
}

func (g *Gateway) process(ctx context.Context, msg *models.Message) {
	chatID := msg.Chat.ID
	text := messageText(msg)
	externalID := strconv.FormatInt(msg.From.ID, 10)

	slog.Debug("TG 消息", "tg_user", msg.From.ID, "chat", chatID, "text_len", len(text))

	u, err := g.store.UserByIdentity(ctx, Provider, externalID)
	if errors.Is(err, store.ErrNotFound) {
		slog.Info("TG 未绑定用户", "tg_user", msg.From.ID, "text_len", len(text))
		g.onboard(ctx, msg, chatID, externalID, text)
		return
	}
	if err != nil {
		slog.Error("查用户失败", "err", err)
		g.reply(ctx, chatID, "系统开小差了，请稍后再试。")
		return
	}
	if u.Status != store.UserActive {
		g.reply(ctx, chatID, "你的账号已被停用，如有疑问请联系管理员。")
		return
	}

	switch commandOf(text, g.botUsername()) {
	case "/new":
		if err := g.orch.NewSession(ctx, u, Provider); err != nil {
			slog.Error("开新会话失败", "err", err)
			g.reply(ctx, chatID, "开新会话失败，请稍后再试。")
			return
		}
		g.reply(ctx, chatID, "🆕 已开启新会话。")
		return
	case "/start":
		g.reply(ctx, chatID, boundStartMessage(u.Name))
		return
	case "/superadmin":
		// 首任超管引导：已绑定但系统无超管的用户可自我晋升。
		if u.IsSuperadmin {
			g.reply(ctx, chatID, "你已经是超级管理员。")
			return
		}
		switch err := g.store.PromoteFirstSuperadmin(ctx, u.ID); {
		case errors.Is(err, store.ErrConflict):
			g.reply(ctx, chatID, "系统已有超级管理员，此命令仅用于首次初始化。")
		case err != nil:
			slog.Error("超管晋升失败", "err", err)
			g.reply(ctx, chatID, "操作失败，请稍后再试。")
		default:
			g.reply(ctx, chatID, "👑 你已成为超级管理员。")
		}
		return
	case "/model":
		g.handleModelCommand(ctx, chatID, u, commandArgs(text))
		return
	}

	// 流式：发占位消息，把最终答复的增量渐进编辑上去（本地模型慢，不让用户干等）。
	g.sendTyping(ctx, chatID)
	ed := g.newStreamEditor(ctx, chatID)
	answer, err := g.orch.HandleMessageStream(ctx, u, Provider, text, ed.onDelta)
	if err != nil {
		slog.Error("对话轮次失败", "user", u.ID, "err", err)
		ed.fail(ctx, "这轮对话出错了，请重试；连续失败请联系管理员。")
		return
	}
	ed.finish(ctx, answer)
}

func (g *Gateway) handleModelCommand(ctx context.Context, chatID int64, u *store.User, args string) {
	if !u.IsSuperadmin {
		g.reply(ctx, chatID, "只有超级管理员能查看或切换模型。")
		return
	}
	args = strings.TrimSpace(args)
	if args == "" {
		g.reply(ctx, chatID, g.modelStatus(ctx))
		return
	}
	if strings.EqualFold(args, "reset") || strings.EqualFold(args, "default") {
		if err := g.store.SetKV(ctx, store.KVAIModel, ""); err != nil {
			slog.Error("重置运行时模型失败", "err", err)
			g.reply(ctx, chatID, "模型重置失败，请稍后再试。")
			return
		}
		g.reply(ctx, chatID, fmt.Sprintf("✅ 已恢复默认模型：<code>%s</code>", html.EscapeString(g.defaultModel)))
		return
	}
	if !validModelName(args) {
		g.reply(ctx, chatID, "模型名不合法。用法：<code>/model mlx-community/DeepSeek-V4-Flash</code>；恢复默认：<code>/model reset</code>。")
		return
	}
	if err := g.store.SetKV(ctx, store.KVAIModel, args); err != nil {
		slog.Error("切换运行时模型失败", "model", args, "err", err)
		g.reply(ctx, chatID, "模型切换失败，请稍后再试。")
		return
	}
	slog.Info("超级管理员切换运行时模型", "user", u.ID, "model", args)
	g.reply(ctx, chatID, fmt.Sprintf("✅ 已切换模型：<code>%s</code>\n后续新一轮对话和 worker 内置智能体都会使用它。", html.EscapeString(args)))
}

func (g *Gateway) modelStatus(ctx context.Context) string {
	model, err := g.store.GetKV(ctx, store.KVAIModel)
	if err != nil {
		slog.Warn("读取运行时模型失败", "err", err)
		model = ""
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Sprintf("当前模型：<code>%s</code>\n来源：配置文件默认值\n切换：<code>/model 模型名</code>\n恢复默认：<code>/model reset</code>",
			html.EscapeString(g.defaultModel))
	}
	return fmt.Sprintf("当前模型：<code>%s</code>\n来源：运行时设置\n默认模型：<code>%s</code>\n切换：<code>/model 模型名</code>\n恢复默认：<code>/model reset</code>",
		html.EscapeString(model), html.EscapeString(g.defaultModel))
}

func validModelName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 160 || len(strings.Fields(s)) != 1 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == '<' || r == '>' || r == '&' {
			return false
		}
	}
	return true
}

// onboard 未绑定用户：超管自动开通；其他人凭真人员工一次性邀请。
func (g *Gateway) onboard(ctx context.Context, msg *models.Message, chatID int64, externalID, text string) {
	name := displayName(msg.From)
	ident := store.Identity{Provider: Provider, ExternalID: externalID, ChatRef: strconv.FormatInt(chatID, 10)}

	// 首任超管引导：全新系统里第一个发 /superadmin 的人成为超管（零配置起步）。
	if text == "/superadmin" {
		u, err := g.store.BootstrapSuperadmin(ctx, name, ident)
		switch {
		case errors.Is(err, store.ErrConflict):
			g.reply(ctx, chatID, "系统已有超级管理员。请向管理员索取员工邀请链接或邀请码加入。")
		case err != nil:
			slog.Error("超管引导失败", "err", err)
			g.reply(ctx, chatID, "初始化失败，请稍后再试。")
		default:
			g.reply(ctx, chatID, fmt.Sprintf("👑 <b>%s</b>，你已成为首位超级管理员。直接说事就行。", html.EscapeString(u.Name)))
		}
		return
	}

	if g.superadmins[msg.From.ID] {
		u, err := g.store.CreateUser(ctx, name, true, ident)
		if err != nil {
			slog.Error("超管开通失败", "err", err)
			g.reply(ctx, chatID, "初始化失败，请稍后再试。")
			return
		}
		g.reply(ctx, chatID, fmt.Sprintf("👑 超级管理员 <b>%s</b> 已开通。直接说事就行。", html.EscapeString(u.Name)))
		return
	}

	key, ok := inviteTokenFromText(text, g.botUsername())
	if !ok {
		hasSuperadmin, err := g.store.HasSuperadmin(ctx)
		if err != nil {
			slog.Warn("查询超管状态失败", "err", err)
			hasSuperadmin = true
		}
		g.reply(ctx, chatID, unboundHelpMessage(!hasSuperadmin))
		return
	}
	// 单事务绑定：Key 无效不会留下半开账号。
	u, invitedBy, err := g.store.BindUserWithKey(ctx, key, name, ident)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			g.reply(ctx, chatID, "员工邀请链接或邀请码无效、已使用或已过期，请向管理员重新索取。")
			return
		}
		slog.Error("绑定失败", "err", err)
		g.reply(ctx, chatID, "绑定失败，请稍后再试。")
		return
	}
	g.reply(ctx, chatID, bindSuccessMessage(u.Name))
	// 入职事件交邀请人的 AI 分析：通知措辞、要不要安排入职跟进，都由 AI 定。
	g.bus.Emit("员工加入", invitedBy,
		fmt.Sprintf("新员工「%s」（用户 #%d）刚通过你签发的邀请完成 Telegram 绑定，正式加入公司。", u.Name, u.ID))
}

func inviteTokenFromText(text, botUsername string) (string, bool) {
	text = strings.TrimSpace(text)
	key := strings.ToLower(text)
	if bindKeyRe.MatchString(key) {
		return key, true
	}
	if commandOf(text, botUsername) != "/start" {
		return "", false
	}
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "", false
	}
	key = strings.ToLower(fields[1])
	if !bindKeyRe.MatchString(key) {
		return "", false
	}
	return key, true
}

func (g *Gateway) userLock(tgID int64) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[tgID]
	if !ok {
		l = &sync.Mutex{}
		g.locks[tgID] = l
	}
	return l
}

func (g *Gateway) reply(ctx context.Context, chatID int64, text string) {
	if err := g.sendChunks(ctx, chatID, text); err != nil {
		slog.Error("回复失败", "chat", chatID, "err", err)
	}
}

// sendChunks 按长度分片发送，避免超过 Telegram 单条消息长度。
func (g *Gateway) sendChunks(ctx context.Context, chatID int64, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "（空回复）"
	}
	slog.Debug("TG 发送", "chat", chatID, "text_len", len(text))
	// 模型不守 HTML 指引时（本地模型常见）把 Markdown 兜底转成 TG HTML。
	text = toTelegramHTML(text)
	for _, chunk := range splitChunks(text, chunkLimit) {
		if err := g.sendOne(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendOne 先按 Telegram HTML 发送（AI 与系统消息均按 HTML 子集排版）；
// 格式非法被 API 拒绝时降级纯文本重发，保证必达。
func (g *Gateway) sendOne(ctx context.Context, chatID int64, chunk string) error {
	_, err := g.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: chunk, ParseMode: models.ParseModeHTML,
	})
	if err == nil {
		return nil
	}
	slog.Debug("HTML 发送被拒，降级纯文本", "chat", chatID, "err", err)
	_, err = g.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: chunk})
	return err
}

func (g *Gateway) sendTyping(ctx context.Context, chatID int64) {
	_, _ = g.bot.SendChatAction(ctx, &bot.SendChatActionParams{ChatID: chatID, Action: models.ChatActionTyping})
}

func (g *Gateway) SendTelegramGroupMessage(ctx context.Context, chatID int64, text string, disableNotification bool) (int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "（空消息）"
	}
	text = toTelegramHTML(text)
	var lastID int
	for _, chunk := range splitChunks(text, chunkLimit) {
		msg, err := g.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID, Text: chunk, ParseMode: models.ParseModeHTML, DisableNotification: disableNotification,
		})
		if err != nil {
			msg, err = g.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID, Text: htmlTagTokenRe.ReplaceAllString(chunk, ""), DisableNotification: disableNotification,
			})
			if err != nil {
				return 0, err
			}
		}
		lastID = msg.ID
	}
	return lastID, nil
}

func (g *Gateway) EditTelegramGroupMessage(ctx context.Context, chatID int64, messageID int, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("消息内容不能为空")
	}
	_, err := g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: chatID, MessageID: messageID, Text: toTelegramHTML(text), ParseMode: models.ParseModeHTML,
	})
	return err
}

func (g *Gateway) DeleteTelegramGroupMessage(ctx context.Context, chatID int64, messageID int) error {
	_, err := g.bot.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: messageID})
	return err
}

func (g *Gateway) PinTelegramGroupMessage(ctx context.Context, chatID int64, messageID int, disableNotification bool) error {
	_, err := g.bot.PinChatMessage(ctx, &bot.PinChatMessageParams{
		ChatID: chatID, MessageID: messageID, DisableNotification: disableNotification,
	})
	return err
}

func (g *Gateway) UnpinTelegramGroupMessage(ctx context.Context, chatID int64, messageID int) error {
	_, err := g.bot.UnpinChatMessage(ctx, &bot.UnpinChatMessageParams{ChatID: chatID, MessageID: messageID})
	return err
}

func (g *Gateway) SetTelegramGroupTitle(ctx context.Context, chatID int64, title string) error {
	_, err := g.bot.SetChatTitle(ctx, &bot.SetChatTitleParams{ChatID: chatID, Title: title})
	if err != nil {
		return err
	}
	if st, serr := g.store.TelegramGroupState(ctx, chatID); serr == nil {
		st.Title = title
		st.UpdatedAt = time.Now()
		_ = g.store.SaveTelegramGroupState(ctx, *st)
	}
	return nil
}

func (g *Gateway) SetTelegramGroupDescription(ctx context.Context, chatID int64, description string) error {
	_, err := g.bot.SetChatDescription(ctx, &bot.SetChatDescriptionParams{ChatID: chatID, Description: description})
	return err
}

// messageText 把消息转成给编排器的文本：纯文本原样返回；带媒体的消息把
// 文件引用（file_id）拼进文本，AI 据此可调用 attach_to_task 等工具处理附件。
func messageText(msg *models.Message) string {
	var parts []string
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = "未命名"
		}
		parts = append(parts, fmt.Sprintf("[用户发来文件「%s」，file_id=%s]", name, msg.Document.FileID))
	}
	if n := len(msg.Photo); n > 0 {
		// Photo 按尺寸升序，取最大的一张。
		parts = append(parts, fmt.Sprintf("[用户发来图片，file_id=%s]", msg.Photo[n-1].FileID))
	}
	if msg.Video != nil {
		parts = append(parts, fmt.Sprintf("[用户发来视频，file_id=%s]", msg.Video.FileID))
	}
	if msg.Voice != nil {
		parts = append(parts, fmt.Sprintf("[用户发来语音，file_id=%s（当前无法转写，请让用户改用文字）]", msg.Voice.FileID))
	}
	if caption := strings.TrimSpace(msg.Caption); caption != "" {
		parts = append(parts, caption)
	}
	if text := strings.TrimSpace(msg.Text); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

// splitChunks 按行优先、字符兜底分片；输入可能已是 Telegram HTML，
// 因此兜底硬切也尽量避开标签与实体中间，减少分片后 HTML 解析失败。
func splitChunks(s string, limit int) []string {
	if len([]rune(s)) <= limit {
		return []string{s}
	}
	var chunks []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if curLen > 0 {
			chunks = append(chunks, strings.TrimRight(cur.String(), "\n"))
			cur.Reset()
			curLen = 0
		}
	}
	for line := range strings.SplitSeq(s, "\n") {
		runes := []rune(line)
		// 单行超限：尽量在 HTML 安全边界切。
		for len(runes) > limit {
			flush()
			cut := htmlSafeCut(runes, limit)
			chunks = append(chunks, string(runes[:cut]))
			runes = runes[cut:]
		}
		if curLen+len(runes)+1 > limit {
			flush()
		}
		cur.WriteString(string(runes))
		cur.WriteByte('\n')
		curLen += len(runes) + 1
	}
	flush()
	return balanceHTMLChunks(chunks)
}

func htmlSafeCut(runes []rune, limit int) int {
	if len(runes) <= limit {
		return len(runes)
	}
	lastSafe := 0
	inTag := false
	entityStart := -1
	for i := 0; i < limit; i++ {
		r := runes[i]
		switch {
		case inTag:
			if r == '>' {
				inTag = false
				lastSafe = i + 1
			}
		case entityStart >= 0:
			if r == ';' {
				entityStart = -1
				lastSafe = i + 1
			} else if unicode.IsSpace(r) || i-entityStart > 12 {
				entityStart = -1
				lastSafe = i
			}
		case r == '<':
			inTag = true
			if i > 0 {
				lastSafe = i
			}
		case r == '&':
			entityStart = i
			if i > 0 {
				lastSafe = i
			}
		default:
			lastSafe = i + 1
		}
	}
	if lastSafe > 0 {
		return lastSafe
	}
	return limit
}

type htmlOpenTag struct {
	name string
	text string
}

func balanceHTMLChunks(chunks []string) []string {
	if len(chunks) == 0 {
		return chunks
	}
	out := make([]string, 0, len(chunks))
	var open []htmlOpenTag
	for _, chunk := range chunks {
		prefix := reopenHTMLTags(open)
		nextOpen := append([]htmlOpenTag(nil), open...)
		nextOpen = scanHTMLTags(chunk, nextOpen)
		suffix := closeHTMLTags(nextOpen)
		out = append(out, prefix+chunk+suffix)
		open = nextOpen
	}
	return out
}

func scanHTMLTags(s string, open []htmlOpenTag) []htmlOpenTag {
	for _, token := range htmlTagTokenRe.FindAllString(s, -1) {
		name := htmlTagName(token)
		if name == "" {
			continue
		}
		if strings.HasPrefix(token, "</") {
			open = popHTMLTag(open, name)
			continue
		}
		open = append(open, htmlOpenTag{name: name, text: token})
	}
	return open
}

func htmlTagName(token string) string {
	token = strings.TrimPrefix(token, "</")
	token = strings.TrimPrefix(token, "<")
	token = strings.TrimRight(token, ">")
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	name := strings.Fields(token)[0]
	return strings.ToLower(strings.TrimPrefix(name, "/"))
}

func popHTMLTag(open []htmlOpenTag, name string) []htmlOpenTag {
	for i := len(open) - 1; i >= 0; i-- {
		if open[i].name == name {
			return append(open[:i], open[i+1:]...)
		}
	}
	return open
}

func reopenHTMLTags(open []htmlOpenTag) string {
	var b strings.Builder
	for _, tag := range open {
		b.WriteString(tag.text)
	}
	return b.String()
}

func closeHTMLTags(open []htmlOpenTag) string {
	var b strings.Builder
	for i := len(open) - 1; i >= 0; i-- {
		b.WriteString("</")
		b.WriteString(open[i].name)
		b.WriteString(">")
	}
	return b.String()
}

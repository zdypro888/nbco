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

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/store"
)

// Provider 渠道标识（identities.provider / chat_sessions.channel）。
const Provider = "telegram"

// 消息分片上限（Telegram 上限 4096 字符，留余量）。
const chunkLimit = 4000

var bindKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Gateway Telegram 网关。
type Gateway struct {
	bot         *bot.Bot
	store       *store.Store
	orch        *chat.Orchestrator
	superadmins map[int64]bool

	mu    sync.Mutex
	locks map[int64]*sync.Mutex // 串行化键：私聊=用户ID（正数），群=chat ID（负数），天然不撞
	self  *models.User          // bot 自身身份（Run 时 GetMe 缓存，@提及与回复检测用）
}

// New 创建网关。
func New(token string, s *store.Store, orch *chat.Orchestrator, superadmins []int64) (*Gateway, error) {
	g := &Gateway{
		store:       s,
		orch:        orch,
		superadmins: map[int64]bool{},
		locks:       map[int64]*sync.Mutex{},
	}
	for _, id := range superadmins {
		g.superadmins[id] = true
	}
	b, err := bot.New(token, bot.WithDefaultHandler(g.handle))
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
	msg := update.Message
	if msg == nil || msg.From == nil || messageText(msg) == "" {
		return
	}
	// 逐 update 起 goroutine 并加锁串行：私聊按用户、群按 chat（群共享会话不并发）。
	// 慢轮次不阻塞其他人/其他群。
	lockKey := msg.From.ID
	isGroup := msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup
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

// --- 群聊 ---

// groupChannel 群共享会话的渠道值。
func groupChannel(chatID int64) string { return fmt.Sprintf("telegram:group:%d", chatID) }

// listenKey 群监听开关的 kv 键。
func listenKey(chatID int64) string { return fmt.Sprintf("tg_listen:%d", chatID) }

// processGroup 群消息：命令 → 显式处理；@提及/回复 bot → 以发言人权限跑群会话；
// 其余消息仅在监听开启时旁听进上下文（不回复）。绝不在群里做绑定引导。
func (g *Gateway) processGroup(ctx context.Context, msg *models.Message) {
	chatID := msg.Chat.ID
	channel := groupChannel(chatID)
	text := messageText(msg)
	u, uerr := g.store.UserByIdentity(ctx, Provider, strconv.FormatInt(msg.From.ID, 10))
	bound := uerr == nil && u.Status == store.UserActive

	switch commandOf(text, g.botUsername()) {
	case "/listen":
		if !bound || !u.IsSuperadmin {
			g.reply(ctx, chatID, "只有超级管理员能开关群监听。")
			return
		}
		on, _ := g.store.GetKV(ctx, listenKey(chatID))
		if on == "1" {
			if err := g.store.SetKV(ctx, listenKey(chatID), ""); err != nil {
				g.reply(ctx, chatID, "操作失败，请稍后再试。")
				return
			}
			g.reply(ctx, chatID, "🔇 已关闭本群监听。之后只有 @我 才会参与。")
			return
		}
		if err := g.store.SetKV(ctx, listenKey(chatID), "1"); err != nil {
			g.reply(ctx, chatID, "操作失败，请稍后再试。")
			return
		}
		_ = g.orch.TouchGroupSession(ctx, u, channel)
		g.reply(ctx, chatID, "🎧 已开启本群监听：我会把群里的讨论记为上下文（不插话），@我 时能接住前文。\n"+
			"注意：若我收不到普通群消息，请在 @BotFather 的 /setprivacy 里选择 Disable。再次 /listen 关闭。")
		return
	case "/new":
		if !bound || !u.IsSuperadmin {
			g.reply(ctx, chatID, "只有超级管理员能重置群会话。")
			return
		}
		if err := g.orch.NewGroupSession(ctx, u, channel); err != nil {
			g.reply(ctx, chatID, "重置失败，请稍后再试。")
			return
		}
		g.reply(ctx, chatID, "🆕 本群会话已重置。")
		return
	}

	mentioned := g.mentioned(msg, text)
	if !mentioned {
		// 旁听：监听开启才记录，谁说的都署名（未绑定用户用 TG 显示名）。
		if on, _ := g.store.GetKV(ctx, listenKey(chatID)); on == "1" {
			speaker := displayName(msg.From)
			if bound {
				speaker = u.Name
			}
			g.orch.RecordGroupMessage(ctx, channel, speaker, text)
		}
		return
	}
	if !bound {
		g.reply(ctx, chatID, "你还未加入公司系统，请先私聊我完成绑定（找管理员要真人员工入职 Key），之后就能在群里 @我 了。")
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
	return msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil &&
		g.botID() != 0 && msg.ReplyToMessage.From.ID == g.botID()
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

// displayName TG 用户显示名（名+姓，否则用户名）。
func displayName(from *models.User) string {
	name := strings.TrimSpace(strings.Join([]string{from.FirstName, from.LastName}, " "))
	if name == "" {
		name = from.Username
	}
	if name == "" {
		name = fmt.Sprintf("成员%d", from.ID)
	}
	return name
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
	b.WriteString("1. 找管理员生成真人员工入职 Key。\n")
	b.WriteString("2. 把那串 32 位 Key 直接发给我。\n")
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

func (g *Gateway) process(ctx context.Context, msg *models.Message) {
	chatID := msg.Chat.ID
	text := messageText(msg)
	externalID := strconv.FormatInt(msg.From.ID, 10)

	slog.Debug("TG 消息", "tg_user", msg.From.ID, "chat", chatID, "text", text)

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

	switch text {
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

// onboard 未绑定用户：超管自动开通；其他人凭真人员工入职 Key。
func (g *Gateway) onboard(ctx context.Context, msg *models.Message, chatID int64, externalID, text string) {
	name := displayName(msg.From)
	ident := store.Identity{Provider: Provider, ExternalID: externalID, ChatRef: strconv.FormatInt(chatID, 10)}

	// 首任超管引导：全新系统里第一个发 /superadmin 的人成为超管（零配置起步）。
	if text == "/superadmin" {
		u, err := g.store.BootstrapSuperadmin(ctx, name, ident)
		switch {
		case errors.Is(err, store.ErrConflict):
			g.reply(ctx, chatID, "系统已有超级管理员。请向管理员索取真人员工入职 Key 加入。")
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

	key := strings.ToLower(text)
	if !bindKeyRe.MatchString(key) {
		hasSuperadmin, err := g.store.HasSuperadmin(ctx)
		if err != nil {
			slog.Warn("查询超管状态失败", "err", err)
			hasSuperadmin = true
		}
		g.reply(ctx, chatID, unboundHelpMessage(!hasSuperadmin))
		return
	}
	// 单事务绑定：Key 无效不会留下半开账号。
	u, err := g.store.BindUserWithKey(ctx, key, name, ident)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			g.reply(ctx, chatID, "真人员工入职 Key 无效或已过期，请向管理员重新索取。")
			return
		}
		slog.Error("绑定失败", "err", err)
		g.reply(ctx, chatID, "绑定失败，请稍后再试。")
		return
	}
	g.reply(ctx, chatID, bindSuccessMessage(u.Name))
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

// splitChunks 按行优先、字符兜底分片。
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
		// 单行超限：硬切。
		for len(runes) > limit {
			flush()
			chunks = append(chunks, string(runes[:limit]))
			runes = runes[limit:]
		}
		if curLen+len(runes)+1 > limit {
			flush()
		}
		cur.WriteString(string(runes))
		cur.WriteByte('\n')
		curLen += len(runes) + 1
	}
	flush()
	return chunks
}

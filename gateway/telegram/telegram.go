// Package telegram 是 Telegram 入口网关：接消息 → 编排器；同时实现 notify.Notifier。
// 中枢不感知 Telegram —— 本包是可替换的外设（原则：接口皆可换，中枢不可换）。
package telegram

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/zdypro888/nbco/ai/stt"
	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/keylock"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/safefs"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	"github.com/zdypro888/nbco/tools"
)

// Provider 渠道标识（identities.provider / chat_sessions.channel）。
const Provider = "telegram"

// 消息分片上限。Telegram 单条上限 4096 字符；这里留出较大余量给 HTML 分片
// 自动补的重开/闭合标签，避免长格式消息被补标签顶过上限后降级纯文本。
const chunkLimit = 3200
const textDebounceWindow = 900 * time.Millisecond
const telegramMessageTimeout = 15 * time.Minute
const groupMonitorDebounce = 20 * time.Second
const groupMonitorMaxWait = 2 * time.Minute
const groupMonitorAnalysisLease = 4 * time.Minute

const groupMonitorSystem = `你是 nbco 的 Telegram 群智能监控器。你的任务是判断最近群聊是否值得提醒监控发起人。

输出规则：
- 如果只是普通通知、闲聊、无行动价值的信息，只输出：NO_NOTIFY
- 如果有明确问题、阻塞、风险、争议、延期、需要管理者知道或跟进的事项，输出一条适合 Telegram HTML 的简短私聊提醒。
- 输出只总结关键问题、涉及对象、建议下一步，不逐条转发。
- 对监控发起人汇报时使用群名、人名和自然语言，不带 Telegram ID、内部会话 ID、系统提示或技术细节。
- 可使用 Telegram 支持的 HTML：<b>、<i>、<code>、<blockquote>。不要使用 Markdown 表格。`

const groupMonitorIntentSystem = `你是 nbco 的 Telegram 群管理意图分类器。判断用户在群里 @bot 的这句话是否是在要求开启或关闭“群智能监控/监听/后续总结提醒”。

只输出严格 JSON：{"intent":"on|off|none"}

分类标准：
- on：用户希望 bot 持续关注本群后续讨论，在出现问题、风险、阻塞、重要事项时总结/提醒/汇报。
- off：用户希望停止、关闭、取消上述持续关注或总结提醒。
- none：普通测试、问答、一次性回复、普通聊天、没有持续监控含义。

不要解释，不要输出 Markdown。`

var bindKeyRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
var htmlTagTokenRe = regexp.MustCompile(`(?i)</?(b|strong|i|em|u|s|del|code|pre|blockquote|a)(?:\s+[^>]*)?>`)
var htmlLooseTagRe = regexp.MustCompile(`(?is)<[^<>\n]*>`)
var htmlMalformedKnownTagRe = regexp.MustCompile(`(?i)</?\s*(b|strong|i|em|u|s|del|code|pre|blockquote|a|table|tr|td|th)\s*[:：，,。；;、]?`)

type pendingTextMessage struct {
	id      int64
	ctx     context.Context
	msg     *models.Message
	texts   []string
	isGroup bool
	lockKey int64
	fromID  int64
	timer   *time.Timer
}

type queuedTelegramMessage struct {
	ctx     context.Context
	msg     *models.Message
	isGroup bool
	lockKey int64
	prev    <-chan struct{}
	done    chan struct{}
}

// Gateway Telegram 网关。
type Gateway struct {
	bot            *bot.Bot
	store          *store.Store
	orch           *chat.Orchestrator
	bus            *events.Bus // 系统事件总线（可为 nil）：入职等事件交 AI 分析决策
	stt            *stt.Client // 语音转写（可为 nil = 未启用，语音消息提示改用文字）
	superadmins    map[int64]bool
	defaultModel   string
	modelBaseURL   string
	modelAPIKey    string
	fileStorePath  string
	publicBaseURL  string
	telegramAPIURL string
	tz             *time.Location

	mu                sync.Mutex
	locks             keylock.Map[int64] // 串行化键：私聊=用户ID（正数），群=chat ID（负数），天然不撞
	self              *models.User       // bot 自身身份（Run 时 GetMe 缓存，@提及与回复检测用）
	pending           map[int64]*pendingTextMessage
	pendingSeq        int64
	dispatchTails     map[int64]chan struct{}
	monitorTimers     map[int64]*time.Timer
	monitorGeneration map[int64]uint64
	monitorInstanceID string
	runCtx            context.Context
	ready             atomic.Bool
	privacyWarned     atomic.Bool
}

// New 创建网关。
func New(token, telegramAPIURL string, s *store.Store, orch *chat.Orchestrator, bus *events.Bus, superadmins []int64, defaultModel, modelBaseURL, modelAPIKey string, sttClient *stt.Client, fileStorePath, publicBaseURL string, tz *time.Location) (*Gateway, error) {
	g := &Gateway{
		store:             s,
		orch:              orch,
		bus:               bus,
		stt:               sttClient,
		superadmins:       map[int64]bool{},
		defaultModel:      strings.TrimSpace(defaultModel),
		modelBaseURL:      strings.TrimSpace(modelBaseURL),
		modelAPIKey:       strings.TrimSpace(modelAPIKey),
		fileStorePath:     strings.TrimSpace(fileStorePath),
		publicBaseURL:     strings.TrimRight(strings.TrimSpace(publicBaseURL), "/"),
		telegramAPIURL:    strings.TrimRight(strings.TrimSpace(telegramAPIURL), "/"),
		tz:                orLocalTimeZone(tz),
		pending:           map[int64]*pendingTextMessage{},
		dispatchTails:     map[int64]chan struct{}{},
		monitorTimers:     map[int64]*time.Timer{},
		monitorGeneration: map[int64]uint64{},
		monitorInstanceID: newGroupMonitorInstanceID(),
	}
	for _, id := range superadmins {
		g.superadmins[id] = true
	}
	options := []bot.Option{
		// Construction must not depend on Telegram network readiness. The gateway
		// performs a retrying GetMe handshake in Run while HTTP/worker APIs stay up.
		bot.WithSkipGetMe(),
		bot.WithDefaultHandler(g.handle),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateMyChatMember,
			models.AllowedUpdateChatMember,
		}),
	}
	if g.telegramAPIURL != "" {
		options = append(options,
			bot.WithServerURL(g.telegramAPIURL),
			bot.WithHTTPClient(time.Minute, newLocalBotAPIHTTPClient()),
		)
	}
	b, err := bot.New(token, options...)
	if err != nil {
		return nil, fmt.Errorf("初始化 Telegram bot: %w", err)
	}
	g.bot = b
	return g, nil
}

func newGroupMonitorInstanceID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	return hex.EncodeToString(sum[:8])
}

// Ready reports whether the Telegram API handshake is currently healthy.
func (g *Gateway) Ready() bool { return g != nil && g.ready.Load() }

// Run 长轮询直到 ctx 结束。Telegram API 尚未就绪时持续退避重试，不影响
// HTTP/MCP/worker 等核心入口继续运行。
func (g *Gateway) Run(ctx context.Context) {
	if !g.waitUntilReady(ctx) {
		return
	}
	defer g.ready.Store(false)
	g.mu.Lock()
	g.runCtx = ctx
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.runCtx = nil
		g.mu.Unlock()
	}()
	g.setupCommands(ctx)
	g.resumeGroupMonitors(ctx)
	defer g.stopGroupMonitorTimers()
	go g.monitorReadiness(ctx)
	g.bot.Start(ctx)
}

func (g *Gateway) waitUntilReady(ctx context.Context) bool {
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		me, err := g.bot.GetMe(attemptCtx)
		cancel()
		if err == nil {
			g.setBotIdentity(ctx, me)
			g.ready.Store(true)
			return true
		}
		g.ready.Store(false)
		slog.Warn("Telegram API 尚未就绪，将重试", "retry_in", backoff, "err", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		backoff = min(backoff*2, 10*time.Second)
	}
	return false
}

func (g *Gateway) setBotIdentity(ctx context.Context, me *models.User) {
	g.mu.Lock()
	g.self = me
	g.mu.Unlock()
	if username := strings.TrimSpace(me.Username); username != "" && g.store != nil {
		if err := g.store.SetKV(ctx, store.KVTelegramBotUsername, username); err != nil {
			slog.Warn("缓存 Telegram bot username 失败", "err", err)
		}
	}
	// 群内纯文本 @提及要送达 bot 必须关闭 privacy mode（BotFather /setprivacy → Disable）。
	if !me.CanReadAllGroupMessages && g.privacyWarned.CompareAndSwap(false, true) {
		slog.Warn("Telegram bot 处于 privacy mode：群内纯 @提及收不到，只有『回复 bot 消息』能触发；" +
			"如需 @提及生效，请在 @BotFather 用 /setprivacy 对本 bot 选 Disable")
	} else if me.CanReadAllGroupMessages {
		g.privacyWarned.Store(false)
	}
}

func (g *Gateway) monitorReadiness(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			g.ready.Store(false)
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			me, err := g.bot.GetMe(pingCtx)
			cancel()
			g.ready.Store(err == nil)
			if err == nil {
				g.setBotIdentity(ctx, me)
			} else {
				slog.Warn("Telegram API 就绪检查失败", "err", err)
			}
		}
	}
}

// setupCommands 按作用域注册命令菜单：群命令（如 /listen）只出现在群里，
// 私聊命令只出现在私聊——快捷键只在有意义的场景展示。失败仅记日志。
func (g *Gateway) setupCommands(ctx context.Context) {
	private := []models.BotCommand{
		{Command: "start", Description: "开始使用 / 查看说明"},
		{Command: "new", Description: "开启新会话（清空对话上下文）"},
		{Command: "model", Description: "查看/切换模型（超管私聊）"},
		{Command: "token", Description: "查看/换发 Access Token"},
	}
	group := []models.BotCommand{
		{Command: "listen", Description: "开/关本群监听（需群管理权限）"},
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
		return fmt.Errorf("telegram 渠道不可用: %w", err)
	}
	chatID, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return fmt.Errorf("chat_ref 非法: %q", ref)
	}
	return g.sendChunks(ctx, chatID, text)
}

func (g *Gateway) SendFile(ctx context.Context, userID int64, fileID int64, caption string) error {
	ref, err := g.store.ChatRef(ctx, userID, Provider)
	if err != nil {
		return fmt.Errorf("telegram 渠道不可用: %w", err)
	}
	chatID, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return fmt.Errorf("chat_ref 非法: %q", ref)
	}
	f, err := g.store.FileByID(ctx, fileID)
	if err != nil {
		return fmt.Errorf("文件不存在: %w", err)
	}
	fp, err := safefs.OpenRegular(g.fileStorePath, f.StoragePath)
	if err != nil {
		return err
	}
	defer fp.Close()
	caption = toTelegramHTML(caption)
	if len([]rune(caption)) > 1024 {
		caption = string([]rune(caption)[:1024])
	}
	_, err = g.bot.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID: chatID,
		Document: &models.InputFileUpload{
			Filename: safeTelegramFilename(f.OriginalName),
			Data:     fp,
		},
		Caption:   caption,
		ParseMode: models.ParseModeHTML,
	})
	if err == nil {
		return nil
	}
	if _, seekErr := fp.Seek(0, io.SeekStart); seekErr != nil {
		return err
	}
	_, err = g.bot.SendDocument(ctx, &bot.SendDocumentParams{
		ChatID: chatID,
		Document: &models.InputFileUpload{
			Filename: safeTelegramFilename(f.OriginalName),
			Data:     fp,
		},
		Caption: telegramPlainText(caption),
	})
	return err
}

func (g *Gateway) handle(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update.MyChatMember != nil {
		g.handleMyChatMember(ctx, update.MyChatMember)
		return
	}
	if update.ChatMember != nil {
		g.handleChatMember(ctx, update.ChatMember)
		return
	}
	msg := update.Message
	if msg == nil {
		return
	}
	if g.handleGroupServiceMessage(ctx, msg) {
		return
	}
	// 路由阶段只判「有没有内容」，不做媒体加工：真正的转写/文本组装在
	// process/processGroup 里做一次（此前 handle 也调 messageText，语音被转写两次）。
	text := strings.TrimSpace(msg.Text)
	isGroup := msg.Chat.Type == models.ChatTypeGroup || msg.Chat.Type == models.ChatTypeSupergroup
	if !hasMessagePayload(msg) {
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
	if g.shouldDebounce(msg, text) {
		g.enqueueTextMessage(ctx, lockKey, isGroup, msg, text)
		return
	}
	g.dispatchMessageAfterPending(ctx, lockKey, isGroup, msg)
}

func (g *Gateway) shouldDebounce(msg *models.Message, text string) bool {
	if msg == nil || strings.TrimSpace(text) == "" {
		return false
	}
	if commandOf(text, g.botUsername()) != "" {
		return false
	}
	// 只合并普通文本。文件/图片/语音等消息含结构化元数据，立即处理更清晰。
	return strings.TrimSpace(msg.Text) != ""
}

func (g *Gateway) enqueueTextMessage(ctx context.Context, lockKey int64, isGroup bool, msg *models.Message, text string) {
	fromID := userID(msg.From)
	clone := *msg
	clone.Text = strings.TrimSpace(text)
	clone.Caption = ""

	var queued []*queuedTelegramMessage
	g.mu.Lock()
	if cur := g.pending[lockKey]; cur != nil && cur.isGroup == isGroup && cur.fromID == fromID && cur.msg.Chat.ID == msg.Chat.ID {
		cur.texts = append(cur.texts, clone.Text)
		cur.timer.Reset(textDebounceWindow)
		g.mu.Unlock()
		return
	}
	if cur := g.pending[lockKey]; cur != nil {
		cur.timer.Stop()
		delete(g.pending, lockKey)
		queued = append(queued, g.queuePendingTextLocked(cur))
	}
	g.pendingSeq++
	id := g.pendingSeq
	p := &pendingTextMessage{
		id: id, ctx: ctx, msg: &clone, texts: []string{clone.Text},
		isGroup: isGroup, lockKey: lockKey, fromID: fromID,
	}
	p.timer = time.AfterFunc(textDebounceWindow, func() { g.flushPendingText(lockKey, id) })
	g.pending[lockKey] = p
	g.mu.Unlock()
	g.startQueuedMessages(queued)
}

func (g *Gateway) flushPendingText(lockKey, id int64) {
	g.mu.Lock()
	p := g.pending[lockKey]
	if p == nil || p.id != id {
		g.mu.Unlock()
		return
	}
	delete(g.pending, lockKey)
	queued := g.queuePendingTextLocked(p)
	g.mu.Unlock()
	g.startQueuedMessage(queued)
}

func (g *Gateway) queuePendingTextLocked(p *pendingTextMessage) *queuedTelegramMessage {
	if p == nil || p.msg == nil {
		return nil
	}
	merged := *p.msg
	merged.Text = strings.Join(p.texts, "\n")
	return g.queueMessageLocked(p.ctx, p.lockKey, p.isGroup, &merged)
}

func (g *Gateway) dispatchMessageAfterPending(ctx context.Context, lockKey int64, isGroup bool, msg *models.Message) {
	g.mu.Lock()
	queued := g.queueMessageAfterPendingLocked(ctx, lockKey, isGroup, msg)
	g.mu.Unlock()
	g.startQueuedMessages(queued)
}

func (g *Gateway) queueMessageAfterPendingLocked(ctx context.Context, lockKey int64, isGroup bool, msg *models.Message) []*queuedTelegramMessage {
	queued := make([]*queuedTelegramMessage, 0, 2)
	if pending := g.pending[lockKey]; pending != nil {
		pending.timer.Stop()
		delete(g.pending, lockKey)
		queued = append(queued, g.queuePendingTextLocked(pending))
	}
	queued = append(queued, g.queueMessageLocked(ctx, lockKey, isGroup, msg))
	return queued
}

func (g *Gateway) queueMessageLocked(ctx context.Context, lockKey int64, isGroup bool, msg *models.Message) *queuedTelegramMessage {
	if msg == nil {
		return nil
	}
	if g.dispatchTails == nil {
		g.dispatchTails = map[int64]chan struct{}{}
	}
	done := make(chan struct{})
	queued := &queuedTelegramMessage{
		ctx: ctx, msg: msg, isGroup: isGroup, lockKey: lockKey,
		prev: g.dispatchTails[lockKey], done: done,
	}
	g.dispatchTails[lockKey] = done
	return queued
}

func (g *Gateway) startQueuedMessages(messages []*queuedTelegramMessage) {
	for _, message := range messages {
		g.startQueuedMessage(message)
	}
}

func (g *Gateway) startQueuedMessage(queued *queuedTelegramMessage) {
	if queued == nil {
		return
	}
	go func() {
		defer g.finishQueuedMessage(queued)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Telegram 消息处理 panic 已恢复", "chat", queued.lockKey, "panic", r)
			}
		}()
		if queued.prev != nil {
			select {
			case <-queued.prev:
			case <-queued.ctx.Done():
				return
			}
		}
		handleCtx, cancel := context.WithTimeout(queued.ctx, telegramMessageTimeout)
		defer cancel()
		release, err := g.locks.AcquireContext(handleCtx, queued.lockKey)
		if err != nil {
			slog.Warn("Telegram 消息等待同会话处理超时，已放弃", "chat", queued.lockKey, "err", err)
			return
		}
		defer release()
		if queued.isGroup {
			g.processGroup(handleCtx, queued.msg)
			return
		}
		if queued.msg.Chat.Type != models.ChatTypePrivate {
			return // channel 等场景不处理
		}
		g.process(handleCtx, queued.msg)
	}()
}

func (g *Gateway) finishQueuedMessage(queued *queuedTelegramMessage) {
	close(queued.done)
	g.mu.Lock()
	if g.dispatchTails[queued.lockKey] == queued.done {
		delete(g.dispatchTails, queued.lockKey)
	}
	g.mu.Unlock()
}

func (g *Gateway) handleChatMember(ctx context.Context, upd *models.ChatMemberUpdated) {
	chat := upd.Chat
	isGroup := chat.Type == models.ChatTypeGroup || chat.Type == models.ChatTypeSupergroup
	if !isGroup {
		return
	}
	member := memberUser(&upd.NewChatMember)
	if member == nil {
		return
	}
	g.saveSeenMember(ctx, chat.ID, member, "", 0)
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
		g.saveSeenMember(ctx, msg.Chat.ID, &u, "", msg.ID)
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

func (g *Gateway) saveSeenMember(ctx context.Context, chatID int64, u *models.User, text string, messageID int) {
	if u == nil || u.ID == 0 {
		return
	}
	if err := g.store.SaveTelegramGroupSeenMember(ctx, store.TelegramGroupSeenMember{
		ChatID:    chatID,
		UserID:    u.ID,
		Name:      displayName(u),
		Username:  u.Username,
		IsBot:     u.IsBot,
		LastSeen:  time.Now(),
		LastText:  text,
		MessageID: messageID,
	}); err != nil {
		slog.Warn("保存 Telegram 群成员可见记录失败", "chat", chatID, "user", u.ID, "err", err)
	}
}

func memberUser(m *models.ChatMember) *models.User {
	if m == nil {
		return nil
	}
	switch m.Type {
	case models.ChatMemberTypeOwner:
		if m.Owner == nil {
			return nil
		}
		return m.Owner.User
	case models.ChatMemberTypeAdministrator:
		if m.Administrator == nil {
			return nil
		}
		return &m.Administrator.User
	case models.ChatMemberTypeMember:
		if m.Member == nil {
			return nil
		}
		return m.Member.User
	case models.ChatMemberTypeRestricted:
		if m.Restricted == nil {
			return nil
		}
		return m.Restricted.User
	case models.ChatMemberTypeLeft:
		if m.Left == nil {
			return nil
		}
		return m.Left.User
	case models.ChatMemberTypeBanned:
		if m.Banned == nil {
			return nil
		}
		return m.Banned.User
	default:
		return nil
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
// 其余消息在监听、事件监控或摘要任一消费者启用时写入共享事实流（不回复）。
// 绝不在群里做绑定引导。
func (g *Gateway) processGroup(ctx context.Context, msg *models.Message) {
	chatID := msg.Chat.ID
	channel := groupChannel(chatID)
	text := g.messageText(ctx, msg)
	memorySourceText := telegramMemorySourceText(msg, text)
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
	monitorOn := false
	if mon, err := g.store.TelegramGroupMonitor(ctx, chatID); err == nil && mon.Enabled {
		monitorOn = true
	}
	digestOn, err := g.store.HasActiveAutomationSchedule(ctx, store.ScheduleSourceTelegramGroupDigest, strconv.FormatInt(chatID, 10))
	if err != nil {
		slog.Warn("读取群摘要采集状态失败", "chat", chatID, "err", err)
	}
	g.saveGroupState(ctx, msg.Chat, string(models.ChatMemberTypeMember), listenOn)
	mentionText := text
	if hasIncomingTelegramFiles(msg) {
		mentionText = nonMediaText(msg)
	}
	mentioned := g.mentioned(msg, mentionText)
	var intakeResults []incomingTelegramFileResult
	fileInstruction := ""
	if hasIncomingTelegramFiles(msg) {
		fileInstruction = strings.TrimSpace(nonMediaText(msg))
		if bound && (mentioned || listenOn || monitorOn || digestOn) {
			intakeResults = g.saveIncomingTelegramFiles(ctx, msg, u)
			saved, failed := splitTelegramFileIntakes(intakeResults)
			text = telegramFileContext(saved, failed, fileInstruction)
		} else {
			text = "[群消息包含文件，但文件没有进入 nbco，也没有可读取的文件内容]"
			if !bound {
				text = "[群成员发送了文件，但发送者尚未绑定；文件没有进入 nbco，也没有可读取的文件内容]"
			}
			if fileInstruction != "" {
				text += "\n" + fileInstruction
			}
		}
	}
	if msg.From != nil {
		g.saveSeenMember(ctx, chatID, msg.From, text, msg.ID)
	}
	envelope := telegramMessageEnvelope(msg, u)

	switch cmd {
	case "/listen":
		if !bound || u == nil || !g.canManageTelegramGroup(ctx, u, chatID) {
			g.reply(ctx, chatID, "你没有管理 Telegram 群的权限。请让超级管理员给你授权 manage_telegram_group。")
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
		if err := g.orch.TouchGroupSession(ctx, u, channel); err != nil {
			slog.Error("建立群监听会话失败", "chat", chatID, "err", err)
			g.reply(ctx, chatID, "开启监听失败，请稍后再试。")
			return
		}
		if err := g.store.SetKV(ctx, listenKey(chatID), "1"); err != nil {
			g.reply(ctx, chatID, "操作失败，请稍后再试。")
			return
		}
		g.saveGroupState(ctx, msg.Chat, string(models.ChatMemberTypeMember), true)
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

	if !mentioned {
		// 采集：任一消费者启用才记录，谁说的都署名（未绑定用户用 TG 显示名）。
		if listenOn || monitorOn || digestOn {
			speaker := displayNameFromMessage(msg)
			if bound {
				speaker = u.Name
			}
			if err := g.orch.RecordGroupMessageWithEnvelope(ctx, g.groupTranscriptOwner(ctx, u), channel, speaker, text, envelope); err != nil {
				slog.Error("Telegram 群消息持久化失败", "chat", chatID, "message", msg.ID, "err", err)
			}
			if monitorOn {
				g.observeGroupMonitor(ctx, msg.Chat, text)
			}
		}
		return
	}
	slog.Info("TG 群提及", "chat", chatID, "tg_user", tgID, "bound", bound, "sender_chat", senderChatTitle(msg))
	if tgID == 0 {
		g.reply(ctx, chatID, "我收到了，但这条消息没有个人 Telegram 身份（可能是匿名管理员或频道身份发言）。请切回个人身份再 @ 我。")
		return
	}
	if !bound {
		if g.handleGroupAutoInvite(ctx, msg, chatID, tgID) {
			return
		}
		g.reply(ctx, chatID, "你还未加入公司系统，请先私聊我完成绑定（找管理员要员工邀请链接或邀请码），之后就能在群里 @我 了。")
		return
	}
	if len(intakeResults) > 0 {
		saved, failed := splitTelegramFileIntakes(intakeResults)
		if len(saved) == 0 || fileInstruction == "" {
			if len(saved) > 0 {
				// A file-only mention still belongs to this shared conversation.
				// Persist the system file IDs without running the model, so a later
				// "处理刚才的文件" can resolve them without exposing private uploads.
				if err := g.orch.TouchGroupSession(ctx, u, channel); err != nil {
					slog.Warn("建立群文件上下文失败", "chat", chatID, "user", u.ID, "err", err)
				} else {
					if err := g.orch.RecordGroupMessageWithEnvelope(ctx, u, channel, u.Name, savedFilesPrompt(saved), envelope); err != nil {
						slog.Error("群文件上下文持久化失败", "chat", chatID, "user", u.ID, "err", err)
					}
				}
			}
			g.replyFileIntakeResults(ctx, chatID, saved, failed, false)
			return
		}
	}
	ask := strings.TrimSpace(stripMention(text, g.botUsername()))
	if ask == "" {
		ask = "（在群里叫了你一声）"
	}
	if g.handleGroupMonitorMention(ctx, msg, u, ask) {
		return
	}
	g.sendTyping(ctx, chatID)
	turnCtx := chat.WithTurnSourceKey(ctx, telegramTurnSourceKey(msg))
	turnCtx = chat.WithMemorySourceText(turnCtx, strings.TrimSpace(stripMention(memorySourceText, g.botUsername())))
	turnCtx = chat.WithMessageEnvelope(turnCtx, envelope)
	reply, err := g.orch.HandleGroupMessageResult(turnCtx, u, channel, u.Name, ask)
	if err != nil {
		if errors.Is(err, store.ErrTurnInProgress) || errors.Is(err, store.ErrTurnFailed) ||
			errors.Is(err, store.ErrConflict) {
			slog.Info("忽略已领取的 Telegram 群消息重放", "chat", chatID, "message", msg.ID, "err", err)
			return
		}
		slog.Error("群对话轮次失败", "chat", chatID, "user", u.ID, "err", err)
		g.reply(ctx, chatID, "这轮对话出错了，请重试。")
		return
	}
	if reply.AlreadyDelivered || (reply.Replayed && reply.DeliveryStatus == "failed") {
		if reply.DeliveryStatus == "failed" {
			slog.Warn("Telegram 群消息曾部分或完整投递失败，禁止盲目自动重发", "chat", chatID, "turn", reply.TurnID)
		}
		return
	}
	if monitorOn {
		g.observeGroupMonitor(ctx, msg.Chat, ask)
	}
	if err := g.sendChunks(ctx, chatID, reply.Text); err != nil {
		slog.Error("群对话答复投递失败", "chat", chatID, "turn", reply.TurnID, "err", err)
		g.recordTurnDelivery(ctx, reply.TurnID, err)
		return
	}
	g.recordTurnDelivery(ctx, reply.TurnID, nil)
}

// groupTranscriptOwner chooses a stable company identity for the shared
// transcript row. The actual speaker remains separately signed in message
// content; ownership only satisfies the chat_sessions foreign key.
func (g *Gateway) groupTranscriptOwner(ctx context.Context, bound *store.User) *store.User {
	if bound != nil && bound.ID > 0 && bound.Status == store.UserActive && bound.IsSuperadmin {
		return bound
	}
	var selectedTelegramID int64
	for telegramID := range g.superadmins {
		if selectedTelegramID == 0 || telegramID < selectedTelegramID {
			selectedTelegramID = telegramID
		}
	}
	if selectedTelegramID == 0 {
		if bound != nil && bound.ID > 0 && bound.Status == store.UserActive {
			return bound
		}
		return nil
	}
	u, err := g.store.UserByIdentity(ctx, Provider, strconv.FormatInt(selectedTelegramID, 10))
	if err == nil && u.Status == store.UserActive {
		return u
	}
	if bound != nil && bound.ID > 0 && bound.Status == store.UserActive {
		return bound
	}
	return nil
}

func (g *Gateway) canManageTelegramGroup(ctx context.Context, u *store.User, chatID int64) bool {
	if u == nil || u.Status != store.UserActive {
		return false
	}
	if u.IsSuperadmin {
		return true
	}
	grants, err := g.store.PermsOf(ctx, u.ID)
	if err != nil {
		slog.Warn("加载 Telegram 群管理权限失败", "user", u.ID, "err", err)
		return false
	}
	return perm.CheckActiveTarget(grants, perm.ActManageTGGroup,
		fmt.Sprintf("telegram:group:%d", chatID))
}

func (g *Gateway) handleGroupMonitorMention(ctx context.Context, msg *models.Message, u *store.User, ask string) bool {
	intent := g.classifyGroupMonitorIntent(ctx, u, ask)
	if intent == "" {
		return false
	}
	chatID := msg.Chat.ID
	if !g.canManageTelegramGroup(ctx, u, chatID) {
		g.reply(ctx, chatID, "你没有管理 Telegram 群的权限。请让超级管理员给你授权 <code>manage_telegram_group</code>。")
		return true
	}
	if intent == "off" {
		_, err := g.store.UpdateTelegramGroupMonitor(ctx, chatID, func(mon *store.TelegramGroupMonitor) error {
			mon.Enabled = false
			mon.GroupTitle = strings.TrimSpace(msg.Chat.Title)
			mon.UpdatedAt = time.Now()
			mon.PendingCount = 0
			mon.BatchStartedAt = time.Time{}
			mon.AnalysisOwner = ""
			mon.AnalysisStartedAt = time.Time{}
			mon.AnalysisThrough = time.Time{}
			mon.AnalysisFailures = 0
			mon.Buffer = nil
			return nil
		})
		if err != nil {
			slog.Error("保存群监控失败", "chat", chatID, "err", err)
			g.reply(ctx, chatID, "关闭监控失败，请稍后再试。")
			return true
		}
		g.reply(ctx, chatID, "🔕 已关闭本群智能监控。")
		return true
	}

	now := time.Now()
	if err := g.orch.TouchGroupSession(ctx, u, groupChannel(chatID)); err != nil {
		slog.Error("建立群监控会话失败", "chat", chatID, "err", err)
		g.reply(ctx, chatID, "开启监控失败，请稍后再试。")
		return true
	}
	_, err := g.store.UpdateTelegramGroupMonitor(ctx, chatID, func(mon *store.TelegramGroupMonitor) error {
		wasEnabled := mon.Enabled
		mon.Enabled = true
		mon.GroupTitle = strings.TrimSpace(msg.Chat.Title)
		mon.Instruction = strings.TrimSpace(ask)
		mon.NotifyUserID = u.ID
		mon.CreatedBy = u.ID
		mon.UpdatedAt = now
		if !wasEnabled {
			mon.LastCheckedAt = now
			mon.BatchStartedAt = time.Time{}
			mon.PendingCount = 0
			mon.AnalysisOwner = ""
			mon.AnalysisStartedAt = time.Time{}
			mon.AnalysisThrough = time.Time{}
			mon.AnalysisFailures = 0
			mon.Buffer = nil
		}
		return nil
	})
	if err != nil {
		slog.Error("保存群监控失败", "chat", chatID, "err", err)
		g.reply(ctx, chatID, "开启监控失败，请稍后再试。")
		return true
	}
	g.reply(ctx, chatID, "👀 已开启本群智能监控。我会记录后续讨论，遇到问题、阻塞或风险时私聊汇总给你；普通消息不会逐条转发。")
	return true
}

func (g *Gateway) classifyGroupMonitorIntent(ctx context.Context, u *store.User, ask string) string {
	if g == nil || g.orch == nil || strings.TrimSpace(ask) == "" {
		return ""
	}
	userID := int64(0)
	if u != nil {
		userID = u.ID
	}
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := g.orch.Summarize(cctx, userID, "telegram_group_monitor_intent", groupMonitorIntentSystem, textfmt.TruncateRunes(ask, 600))
	if err != nil {
		slog.Warn("群监控意图分类失败", "user", userID, "err", err)
		return ""
	}
	raw := extractTelegramJSONObject(out)
	var parsed struct {
		Intent string `json:"intent"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		slog.Warn("群监控意图分类 JSON 解析失败", "user", userID, "reply_sha", telegramTextHash(out), "err", err)
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Intent)) {
	case "on":
		return "on"
	case "off":
		return "off"
	default:
		return ""
	}
}

func extractTelegramJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return s
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func telegramTextHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func (g *Gateway) observeGroupMonitor(ctx context.Context, chat models.Chat, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	now := time.Now()
	mon, err := g.store.UpdateTelegramGroupMonitor(ctx, chat.ID, func(mon *store.TelegramGroupMonitor) error {
		if !mon.Enabled || mon.NotifyUserID == 0 {
			return nil
		}
		if mon.PendingCount == 0 || mon.BatchStartedAt.IsZero() {
			mon.BatchStartedAt = now
		}
		mon.PendingCount++
		mon.UpdatedAt = now
		return nil
	})
	if err != nil {
		slog.Warn("保存群监控待分析状态失败", "chat", chat.ID, "err", err)
		return
	}
	if mon == nil || !mon.Enabled || mon.NotifyUserID == 0 {
		return
	}
	g.scheduleGroupMonitor(chat.ID, mon.BatchStartedAt)
}

func groupMonitorEvaluationDelay(startedAt, now time.Time) time.Duration {
	if startedAt.IsZero() {
		return groupMonitorDebounce
	}
	remaining := startedAt.Add(groupMonitorMaxWait).Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining < groupMonitorDebounce {
		return remaining
	}
	return groupMonitorDebounce
}

func (g *Gateway) scheduleGroupMonitor(chatID int64, startedAt time.Time) {
	g.scheduleGroupMonitorAfter(chatID, groupMonitorEvaluationDelay(startedAt, time.Now()))
}

func (g *Gateway) scheduleGroupMonitorRetry(chatID int64, failures int) {
	g.scheduleGroupMonitorAfter(chatID, groupMonitorRetryDelay(failures))
}

func groupMonitorRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := groupMonitorDebounce
	for i := 1; i < failures && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		return 30 * time.Minute
	}
	return delay
}

func (g *Gateway) scheduleGroupMonitorAfter(chatID int64, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	g.mu.Lock()
	if g.runCtx != nil && g.runCtx.Err() != nil {
		g.mu.Unlock()
		return
	}
	if timer := g.monitorTimers[chatID]; timer != nil {
		timer.Stop()
	}
	g.monitorGeneration[chatID]++
	generation := g.monitorGeneration[chatID]
	g.monitorTimers[chatID] = time.AfterFunc(delay, func() { g.flushGroupMonitor(chatID, generation) })
	g.mu.Unlock()
}

func (g *Gateway) flushGroupMonitor(chatID int64, generation uint64) {
	g.mu.Lock()
	if g.monitorGeneration[chatID] != generation {
		g.mu.Unlock()
		return
	}
	delete(g.monitorTimers, chatID)
	delete(g.monitorGeneration, chatID)
	g.mu.Unlock()

	parent := g.monitorContext()
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	release, err := g.locks.AcquireContext(ctx, chatID)
	if err != nil {
		slog.Warn("群监控等待会话锁超时", "chat", chatID, "err", err)
		return
	}
	defer release()
	mon, err := g.store.TelegramGroupMonitor(ctx, chatID)
	if err != nil || mon == nil || !mon.Enabled {
		return
	}
	now := time.Now()
	if !mon.AnalysisThrough.IsZero() {
		leaseAlive := groupMonitorLeaseAlive(mon, now)
		if leaseAlive {
			g.scheduleGroupMonitorAfter(chatID, groupMonitorDebounce)
			return
		}
		staleOwner := mon.AnalysisOwner
		staleThrough := mon.AnalysisThrough
		mon, err = g.store.UpdateTelegramGroupMonitor(ctx, chatID, func(current *store.TelegramGroupMonitor) error {
			if current.AnalysisOwner != staleOwner || !current.AnalysisThrough.Equal(staleThrough) {
				return nil
			}
			current.AnalysisOwner = ""
			current.AnalysisStartedAt = time.Time{}
			current.AnalysisThrough = time.Time{}
			if current.Enabled && current.PendingCount == 0 {
				current.PendingCount = 1
				current.BatchStartedAt = now
			}
			current.UpdatedAt = now
			return nil
		})
		if err != nil || mon == nil || !mon.Enabled {
			return
		}
	}
	if mon.PendingCount <= 0 {
		return
	}
	to := now
	from := mon.LastCheckedAt
	if from.IsZero() || from.Before(mon.CreatedAt) {
		from = mon.CreatedAt
	}
	page, err := g.store.ListChannelMessages(ctx, groupChannel(chatID), from, to, 300)
	if err != nil {
		slog.Warn("读取群监控消息批次失败", "chat", chatID, "err", err)
		updated, saveErr := g.store.UpdateTelegramGroupMonitor(ctx, chatID, func(current *store.TelegramGroupMonitor) error {
			if !current.Enabled {
				return nil
			}
			current.AnalysisFailures++
			if current.BatchStartedAt.IsZero() {
				current.BatchStartedAt = time.Now()
			}
			current.UpdatedAt = time.Now()
			return nil
		})
		if saveErr != nil {
			slog.Warn("保存群监控重试状态失败", "chat", chatID, "err", saveErr)
			return
		}
		if updated == nil || !updated.Enabled {
			return
		}
		g.scheduleGroupMonitorRetry(chatID, updated.AnalysisFailures)
		return
	}
	if len(page.Messages) == 0 {
		_, err := g.store.UpdateTelegramGroupMonitor(ctx, chatID, func(current *store.TelegramGroupMonitor) error {
			if !current.Enabled {
				return nil
			}
			current.LastCheckedAt = to
			current.BatchStartedAt = time.Time{}
			current.PendingCount = 0
			current.AnalysisFailures = 0
			current.Buffer = nil
			current.UpdatedAt = to
			return nil
		})
		if err != nil {
			slog.Warn("确认空群监控批次失败", "chat", chatID, "err", err)
		}
		return
	}
	lines := make([]string, 0, len(page.Messages)+1)
	if page.Total > int64(len(page.Messages)) {
		lines = append(lines, fmt.Sprintf("（本批共 %d 条，仅分析最新 %d 条）", page.Total, len(page.Messages)))
	}
	for _, message := range page.Messages {
		lines = append(lines, fmt.Sprintf("%s %s", message.CreatedAt.In(g.timeZone()).Format("2006-01-02 15:04"), message.Content))
	}
	claimed := false
	mon, err = g.store.UpdateTelegramGroupMonitor(ctx, chatID, func(current *store.TelegramGroupMonitor) error {
		if !current.Enabled || current.PendingCount <= 0 || !current.AnalysisThrough.IsZero() {
			return nil
		}
		current.AnalysisOwner = g.monitorInstanceID
		current.AnalysisStartedAt = time.Now()
		current.AnalysisThrough = to
		current.BatchStartedAt = time.Time{}
		current.PendingCount = 0
		current.Buffer = nil
		current.UpdatedAt = time.Now()
		claimed = true
		return nil
	})
	if err != nil {
		slog.Warn("认领群监控分析批次失败", "chat", chatID, "err", err)
		g.scheduleGroupMonitorAfter(chatID, groupMonitorDebounce)
		return
	}
	if !claimed || mon == nil {
		return
	}
	snapshot := *mon
	go g.evaluateGroupMonitor(parent, snapshot, lines, to)
}

func (g *Gateway) monitorContext() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runCtx != nil {
		return g.runCtx
	}
	return context.Background()
}

func groupMonitorLeaseAlive(mon *store.TelegramGroupMonitor, now time.Time) bool {
	if mon == nil || strings.TrimSpace(mon.AnalysisOwner) == "" || mon.AnalysisThrough.IsZero() || mon.AnalysisStartedAt.IsZero() {
		return false
	}
	// A slightly future timestamp can occur with host clock skew; it is safer to
	// honor that lease than to run the same analysis twice.
	return now.Before(mon.AnalysisStartedAt.Add(groupMonitorAnalysisLease))
}

func (g *Gateway) resumeGroupMonitors(ctx context.Context) {
	monitors, err := g.store.ListTelegramGroupMonitors(ctx, 500)
	if err != nil {
		slog.Warn("恢复群监控批次失败", "err", err)
		return
	}
	for _, mon := range monitors {
		if !mon.Enabled || (mon.PendingCount <= 0 && mon.AnalysisThrough.IsZero()) {
			continue
		}
		startedAt := mon.BatchStartedAt
		if startedAt.IsZero() {
			startedAt = mon.AnalysisStartedAt
		}
		if startedAt.IsZero() {
			startedAt = mon.UpdatedAt
		}
		g.scheduleGroupMonitor(mon.ChatID, startedAt)
	}
}

func (g *Gateway) stopGroupMonitorTimers() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for chatID, timer := range g.monitorTimers {
		timer.Stop()
		delete(g.monitorTimers, chatID)
		delete(g.monitorGeneration, chatID)
	}
}

func (g *Gateway) evaluateGroupMonitor(parent context.Context, mon store.TelegramGroupMonitor, lines []string, through time.Time) {
	success := false
	notified := false
	defer func() {
		if r := recover(); r != nil {
			slog.Error("群监控后台 panic 已恢复", "chat", mon.ChatID, "panic", r)
		}
		g.finishGroupMonitorAnalysis(mon.ChatID, through, success, notified)
	}()
	if len(lines) == 0 {
		success = true
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	title := strings.TrimSpace(mon.GroupTitle)
	if title == "" {
		title = fmt.Sprintf("Telegram 群 %d", mon.ChatID)
	}
	projectLine := ""
	if pj, err := g.store.TelegramGroupProject(ctx, mon.ChatID); err == nil && pj != nil {
		projectLine = "绑定项目：" + pj.Name + "\n"
	}
	input := fmt.Sprintf("群名：%s\n%s监控要求：%s\n\n最近群聊：\n%s",
		title, projectLine, strings.TrimSpace(mon.Instruction), strings.Join(lines, "\n"))
	var out string
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 80*time.Second)
		out, err = g.orch.Summarize(attemptCtx, mon.NotifyUserID, "telegram_group_monitor", groupMonitorSystem, input)
		attemptCancel()
		if err == nil {
			break
		}
		slog.Warn("群监控 AI 判断失败", "chat", mon.ChatID, "user", mon.NotifyUserID, "attempt", attempt, "err", err)
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
	if err != nil {
		return
	}
	if groupMonitorNoNotify(out) {
		success = true
		return
	}
	latest, loadErr := g.store.TelegramGroupMonitor(ctx, mon.ChatID)
	if loadErr != nil || latest == nil {
		return
	}
	if !latest.Enabled || latest.AnalysisOwner != g.monitorInstanceID || !latest.AnalysisThrough.Equal(through) {
		success = true
		return
	}
	if latest.NotifyUserID != mon.NotifyUserID || latest.Instruction != mon.Instruction || latest.GroupTitle != mon.GroupTitle {
		return
	}
	chatID, err := g.privateTelegramChatID(ctx, mon.NotifyUserID)
	if err != nil {
		slog.Warn("群监控找不到通知用户 Telegram", "chat", mon.ChatID, "user", mon.NotifyUserID, "err", err)
		return
	}
	msg := fmt.Sprintf("📣 <b>群监控提醒</b>｜%s\n\n%s", html.EscapeString(title), out)
	if err := g.sendChunks(ctx, chatID, msg); err != nil {
		slog.Warn("发送群监控提醒失败", "chat", mon.ChatID, "user", mon.NotifyUserID, "err", err)
		return
	}
	notified = true
	success = true
}

func (g *Gateway) finishGroupMonitorAnalysis(chatID int64, through time.Time, success, notified bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	release, err := g.locks.AcquireContext(ctx, chatID)
	if err != nil {
		slog.Warn("群监控收尾等待会话锁超时", "chat", chatID, "err", err)
		return
	}
	defer release()
	now := time.Now()
	updated, err := g.store.UpdateTelegramGroupMonitor(ctx, chatID, func(mon *store.TelegramGroupMonitor) error {
		if mon.AnalysisOwner != g.monitorInstanceID || !mon.AnalysisThrough.Equal(through) {
			return nil
		}
		mon.AnalysisOwner = ""
		mon.AnalysisStartedAt = time.Time{}
		mon.AnalysisThrough = time.Time{}
		mon.UpdatedAt = now
		if !mon.Enabled {
			mon.PendingCount = 0
			mon.BatchStartedAt = time.Time{}
			return nil
		}
		if success {
			mon.LastCheckedAt = through
			mon.AnalysisFailures = 0
			if notified {
				mon.LastNotifiedAt = now
			}
		} else {
			mon.AnalysisFailures++
			if mon.PendingCount == 0 {
				mon.PendingCount = 1
			}
			if mon.BatchStartedAt.IsZero() {
				mon.BatchStartedAt = now
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("完成群监控分析批次失败", "chat", chatID, "err", err)
		g.scheduleGroupMonitorAfter(chatID, groupMonitorDebounce)
		return
	}
	if updated == nil || !updated.Enabled || updated.PendingCount <= 0 {
		return
	}
	if success {
		g.scheduleGroupMonitor(chatID, updated.BatchStartedAt)
		return
	}
	g.scheduleGroupMonitorRetry(chatID, updated.AnalysisFailures)
}

func groupMonitorNoNotify(text string) bool {
	clean := strings.TrimSpace(htmlTagTokenRe.ReplaceAllString(text, ""))
	clean = strings.Trim(clean, " \n\r\t。.!！")
	return strings.EqualFold(clean, "NO_NOTIFY") || strings.EqualFold(clean, "无需提醒")
}

func (g *Gateway) privateTelegramChatID(ctx context.Context, userID int64) (int64, error) {
	ident, err := g.store.IdentityOfUser(ctx, userID, Provider)
	if err != nil {
		return 0, err
	}
	raw := strings.TrimSpace(ident.ChatRef)
	if raw == "" {
		raw = strings.TrimSpace(ident.ExternalID)
	}
	chatID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || chatID == 0 {
		return 0, fmt.Errorf("telegram 私聊地址不可用")
	}
	return chatID, nil
}

func (g *Gateway) handleGroupAutoInvite(ctx context.Context, msg *models.Message, chatID, tgID int64) bool {
	if tgID == 0 {
		return false
	}
	raw, err := g.store.GetKV(ctx, store.TelegramGroupAutoInviteKey(chatID))
	if err != nil {
		slog.Warn("读取 Telegram 群自动邀请配置失败", "chat", chatID, "err", err)
		return false
	}
	createdBy, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || createdBy == 0 {
		return false
	}
	name := displayNameFromMessage(msg)
	key := ""
	var expiresAt time.Time
	if inv, err := g.store.TelegramPendingEmployeeInvite(ctx, tgID); err == nil {
		key, expiresAt = inv.Key, inv.ExpiresAt
		if strings.TrimSpace(inv.Name) != "" {
			name = inv.Name
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		slog.Error("读取群自动邀请待领取记录失败", "chat", chatID, "tg_user", tgID, "err", err)
		g.reply(ctx, chatID, "自动邀请创建失败，请稍后再试。")
		return true
	} else {
		bk, err := g.store.CreateBindInvite(ctx, createdBy, 24*time.Hour, name, "", "Telegram 群自动邀请："+msg.Chat.Title)
		if err != nil {
			slog.Error("创建群自动邀请失败", "chat", chatID, "tg_user", tgID, "err", err)
			g.reply(ctx, chatID, "自动邀请创建失败，请稍后再试。")
			return true
		}
		key, expiresAt = bk.Key, bk.ExpiresAt
		if err := g.store.SaveTelegramPendingEmployeeInvite(ctx, store.TelegramPendingEmployeeInvite{
			TelegramUserID: tgID,
			GroupChatID:    chatID,
			Key:            key,
			Name:           name,
			CreatedBy:      createdBy,
			ExpiresAt:      expiresAt,
		}); err != nil {
			slog.Error("保存群自动邀请待领取记录失败", "chat", chatID, "tg_user", tgID, "err", err)
			g.reply(ctx, chatID, "自动邀请创建失败，请稍后再试。")
			return true
		}
	}
	private := fmt.Sprintf("🎟 <b>%s</b>，这是你的公司系统一次性邀请。\n\n", html.EscapeString(name))
	if link := g.employeeInviteLink(key); link != "" {
		private += fmt.Sprintf("点击绑定：%s\n", html.EscapeString(link))
	}
	private += fmt.Sprintf("兜底邀请码：<code>%s</code>\n有效期至 %s，仅可使用一次。",
		html.EscapeString(key), g.formatTime(expiresAt))
	if _, err := g.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: tgID, Text: private, ParseMode: models.ParseModeHTML}); err == nil {
		g.reply(ctx, chatID, fmt.Sprintf("✅ 已把一次性邀请私发给 %s。", html.EscapeString(name)))
		return true
	}
	g.reply(ctx, chatID, fmt.Sprintf("✅ 已为 %s 准备好一次性邀请。请他私聊我发送 /start，我会自动完成绑定；邀请码不会在群里公开。", html.EscapeString(name)))
	return true
}

func (g *Gateway) employeeInviteLink(key string) string {
	username := strings.TrimPrefix(strings.TrimSpace(g.botUsername()), "@")
	if username == "" {
		return ""
	}
	return "https://t.me/" + username + "?start=" + key
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
	text := g.messageText(ctx, msg)
	memorySourceText := telegramMemorySourceText(msg, text)
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

	if intakes := g.saveIncomingTelegramFiles(ctx, msg, u); len(intakes) > 0 {
		instruction := strings.TrimSpace(nonMediaText(msg))
		saved, failed := splitTelegramFileIntakes(intakes)
		if len(saved) == 0 {
			g.replyFileIntakeResults(ctx, chatID, saved, failed, true)
			return
		}
		if instruction == "" {
			g.replyFileIntakeResults(ctx, chatID, saved, failed, true)
			return
		}
		text = telegramFileContext(saved, failed, instruction)
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
	case "/token":
		g.handleTokenCommand(ctx, chatID, u, commandArgs(text))
		return
	}

	// 流式：发占位消息，把最终答复的增量渐进编辑上去（本地模型慢，不让用户干等）。
	g.sendTyping(ctx, chatID)
	ed := g.newStreamEditor(ctx, chatID)
	turnCtx := chat.WithTurnSourceKey(ctx, telegramTurnSourceKey(msg))
	turnCtx = chat.WithMemorySourceText(turnCtx, memorySourceText)
	turnCtx = chat.WithMessageEnvelope(turnCtx, telegramMessageEnvelope(msg, u))
	reply, err := g.orch.HandleMessageStreamResult(turnCtx, u, Provider, text, ed.onDelta)
	if err != nil {
		if errors.Is(err, store.ErrTurnInProgress) || errors.Is(err, store.ErrTurnFailed) ||
			errors.Is(err, store.ErrConflict) {
			slog.Info("忽略已领取的 Telegram 私聊消息重放", "chat", chatID, "message", msg.ID, "err", err)
			ed.discard(ctx)
			return
		}
		slog.Error("对话轮次失败", "user", u.ID, "err", err)
		ed.fail(ctx, "这轮对话出错了，请重试；连续失败请联系管理员。")
		return
	}
	if reply.AlreadyDelivered || (reply.Replayed && reply.DeliveryStatus == "failed") {
		if reply.DeliveryStatus == "failed" {
			slog.Warn("Telegram 私聊消息曾部分或完整投递失败，禁止盲目自动重发", "chat", chatID, "turn", reply.TurnID)
		}
		ed.discard(ctx)
		return
	}
	if err := ed.finish(ctx, reply.Text); err != nil {
		slog.Error("对话答复投递失败", "user", u.ID, "turn", reply.TurnID, "err", err)
		g.recordTurnDelivery(ctx, reply.TurnID, err)
		return
	}
	g.recordTurnDelivery(ctx, reply.TurnID, nil)
}

func telegramTurnSourceKey(msg *models.Message) string {
	if msg == nil {
		return ""
	}
	return fmt.Sprintf("telegram:message:%d:%d", msg.Chat.ID, msg.ID)
}

func telegramMessageEnvelope(msg *models.Message, u *store.User) store.MessageEnvelope {
	if msg == nil {
		return store.MessageEnvelope{}
	}
	var actorUserID *int64
	if u != nil {
		id := u.ID
		actorUserID = &id
	}
	externalActor := ""
	if msg.From != nil {
		externalActor = strconv.FormatInt(msg.From.ID, 10)
	} else if msg.SenderChat != nil {
		externalActor = "chat:" + strconv.FormatInt(msg.SenderChat.ID, 10)
	}
	replyRef := ""
	if msg.ReplyToMessage != nil {
		replyRef = strconv.Itoa(msg.ReplyToMessage.ID)
	}
	threadRef := ""
	if msg.MessageThreadID != 0 {
		threadRef = strconv.Itoa(msg.MessageThreadID)
	}
	var sourceAt *time.Time
	if msg.Date > 0 {
		at := time.Unix(int64(msg.Date), 0).UTC()
		sourceAt = &at
	}
	metadata, _ := json.Marshal(map[string]any{
		"edit_date":      msg.EditDate,
		"media_group_id": msg.MediaGroupID,
		"sender_chat_id": func() int64 {
			if msg.SenderChat != nil {
				return msg.SenderChat.ID
			}
			return 0
		}(),
	})
	displayName := displayNameFromMessage(msg)
	if u != nil {
		displayName = u.Name
	}
	return store.MessageEnvelope{
		Provider: Provider, ExternalChatRef: strconv.FormatInt(msg.Chat.ID, 10),
		ExternalMessageRef: strconv.Itoa(msg.ID), ActorUserID: actorUserID,
		ExternalActorRef: externalActor, ActorDisplayName: displayName,
		ReplyToExternalRef: replyRef, ThreadRef: threadRef, SourceCreatedAt: sourceAt,
		Metadata: metadata,
	}
}

func telegramMemorySourceText(msg *models.Message, fallback string) string {
	if raw := strings.TrimSpace(nonMediaText(msg)); raw != "" {
		return raw
	}
	return strings.TrimSpace(fallback)
}

func (g *Gateway) recordTurnDelivery(ctx context.Context, turnID int64, cause error) {
	if turnID <= 0 {
		return
	}
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var err error
	if cause == nil {
		err = g.orch.MarkTurnDelivered(ackCtx, turnID)
	} else {
		err = g.orch.MarkTurnDeliveryFailed(ackCtx, turnID, cause)
	}
	if err != nil {
		slog.Warn("对话投递状态落库失败", "turn", turnID, "delivery_error", cause, "err", err)
	}
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
		g.reply(ctx, chatID, "模型名不合法。先用 <code>/model</code> 查看当前可选模型；恢复默认：<code>/model reset</code>。")
		return
	}
	loaded, err := g.loadedModels(ctx)
	if err != nil {
		slog.Warn("查询可选模型失败，拒绝切换", "err", err)
		g.reply(ctx, chatID, "暂时无法读取可选模型列表，未切换。请稍后再试。")
		return
	}
	if !modelInList(args, loaded) {
		g.reply(ctx, chatID, "这个模型当前不可用，未切换。\n\n"+loadedModelsHelp(loaded))
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

const apiTokenConfirmTTL = 10 * time.Minute

func apiTokenConfirmKey(userID int64) string {
	return fmt.Sprintf("telegram.pending_api_token:%d", userID)
}

func (g *Gateway) handleTokenCommand(ctx context.Context, chatID int64, u *store.User, args string) {
	args = strings.ToLower(strings.TrimSpace(args))
	switch args {
	case "":
		g.reply(ctx, chatID, g.apiTokenStatusMessage(ctx, u))
	case "new", "rotate", "regen", "regenerate":
		expiresAt := time.Now().UTC().Add(apiTokenConfirmTTL)
		if err := g.store.SetKV(ctx, apiTokenConfirmKey(u.ID), strconv.FormatInt(expiresAt.Unix(), 10)); err != nil {
			slog.Error("登记 Access Token 换发确认失败", "user", u.ID, "err", err)
			g.reply(ctx, chatID, "登记确认失败，请稍后再试。")
			return
		}
		g.reply(ctx, chatID,
			"⚠️ <b>Access Token 换发确认</b>\n\n"+
				"这会立即作废你当前的控制中心/API Access Token，并生成一个新的 token。\n"+
				"旧 token 明文无法查询；新 token 只会显示一次。\n\n"+
				"确认换发请在 10 分钟内发送：<code>/token confirm</code>\n"+
				"取消请发送：<code>/token cancel</code>")
	case "confirm":
		raw, err := g.store.GetKV(ctx, apiTokenConfirmKey(u.ID))
		if err != nil {
			slog.Error("读取 Access Token 换发确认失败", "user", u.ID, "err", err)
			g.reply(ctx, chatID, "读取确认状态失败，请稍后再试。")
			return
		}
		expiresUnix, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if expiresUnix <= time.Now().UTC().Unix() {
			_ = g.store.SetKV(ctx, apiTokenConfirmKey(u.ID), "")
			g.reply(ctx, chatID, "没有有效的换发确认。请先发送 <code>/token new</code>，再发送 <code>/token confirm</code>。")
			return
		}
		plain, err := g.store.IssueAPIToken(ctx, u.ID)
		if err != nil {
			slog.Error("Access Token 换发失败", "user", u.ID, "err", err)
			g.reply(ctx, chatID, "Access Token 换发失败，请稍后再试。")
			return
		}
		_ = g.store.SetKV(ctx, apiTokenConfirmKey(u.ID), "")
		g.reply(ctx, chatID,
			"✅ <b>新的 Access Token 已生成</b>\n\n"+
				"<code>"+html.EscapeString(plain)+"</code>\n\n"+
				"请现在保存；系统只保存哈希，之后无法查询明文。")
	case "cancel":
		_ = g.store.SetKV(ctx, apiTokenConfirmKey(u.ID), "")
		g.reply(ctx, chatID, "已取消 Access Token 换发。")
	default:
		g.reply(ctx, chatID,
			"用法：\n"+
				"• <code>/token</code> 查看是否已有 token\n"+
				"• <code>/token new</code> 申请换发\n"+
				"• <code>/token confirm</code> 确认换发并显示新 token\n"+
				"• <code>/token cancel</code> 取消")
	}
}

func (g *Gateway) apiTokenStatusMessage(ctx context.Context, u *store.User) string {
	st, err := g.store.APITokenStatus(ctx, u.ID)
	if err != nil {
		slog.Error("读取 Access Token 状态失败", "user", u.ID, "err", err)
		return "读取 Access Token 状态失败，请稍后再试。"
	}
	if st == nil || !st.Exists {
		return "当前没有控制中心/API Access Token。\n\n获取新的：<code>/token new</code>"
	}
	return fmt.Sprintf("当前已有控制中心/API Access Token。\n创建时间：<code>%s</code>\n\n明文无法查询；忘记了只能换发：<code>/token new</code>",
		html.EscapeString(g.formatTime(st.CreatedAt)))
}

func (g *Gateway) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	loc := g.timeZone()
	return t.In(loc).Format("2006-01-02 15:04:05 -07:00") + " (" + loc.String() + ")"
}

func (g *Gateway) timeZone() *time.Location {
	return orLocalTimeZone(g.tz)
}

func orLocalTimeZone(tz *time.Location) *time.Location {
	if tz != nil {
		return tz
	}
	return time.Local
}

func (g *Gateway) modelStatus(ctx context.Context) string {
	model, err := g.store.GetKV(ctx, store.KVAIModel)
	if err != nil {
		slog.Warn("读取运行时模型失败", "err", err)
		model = ""
	}
	model = strings.TrimSpace(model)
	loaded, lerr := g.loadedModels(ctx)
	loadedText := ""
	if lerr != nil {
		loadedText = "\n\n可选模型：查询失败，稍后再试。"
	} else {
		loadedText = "\n\n" + loadedModelsHelp(loaded)
	}
	if model == "" {
		return fmt.Sprintf("当前模型：<code>%s</code>\n来源：配置文件默认值\n切换：<code>/model 模型名</code>\n恢复默认：<code>/model reset</code>%s",
			html.EscapeString(g.defaultModel), loadedText)
	}
	return fmt.Sprintf("当前模型：<code>%s</code>\n来源：运行时设置\n默认模型：<code>%s</code>\n切换：<code>/model 模型名</code>\n恢复默认：<code>/model reset</code>%s",
		html.EscapeString(model), html.EscapeString(g.defaultModel), loadedText)
}

func (g *Gateway) loadedModels(ctx context.Context) ([]string, error) {
	apiBase := strings.TrimRight(strings.TrimSpace(g.modelBaseURL), "/")
	if apiBase == "" {
		return nil, errors.New("ai base_url 未配置")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
	}
	key := strings.TrimSpace(g.modelAPIKey)
	// Prefer a gateway's runtime list when it exposes the Ollama-compatible
	// endpoint. Official OpenAI and generic compatible providers fall back to
	// the standard model catalog.
	runtimeBase := strings.TrimSuffix(apiBase, "/v1")
	models, runtimeErr := fetchOllamaRuntimeModels(ctx, runtimeBase+"/ollama/api/ps", key)
	if runtimeErr == nil {
		return models, nil
	}
	models, catalogErr := fetchOpenAIModels(ctx, apiBase+"/models", key)
	if catalogErr != nil {
		return nil, errors.Join(runtimeErr, catalogErr)
	}
	return models, nil
}

func fetchModelJSON(ctx context.Context, endpoint, key string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s status %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func fetchOllamaRuntimeModels(ctx context.Context, endpoint, key string) ([]string, error) {
	var body struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := fetchModelJSON(ctx, endpoint, key, &body); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range body.Models {
		name := strings.TrimSpace(m.Model)
		if name == "" {
			name = strings.TrimSpace(m.Name)
		}
		if name == "" || seen[name] || !validModelName(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

func fetchOpenAIModels(ctx context.Context, endpoint, key string) ([]string, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := fetchModelJSON(ctx, endpoint, key, &body); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(body.Data))
	for _, item := range body.Data {
		name := strings.TrimSpace(item.ID)
		if name == "" || seen[name] || !validModelName(name) {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func loadedModelsHelp(models []string) string {
	if len(models) == 0 {
		return "可选模型：暂无。"
	}
	var b strings.Builder
	b.WriteString("可选模型（只能从这里选择）：\n")
	for _, m := range models {
		fmt.Fprintf(&b, "- <code>%s</code>\n", html.EscapeString(m))
	}
	b.WriteString("\n切换示例：<code>/model ")
	b.WriteString(html.EscapeString(models[0]))
	b.WriteString("</code>")
	return strings.TrimSpace(b.String())
}

func modelInList(name string, models []string) bool {
	for _, m := range models {
		if name == m {
			return true
		}
	}
	return false
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
	if ok {
		// An explicit token always wins over an older group-issued pending invite.
		// This lets the user recover by presenting a newly issued credential.
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
		if err := g.store.ClearTelegramPendingEmployeeInvite(ctx, msg.From.ID); err != nil {
			slog.Warn("清理已被显式邀请覆盖的待领取记录失败", "tg_user", msg.From.ID, "err", err)
		}
		g.reply(ctx, chatID, bindSuccessMessage(u.Name))
		g.bus.EmitRequired("员工加入", invitedBy,
			fmt.Sprintf("新员工「%s」刚通过你签发的邀请完成 Telegram 绑定，正式加入公司。", u.Name))
		return
	}

	if g.consumePendingEmployeeInvite(ctx, msg.From.ID, chatID, name, ident) {
		return
	}

	hasSuperadmin, err := g.store.HasSuperadmin(ctx)
	if err != nil {
		slog.Warn("查询超管状态失败", "err", err)
		hasSuperadmin = true
	}
	g.reply(ctx, chatID, unboundHelpMessage(!hasSuperadmin))
}

func (g *Gateway) consumePendingEmployeeInvite(ctx context.Context, tgUserID, chatID int64, name string, ident store.Identity) bool {
	inv, err := g.store.TelegramPendingEmployeeInvite(ctx, tgUserID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("读取 Telegram 待领取邀请失败", "tg_user", tgUserID, "err", err)
		}
		return false
	}
	if strings.TrimSpace(inv.Name) != "" {
		name = inv.Name
	}
	u, invitedBy, err := g.store.BindUserWithKey(ctx, inv.Key, name, ident)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if clearErr := g.store.ClearTelegramPendingEmployeeInvite(ctx, tgUserID); clearErr != nil {
				slog.Warn("清理已失效待领取邀请失败", "tg_user", tgUserID, "err", clearErr)
			}
			g.reply(ctx, chatID, "这枚待领取邀请已失效，请让管理员重新授权。")
			return true
		}
		slog.Error("待领取邀请绑定失败", "tg_user", tgUserID, "err", err)
		g.reply(ctx, chatID, "绑定失败，请稍后再试。")
		return true
	}
	if clearErr := g.store.ClearTelegramPendingEmployeeInvite(ctx, tgUserID); clearErr != nil {
		slog.Warn("清理已领取邀请失败", "tg_user", tgUserID, "err", clearErr)
	}
	g.reply(ctx, chatID, bindSuccessMessage(u.Name))
	g.bus.EmitRequired("员工加入", invitedBy,
		fmt.Sprintf("新员工「%s」刚通过群自动邀请完成 Telegram 绑定，正式加入公司。", u.Name))
	return true
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
	_, err = g.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: telegramPlainText(chunk)})
	return err
}

func (g *Gateway) sendTyping(ctx context.Context, chatID int64) {
	_, _ = g.bot.SendChatAction(ctx, &bot.SendChatActionParams{ChatID: chatID, Action: models.ChatActionTyping})
}

// EnsureTelegramGroupSession 在采集能力报告成功前建立持久消息落点。
func (g *Gateway) EnsureTelegramGroupSession(ctx context.Context, chatID int64, ownerID int64) error {
	if g == nil || g.orch == nil || g.store == nil || chatID == 0 || ownerID <= 0 {
		return errors.New("telegram 群会话参数无效")
	}
	u, err := g.store.UserByID(ctx, ownerID)
	if err != nil {
		return err
	}
	if u.Status != store.UserActive {
		return errors.New("群会话所属用户未激活")
	}
	return g.orch.TouchGroupSession(ctx, u, groupChannel(chatID))
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
				ChatID: chatID, Text: telegramPlainText(chunk), DisableNotification: disableNotification,
			})
			if err != nil {
				return 0, err
			}
		}
		lastID = msg.ID
	}
	return lastID, nil
}

func (g *Gateway) GetTelegramGroupMemberCount(ctx context.Context, chatID int64) (int, error) {
	return g.bot.GetChatMemberCount(ctx, &bot.GetChatMemberCountParams{ChatID: chatID})
}

func (g *Gateway) GetTelegramGroupAdministrators(ctx context.Context, chatID int64) ([]tools.TelegramGroupMember, error) {
	admins, err := g.bot.GetChatAdministrators(ctx, &bot.GetChatAdministratorsParams{ChatID: chatID})
	if err != nil {
		return nil, err
	}
	out := make([]tools.TelegramGroupMember, 0, len(admins))
	for i := range admins {
		if m := telegramGroupMemberFromChatMember(&admins[i], 0); m != nil {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (g *Gateway) GetTelegramGroupMember(ctx context.Context, chatID int64, userID int64) (*tools.TelegramGroupMember, error) {
	member, err := g.bot.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return nil, err
	}
	if m := telegramGroupMemberFromChatMember(member, userID); m != nil {
		return m, nil
	}
	return &tools.TelegramGroupMember{UserID: userID, Status: string(member.Type)}, nil
}

func (g *Gateway) GetTelegramGroupBotMember(ctx context.Context, chatID int64) (*tools.TelegramGroupMember, error) {
	id := g.botID()
	if id == 0 {
		me, err := g.bot.GetMe(ctx)
		if err != nil {
			return nil, err
		}
		g.mu.Lock()
		g.self = me
		g.mu.Unlock()
		id = me.ID
	}
	return g.GetTelegramGroupMember(ctx, chatID, id)
}

func telegramGroupMemberFromUser(u *models.User, status string) tools.TelegramGroupMember {
	return tools.TelegramGroupMember{
		UserID:   u.ID,
		Name:     displayName(u),
		Username: u.Username,
		Status:   status,
		IsBot:    u.IsBot,
	}
}

func telegramGroupMemberFromChatMember(m *models.ChatMember, fallbackUserID int64) *tools.TelegramGroupMember {
	if m == nil {
		return nil
	}
	u := memberUser(m)
	if u == nil {
		return &tools.TelegramGroupMember{UserID: fallbackUserID, Status: string(m.Type)}
	}
	out := telegramGroupMemberFromUser(u, string(m.Type))
	if m.Type == models.ChatMemberTypeOwner {
		out.Rights = []string{"owner"}
	}
	if m.Type == models.ChatMemberTypeAdministrator && m.Administrator != nil {
		out.Rights = telegramAdminRights(m.Administrator)
	}
	return &out
}

func telegramAdminRights(a *models.ChatMemberAdministrator) []string {
	if a == nil {
		return nil
	}
	var rights []string
	add := func(ok bool, name string) {
		if ok {
			rights = append(rights, name)
		}
	}
	add(a.CanManageChat, "manage_chat")
	add(a.CanDeleteMessages, "delete_messages")
	add(a.CanManageVideoChats, "manage_video_chats")
	add(a.CanRestrictMembers, "restrict_members")
	add(a.CanPromoteMembers, "promote_members")
	add(a.CanChangeInfo, "change_info")
	add(a.CanInviteUsers, "invite_users")
	add(a.CanPostMessages, "post_messages")
	add(a.CanEditMessages, "edit_messages")
	add(a.CanPinMessages, "pin_messages")
	add(a.CanManageTopics, "manage_topics")
	return rights
}

func (g *Gateway) EditTelegramGroupMessage(ctx context.Context, chatID int64, messageID int, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("消息内容不能为空")
	}
	htmlText := toTelegramHTML(text)
	_, err := g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: chatID, MessageID: messageID, Text: htmlText, ParseMode: models.ParseModeHTML,
	})
	if err == nil {
		return nil
	}
	_, err = g.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID: chatID, MessageID: messageID, Text: telegramPlainText(htmlText),
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

// messageText 把消息转成安全文本。Telegram 原始 file_id 不进入模型上下文；
// 可分析文件必须先经过统一接收流水并转换成 nbco 系统 file_id。
func (g *Gateway) messageText(ctx context.Context, msg *models.Message) string {
	var parts []string
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = "未命名"
		}
		parts = append(parts, fmt.Sprintf("[用户发来文件「%s」]", name))
	}
	if n := len(msg.Photo); n > 0 {
		// Photo 按尺寸升序，取最大的一张。
		parts = append(parts, "[用户发来图片]")
	}
	if msg.Video != nil {
		parts = append(parts, "[用户发来视频]")
	}
	if msg.Animation != nil {
		parts = append(parts, "[用户发来动画文件]")
	}
	if msg.Audio != nil {
		parts = append(parts, "[用户发来音频文件]")
	}
	if msg.VideoNote != nil {
		parts = append(parts, "[用户发来视频留言]")
	}
	if msg.Voice != nil {
		if text := g.transcribeVoice(ctx, msg.Voice.FileID); text != "" {
			parts = append(parts, "[语音转写] "+text)
		} else if g.stt != nil {
			parts = append(parts, "[用户发来语音，转写失败，请让用户改用文字或稍后重试]")
		} else {
			parts = append(parts, "[用户发来语音，但未配置转写服务，请让用户改用文字]")
		}
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

func telegramPlainText(s string) string {
	s = htmlLooseTagRe.ReplaceAllString(s, "")
	s = htmlMalformedKnownTagRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(html.UnescapeString(s))
	if s == "" {
		return "（空回复）"
	}
	return s
}

// voiceDownloadLimit 语音文件下载上限（TG 语音条通常远小于此）。
const voiceDownloadLimit = 20 << 20
const telegramCloudFileDownloadLimit = 20 << 20
const telegramFileDownloadLimit = 200 << 20

type incomingTelegramFile struct {
	fileID      string
	uniqueID    string
	externalRef string
	name        string
	mime        string
	sizeBytes   int64
}

type incomingTelegramFileResult struct {
	input        incomingTelegramFile
	file         *store.File
	intake       *store.FileIntake
	errorCode    string
	errorMessage string
	err          error
}

type telegramFileIntakeError struct {
	code        string
	userMessage string
	cause       error
}

func (e *telegramFileIntakeError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return e.userMessage
}

func (e *telegramFileIntakeError) Unwrap() error { return e.cause }

func newTelegramFileIntakeError(code, userMessage string, cause error) error {
	return &telegramFileIntakeError{code: code, userMessage: userMessage, cause: cause}
}

func telegramFileIntakeFailure(err error) (string, string) {
	var intakeErr *telegramFileIntakeError
	if errors.As(err, &intakeErr) {
		return intakeErr.code, intakeErr.userMessage
	}
	return "receive_failed", "文件接收失败，文件没有进入 nbco。"
}

func (g *Gateway) saveIncomingTelegramFiles(ctx context.Context, msg *models.Message, u *store.User) []incomingTelegramFileResult {
	if msg == nil || u == nil {
		return nil
	}
	var incoming []incomingTelegramFile
	if msg.Document != nil {
		name := strings.TrimSpace(msg.Document.FileName)
		if name == "" {
			name = "document"
		}
		incoming = append(incoming, incomingTelegramFile{
			fileID: msg.Document.FileID, uniqueID: msg.Document.FileUniqueID,
			name: name, mime: msg.Document.MimeType, sizeBytes: msg.Document.FileSize,
		})
	}
	if n := len(msg.Photo); n > 0 {
		photo := msg.Photo[n-1]
		incoming = append(incoming, incomingTelegramFile{
			fileID: photo.FileID, uniqueID: photo.FileUniqueID,
			name: fmt.Sprintf("photo-%d.jpg", msg.ID), mime: "image/jpeg", sizeBytes: int64(photo.FileSize),
		})
	}
	if msg.Video != nil {
		name := strings.TrimSpace(msg.Video.FileName)
		if name == "" {
			name = fmt.Sprintf("video-%d.mp4", msg.ID)
		}
		incoming = append(incoming, incomingTelegramFile{
			fileID: msg.Video.FileID, uniqueID: msg.Video.FileUniqueID,
			name: name, mime: msg.Video.MimeType, sizeBytes: msg.Video.FileSize,
		})
	}
	if msg.Animation != nil {
		name := strings.TrimSpace(msg.Animation.FileName)
		if name == "" {
			name = fmt.Sprintf("animation-%d.mp4", msg.ID)
		}
		incoming = append(incoming, incomingTelegramFile{
			fileID: msg.Animation.FileID, uniqueID: msg.Animation.FileUniqueID,
			name: name, mime: msg.Animation.MimeType, sizeBytes: msg.Animation.FileSize,
		})
	}
	if msg.Audio != nil {
		name := strings.TrimSpace(msg.Audio.FileName)
		if name == "" {
			name = fmt.Sprintf("audio-%d", msg.ID)
		}
		incoming = append(incoming, incomingTelegramFile{
			fileID: msg.Audio.FileID, uniqueID: msg.Audio.FileUniqueID,
			name: name, mime: msg.Audio.MimeType, sizeBytes: msg.Audio.FileSize,
		})
	}
	if msg.VideoNote != nil {
		incoming = append(incoming, incomingTelegramFile{
			fileID: msg.VideoNote.FileID, uniqueID: msg.VideoNote.FileUniqueID,
			name: fmt.Sprintf("video-note-%d.mp4", msg.ID), mime: "video/mp4", sizeBytes: int64(msg.VideoNote.FileSize),
		})
	}
	if len(incoming) == 0 {
		return nil
	}
	results := make([]incomingTelegramFileResult, 0, len(incoming))
	for i := range incoming {
		in := incoming[i]
		in.externalRef = telegramFileExternalRef(msg.ID, in.uniqueID, in.fileID)
		result := incomingTelegramFileResult{input: in}
		intake, err := g.store.CreateFileIntake(ctx, store.FileIntake{
			UserID: u.ID, Source: "telegram", ExternalRef: in.externalRef,
			OriginalName: in.name, MIMEType: in.mime, SizeBytes: in.sizeBytes,
		})
		if err != nil {
			result.errorCode = "intake_record_failed"
			result.errorMessage = "系统未能建立文件接收记录，文件没有进入 nbco。"
			result.err = err
			slog.Warn("Telegram 文件接收记录创建失败", "user", u.ID, "name", in.name, "err", err)
			results = append(results, result)
			continue
		}
		result.intake = intake
		if intake.Status == "saved" && intake.FileID != nil {
			f, fileErr := g.store.FileByID(ctx, *intake.FileID)
			if fileErr == nil {
				result.file = f
				results = append(results, result)
				continue
			}
			slog.Warn("Telegram 重放接收记录指向缺失文件", "user", u.ID, "intake", intake.ID, "file", *intake.FileID, "err", fileErr)
		}
		f, err := g.saveTelegramFile(ctx, u.ID, in)
		if err != nil {
			result.errorCode, result.errorMessage = telegramFileIntakeFailure(err)
			result.err = err
			if markErr := g.store.FailFileIntake(ctx, intake.ID, result.errorCode, result.errorMessage); markErr != nil {
				slog.Warn("Telegram 文件失败状态保存失败", "user", u.ID, "intake", intake.ID, "err", markErr)
			}
			slog.Warn("Telegram 文件暂存失败", "user", u.ID, "name", in.name,
				"size", in.sizeBytes, "code", result.errorCode, "err", err)
			results = append(results, result)
			continue
		}
		result.file = f
		canonicalFileID, err := g.store.CompleteFileIntake(ctx, intake.ID, f.ID, telegramMaterialSourceRef(msg))
		if err != nil {
			slog.Warn("Telegram 文件成功状态保存失败", "user", u.ID, "intake", intake.ID, "file", f.ID, "err", err)
			_ = g.store.DeleteOrphanFileRow(ctx, f.ID)
			result.file = nil
			result.errorCode = "intake_finalize_failed"
			result.errorMessage = "文件已下载但未能完成接收登记，请重新发送。"
			result.err = err
			results = append(results, result)
			continue
		}
		if canonicalFileID != f.ID {
			_ = g.store.DeleteOrphanFileRow(ctx, f.ID)
			f, err = g.store.FileByID(ctx, canonicalFileID)
			if err != nil {
				result.file = nil
				result.errorCode = "intake_replay_file_missing"
				result.errorMessage = "文件接收记录存在，但原文件暂时不可用。"
				result.err = err
				results = append(results, result)
				continue
			}
		}
		result.file = f
		results = append(results, result)
	}
	return results
}

func (g *Gateway) saveTelegramFile(ctx context.Context, userID int64, in incomingTelegramFile) (*store.File, error) {
	if strings.TrimSpace(g.fileStorePath) == "" {
		return nil, newTelegramFileIntakeError("storage_not_configured", "文件存储尚未配置，文件没有进入 nbco。", errors.New("file_store_path 未配置"))
	}
	if in.sizeBytes > telegramFileDownloadLimit {
		return nil, newTelegramFileIntakeError("nbco_size_limit",
			fmt.Sprintf("文件超过 nbco 的 %s 接收上限。", formatTelegramBytes(telegramFileDownloadLimit)), nil)
	}
	if g.usesTelegramCloudAPI() && in.sizeBytes > telegramCloudFileDownloadLimit {
		return nil, newTelegramFileIntakeError("telegram_cloud_limit",
			fmt.Sprintf("Telegram 云端 Bot API 只能下载不超过 %s 的文件；文件没有进入 nbco。", formatTelegramBytes(telegramCloudFileDownloadLimit)), nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tf, err := g.bot.GetFile(ctx, &bot.GetFileParams{FileID: in.fileID})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "file is too big") {
			return nil, newTelegramFileIntakeError("telegram_cloud_limit",
				fmt.Sprintf("Telegram 云端 Bot API 只能下载不超过 %s 的文件；文件没有进入 nbco。", formatTelegramBytes(telegramCloudFileDownloadLimit)), err)
		}
		return nil, newTelegramFileIntakeError("telegram_get_file_failed", "Telegram 未提供可下载的文件信息，请稍后重试。", err)
	}
	src, contentType, err := g.openTelegramFile(ctx, tf)
	if err != nil {
		code := "telegram_download_failed"
		message := "Telegram 文件下载失败，请稍后重试。"
		if filepath.IsAbs(tf.FilePath) {
			code = "local_bot_api_file_failed"
			message = "本机 Telegram 文件服务未能打开该文件，请稍后重试。"
		}
		return nil, newTelegramFileIntakeError(code, message, err)
	}
	defer src.Close()
	if contentType == "" {
		contentType = in.mime
	}
	f, err := g.persistTelegramFile(ctx, userID, in, src, contentType)
	if err != nil {
		return nil, wrapTelegramFileStorageError(err)
	}
	return f, nil
}

func (g *Gateway) openTelegramFile(ctx context.Context, file *models.File) (io.ReadCloser, string, error) {
	if file == nil || strings.TrimSpace(file.FilePath) == "" {
		return nil, "", errors.New("telegram file path is empty")
	}
	if filepath.IsAbs(file.FilePath) {
		if g.usesTelegramCloudAPI() {
			return nil, "", errors.New("cloud Bot API returned an absolute file path")
		}
		local, err := os.Open(file.FilePath)
		return local, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.bot.FileDownloadLink(file), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := telegramFileHTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("download status: %d", resp.StatusCode)
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

func (g *Gateway) usesTelegramCloudAPI() bool {
	return g.telegramAPIURL == "" || strings.Contains(strings.ToLower(g.telegramAPIURL), "api.telegram.org")
}

func wrapTelegramFileStorageError(err error) error {
	var intakeErr *telegramFileIntakeError
	if errors.As(err, &intakeErr) {
		return err
	}
	return newTelegramFileIntakeError("storage_failed", "文件下载后暂存失败，请稍后重试。", err)
}

func (g *Gateway) persistTelegramFile(ctx context.Context, userID int64, in incomingTelegramFile, src io.Reader, contentType string) (*store.File, error) {
	if err := safefs.EnsurePrivateDir(g.fileStorePath); err != nil {
		return nil, fmt.Errorf("创建文件存储目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(g.fileStorePath, ".tg-upload-*")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	h := sha256.New()
	limited := &io.LimitedReader{R: src, N: telegramFileDownloadLimit + 1}
	n, err := io.Copy(tmp, io.TeeReader(limited, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("保存下载文件失败: %w", err)
	}
	if n > telegramFileDownloadLimit || limited.N == 0 {
		return nil, newTelegramFileIntakeError("nbco_size_limit",
			fmt.Sprintf("文件超过 nbco 的 %s 接收上限。", formatTelegramBytes(telegramFileDownloadLimit)), nil)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	rel := filepath.Join(sum[:2], sum)
	dst := filepath.Join(g.fileStorePath, rel)
	if err := safefs.EnsurePrivateDir(filepath.Dir(dst)); err != nil {
		return nil, fmt.Errorf("创建存储目录失败: %w", err)
	}
	moved, err := safefs.InstallContentFile(tmpName, dst, sum)
	if err != nil {
		return nil, fmt.Errorf("落盘失败: %w", err)
	}
	if moved {
		tmpName = ""
	}
	name := filepath.Base(strings.TrimSpace(in.name))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "telegram-file"
	}
	mimeType := strings.TrimSpace(in.mime)
	if mimeType == "" {
		mimeType = contentType
	}
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(name))
	}
	uid := userID
	return g.store.CreateFile(ctx, &store.File{
		Source: "telegram", OriginalName: name, MIMEType: mimeType,
		SizeBytes: n, SHA256: sum, StoragePath: rel, CreatedBy: &uid,
	})
}

func telegramFileExternalRef(messageID int, uniqueID, fileID string) string {
	ref := strings.TrimSpace(uniqueID)
	if ref == "" {
		sum := sha256.Sum256([]byte(fileID))
		ref = hex.EncodeToString(sum[:8])
	}
	return fmt.Sprintf("%d:%s", messageID, ref)
}

func telegramMaterialSourceRef(msg *models.Message) string {
	if msg == nil {
		return ""
	}
	if mediaGroupID := strings.TrimSpace(msg.MediaGroupID); mediaGroupID != "" {
		return "media-group:" + mediaGroupID
	}
	return strconv.Itoa(msg.ID)
}

func splitTelegramFileIntakes(results []incomingTelegramFileResult) ([]store.File, []incomingTelegramFileResult) {
	saved := make([]store.File, 0, len(results))
	failed := make([]incomingTelegramFileResult, 0, len(results))
	for _, result := range results {
		if result.file != nil {
			saved = append(saved, *result.file)
		} else {
			failed = append(failed, result)
		}
	}
	return saved, failed
}

func failedFilesPrompt(failed []incomingTelegramFileResult) string {
	var b strings.Builder
	b.WriteString("[本轮未能进入 nbco 的文件]\n")
	for _, result := range failed {
		fmt.Fprintf(&b, "- 文件名=%s；大小=%s；状态=接收失败；原因=%s\n",
			result.input.name, formatTelegramBytes(result.input.sizeBytes), result.errorMessage)
	}
	b.WriteString("不得声称这些文件已保存、已读取或可分析，也不要编造不存在的上传命令。")
	return strings.TrimSpace(b.String())
}

func telegramFileContext(saved []store.File, failed []incomingTelegramFileResult, instruction string) string {
	parts := make([]string, 0, 3)
	if len(saved) > 0 {
		parts = append(parts, savedFilesPrompt(saved))
	}
	if len(failed) > 0 {
		parts = append(parts, failedFilesPrompt(failed))
	}
	if instruction = strings.TrimSpace(instruction); instruction != "" {
		parts = append(parts, instruction)
	}
	return strings.Join(parts, "\n")
}

func (g *Gateway) replyFileIntakeResults(ctx context.Context, chatID int64, saved []store.File, failed []incomingTelegramFileResult, offerFileCenter bool) {
	var b strings.Builder
	if len(saved) > 0 {
		fmt.Fprintf(&b, "📎 已收到并暂存 <b>%d</b> 个文件。", len(saved))
		if len(failed) == 0 {
			b.WriteString("告诉我接下来要怎么处理它们。")
		}
		b.WriteString("\n")
	}
	if len(failed) > 0 {
		b.WriteString("⚠️ <b>以下文件没有进入 nbco</b>\n")
		cloudLimited := false
		for _, result := range failed {
			fmt.Fprintf(&b, "• <b>%s</b>（%s）：%s\n",
				html.EscapeString(result.input.name), formatTelegramBytes(result.input.sizeBytes), html.EscapeString(result.errorMessage))
			cloudLimited = cloudLimited || result.errorCode == "telegram_cloud_limit"
		}
		if cloudLimited {
			fmt.Fprintf(&b, "可以把文件拆分到 %s 以内后重新发送，", formatTelegramBytes(telegramCloudFileDownloadLimit))
		} else {
			b.WriteString("可以稍后重试，")
		}
		if offerFileCenter {
			b.WriteString("或通过 nbco 文件中心直接上传。")
		} else {
			b.WriteString("或私聊我后从 nbco 文件中心上传。")
		}
	}
	text := strings.TrimSpace(b.String())
	uploadURL := g.fileCenterURL()
	if !offerFileCenter || uploadURL == "" || len(failed) == 0 {
		g.reply(ctx, chatID, text)
		return
	}
	_, err := g.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML,
		ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "打开 nbco 文件中心", WebApp: &models.WebAppInfo{URL: uploadURL}},
		}}},
	})
	if err != nil {
		slog.Warn("发送文件中心按钮失败，降级普通回复", "chat", chatID, "err", err)
		g.reply(ctx, chatID, text+"\n"+html.EscapeString(uploadURL))
	}
}

func (g *Gateway) fileCenterURL() string {
	if g.publicBaseURL == "" {
		return ""
	}
	parsed, err := url.Parse(g.publicBaseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	return g.publicBaseURL + "/?view=files"
}

func safeTelegramFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "nbco-file"
	}
	return name
}

func savedFilesPrompt(files []store.File) string {
	var b strings.Builder
	b.WriteString("[用户刚上传并暂存到 nbco 的文件]\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- file_id=%d；文件名=%s；大小=%s；类型=%s\n", f.ID, f.OriginalName, formatTelegramBytes(f.SizeBytes), f.MIMEType)
	}
	b.WriteString("这些是系统文件 ID；需要读取或分析内容时用 analyze_company_materials，不要使用 Telegram 原始 file_id。")
	return b.String()
}

func nonMediaText(msg *models.Message) string {
	var parts []string
	if caption := strings.TrimSpace(msg.Caption); caption != "" {
		parts = append(parts, caption)
	}
	if text := strings.TrimSpace(msg.Text); text != "" {
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

// formatTelegramBytes 转发到 textfmt.FormatBytes（跨包共享实现）。
func formatTelegramBytes(n int64) string { return textfmt.FormatBytes(n) }

// transcribeVoice 下载 Telegram 语音并经 STT 服务转写。任何失败返回空串，
// 调用方回退为占位提示——语音是增强，不该让消息处理失败。
// File downloads share the same upper bound as saveTelegramFile. The previous
// voice-only two-minute client timeout also covered 200MB local Bot API files
// and could abort a valid transfer before its five-minute operation deadline.
var telegramFileHTTP = &http.Client{Timeout: 5 * time.Minute}

func (g *Gateway) transcribeVoice(ctx context.Context, fileID string) string {
	if g.stt == nil {
		return ""
	}
	// 独立超时：传入的可能是 bot 长轮询的进程级 ctx，下载 stall 时若无上限，
	// 这里会在持有 per-chat 锁的状态下永久阻塞，该用户/群的消息从此全部排队。
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	f, err := g.bot.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		slog.Warn("语音文件信息获取失败", "err", err)
		return ""
	}
	src, _, err := g.openTelegramFile(ctx, f)
	if err != nil {
		slog.Warn("语音下载失败", "err", err)
		return ""
	}
	defer src.Close()
	text, err := g.stt.Transcribe(ctx, "voice.ogg", io.LimitReader(src, voiceDownloadLimit))
	if err != nil {
		slog.Warn("语音转写失败", "err", err)
		return ""
	}
	return text
}

// hasMessagePayload 消息是否有可处理内容（文本/媒体/说明文字任一）。
// 路由用的轻量判断，不触发语音转写等重加工。
func hasMessagePayload(msg *models.Message) bool {
	return strings.TrimSpace(msg.Text) != "" || strings.TrimSpace(msg.Caption) != "" ||
		hasIncomingTelegramFiles(msg) || msg.Voice != nil
}

func hasIncomingTelegramFiles(msg *models.Message) bool {
	return msg != nil && (msg.Document != nil || len(msg.Photo) > 0 || msg.Video != nil ||
		msg.Animation != nil || msg.Audio != nil || msg.VideoNote != nil)
}

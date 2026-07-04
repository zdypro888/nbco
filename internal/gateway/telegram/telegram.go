// Package telegram 是 Telegram 入口网关：接消息 → 编排器；同时实现 notify.Notifier。
// 中枢不感知 Telegram —— 本包是可替换的外设（原则：接口皆可换，中枢不可换）。
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/zdypro888/nbco/internal/chat"
	"github.com/zdypro888/nbco/internal/store"
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
	locks map[int64]*sync.Mutex // 按用户串行化，避免并发轮次互相踩会话
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

// Run 长轮询直到 ctx 结束。
func (g *Gateway) Run(ctx context.Context) { g.bot.Start(ctx) }

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
	// 逐 update 起 goroutine，按用户加锁：慢轮次不阻塞他人，同人消息不并发。
	go func() {
		lock := g.userLock(msg.From.ID)
		lock.Lock()
		defer lock.Unlock()
		g.process(ctx, msg)
	}()
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
		g.reply(ctx, chatID, fmt.Sprintf("你好，%s。直接说事就行 —— 任务、提醒、查进展都可以。/new 开新会话。", u.Name))
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

	g.sendTyping(ctx, chatID)
	answer, err := g.orch.HandleMessage(ctx, u, Provider, text)
	if err != nil {
		slog.Error("对话轮次失败", "user", u.ID, "err", err)
		g.reply(ctx, chatID, "这轮对话出错了，请重试；连续失败请联系管理员。")
		return
	}
	g.reply(ctx, chatID, answer)
}

// onboard 未绑定用户：超管自动开通；其他人凭绑定 Key。
func (g *Gateway) onboard(ctx context.Context, msg *models.Message, chatID int64, externalID, text string) {
	name := strings.TrimSpace(strings.Join([]string{msg.From.FirstName, msg.From.LastName}, " "))
	if name == "" {
		name = msg.From.Username
	}
	ident := store.Identity{Provider: Provider, ExternalID: externalID, ChatRef: strconv.FormatInt(chatID, 10)}

	// 首任超管引导：全新系统里第一个发 /superadmin 的人成为超管（零配置起步）。
	if text == "/superadmin" {
		u, err := g.store.BootstrapSuperadmin(ctx, name, ident)
		switch {
		case errors.Is(err, store.ErrConflict):
			g.reply(ctx, chatID, "系统已有超级管理员。请向管理员索取绑定 Key 加入。")
		case err != nil:
			slog.Error("超管引导失败", "err", err)
			g.reply(ctx, chatID, "初始化失败，请稍后再试。")
		default:
			g.reply(ctx, chatID, fmt.Sprintf("👑 %s，你已成为首位超级管理员（ID %d）。直接说事就行。", u.Name, u.ID))
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
		g.reply(ctx, chatID, fmt.Sprintf("👑 超级管理员 %s 已开通（ID %d）。直接说事就行。", u.Name, u.ID))
		return
	}

	key := strings.ToLower(text)
	if !bindKeyRe.MatchString(key) {
		g.reply(ctx, chatID, "你还未加入系统。请把管理员发给你的绑定 Key 发过来（32 位字符串）；全新系统可发送 /superadmin 成为首位管理员。")
		return
	}
	// 单事务绑定：Key 无效不会留下半开账号。
	u, err := g.store.BindUserWithKey(ctx, key, name, ident)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			g.reply(ctx, chatID, "绑定 Key 无效或已过期，请向管理员重新索取。")
			return
		}
		slog.Error("绑定失败", "err", err)
		g.reply(ctx, chatID, "绑定失败，请稍后再试。")
		return
	}
	g.reply(ctx, chatID, fmt.Sprintf("✅ 欢迎加入，%s（ID %d）。先自我介绍一下吧，我会帮你存档。", u.Name, u.ID))
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

// sendChunks 按长度分片发送（修复 legacy 超 4096 发送失败问题）。
func (g *Gateway) sendChunks(ctx context.Context, chatID int64, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "（空回复）"
	}
	slog.Debug("TG 发送", "chat", chatID, "text_len", len(text))
	for _, chunk := range splitChunks(text, chunkLimit) {
		if _, err := g.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: chunk}); err != nil {
			return err
		}
	}
	return nil
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

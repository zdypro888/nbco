// Package chat 是对话编排器：会话管理（落库）、系统提示组装、引擎调度。
// 入口（TG / Web / API）只需要拿到用户后调 HandleMessage。
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/store"
	"github.com/zdypro888/nbco/internal/tools"
)

// historyLimit 每轮重放给引擎的最近消息条数（仅 eino 引擎使用）。
const historyLimit = 40

// Orchestrator 对话编排器。
type Orchestrator struct {
	store  *store.Store
	engine ai.Engine
	deps   tools.Deps
	tz     *time.Location

	mu    sync.Mutex
	locks map[int64]*sync.Mutex // 同一用户的轮次串行：用户消息与系统触发轮次（催办/周报）不互踩会话
}

// New 创建编排器。
func New(s *store.Store, engine ai.Engine, deps tools.Deps, tz *time.Location) *Orchestrator {
	return &Orchestrator{store: s, engine: engine, deps: deps, tz: tz, locks: map[int64]*sync.Mutex{}}
}

func (o *Orchestrator) userLock(id int64) *sync.Mutex {
	o.mu.Lock()
	defer o.mu.Unlock()
	l, ok := o.locks[id]
	if !ok {
		l = &sync.Mutex{}
		o.locks[id] = l
	}
	return l
}

// HandleMessage 处理用户在某渠道的一轮输入，返回给用户的答复。
// 系统触发的轮次（催办/周报）同样走这里：调度器把系统指令作为输入传入。
func (o *Orchestrator) HandleMessage(ctx context.Context, u *store.User, channel, text string) (string, error) {
	lock := o.userLock(u.ID)
	lock.Lock()
	defer lock.Unlock()

	sess, err := o.ensureSession(ctx, u, channel)
	if err != nil {
		return "", err
	}

	system, err := o.systemPrompt(ctx, u, channel)
	if err != nil {
		return "", err
	}

	start := time.Now()
	slog.Info("轮次开始", "user", u.ID, "channel", channel, "session", sess.ID, "text_len", len(text))
	slog.Debug("轮次输入", "session", sess.ID, "text", text)

	req := &ai.TurnRequest{
		SessionID:     fmt.Sprintf("%d", sess.ID),
		EngineSession: sess.EngineRef,
		System:        system,
		UserText:      text,
		Tools:         tools.ForUser(o.deps, u, &sess.ID),
		// 实时轨迹：工具调用与产出上报到日志（审计层另行落库）。
		OnEvent: func(s ai.Step) {
			switch {
			case s.Kind == ai.StepToolCall && s.Err != "":
				slog.Warn("工具调用失败", "session", sess.ID, "tool", s.ToolName, "err", s.Err)
			case s.Kind == ai.StepToolCall:
				slog.Info("工具调用", "session", sess.ID, "tool", s.ToolName, "result_len", len(s.Result))
				slog.Debug("工具调用详情", "session", sess.ID, "tool", s.ToolName,
					"args", string(s.Args), "result", truncate(s.Result, 500))
			case s.Kind == ai.StepText:
				slog.Debug("模型产出", "session", sess.ID, "text_len", len(s.Result))
			}
		},
	}
	// eino 引擎需要重放历史；claudecli 用 EngineSession 自带记忆。
	msgs, err := o.store.MessagesOf(ctx, sess.ID, historyLimit)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		req.History = append(req.History, ai.Message{Role: ai.Role(m.Role), Content: m.Content})
	}

	// 用户消息先落库：引擎失败时输入也不丢（历史已取出，本轮不会重复重放）。
	// 失败轮次会留下孤立的 user 消息，einoengine 重放时做同角色合并兜底。
	if err := o.store.AppendMessage(ctx, sess.ID, string(ai.RoleUser), text); err != nil {
		slog.Warn("用户消息落库失败", "err", err)
	}

	res, err := o.engine.RunTurn(ctx, req)
	if err != nil {
		slog.Warn("轮次失败", "session", sess.ID, "dur", time.Since(start).Round(time.Millisecond), "err", err)
		return "", fmt.Errorf("AI 引擎失败: %w", err)
	}
	slog.Info("轮次完成", "session", sess.ID, "dur", time.Since(start).Round(time.Millisecond),
		"steps", len(res.Steps), "in_tokens", res.Usage.InputTokens, "out_tokens", res.Usage.OutputTokens,
		"reply_len", len(res.Text))
	slog.Debug("轮次答复", "session", sess.ID, "text", truncate(res.Text, 500))

	// 落库：助手答复 + 引擎侧会话标识。审计层已记录工具轨迹。
	if err := o.store.AppendMessage(ctx, sess.ID, string(ai.RoleAssistant), res.Text); err != nil {
		slog.Warn("助手消息落库失败", "err", err)
	}
	if res.EngineSession != "" && res.EngineSession != sess.EngineRef {
		if err := o.store.SetSessionEngineRef(ctx, sess.ID, res.EngineSession); err != nil {
			slog.Warn("引擎会话标识落库失败", "err", err)
		}
	}
	return res.Text, nil
}

// NewSession 强制开新会话（用户主动重开）。等待该用户进行中的轮次结束。
func (o *Orchestrator) NewSession(ctx context.Context, u *store.User, channel string) error {
	lock := o.userLock(u.ID)
	lock.Lock()
	defer lock.Unlock()
	_, err := o.store.StartSession(ctx, u.ID, channel, o.engine.Name())
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ensureSession 取活跃会话；不存在或引擎不匹配时新开。
func (o *Orchestrator) ensureSession(ctx context.Context, u *store.User, channel string) (*store.ChatSession, error) {
	sess, err := o.store.ActiveSession(ctx, u.ID, channel)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return o.store.StartSession(ctx, u.ID, channel, o.engine.Name())
	case err != nil:
		return nil, err
	}
	if sess.Engine != o.engine.Name() {
		// 配置切换过引擎：旧会话的引擎状态不兼容，直接开新会话。
		return o.store.StartSession(ctx, u.ID, channel, o.engine.Name())
	}
	return sess, nil
}

// channelStyle 各渠道的输出格式指引。键与入口网关写入会话的 channel 值约定一致
// （数据耦合而非包依赖：新增渠道加一行即可，不认识的渠道回退纯文本）。
var channelStyle = map[string]string{
	"telegram": "输出格式（Telegram HTML）：\n" +
		"- 用 Telegram 支持的 HTML 标签排版：<b>粗体</b>、<i>斜体</i>、<u>下划线</u>、<s>删除线</s>、<code>行内代码</code>、<pre>多行代码</pre>、<blockquote>引用</blockquote>、<a href=\"URL\">链接</a>。\n" +
		"- 除上述标签外不支持任何 HTML；也不支持 Markdown（**加粗**、# 标题、[链接]()、表格），绝不要输出这些标记。\n" +
		"- 列表用「• 」或贴切的 emoji 开头的行；层级用缩进两个空格表达。\n" +
		"- 正文里的 <、>、& 必须写成 &lt;、&gt;、&amp;（标签本身除外）。\n" +
		"- 善用 emoji 让消息生动醒目：状态（✅ ⏳ 🔴 ⚠️）、板块（📋 📊 💡 🗓）、动作（📌 🚀 🎉），标题和关键行都可以配，但一行别超过两个。\n",
	"api": "输出格式：纯文本，不要 Markdown 或 HTML 标记；列表用「• 」或 emoji 开头的行；适度使用 emoji。\n",
}

// systemPrompt 组装系统提示：身份、当前用户、时间、渠道格式、激活角色。
func (o *Orchestrator) systemPrompt(ctx context.Context, u *store.User, channel string) (string, error) {
	var b strings.Builder
	b.WriteString("你是 nbco，公司的 AI 运营中枢：既是每个员工的助理，也是管理流程的执行者。\n")
	b.WriteString("你通过工具完成一切业务操作（用户、画像、权限、项目任务、提醒）。工具集已按当前用户的权限裁剪，权限不足时工具会返回提示，如实转告即可。\n")
	b.WriteString("原则：\n")
	b.WriteString("- 优先用工具查询真实数据，不要凭空编造用户、任务或权限状态。\n")
	b.WriteString("- 建设性操作直接执行，不要反问确认：用户给了信息就立即存档（信息字段未定义时，超管直接用 add_info_field 定义后再存；普通用户存入自我介绍），要建任务就建，要设提醒就设。只有删除项目/任务这类不可逆操作才先确认。\n")
	b.WriteString("- 不向用户展示内部技术细节：数字用户 ID、TG ID、会话 ID 一律不提，提到人只用名字；任务可用 #编号 引用。身份绑定系统已自动管理，绝不建议用户记录 TG ID 之类系统已知的信息。\n")
	b.WriteString("- 回复用用户的语言，简洁直接。\n")
	b.WriteString("- 对话中出现有复用价值的结论（决策、方案、流程、客户约定），主动存入知识库（save_knowledge）；回答公司事实类问题前先 search_knowledge。\n")
	b.WriteString("- 以 [系统定时触发· 开头的输入来自系统调度器而非用户本人，按其中的指示产出要推送给用户的内容。\n\n")

	if style, ok := channelStyle[channel]; ok {
		b.WriteString(style)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "当前用户：%s（ID %d）", u.Name, u.ID)
	if u.IsSuperadmin {
		b.WriteString("，超级管理员")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "当前时间：%s（%s）\n", time.Now().In(o.tz).Format("2006-01-02 15:04 Monday"), o.tz.String())

	// 激活角色注入。
	role, err := o.store.ActiveRole(ctx, u.ID)
	if err == nil {
		fmt.Fprintf(&b, "\n当前激活角色「%s」，请按以下设定工作：\n%s\n", role.Name, role.Prompt)
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return b.String(), nil
}

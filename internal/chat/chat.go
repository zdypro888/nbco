// Package chat 是对话编排器：会话管理（落库）、系统提示组装、引擎调度。
// 入口（TG / Web / API）只需要拿到用户后调 HandleMessage。
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
}

// New 创建编排器。
func New(s *store.Store, engine ai.Engine, deps tools.Deps, tz *time.Location) *Orchestrator {
	return &Orchestrator{store: s, engine: engine, deps: deps, tz: tz}
}

// HandleMessage 处理用户在某渠道的一轮输入，返回给用户的答复。
func (o *Orchestrator) HandleMessage(ctx context.Context, u *store.User, channel, text string) (string, error) {
	sess, err := o.ensureSession(ctx, u, channel)
	if err != nil {
		return "", err
	}

	system, err := o.systemPrompt(ctx, u)
	if err != nil {
		return "", err
	}

	req := &ai.TurnRequest{
		SessionID:     fmt.Sprintf("%d", sess.ID),
		EngineSession: sess.EngineRef,
		System:        system,
		UserText:      text,
		Tools:         tools.ForUser(o.deps, u, &sess.ID),
	}
	// eino 引擎需要重放历史；claudecli 用 EngineSession 自带记忆。
	msgs, err := o.store.MessagesOf(ctx, sess.ID, historyLimit)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		req.History = append(req.History, ai.Message{Role: ai.Role(m.Role), Content: m.Content})
	}

	res, err := o.engine.RunTurn(ctx, req)
	if err != nil {
		return "", fmt.Errorf("AI 引擎失败: %w", err)
	}

	// 落库：本轮两条消息 + 引擎侧会话标识。审计层已记录工具轨迹。
	if err := o.store.AppendMessage(ctx, sess.ID, string(ai.RoleUser), text); err != nil {
		slog.Warn("用户消息落库失败", "err", err)
	}
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

// NewSession 强制开新会话（用户主动重开）。
func (o *Orchestrator) NewSession(ctx context.Context, u *store.User, channel string) error {
	_, err := o.store.StartSession(ctx, u.ID, channel, o.engine.Name())
	return err
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

// systemPrompt 组装系统提示：身份、当前用户、时间、激活角色。
func (o *Orchestrator) systemPrompt(ctx context.Context, u *store.User) (string, error) {
	var b strings.Builder
	b.WriteString("你是 nbco，公司的 AI 运营中枢：既是每个员工的助理，也是管理流程的执行者。\n")
	b.WriteString("你通过工具完成一切业务操作（用户、画像、权限、项目任务、提醒）。工具集已按当前用户的权限裁剪，权限不足时工具会返回提示，如实转告即可。\n")
	b.WriteString("原则：\n")
	b.WriteString("- 优先用工具查询真实数据，不要凭空编造用户、任务或权限状态。\n")
	b.WriteString("- 删除项目、删除任务等不可逆操作，先与用户确认再执行。\n")
	b.WriteString("- 回复用用户的语言，简洁直接，避免罗列内部 ID 之外的技术细节。\n\n")

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

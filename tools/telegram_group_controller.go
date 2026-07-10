package tools

import (
	"context"
	"errors"
	"sync"
)

// TelegramGroupController 是工具层对 Telegram 群的控制接口。
// 具体实现由 gateway/telegram 注入；工具层不直接依赖 Telegram 包。
type TelegramGroupController interface {
	EnsureTelegramGroupSession(ctx context.Context, chatID int64, ownerID int64) error
	GetTelegramGroupMemberCount(ctx context.Context, chatID int64) (int, error)
	GetTelegramGroupAdministrators(ctx context.Context, chatID int64) ([]TelegramGroupMember, error)
	GetTelegramGroupMember(ctx context.Context, chatID int64, userID int64) (*TelegramGroupMember, error)
	GetTelegramGroupBotMember(ctx context.Context, chatID int64) (*TelegramGroupMember, error)
	SendTelegramGroupMessage(ctx context.Context, chatID int64, text string, disableNotification bool) (messageID int, err error)
	EditTelegramGroupMessage(ctx context.Context, chatID int64, messageID int, text string) error
	DeleteTelegramGroupMessage(ctx context.Context, chatID int64, messageID int) error
	PinTelegramGroupMessage(ctx context.Context, chatID int64, messageID int, disableNotification bool) error
	UnpinTelegramGroupMessage(ctx context.Context, chatID int64, messageID int) error
	SetTelegramGroupTitle(ctx context.Context, chatID int64, title string) error
	SetTelegramGroupDescription(ctx context.Context, chatID int64, description string) error
}

func (h *TelegramGroupHub) EnsureTelegramGroupSession(ctx context.Context, chatID int64, ownerID int64) error {
	c, err := h.controller()
	if err != nil {
		return err
	}
	return c.EnsureTelegramGroupSession(ctx, chatID, ownerID)
}

type TelegramGroupMember struct {
	UserID   int64
	Name     string
	Username string
	Status   string
	IsBot    bool
	Rights   []string
}

// TelegramGroupHub 解决装配期循环：chat/tools 先持有 hub，Telegram 网关启动后注入实现。
type TelegramGroupHub struct {
	mu sync.RWMutex
	c  TelegramGroupController
}

func (h *TelegramGroupHub) Set(c TelegramGroupController) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.c = c
}

func (h *TelegramGroupHub) controller() (TelegramGroupController, error) {
	h.mu.RLock()
	c := h.c
	h.mu.RUnlock()
	if c == nil {
		return nil, errors.New("telegram 群控制器尚未就绪")
	}
	return c, nil
}

func (h *TelegramGroupHub) SendTelegramGroupMessage(ctx context.Context, chatID int64, text string, disableNotification bool) (int, error) {
	c, err := h.controller()
	if err != nil {
		return 0, err
	}
	return c.SendTelegramGroupMessage(ctx, chatID, text, disableNotification)
}

func (h *TelegramGroupHub) GetTelegramGroupMemberCount(ctx context.Context, chatID int64) (int, error) {
	c, err := h.controller()
	if err != nil {
		return 0, err
	}
	return c.GetTelegramGroupMemberCount(ctx, chatID)
}

func (h *TelegramGroupHub) GetTelegramGroupAdministrators(ctx context.Context, chatID int64) ([]TelegramGroupMember, error) {
	c, err := h.controller()
	if err != nil {
		return nil, err
	}
	return c.GetTelegramGroupAdministrators(ctx, chatID)
}

func (h *TelegramGroupHub) GetTelegramGroupMember(ctx context.Context, chatID int64, userID int64) (*TelegramGroupMember, error) {
	c, err := h.controller()
	if err != nil {
		return nil, err
	}
	return c.GetTelegramGroupMember(ctx, chatID, userID)
}

func (h *TelegramGroupHub) GetTelegramGroupBotMember(ctx context.Context, chatID int64) (*TelegramGroupMember, error) {
	c, err := h.controller()
	if err != nil {
		return nil, err
	}
	return c.GetTelegramGroupBotMember(ctx, chatID)
}

func (h *TelegramGroupHub) EditTelegramGroupMessage(ctx context.Context, chatID int64, messageID int, text string) error {
	c, err := h.controller()
	if err != nil {
		return err
	}
	return c.EditTelegramGroupMessage(ctx, chatID, messageID, text)
}

func (h *TelegramGroupHub) DeleteTelegramGroupMessage(ctx context.Context, chatID int64, messageID int) error {
	c, err := h.controller()
	if err != nil {
		return err
	}
	return c.DeleteTelegramGroupMessage(ctx, chatID, messageID)
}

func (h *TelegramGroupHub) PinTelegramGroupMessage(ctx context.Context, chatID int64, messageID int, disableNotification bool) error {
	c, err := h.controller()
	if err != nil {
		return err
	}
	return c.PinTelegramGroupMessage(ctx, chatID, messageID, disableNotification)
}

func (h *TelegramGroupHub) UnpinTelegramGroupMessage(ctx context.Context, chatID int64, messageID int) error {
	c, err := h.controller()
	if err != nil {
		return err
	}
	return c.UnpinTelegramGroupMessage(ctx, chatID, messageID)
}

func (h *TelegramGroupHub) SetTelegramGroupTitle(ctx context.Context, chatID int64, title string) error {
	c, err := h.controller()
	if err != nil {
		return err
	}
	return c.SetTelegramGroupTitle(ctx, chatID, title)
}

func (h *TelegramGroupHub) SetTelegramGroupDescription(ctx context.Context, chatID int64, description string) error {
	c, err := h.controller()
	if err != nil {
		return err
	}
	return c.SetTelegramGroupDescription(ctx, chatID, description)
}

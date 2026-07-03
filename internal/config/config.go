// Package config 加载 nbco.json 配置。
//
// 配置原则：进程完全无状态，所有运行状态落库；配置只描述外部依赖与开关。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// AI 引擎类型。
const (
	EngineEino      = "eino"      // 直接调 API（eino ADK），客户自带 key 的产品路径
	EngineClaudeCLI = "claudecli" // 驱动 claude CLI（headless -p + MCP 回连）
)

// eino 引擎的模型 provider。
const (
	ProviderClaude = "claude"
	ProviderOpenAI = "openai"
)

// AIConfig AI 引擎配置。
type AIConfig struct {
	Engine      string  `json:"engine"`     // eino | claudecli
	Provider    string  `json:"provider"`   // eino: claude | openai
	APIKey      string  `json:"api_key"`    // eino 引擎用
	BaseURL     string  `json:"base_url"`   // 可选，自建网关
	Model       string  `json:"model"`      // 模型名，必填（eino）
	MaxTokens   int     `json:"max_tokens"` // 默认 4096
	Temperature float32 `json:"temperature"`
	MaxTurns    int     `json:"max_turns"` // tool 循环上限，默认 16

	ClaudeCmd string `json:"claude_cmd"` // claudecli 引擎的可执行文件，默认 "claude"
}

// Config 全量配置。
type Config struct {
	TelegramToken string  `json:"telegram_token"`
	Superadmins   []int64 `json:"superadmins"`
	PostgresDSN   string  `json:"postgres_dsn"`
	Listen        string  `json:"listen"` // MCP/HTTP 监听地址，默认 127.0.0.1:8900
	// PublicBaseURL claude CLI 回连 MCP 的基地址；默认由 Listen 推导为 http://127.0.0.1:<port>
	PublicBaseURL string   `json:"public_base_url"`
	AI            AIConfig `json:"ai"`
	// DailySummaryHour 每日待办汇总的本地小时（0-23），-1 关闭。默认 9。
	DailySummaryHour *int   `json:"daily_summary_hour"`
	Timezone         string `json:"timezone"` // IANA 时区，默认 Asia/Shanghai
}

// Load 从文件读取并校验配置。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8900"
	}
	if c.Timezone == "" {
		c.Timezone = "Asia/Shanghai"
	}
	if c.DailySummaryHour == nil {
		h := 9
		c.DailySummaryHour = &h
	}
	if c.AI.Engine == "" {
		c.AI.Engine = EngineEino
	}
	if c.AI.Provider == "" {
		c.AI.Provider = ProviderClaude
	}
	if c.AI.MaxTokens <= 0 {
		c.AI.MaxTokens = 4096
	}
	if c.AI.MaxTurns <= 0 {
		c.AI.MaxTurns = 16
	}
	if c.AI.ClaudeCmd == "" {
		c.AI.ClaudeCmd = "claude"
	}
}

func (c *Config) validate() error {
	var errs []error
	if strings.TrimSpace(c.TelegramToken) == "" {
		errs = append(errs, errors.New("telegram_token 必填"))
	}
	if len(c.Superadmins) == 0 {
		errs = append(errs, errors.New("superadmins 至少一个"))
	}
	if strings.TrimSpace(c.PostgresDSN) == "" {
		errs = append(errs, errors.New("postgres_dsn 必填"))
	}
	switch c.AI.Engine {
	case EngineEino:
		if strings.TrimSpace(c.AI.APIKey) == "" {
			errs = append(errs, errors.New("ai.api_key 必填（eino 引擎）"))
		}
		if strings.TrimSpace(c.AI.Model) == "" {
			errs = append(errs, errors.New("ai.model 必填（eino 引擎）"))
		}
		if c.AI.Provider != ProviderClaude && c.AI.Provider != ProviderOpenAI {
			errs = append(errs, fmt.Errorf("ai.provider 不支持: %q", c.AI.Provider))
		}
	case EngineClaudeCLI:
		// claude CLI 自带认证（订阅或环境变量 ANTHROPIC_API_KEY），不强制 api_key
	default:
		errs = append(errs, fmt.Errorf("ai.engine 不支持: %q", c.AI.Engine))
	}
	return errors.Join(errs...)
}

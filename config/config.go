// Package config 加载 nbco.json 配置。
//
// 配置原则：进程完全无状态，所有运行状态落库；配置只描述外部依赖与开关。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// AI 引擎类型。
const (
	EngineEino = "eino" // 直接调 API（eino ADK），客户自带 key 的产品路径
)

// eino 引擎的模型 provider。
const (
	ProviderClaude = "claude"
	ProviderOpenAI = "openai"
)

// AIConfig AI 引擎配置。
type AIConfig struct {
	Engine      string  `json:"engine"`     // 仅支持 eino；CLI 自动执行走 nbco-worker 交互式 PTY
	Provider    string  `json:"provider"`   // eino: claude | openai
	APIKey      string  `json:"api_key"`    // eino 引擎用
	BaseURL     string  `json:"base_url"`   // 可选，自建网关
	Model       string  `json:"model"`      // 模型名，必填（eino）
	MaxTokens   int     `json:"max_tokens"` // 默认 4096
	Temperature float32 `json:"temperature"`
	MaxTurns    int     `json:"max_turns"` // tool 循环上限，默认 16
	// StreamReasoning 控制流式阶段是否把模型推理内容展示给用户；默认 false。
	StreamReasoning bool `json:"stream_reasoning"`
	// 语义检索的 embedding 配置（可选）。EmbedModel 空=不启用，知识检索回退词法。
	// EmbedBaseURL / EmbedAPIKey 空时回退 BaseURL / APIKey（同一 OpenAI 兼容网关
	// 常同时提供 chat 与 embeddings）。指向你的本地 embedding 服务即可激活。
	EmbedModel   string `json:"embed_model"`
	EmbedBaseURL string `json:"embed_base_url"`
	EmbedAPIKey  string `json:"embed_api_key"`
}

// MCPServer 外接 MCP 工具服务（Streamable HTTP）。
// 其工具经统一审计层并入所有用户的工具集，各引擎通吃。
type MCPServer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"` // 可选，如 Authorization
}

// Config 全量配置。
type Config struct {
	TelegramToken string  `json:"telegram_token"`
	Superadmins   []int64 `json:"superadmins"`
	PostgresDSN   string  `json:"postgres_dsn"`
	Listen        string  `json:"listen"`    // MCP/HTTP 监听地址，默认 127.0.0.1:8900
	LogLevel      string  `json:"log_level"` // debug | info | warn | error，默认 info
	FileStorePath string  `json:"file_store_path"`
	// WorkerDownloadPath 保存 nbco-worker 多平台发行二进制；为空默认 downloads。
	WorkerDownloadPath string `json:"worker_download_path"`
	TLSCertFile        string `json:"tls_cert_file"` // 可选；配置后 HTTP 服务改用 HTTPS
	TLSKeyFile         string `json:"tls_key_file"`  // 可选；PEM bundle 可与 tls_cert_file 指向同一文件
	// PublicBaseURL 对外基地址（如 https://im.app:8443）：worker 安装指引等面向
	// 用户的文案用它拼真实地址；为空时文案用占位符。也保留给外部回调集成。
	PublicBaseURL string   `json:"public_base_url"`
	AI            AIConfig `json:"ai"`
	// MCPServers 外接 MCP 工具服务列表（可选）。
	MCPServers []MCPServer `json:"mcp_servers"`
	// DailySummaryHour 每日待办汇总的本地小时（0-23），-1 关闭。默认 9。
	DailySummaryHour *int   `json:"daily_summary_hour"`
	Timezone         string `json:"timezone"` // IANA 时区，默认 Asia/Shanghai
	// SchedAIConcurrency 调度器同时进行的 AI 轮次上限（催办/周报/定时 AI 推送）。
	// 每轮 AI 都要调一次模型 API，此上限防止「早安问候推给全体」时几百轮并发打爆
	// 后端网关。默认 4；设小更省、更慢，设大更快、更吃后端。
	SchedAIConcurrency int `json:"sched_ai_concurrency"`
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
	if c.SchedAIConcurrency <= 0 {
		c.SchedAIConcurrency = 4
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
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.FileStorePath == "" {
		c.FileStorePath = "files"
	}
	if c.WorkerDownloadPath == "" {
		c.WorkerDownloadPath = "downloads"
	}
}

// SlogLevel 配置的日志级别（Load 已校验合法性）。
func (c *Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (c *Config) validate() error {
	var errs []error
	// telegram_token 可留空：此时不启动 Telegram 网关，HTTP/API/MCP/worker 仍可用。
	// superadmins 可留空：启用 Telegram 时，全新系统里第一个发 /superadmin 的人自动成为超管。
	if strings.TrimSpace(c.PostgresDSN) == "" {
		errs = append(errs, errors.New("postgres_dsn 必填"))
	}
	if (strings.TrimSpace(c.TLSCertFile) == "") != (strings.TrimSpace(c.TLSKeyFile) == "") {
		errs = append(errs, errors.New("tls_cert_file 与 tls_key_file 必须同时配置"))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log_level 不支持: %q", c.LogLevel))
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
	default:
		errs = append(errs, fmt.Errorf("ai.engine 不支持: %q（中枢只支持 eino；CLI 自动干活请用 nbco-worker 交互式 PTY）", c.AI.Engine))
	}
	for i, m := range c.MCPServers {
		if strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.URL) == "" {
			errs = append(errs, fmt.Errorf("mcp_servers[%d]: name 与 url 必填", i))
		}
	}
	return errors.Join(errs...)
}

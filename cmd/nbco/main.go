// nbco：单二进制公司运营中枢。
// 一个进程装下全部：Telegram 网关、HTTP API/MCP、AI 引擎、调度器；状态全部落库。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/ai/einoengine"
	"github.com/zdypro888/nbco/ai/embed"
	"github.com/zdypro888/nbco/ai/stt"
	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/config"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/gateway/httpapi"
	"github.com/zdypro888/nbco/gateway/telegram"
	"github.com/zdypro888/nbco/knowledge"
	"github.com/zdypro888/nbco/mcptools"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/sched"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
	"github.com/zdypro888/nbco/workerhub"
)

func main() {
	configPath := flag.String("config", "nbco.json", "配置文件路径")
	flag.Parse()
	if err := run(*configPath); err != nil {
		slog.Error("退出", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.SlogLevel()})))
	tz, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("时区 %q: %w", cfg.Timezone, err)
	}

	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	hub := &notify.Hub{}
	tgGroups := &tools.TelegramGroupHub{}
	eventHub := &tools.EventHub{} // 事件总线后注入容器（bus 依赖 orch，orch 依赖 deps）

	// 语义检索的 embedder（可选）：配了 ai.embed_model 才构建，否则知识检索回退词法。
	embedder, err := embed.New(cfg.AI)
	if err != nil {
		return err
	}
	kb := knowledge.New(st, embedder)
	if embedder != nil {
		slog.Info("语义检索已启用", "embed_model", embedder.Model())
		go backfillKnowledge(ctx, kb)
		go backfillMessages(ctx, kb) // 情景记忆：存量消息补向量
	}

	deps := tools.Deps{
		Store:                    st,
		Notifier:                 hub,
		TZ:                       tz,
		Knowledge:                kb,
		Workers:                  workerhub.New(),
		AIStreamReasoningDefault: cfg.AI.StreamReasoning,
		PublicBaseURL:            cfg.PublicBaseURL,
		TelegramGroups:           tgGroups,
		Events:                   eventHub,
	}

	// 外接 MCP 工具：连不上只警告不阻断启动（外部服务不可用不该拖垮中枢）。
	for _, srv := range cfg.MCPServers {
		ext, closeFn, err := mcptools.Connect(ctx, srv)
		if err != nil {
			slog.Warn("外接 MCP 服务不可用，已跳过", "server", srv.Name, "err", err)
			continue
		}
		defer closeFn()
		deps.Extra = append(deps.Extra, ext...)
		slog.Info("外接 MCP 工具已接入", "server", srv.Name, "tools", len(ext))
	}

	engine, err := buildEngine(ctx, cfg)
	if err != nil {
		return err
	}
	deps.ScriptAI = func(ctx context.Context, u *store.User, prompt string) (string, error) {
		actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		name := ""
		if u != nil {
			name = u.Name
		}
		model := strings.TrimSpace(cfg.AI.Model)
		if runtimeModel, err := st.GetKV(actx, store.KVAIModel); err == nil && strings.TrimSpace(runtimeModel) != "" {
			model = strings.TrimSpace(runtimeModel)
		}
		res, err := engine.RunTurn(actx, &ai.TurnRequest{
			SessionID: "script-ai",
			System:    "你是 nbco 脚本工具的受控 AI 子调用。只回答本次脚本请求需要的结果，不调用工具，不输出无关解释。",
			UserText:  fmt.Sprintf("调用者：%s\n\n%s", name, prompt),
			Model:     model,
		})
		if err != nil {
			return "", err
		}
		return res.Text, nil
	}

	orch := chat.New(st, engine, deps, tz, cfg.AI.StreamReasoning)

	// AI 催办/周报/事件轮次挂在可用入口渠道上；没有 Telegram 时用 HTTP/API 会话。
	systemChannel := httpapi.Channel
	if strings.TrimSpace(cfg.TelegramToken) != "" {
		systemChannel = telegram.Provider
	}
	// 系统事件总线：领域事件交 AI 分析决定通知与行动（与调度器共用渠道与并发上限）。
	bus := events.New(st, orch, hub, systemChannel, cfg.SchedAIConcurrency)
	eventHub.Set(bus)

	// worker 内置智能体的模型管道：与中枢对话共用模型配置，HTTP 层按 provider
	// 适配 OpenAI 或 Claude/Anthropic 兼容协议。
	llm := httpapi.LLMConfig{
		Provider: cfg.AI.Provider, BaseURL: cfg.AI.BaseURL, APIKey: cfg.AI.APIKey,
		Model: cfg.AI.Model, MaxTokens: cfg.AI.MaxTokens, TimeoutMS: cfg.AI.TimeoutMS,
	}
	api := httpapi.New(st, orch, deps, bus, llm, cfg.FileStorePath, cfg.WorkerDownloadPath)

	var tg *telegram.Gateway
	if strings.TrimSpace(cfg.TelegramToken) != "" {
		var err error
		sttClient := stt.New(cfg.AI) // 未配置 stt_model 时为 nil，语音消息提示改用文字
		if sttClient != nil {
			slog.Info("语音转写已启用", "stt_model", cfg.AI.STTModel)
		}
		tg, err = telegram.New(cfg.TelegramToken, st, orch, bus, cfg.Superadmins, cfg.AI.Model, cfg.AI.BaseURL, cfg.AI.APIKey, sttClient, cfg.FileStorePath)
		if err != nil {
			return err
		}
		hub.Set(tg)
		tgGroups.Set(tg)
	} else {
		slog.Info("未配置 telegram_token，跳过 Telegram 网关；HTTP/API/MCP/worker 仍可用")
	}
	scheduler := sched.New(st, hub, orch, bus, systemChannel, tz, *cfg.DailySummaryHour, cfg.SchedAIConcurrency)

	scheme := "http"
	if strings.TrimSpace(cfg.TLSCertFile) != "" {
		scheme = "https"
	}
	slog.Info("nbco 启动", "engine", engine.Name(), "listen", cfg.Listen, "scheme", scheme, "tz", tz.String())

	errCh := make(chan error, 2)
	go func() { errCh <- api.Serve(ctx, cfg.Listen, cfg.TLSCertFile, cfg.TLSKeyFile) }()
	go scheduler.Run(ctx)
	if tg != nil {
		go tg.Run(ctx)
	}

	select {
	case <-ctx.Done():
		slog.Info("收到退出信号，正在关停")
		return nil
	case err := <-errCh:
		return err
	}
}

// buildEngine 按配置构建 AI 引擎。中枢只走 API 引擎；CLI 只允许在 worker 端用交互式 PTY。
func buildEngine(ctx context.Context, cfg *config.Config) (ai.Engine, error) {
	if cfg.AI.Engine == config.EngineEino {
		eng, err := einoengine.New(ctx, cfg.AI)
		return eng, err
	}
	return nil, fmt.Errorf("不支持的 ai.engine: %q（中枢只支持 eino；CLI 自动干活请用 nbco-worker 交互式 PTY）", cfg.AI.Engine)
}

// backfillKnowledge 启动后分批给存量知识补 embedding（首次启用语义检索或换模型时）。
// 收敛与防死循环：探测/DB 出错即停；用 id 游标扫描整库，单条内容失败只跳过
// 本轮，不挡住后续知识；拉不满一批即停（已扫完）；批间小憩，不打爆本地服务。
func backfillKnowledge(ctx context.Context, kb *knowledge.Service) {
	const batch = 64
	total := 0
	var cursor int64
	for ctx.Err() == nil {
		res, err := kb.Backfill(ctx, batch, cursor)
		if err != nil {
			slog.Warn("知识 embedding 回填中止（服务不可用，重启后再试）", "err", err)
			break
		}
		total += res.Embedded
		if res.LastID > cursor {
			cursor = res.LastID
		}
		if res.Attempted == 0 || !res.HasMore {
			break // 已扫完
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second): // 批间小憩
		}
	}
	if total > 0 {
		slog.Info("知识 embedding 回填完成", "count", total)
	}
}

// backfillMessages 给存量会话消息补 embedding（情景记忆），节奏同知识回填。
func backfillMessages(ctx context.Context, kb *knowledge.Service) {
	const batch = 64
	total := 0
	var cursor int64
	for ctx.Err() == nil {
		res, err := kb.BackfillMessages(ctx, batch, cursor)
		if err != nil {
			slog.Warn("消息 embedding 回填中止（服务不可用，重启后再试）", "err", err)
			break
		}
		total += res.Embedded
		if res.LastID > cursor {
			cursor = res.LastID
		}
		if res.Attempted == 0 || !res.HasMore {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	if total > 0 {
		slog.Info("消息 embedding 回填完成", "count", total)
	}
}

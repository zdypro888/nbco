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
	"syscall"
	"time"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/ai/einoengine"
	"github.com/zdypro888/nbco/internal/chat"
	"github.com/zdypro888/nbco/internal/config"
	"github.com/zdypro888/nbco/internal/gateway/httpapi"
	"github.com/zdypro888/nbco/internal/gateway/telegram"
	"github.com/zdypro888/nbco/internal/mcptools"
	"github.com/zdypro888/nbco/internal/notify"
	"github.com/zdypro888/nbco/internal/sched"
	"github.com/zdypro888/nbco/internal/store"
	"github.com/zdypro888/nbco/internal/tools"
	"github.com/zdypro888/nbco/internal/workerhub"
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
	deps := tools.Deps{Store: st, Notifier: hub, TZ: tz, Workers: workerhub.New()}

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

	orch := chat.New(st, engine, deps, tz)

	tg, err := telegram.New(cfg.TelegramToken, st, orch, cfg.Superadmins)
	if err != nil {
		return err
	}
	hub.Set(tg)

	api := httpapi.New(st, orch, deps)

	// AI 催办/周报轮次挂在 Telegram 渠道会话上（与主入口一致，上下文连续）。
	scheduler := sched.New(st, hub, orch, telegram.Provider, tz, *cfg.DailySummaryHour, cfg.SchedAIConcurrency)

	slog.Info("nbco 启动", "engine", engine.Name(), "listen", cfg.Listen, "tz", tz.String())

	errCh := make(chan error, 2)
	go func() { errCh <- api.Serve(ctx, cfg.Listen) }()
	go scheduler.Run(ctx)
	go tg.Run(ctx)

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

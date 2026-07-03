// nbco：单二进制公司运营中枢。
// 一个进程装下全部：Telegram 网关、HTTP API/MCP、AI 引擎、调度器；状态全部落库。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zdypro888/nbco/internal/ai"
	"github.com/zdypro888/nbco/internal/ai/claudecli"
	"github.com/zdypro888/nbco/internal/ai/einoengine"
	"github.com/zdypro888/nbco/internal/chat"
	"github.com/zdypro888/nbco/internal/config"
	"github.com/zdypro888/nbco/internal/gateway/httpapi"
	"github.com/zdypro888/nbco/internal/gateway/telegram"
	"github.com/zdypro888/nbco/internal/notify"
	"github.com/zdypro888/nbco/internal/sched"
	"github.com/zdypro888/nbco/internal/store"
	"github.com/zdypro888/nbco/internal/tools"
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
	deps := tools.Deps{Store: st, Notifier: hub, TZ: tz}

	engine, cliHandler, err := buildEngine(ctx, cfg)
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
	api.CLIHandler = cliHandler

	scheduler := sched.New(st, hub, tz, *cfg.DailySummaryHour)

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

// buildEngine 按配置构建 AI 引擎；claudecli 引擎同时返回回连 MCP handler。
func buildEngine(ctx context.Context, cfg *config.Config) (ai.Engine, http.Handler, error) {
	switch cfg.AI.Engine {
	case config.EngineEino:
		eng, err := einoengine.New(ctx, cfg.AI)
		return eng, nil, err
	case config.EngineClaudeCLI:
		registry := claudecli.NewRegistry()
		base := strings.TrimRight(cfg.PublicBaseURL, "/")
		if base == "" {
			_, port, err := net.SplitHostPort(cfg.Listen)
			if err != nil {
				return nil, nil, fmt.Errorf("listen 地址 %q: %w", cfg.Listen, err)
			}
			base = "http://127.0.0.1:" + port
		}
		eng, err := claudecli.New(cfg.AI, registry, base+"/mcp/cli")
		return eng, registry.Handler(), err
	default:
		return nil, nil, fmt.Errorf("不支持的 ai.engine: %q", cfg.AI.Engine)
	}
}

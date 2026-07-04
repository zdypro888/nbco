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
	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/config"
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

	// 语义检索的 embedder（可选）：配了 ai.embed_model 才构建，否则知识检索回退词法。
	embedder, err := embed.New(cfg.AI)
	if err != nil {
		return err
	}
	kb := knowledge.New(st, embedder)
	if embedder != nil {
		slog.Info("语义检索已启用", "embed_model", embedder.Model())
		go backfillKnowledge(ctx, kb)
	}

	deps := tools.Deps{
		Store:                    st,
		Notifier:                 hub,
		TZ:                       tz,
		Knowledge:                kb,
		Workers:                  workerhub.New(),
		AIStreamReasoningDefault: cfg.AI.StreamReasoning,
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

	orch := chat.New(st, engine, deps, tz, cfg.AI.StreamReasoning)

	api := httpapi.New(st, orch, deps, cfg.FileStorePath)

	// AI 催办/周报轮次挂在可用入口渠道上；没有 Telegram 时用 HTTP/API 会话。
	schedulerChannel := httpapi.Channel
	var tg *telegram.Gateway
	if strings.TrimSpace(cfg.TelegramToken) != "" {
		var err error
		tg, err = telegram.New(cfg.TelegramToken, st, orch, cfg.Superadmins)
		if err != nil {
			return err
		}
		hub.Set(tg)
		schedulerChannel = telegram.Provider
	} else {
		slog.Info("未配置 telegram_token，跳过 Telegram 网关；HTTP/API/MCP/worker 仍可用")
	}
	scheduler := sched.New(st, hub, orch, schedulerChannel, tz, *cfg.DailySummaryHour, cfg.SchedAIConcurrency)

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
// 收敛与防死循环：探测/DB 出错即停；本批「零成功」即停（服务不可用，不空转重试）；
// 拉不满一批即停（已补完）；批间小憩，不打爆本地 embedding 服务。
func backfillKnowledge(ctx context.Context, kb *knowledge.Service) {
	const batch = 64
	total := 0
	for ctx.Err() == nil {
		attempted, embedded, err := kb.Backfill(ctx, batch)
		if err != nil {
			slog.Warn("知识 embedding 回填中止（服务不可用，重启后再试）", "err", err)
			break
		}
		total += embedded
		if embedded == 0 || attempted < batch {
			break // 零推进（服务失败）或已补完
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

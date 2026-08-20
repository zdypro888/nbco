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
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/ai/einoengine"
	"github.com/zdypro888/nbco/ai/embed"
	"github.com/zdypro888/nbco/ai/stt"
	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/config"
	"github.com/zdypro888/nbco/documentindex"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/gateway/httpapi"
	"github.com/zdypro888/nbco/gateway/telegram"
	"github.com/zdypro888/nbco/knowledge"
	"github.com/zdypro888/nbco/mcptools"
	"github.com/zdypro888/nbco/notify"
	"github.com/zdypro888/nbco/sched"
	"github.com/zdypro888/nbco/semantic"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
	"github.com/zdypro888/nbco/vectorstore"
	"github.com/zdypro888/nbco/workerhub"
)

var version = "dev"

func runtimeVersion() string {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(version, info)
}

func resolveVersion(linked string, info *debug.BuildInfo) string {
	if linked = strings.TrimSpace(linked); linked != "" && linked != "dev" {
		return linked
	}
	if info == nil {
		return "dev"
	}
	revision := ""
	dirty := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if dirty {
			revision += "-dirty"
		}
		return revision
	}
	if moduleVersion := strings.TrimSpace(info.Main.Version); moduleVersion != "" && moduleVersion != "(devel)" {
		return moduleVersion
	}
	return "dev"
}

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
	httpapi.Version = runtimeVersion()
	tz, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("时区 %q: %w", cfg.Timezone, err)
	}

	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer st.Close()
	staleTurnAge := time.Duration(cfg.AI.TurnTimeoutMS)*time.Millisecond + 2*time.Minute
	if count, err := st.FailStaleConversationTurns(ctx, staleTurnAge); err != nil {
		return fmt.Errorf("清理失联对话轮次: %w", err)
	} else if count > 0 {
		slog.Warn("已关闭失联对话轮次，禁止自动重放未知副作用", "count", count)
	}

	hub := &notify.Hub{}
	tgGroups := &tools.TelegramGroupHub{}
	eventHub := &tools.EventHub{} // 事件总线后注入容器（bus 依赖 orch，orch 依赖 deps）

	// 语义检索的 embedder（可选）：配了 ai.embed_model 才构建，否则知识检索回退词法。
	embedder, err := embed.New(cfg.AI)
	if err != nil {
		return err
	}
	var semanticService *semantic.Service
	if cfg.Qdrant.Enabled() {
		qdrantStore, err := vectorstore.NewQdrant(vectorstore.QdrantConfig{
			URL: cfg.Qdrant.URL, APIKey: cfg.Qdrant.APIKey,
			CollectionPrefix: cfg.Qdrant.CollectionPrefix,
		})
		if err != nil {
			return err
		}
		defer func() {
			if err := qdrantStore.Close(); err != nil {
				slog.Warn("关闭 Qdrant 客户端失败", "err", err)
			}
		}()
		semanticService = semantic.New(st, embedder, qdrantStore)
		go semanticService.Run(ctx,
			time.Duration(cfg.Qdrant.SyncIntervalSeconds)*time.Second,
			time.Duration(cfg.Qdrant.SyncTimeoutSeconds)*time.Second)
		slog.Info("Qdrant 统一语义索引已配置", "url", cfg.Qdrant.URL, "collection_prefix", cfg.Qdrant.CollectionPrefix)
	}
	kb := knowledge.New(st, embedder, semanticService)
	if embedder != nil {
		slog.Info("语义检索已启用", "embed_model", embedder.Model())
		var knowledgeBackfillMu, messageBackfillMu sync.Mutex
		go runBackfillLoop(ctx, "knowledge", 10*time.Minute, func(ctx context.Context) {
			runBackfillExclusive(&knowledgeBackfillMu, func() { backfillKnowledge(ctx, kb, false) })
		})
		go runBackfillLoop(ctx, "messages", 10*time.Minute, func(ctx context.Context) {
			runBackfillExclusive(&messageBackfillMu, func() { backfillMessages(ctx, kb, false) }) // 情景记忆：只消费未完成标记
		})
		if semanticService != nil {
			go runBackfillLoop(ctx, "semantic_reconciliation", 6*time.Hour, func(ctx context.Context) {
				runBackfillExclusive(&knowledgeBackfillMu, func() { backfillKnowledge(ctx, kb, true) })
				runBackfillExclusive(&messageBackfillMu, func() { backfillMessages(ctx, kb, true) })
			})
		}
	}
	fileIndexer := documentindex.New(st, semanticService, cfg.FileStorePath)
	if fileIndexer.Enabled() {
		go fileIndexer.Run(ctx, 0)
		slog.Info("文件正文后台索引已启用", "file_store_path", cfg.FileStorePath)
	}

	deps := tools.Deps{
		Store:                    st,
		Notifier:                 hub,
		TZ:                       tz,
		BrandName:                cfg.BrandName,
		Knowledge:                kb,
		Semantic:                 semanticService,
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

	engine, err := buildEngine(ctx, cfg, st)
	if err != nil {
		return err
	}
	deps.SubcallAI = func(ctx context.Context, u *store.User, call tools.SubcallRequest) (string, error) {
		actx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		name := ""
		userID := int64(0)
		if u != nil {
			name = u.Name
			userID = u.ID
		}
		purpose := strings.TrimSpace(call.Purpose)
		if purpose == "" {
			purpose = "internal"
		}
		model := strings.TrimSpace(cfg.AI.Model)
		if runtimeModel, err := st.GetKV(actx, store.KVAIModel); err == nil && strings.TrimSpace(runtimeModel) != "" {
			model = strings.TrimSpace(runtimeModel)
		}
		res, err := engine.RunTurn(actx, &ai.TurnRequest{
			Mode:            ai.TurnModeOneShot,
			SessionID:       "subcall:" + purpose,
			System:          "你是公司运营系统内部的受控 AI 子调用。只完成指定的单一分析任务，不调用工具，不输出无关解释。",
			UserText:        fmt.Sprintf("调用者：%s\n\n%s", name, call.Prompt),
			Model:           model,
			MaxOutputTokens: call.MaxOutputTokens,
			Reasoning:       call.Reasoning,
			JSONOutput:      call.JSONOutput,
		})
		if err != nil {
			return "", err
		}
		if userID > 0 && (res.Usage.InputTokens > 0 || res.Usage.OutputTokens > 0) {
			if err := st.RecordAIUsage(actx, store.AIUsage{
				UserID: userID, Kind: "subcall_" + purpose, Model: model,
				InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens,
			}); err != nil {
				slog.Warn("AI 子调用用量落库失败", "purpose", purpose, "err", err)
			}
		}
		return res.Text, nil
	}

	orch := chat.New(st, engine, deps, tz, cfg.AI.StreamReasoning, time.Duration(cfg.AI.TurnTimeoutMS)*time.Millisecond)
	go orch.RunMemoryMiner(ctx)

	// AI 催办/周报/事件轮次挂在可用入口渠道上；没有 Telegram 时用 HTTP/API 会话。
	systemChannel := httpapi.Channel
	if strings.TrimSpace(cfg.TelegramToken) != "" {
		systemChannel = telegram.Provider
	}
	// 系统事件总线：领域事件交 AI 分析决定通知与行动（与调度器共用渠道与并发上限）。
	bus := events.New(st, orch, hub, systemChannel, cfg.SchedAIConcurrency)
	eventHub.Set(bus)
	go bus.Run(ctx)

	// worker 内置智能体的模型管道：与中枢对话共用模型配置，HTTP 层按 provider
	// 适配 OpenAI 或 Claude/Anthropic 兼容协议。
	llm := httpapi.LLMConfig{
		Provider: cfg.AI.Provider, BaseURL: cfg.AI.BaseURL, APIKey: cfg.AI.APIKey,
		Model: cfg.AI.Model, MaxTokens: cfg.AI.MaxTokens,
		MaxCompletionTokens: cfg.AI.MaxCompletionTokens, ReasoningEffort: cfg.AI.ReasoningEffort,
		TimeoutMS: cfg.AI.TimeoutMS,
	}
	api := httpapi.New(st, orch, deps, bus, llm, cfg.FileStorePath, cfg.WorkerDownloadPath, cfg.TelegramToken)
	if err := api.EnableIHTML(); err != nil {
		return fmt.Errorf("启用 ihtml 动态工作台: %w", err)
	}
	defer func() {
		if err := api.Close(); err != nil {
			slog.Warn("关闭 ihtml 动态工作台失败", "err", err)
		}
	}()
	slog.Info("ihtml 动态工作台已启用", "path", "/ui/", "agent", "shared-eino")

	var tg *telegram.Gateway
	if strings.TrimSpace(cfg.TelegramToken) != "" {
		var err error
		sttClient := stt.New(cfg.AI) // 未配置 stt_model 时为 nil，语音消息提示改用文字
		if sttClient != nil {
			slog.Info("语音转写已启用", "stt_model", cfg.AI.STTModel)
		}
		tg, err = telegram.New(cfg.TelegramToken, cfg.TelegramAPIURL, cfg.TelegramWebhookURL, st, orch, bus, cfg.Superadmins, cfg.AI.Model, cfg.AI.BaseURL, cfg.AI.APIKey, sttClient, cfg.FileStorePath, cfg.PublicBaseURL, cfg.BrandName, tz)
		if err != nil {
			return err
		}
		if tg.WebhookEnabled() {
			if err := api.SetTelegramWebhook(tg.WebhookPath(), tg.WebhookHandler()); err != nil {
				return fmt.Errorf("挂载 Telegram webhook: %w", err)
			}
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
	slog.Info("nbco 启动", "version", httpapi.Version, "engine", engine.Name(), "listen", cfg.Listen, "scheme", scheme, "tz", tz.String())

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
func buildEngine(ctx context.Context, cfg *config.Config, st *store.Store) (ai.Engine, error) {
	if cfg.AI.Engine == config.EngineEino {
		eng, err := einoengine.New(ctx, cfg.AI,
			einoengine.WithRuntimeStore(einoengine.NewPostgresRuntimeStore(st.Pool())))
		return eng, err
	}
	return nil, fmt.Errorf("不支持的 ai.engine: %q（中枢只支持 eino；CLI 自动干活请用 nbco-worker 交互式 PTY）", cfg.AI.Engine)
}

// runBackfillLoop 启动即执行，随后周期重扫。异步嵌入在洪峰时会主动丢弃
// 请求以保护主链路，周期回填保证这些记录无需重启也能最终补齐。
func runBackfillLoop(ctx context.Context, name string, interval time.Duration, run func(context.Context)) {
	if interval <= 0 || run == nil {
		return
	}
	for ctx.Err() == nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("embedding 回填 panic 已恢复", "kind", name, "panic", r)
				}
			}()
			run(ctx)
		}()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func runBackfillExclusive(mu *sync.Mutex, run func()) {
	mu.Lock()
	defer mu.Unlock()
	run()
}

// backfillKnowledge 分批给存量知识补 embedding（首次启用语义检索、换模型或
// 热路径限流漏嵌入时）。
// 收敛与防死循环：探测/DB 出错即停；用 id 游标扫描整库，单条内容失败只跳过
// 本轮，不挡住后续知识；拉不满一批即停（已扫完）；批间小憩，不打爆本地服务。
func backfillKnowledge(ctx context.Context, kb *knowledge.Service, reconcile bool) {
	const batch = 64
	total := 0
	complete := false
	var cursor int64
	for ctx.Err() == nil {
		var res knowledge.BackfillResult
		var err error
		if reconcile {
			res, err = kb.ReconcileKnowledge(ctx, batch, cursor)
		} else {
			res, err = kb.Backfill(ctx, batch, cursor)
		}
		if err != nil {
			slog.Warn("知识 embedding 本轮回填中止（服务不可用，稍后重试）", "err", err)
			break
		}
		total += res.Embedded
		if res.LastID > cursor {
			cursor = res.LastID
		}
		if res.Attempted == 0 || !res.HasMore {
			complete = true
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
	if complete {
		if err := kb.ClearLegacyKnowledgeVectors(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("知识 PostgreSQL 旧向量清理失败", "err", err)
		}
	}
	if reconcile {
		if err := kb.CleanupKnowledgeIndex(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("知识 Qdrant 孤儿索引清理失败", "err", err)
		}
	}
}

// backfillMessages 给存量会话消息补 embedding（情景记忆），节奏同知识回填。
func backfillMessages(ctx context.Context, kb *knowledge.Service, reconcile bool) {
	const batch = 64
	total := 0
	complete := false
	var cursor int64
	for ctx.Err() == nil {
		var res knowledge.BackfillResult
		var err error
		if reconcile {
			res, err = kb.ReconcileMessages(ctx, batch, cursor)
		} else {
			res, err = kb.BackfillMessages(ctx, batch, cursor)
		}
		if err != nil {
			slog.Warn("消息 embedding 本轮回填中止（服务不可用，稍后重试）", "err", err)
			break
		}
		total += res.Embedded
		if res.LastID > cursor {
			cursor = res.LastID
		}
		if res.Attempted == 0 || !res.HasMore {
			complete = true
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
	if complete {
		if err := kb.ClearLegacyMessageVectors(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("消息 PostgreSQL 旧向量清理失败", "err", err)
		}
	}
	if reconcile {
		if err := kb.CleanupMessageIndex(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("消息 Qdrant 孤儿索引清理失败", "err", err)
		}
	}
}

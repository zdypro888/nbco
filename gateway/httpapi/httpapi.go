// Package httpapi 是 HTTP 入口：内嵌 Web 页 + REST 对话/任务接口 + 对外 MCP 端点 + CLI 回连端点。
// 认证支持 Authorization: Bearer <api token> 与 Telegram Mini App 签名；
// 两者都不使用查询参数，避免凭据进入访问日志。
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/config"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/knowledge"
	"github.com/zdypro888/nbco/mcpbridge"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
	"github.com/zdypro888/nbco/tools"
	"github.com/zdypro888/nbco/workerproto"
)

// Channel HTTP 渠道标识（Web 页与 REST 共用同一会话）。
const Channel = "api"

const maxJSONBodyBytes = 1 << 20

var Version = "dev"

func workerFinalization(id, claimID, kind string, payload any) store.WorkerRunFinalization {
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	hash := fmt.Sprintf("%x", sum[:])
	id = strings.TrimSpace(id)
	if id == "" {
		legacy := sha256.Sum256([]byte(strings.TrimSpace(claimID) + "\x00" + kind + "\x00" + hash))
		id = "legacy-" + fmt.Sprintf("%x", legacy[:16])
	}
	return store.WorkerRunFinalization{ID: id, Hash: hash}
}

//go:embed web/index.html
var indexHTML []byte

//go:embed web/app.css web/app.js
var webAssets embed.FS

// LLMConfig worker 内置智能体的模型管道配置（/api/worker/llm 透传代理）。
// 中枢只做管道：model 服务端钉死、API key 不出中枢、内容不解析。
type LLMConfig struct {
	Provider            string
	BaseURL             string // OpenAI 或 Claude/Anthropic 兼容网关地址；空 = 管道关闭
	APIKey              string
	Model               string
	MaxTokens           int
	MaxCompletionTokens int
	ReasoningEffort     string
	TimeoutMS           int
}

// Server HTTP 入口。
type Server struct {
	store         *store.Store
	orch          *chat.Orchestrator
	deps          tools.Deps
	bus           *events.Bus // 系统事件总线（可为 nil）：worker 上线/任务提交等交 AI 分析
	llm           LLMConfig
	llmMu         sync.Mutex
	llmSem        chan struct{} // llm 管道限并发（与 sched/events 同理：护住模型网关）
	fileStorePath string
	downloadPath  string
	telegramToken string
	ihtmlHandler  http.Handler
	ihtmlTickets  *ihtmlTicketManager
	ihtmlClose    func() error
}

// New 创建 HTTP 入口。
func New(s *store.Store, orch *chat.Orchestrator, deps tools.Deps, bus *events.Bus, llm LLMConfig, fileStorePath, downloadPath, telegramToken string) *Server {
	return &Server{store: s, orch: orch, deps: deps, bus: bus, llm: llm,
		llmSem: make(chan struct{}, llmProxyConcurrency), fileStorePath: fileStorePath,
		downloadPath: downloadPath, telegramToken: strings.TrimSpace(telegramToken)}
}

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		nonce, err := newCSPNonce()
		if err != nil {
			http.Error(w, "control center unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy(nonce))
		_, _ = w.Write(bytes.ReplaceAll(indexHTML, []byte("{{CSP_NONCE}}"), []byte(nonce)))
	})
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(mustWebAssetFS()))))
	if s.ihtmlHandler != nil {
		workspaceRedirect := http.RedirectHandler("/?view=workspace", http.StatusTemporaryRedirect)
		mux.Handle("GET /ui", workspaceRedirect)
		mux.Handle("GET /ui/{$}", workspaceRedirect)
		mux.Handle("/ui/", http.StripPrefix("/ui", s.ihtmlHandler))
		mux.HandleFunc("POST /api/ihtml/ticket", s.handleIHTMLTicket)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// 探活 DB：死 200 会让流量继续打到一个 DB 已断的实例。短超时避免 healthz 自身拖垮。
		if s.store == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.store.Ping(ctx); err != nil {
			slog.Warn("healthz DB 探活失败", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("db unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /downloads/worker/{name}", s.handleWorkerDownloadBinary)
	mux.HandleFunc("POST /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/users", s.handleUsers)
	mux.HandleFunc("GET /api/me/tasks", s.handleMyTasks)
	mux.HandleFunc("GET /api/me/review", s.handleReview)
	mux.HandleFunc("GET /api/me/assigned", s.handleAssigned)
	mux.HandleFunc("GET /api/schedules", s.handleSchedules)
	mux.HandleFunc("POST /api/tools/{name}", s.handleToolInvoke)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/admin/workers", s.handleAdminWorkers)
	mux.HandleFunc("GET /api/admin/task-queue", s.handleAdminTaskQueue)
	mux.HandleFunc("GET /api/admin/worker-runs", s.handleAdminWorkerRuns)
	mux.HandleFunc("GET /api/admin/learning", s.handleAdminLearning)
	mux.HandleFunc("GET /api/admin/decisions", s.handleAdminDecisions)
	mux.HandleFunc("GET /api/admin/approvals", s.handleAdminApprovals)
	mux.HandleFunc("GET /api/admin/action-turns", s.handleAdminActionTurns)
	mux.HandleFunc("GET /api/admin/ops", s.handleAdminOps)
	mux.HandleFunc("GET /api/admin/capabilities", s.handleAdminCapabilities)
	mux.HandleFunc("GET /api/admin/workflows", s.handleAdminWorkflows)
	mux.HandleFunc("POST /api/admin/workflows/start", s.handleAdminStartWorkflow)
	mux.HandleFunc("GET /api/admin/ai-settings", s.handleAdminAISettings)
	mux.HandleFunc("POST /api/admin/ai-settings", s.handleAdminSetAISettings)
	mux.HandleFunc("GET /api/files", s.handleListFiles)
	mux.HandleFunc("POST /api/files", s.handleUploadFile)
	mux.HandleFunc("GET /api/files/{id}", s.handleDownloadFile)
	mux.HandleFunc("POST /api/tasks/{id}/attachments", s.handleAttachFile)
	// AI 员工（worker client）接口。任务队列走 HTTP（DB 为准），WS 做实时增强。
	mux.HandleFunc("POST /api/worker/bind", s.handleWorkerBind)
	mux.HandleFunc("POST /api/worker/capabilities", s.handleWorkerCapabilities)
	mux.HandleFunc("POST /api/worker/llm", s.handleWorkerLLM)
	mux.HandleFunc("GET /api/worker/next", s.handleWorkerNext)
	mux.HandleFunc("POST /api/worker/heartbeat", s.handleWorkerHeartbeat)
	mux.HandleFunc("POST /api/worker/progress", s.handleWorkerProgress)
	mux.HandleFunc("POST /api/worker/session", s.handleWorkerSession)
	mux.HandleFunc("POST /api/worker/request-input", s.handleWorkerRequestInput)
	mux.HandleFunc("POST /api/worker/fail", s.handleWorkerFail)
	mux.HandleFunc("POST /api/worker/submit", s.handleWorkerSubmit)
	mux.HandleFunc("GET /api/worker/files/{id}", s.handleWorkerDownloadFile)
	mux.HandleFunc("POST /api/worker/artifacts", s.handleWorkerArtifact)
	mux.HandleFunc("GET /api/worker/ws", s.handleWorkerWS)
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(s.mcpServer, nil))
	return securityHeaders(mux)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(s.telegramToken) != "" {
		ready, ok := s.deps.Notifier.(interface{ Ready() bool })
		if !ok || !ready.Ready() {
			http.Error(w, "telegram unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy(""))
		next.ServeHTTP(w, r)
	})
}

func newCSPNonce() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw[:]), nil
}

func contentSecurityPolicy(scriptNonce string) string {
	scriptSrc := "'self' https://telegram.org"
	if scriptNonce = strings.TrimSpace(scriptNonce); scriptNonce != "" {
		scriptSrc += " 'nonce-" + scriptNonce + "'"
	}
	return "default-src 'self'; script-src " + scriptSrc + "; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; font-src 'self' https://cdn.jsdelivr.net data:; img-src 'self' data: blob:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'self' https://web.telegram.org https://*.telegram.org"
}

func mustWebAssetFS() fs.FS {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}
	return sub
}

// authenticate 解析 Bearer token 并返回启用状态的用户。
func (s *Server) authenticate(r *http.Request) *store.User {
	if strings.TrimSpace(r.Header.Get("X-Ihtml-User")) != "" {
		u, err := s.ihtmlUserFromTicket(r, false)
		if err != nil {
			return nil
		}
		return u
	}
	if raw := strings.TrimSpace(r.Header.Get("X-Telegram-Init-Data")); raw != "" {
		tgID, ok := validateTelegramInitData(raw, s.telegramToken, time.Now(), telegramInitDataMaxAge)
		if !ok {
			return nil
		}
		u, err := s.store.UserByIdentity(r.Context(), "telegram", strconv.FormatInt(tgID, 10))
		if err != nil || u.Status != store.UserActive {
			return nil
		}
		return u
	}
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if token == "" || token == auth {
		return nil
	}
	u, err := s.store.UserByAPIToken(r.Context(), token)
	if err != nil || u.Status != store.UserActive {
		return nil
	}
	return u
}

// requireUser 认证失败时写 401 并返回 nil。
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.authenticate(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未认证"})
	}
	return u
}

func writeJSON(w http.ResponseWriter, code int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	return json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// truncateRunes 转发到 textfmt.TruncateRunes（跨包共享实现）。
func truncateRunes(s string, n int) string { return textfmt.TruncateRunes(s, n) }

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message 必填"})
		return
	}
	// 对话可能耗时数分钟，脱离请求超时限制交给引擎自身控制。
	answer, err := s.orch.HandleMessage(r.Context(), u, Channel, req.Message)
	if err != nil {
		slog.Error("API 对话失败", "user", u.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "对话处理失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reply": answer})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "name": u.Name, "is_superadmin": u.IsSuperadmin,
		"is_worker": u.IsWorker, "owner_id": u.OwnerID,
	})
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit 必须是 1 到 100 的整数"})
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 10_000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "offset 必须是 0 到 10000 的整数"})
			return
		}
		offset = parsed
	}
	filters := map[string]string{}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		if status != store.UserActive && status != store.UserDisabled {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status 必须是 active 或 disabled"})
			return
		}
		filters["status"] = status
	}
	switch kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind {
	case "":
	case "human":
		filters["is_worker"] = "false"
	case "worker":
		filters["is_worker"] = "true"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind 必须是 human 或 worker"})
		return
	}
	var terms []string
	if query := strings.TrimSpace(r.URL.Query().Get("q")); query != "" {
		terms = []string{query}
	}
	rows, err := s.store.ReadData(r.Context(), u.ID, u.IsSuperadmin, store.DataReadQuery{
		Source: "users", Terms: terms, Filters: filters, Limit: limit, Offset: offset,
	})
	if err != nil {
		slog.Error("读取成员目录失败", "user", u.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取成员目录失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users": rows, "limit": limit, "offset": offset, "next_offset": offset + len(rows),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": Version,
		"go":      runtime.Version(),
	})
}

func (s *Server) requireSuper(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.requireUser(w, r)
	if u == nil {
		return nil
	}
	if !u.IsSuperadmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "需要超级管理员权限"})
		return nil
	}
	return u
}

func (s *Server) handleAdminWorkers(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	ownerID := u.ID
	if u.IsSuperadmin {
		ownerID = 0
	}
	ws, err := s.store.ListWorkers(r.Context(), ownerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 worker 失败"})
		return
	}
	ids := make([]int64, 0, len(ws))
	for _, worker := range ws {
		ids = append(ids, worker.ID)
	}
	caps, err := s.store.WorkerCapabilities(r.Context(), ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 worker 能力失败"})
		return
	}
	out := make([]map[string]any, 0, len(ws))
	for _, worker := range ws {
		out = append(out, map[string]any{
			"id": worker.ID, "name": worker.Name, "status": worker.Status,
			"owner_id": worker.OwnerID, "admin": worker.IsSuperadmin,
			"online": dOnline(s, worker.ID), "last_seen": worker.WorkerLastSeen,
			"capability": caps[worker.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": out})
}

func dOnline(s *Server, workerID int64) bool {
	return s.deps.Workers != nil && s.deps.Workers.Online(workerID)
}

func (s *Server) handleAdminTaskQueue(w http.ResponseWriter, r *http.Request) {
	u := s.requireSuper(w, r)
	if u == nil {
		return
	}
	ts, err := s.store.TaskQueue(r.Context(), strings.TrimSpace(r.URL.Query().Get("scope")), 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取任务队列失败"})
		return
	}
	names, err := s.userNames(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	tasks, err := s.tasksJSON(r.Context(), ts, names)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取任务参与者失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (s *Server) handleAdminWorkerRuns(w http.ResponseWriter, r *http.Request) {
	u := s.requireSuper(w, r)
	if u == nil {
		return
	}
	runs, err := s.store.ListWorkerRuns(r.Context(), u.ID, true, strings.TrimSpace(r.URL.Query().Get("scope")), 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 Worker 执行队列失败"})
		return
	}
	out := make([]workerRunJSON, 0, len(runs))
	names, err := s.userNames(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取执行参与者失败"})
		return
	}
	for _, run := range runs {
		item := toWorkerRunJSON(run)
		item.WorkerName = names[run.WorkerID]
		item.RequestedByName = names[run.RequestedBy]
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (s *Server) handleAdminLearning(w http.ResponseWriter, r *http.Request) {
	u := s.requireSuper(w, r)
	if u == nil {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = store.LearningStatusPending
	}
	_, _ = s.store.ScoreLearningCandidates(r.Context(), 200)
	items, err := s.store.ListLearningCandidates(r.Context(), status, strings.TrimSpace(r.URL.Query().Get("kind")), 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取学习候选失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": items})
}

func (s *Server) handleAdminDecisions(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	_, _ = s.store.BuildDecisionQueue(r.Context(), u.ID)
	items, err := s.store.ListDecisionItems(r.Context(), u.ID, "open", 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取决策队列失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": items})
}

func (s *Server) handleAdminApprovals(w http.ResponseWriter, r *http.Request) {
	u := s.requireSuper(w, r)
	if u == nil {
		return
	}
	items, err := s.store.ListPendingApprovals(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取待确认操作失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": items})
}

func (s *Server) handleAdminActionTurns(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	userID := u.ID
	if u.IsSuperadmin && scope == "all" {
		userID = 0
	}
	items, err := s.store.ListActionTurns(r.Context(), userID, 80)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取动作日志失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"turns": items})
}

func (s *Server) handleAdminOps(w http.ResponseWriter, r *http.Request) {
	u := s.requireSuper(w, r)
	if u == nil {
		return
	}
	migrations, err := s.store.AppliedMigrations(r.Context(), 20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取迁移状态失败"})
		return
	}
	einoRuntime, err := s.store.EinoRuntimeStats(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取 Eino 运行状态失败"})
		return
	}
	fileIndex, err := s.store.FileContentIndexStats(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取文件索引状态失败"})
		return
	}
	fileIndexStatus := structToMap(fileIndex)
	fileIndexStatus["vector_configured"] = s.deps.Semantic != nil && s.deps.Semantic.Enabled()
	messageIndex := map[string]any{"configured": false}
	if s.deps.Knowledge != nil && s.deps.Semantic != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		stats, statsErr := s.deps.Knowledge.MessageIndexStats(ctx)
		cancel()
		if statsErr != nil {
			messageIndex = map[string]any{"configured": true, "error": statsErr.Error()}
		} else {
			messageIndex = structToMap(stats)
			messageIndex["configured"] = true
		}
	}
	semanticStatus := map[string]any{"configured": false}
	if s.deps.Semantic != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		semanticStatus = structToMap(s.deps.Semantic.Health(ctx))
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    Version,
		"go":         runtime.Version(),
		"migrations": migrations,
		"workers": map[string]any{
			"hub_configured": s.deps.Workers != nil,
		},
		"engine":         s.engineHealth(),
		"eino_runtime":   einoRuntime,
		"semantic_index": semanticStatus,
		"file_index":     fileIndexStatus,
		"message_index":  messageIndex,
	})
}

func structToMap(value any) map[string]any {
	raw, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"configured": true, "error": err.Error()}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"configured": true, "error": err.Error()}
	}
	if out == nil {
		return make(map[string]any)
	}
	return out
}

func (s *Server) handleAdminCapabilities(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	deps := s.deps
	if deps.Store == nil {
		deps.Store = s.store
	}
	caps, err := tools.CapabilityRegistry(r.Context(), deps, u, u.IsSuperadmin)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取能力目录失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": caps})
}

func (s *Server) handleToolInvoke(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "工具不存在或无权限"})
		return
	}
	var args json.RawMessage
	if err := decodeJSON(w, r, &args); err != nil || !json.Valid(args) ||
		!strings.HasPrefix(strings.TrimSpace(string(args)), "{") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "工具参数必须是 JSON 对象"})
		return
	}
	deps := s.deps
	if deps.Store == nil {
		deps.Store = s.store
	}
	toolset := tools.StripApprovalRequired(tools.ForUserContext(r.Context(), deps, u, nil))
	for _, item := range toolset {
		if item.Name != name {
			continue
		}
		result, err := item.Handler(r.Context(), args)
		if err != nil {
			slog.Error("HTTP 工具调用失败", "user", u.ID, "tool", name, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "工具执行失败"})
			return
		}
		response := map[string]any{"result": result}
		if parsed, ok := tools.ParseToolResult(result); ok {
			response["result"] = parsed.Message
			response["status"] = parsed.Status
			response["completion"] = parsed.Completion
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "工具不存在或无权限"})
}

func (s *Server) handleAdminWorkflows(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": tools.ListWorkflowTemplates()})
}

func (s *Server) handleAdminStartWorkflow(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	var req struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 必填"})
		return
	}
	deps := s.deps
	if deps.Store == nil {
		deps.Store = s.store
	}
	ok, reason, err := tools.CanStartWorkflow(r.Context(), deps, u, req.Name)
	if err != nil {
		slog.Error("校验工作流权限失败", "user", u.ID, "workflow", req.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "校验权限失败"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": reason})
		return
	}
	out, err := tools.StartWorkflow(r.Context(), deps, u, req.Name, req.Args)
	if err != nil {
		slog.Error("启动工作流失败", "user", u.ID, "workflow", req.Name, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "启动工作流失败"})
		return
	}
	response := map[string]any{"result": out}
	if result, ok := tools.ParseToolResult(out); ok {
		response["result"] = result.Message
		response["status"] = result.Status
		response["completion"] = result.Completion
	}
	writeJSON(w, http.StatusOK, response)
}

// engineHealth 返回引擎连续失败数与最近错误，供超管在 /api/admin/ops 看引擎是否挂了。
func (s *Server) engineHealth() map[string]any {
	if s.orch == nil {
		return map[string]any{"configured": false}
	}
	fails, lastErr := s.orch.EngineHealth()
	return map[string]any{
		"configured":        true,
		"consecutive_fails": fails,
		"last_error":        lastErr,
	}
}

func (s *Server) handleMyTasks(w http.ResponseWriter, r *http.Request) {
	s.taskList(w, r, func(ctx context.Context, u *store.User) ([]*store.Task, error) {
		return s.store.TasksOfAssignee(ctx, u.ID, true)
	})
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	s.taskList(w, r, func(ctx context.Context, u *store.User) ([]*store.Task, error) {
		return s.store.TasksAwaitingReview(ctx, u.ID)
	})
}

func (s *Server) handleAssigned(w http.ResponseWriter, r *http.Request) {
	s.taskList(w, r, func(ctx context.Context, u *store.User) ([]*store.Task, error) {
		return s.store.TasksOfAssigner(ctx, u.ID)
	})
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	items, err := s.store.SchedulesVisible(r.Context(), u.ID, u.IsSuperadmin, strings.TrimSpace(r.URL.Query().Get("status")), 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取定时任务失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": schedulesJSON(items)})
}

func (s *Server) taskList(w http.ResponseWriter, r *http.Request,
	fetch func(context.Context, *store.User) ([]*store.Task, error)) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	ts, err := fetch(r.Context(), u)
	if err != nil {
		slog.Error("任务列表查询失败", "user", u.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	names, err := s.userNames(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	tasks, err := s.tasksJSON(r.Context(), ts, names)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取任务参与者失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// handleOverview 老板全景（超管专用）。
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !u.IsSuperadmin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "仅超管可见"})
		return
	}
	ctx := r.Context()
	stats, err := s.store.GlobalTaskStats(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	counts, err := s.store.ProjectTaskCounts(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	overdue, err := s.store.OverdueTasks(ctx, 20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	names, err := s.userNames(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	type projJSON struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Status   string `json:"status"`
		Open     int64  `json:"open"`
		Awaiting int64  `json:"awaiting"`
		Accepted int64  `json:"accepted"`
	}
	pjs := make([]projJSON, 0, len(projects))
	for _, p := range projects {
		c := counts[p.ID]
		pjs = append(pjs, projJSON{ID: p.ID, Name: p.Name, Status: p.Status,
			Open: c.Open, Awaiting: c.Awaiting, Accepted: c.Accepted})
	}
	overdueJSON, err := s.tasksJSON(ctx, overdue, names)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取任务参与者失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stats": map[string]int64{
			"open": stats.Open, "awaiting_input": stats.AwaitingInput, "overdue": stats.Overdue,
			"awaiting": stats.Awaiting, "done_week": stats.DoneSince,
		},
		"projects": pjs,
		"overdue":  overdueJSON,
	})
}

// taskJSON 任务的对外表示（带人名，前端免二次查询）。
type taskJSON struct {
	ID               int64                   `json:"id"`
	ProjectID        int64                   `json:"project_id"`
	ParentID         *int64                  `json:"parent_id,omitempty"`
	Title            string                  `json:"title"`
	Goal             string                  `json:"goal,omitempty"`
	Description      string                  `json:"description,omitempty"`
	Acceptance       string                  `json:"acceptance,omitempty"`
	Status           string                  `json:"status"`
	Revision         int64                   `json:"revision"`
	Priority         string                  `json:"priority"`
	Deadline         *time.Time              `json:"deadline,omitempty"`
	CreatedAt        time.Time               `json:"created_at"`
	UpdatedAt        time.Time               `json:"updated_at"`
	AssignerID       int64                   `json:"assigner_id"`
	AssignerName     string                  `json:"assigner_name"`
	AssigneeID       int64                   `json:"assignee_id"`
	AssigneeName     string                  `json:"assignee_name"`
	Participants     []store.TaskParticipant `json:"participants"`
	SubmittedBy      *int64                  `json:"submitted_by,omitempty"`
	SubmittedByName  string                  `json:"submitted_by_name,omitempty"`
	SubmittedAt      *time.Time              `json:"submitted_at,omitempty"`
	CancelReason     string                  `json:"cancel_reason,omitempty"`
	CancelledAt      *time.Time              `json:"cancelled_at,omitempty"`
	SupersededBy     *int64                  `json:"superseded_by,omitempty"`
	NudgeCount       int64                   `json:"nudge_count,omitempty"`
	Execution        *workerRunJSON          `json:"execution,omitempty"`
	LatestProgress   string                  `json:"latest_progress,omitempty"`
	LatestProgressAt *time.Time              `json:"latest_progress_at,omitempty"`
}

type workerRunJSON struct {
	ID              int64      `json:"id"`
	TaskID          *int64     `json:"task_id,omitempty"`
	TaskRevision    *int64     `json:"task_revision,omitempty"`
	WorkerID        int64      `json:"worker_id"`
	WorkerName      string     `json:"worker_name,omitempty"`
	RequestedBy     int64      `json:"requested_by"`
	RequestedByName string     `json:"requested_by_name,omitempty"`
	Executor        string     `json:"executor"`
	Title           string     `json:"title"`
	ScopeKey        string     `json:"scope_key,omitempty"`
	ScopeTitle      string     `json:"scope_title,omitempty"`
	Status          string     `json:"status"`
	Outcome         string     `json:"outcome,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Attempts        int        `json:"attempts"`
	Failures        int        `json:"failures"`
	AvailableAt     time.Time  `json:"available_at"`
	LastError       string     `json:"last_error,omitempty"`
	Summary         string     `json:"summary,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func toWorkerRunJSON(run *store.WorkerRun) workerRunJSON {
	return workerRunJSON{
		ID: run.ID, TaskID: run.TaskID, TaskRevision: run.TaskRevision, WorkerID: run.WorkerID, RequestedBy: run.RequestedBy,
		Executor: string(run.Executor), Title: run.Title, ScopeKey: run.ScopeKey, ScopeTitle: run.ScopeTitle, Status: run.Status,
		Outcome: run.Outcome, ExitCode: run.ExitCode, Attempts: run.Attempts, Failures: run.Failures, AvailableAt: run.AvailableAt,
		LastError: run.LastError, Summary: run.Summary, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		CompletedAt: run.CompletedAt,
	}
}

func (s *Server) tasksJSON(ctx context.Context, ts []*store.Task, names map[int64]string) ([]taskJSON, error) {
	taskIDs := make([]int64, 0, len(ts))
	for _, task := range ts {
		taskIDs = append(taskIDs, task.ID)
	}
	participants, err := s.store.TaskParticipantsForTasks(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	latestProgress, err := s.store.LatestProgressForTasks(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	latestRuns, err := s.store.LatestWorkerRunsForTasks(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	out := make([]taskJSON, 0, len(ts))
	for _, t := range ts {
		taskParticipants := participants[t.ID]
		if taskParticipants == nil {
			taskParticipants = []store.TaskParticipant{}
		}
		submittedByName := ""
		if t.SubmittedBy != nil {
			submittedByName = names[*t.SubmittedBy]
		}
		var latest string
		var latestAt *time.Time
		if progress, ok := latestProgress[t.ID]; ok {
			latest = truncateRunes(progress.Content, 4000)
			at := progress.CreatedAt
			latestAt = &at
		}
		out = append(out, taskJSON{
			ID: t.ID, ProjectID: t.ProjectID, ParentID: t.ParentID,
			Title: t.Title, Goal: t.Goal, Description: t.Description, Acceptance: t.Acceptance,
			Status:   t.Status,
			Revision: t.Revision,
			Priority: t.Priority, Deadline: t.Deadline, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			AssignerID: t.AssignerID, AssignerName: names[t.AssignerID],
			AssigneeID: t.AssigneeID, AssigneeName: names[t.AssigneeID],
			Participants: taskParticipants, SubmittedBy: t.SubmittedBy, SubmittedByName: submittedByName, SubmittedAt: t.SubmittedAt,
			CancelReason: t.CancelReason, CancelledAt: t.CancelledAt, SupersededBy: t.SupersededBy,
			NudgeCount:     t.NudgeCount,
			LatestProgress: latest, LatestProgressAt: latestAt,
		})
		if run := latestRuns[t.ID]; run != nil {
			value := toWorkerRunJSON(run)
			value.WorkerName = names[run.WorkerID]
			value.RequestedByName = names[run.RequestedBy]
			out[len(out)-1].Execution = &value
		}
	}
	return out, nil
}

type scheduleJSON struct {
	ID           int64      `json:"id"`
	UserID       int64      `json:"user_id"`
	ReceiverName string     `json:"receiver_name"`
	CreatedBy    int64      `json:"created_by"`
	CreatorName  string     `json:"creator_name"`
	Target       string     `json:"target"`
	Mode         string     `json:"mode"`
	Kind         string     `json:"kind"`
	Message      string     `json:"message"`
	Title        string     `json:"title,omitempty"`
	SourceKind   string     `json:"source_kind,omitempty"`
	SourceKey    string     `json:"source_key,omitempty"`
	FireAt       time.Time  `json:"fire_at"`
	DailyAt      string     `json:"daily_at,omitempty"`
	Weekdays     string     `json:"weekdays,omitempty"`
	IntervalS    int64      `json:"interval_s,omitempty"`
	Status       string     `json:"status"`
	LastFired    *time.Time `json:"last_fired,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func schedulesJSON(items []store.ScheduleView) []scheduleJSON {
	out := make([]scheduleJSON, 0, len(items))
	for _, sc := range items {
		out = append(out, scheduleJSON{
			ID: sc.ID, UserID: sc.UserID, ReceiverName: sc.ReceiverName,
			CreatedBy: sc.CreatedBy, CreatorName: sc.CreatorName, Target: sc.Target,
			Mode: sc.Mode, Kind: sc.Kind, Message: sc.Message, Title: sc.Title,
			SourceKind: sc.SourceKind, SourceKey: sc.SourceKey, FireAt: sc.FireAt,
			DailyAt: sc.DailyAt, Weekdays: sc.Weekdays, IntervalS: sc.IntervalS,
			Status: sc.Status, LastFired: sc.LastFired, CreatedAt: sc.CreatedAt,
		})
	}
	return out
}

func (s *Server) userNames(ctx context.Context) (map[int64]string, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[int64]string, len(users))
	for _, u := range users {
		m[u.ID] = u.Name
	}
	return m, nil
}

// --- AI 员工（worker）接口 ---

const (
	workerKnowledgeHits     = 4
	workerHistoryEntries    = 10 // 领取任务时随带的最近过程记录条数（返工时含打回理由）
	workerContextLineRunes  = 1800
	workerContextTotalRunes = 16000
	workerHistoryLineRunes  = 2000
	workerHistoryTotalRunes = 12000
)

type workerFileJSON struct {
	fileJSON
	Kind    string `json:"kind,omitempty"`
	Caption string `json:"caption,omitempty"`
}

type workerSessionJSON struct {
	ID               int64  `json:"id"`
	Engine           string `json:"engine"`
	ScopeType        string `json:"scope_type"`
	ScopeKey         string `json:"scope_key"`
	Title            string `json:"title"`
	Workdir          string `json:"workdir,omitempty"`
	EngineSessionRef string `json:"engine_session_ref,omitempty"`
	Summary          string `json:"summary,omitempty"`
}

// requireWorker 认证并要求是 worker 用户。
func (s *Server) requireWorker(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.requireUser(w, r)
	if u == nil {
		return nil
	}
	if !u.IsWorker {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "仅 AI 员工令牌可用"})
		return nil
	}
	return u
}

// handleWorkerBind 用一次性绑定码兑换 Worker Access Token。
// 无需认证——绑定码本身就是凭据（短时效、一次一用、哈希落库）。
// 这样长期 token 只出现在工作机与本响应，绝不进入聊天与会话历史。
func (s *Server) handleWorkerBind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "code 必填"})
		return
	}
	u, token, err := s.store.RedeemWorkerBindCode(r.Context(), strings.TrimSpace(req.Code))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "绑定码无效或已过期，请让超管重新签发"})
		return
	}
	if err != nil {
		slog.Error("worker 绑定码兑换失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "兑换失败"})
		return
	}
	slog.Info("worker 绑定码兑换成功", "worker", u.ID)
	// 上线事件交监护人的 AI 分析：要不要通知、要不要顺手派活，AI 说了算。
	if u.OwnerID != nil {
		s.bus.Emit("AI员工上线", *u.OwnerID,
			fmt.Sprintf("AI 员工「%s」刚在工作机完成绑定，已可领取任务。", u.Name))
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "worker_id": u.ID, "worker_name": u.Name})
}

func (s *Server) handleWorkerCapabilities(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		Engine       string          `json:"engine"`
		CLIName      string          `json:"cli_name"`
		CLIVersion   string          `json:"cli_version"`
		OS           string          `json:"os"`
		Arch         string          `json:"arch"`
		Hostname     string          `json:"hostname"`
		Workdir      string          `json:"workdir"`
		Capabilities []string        `json:"capabilities"`
		Metadata     json.RawMessage `json:"metadata"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON 无效"})
		return
	}
	cap, err := s.store.UpsertWorkerCapability(r.Context(), store.WorkerCapabilityInput{
		WorkerID: u.ID, Engine: req.Engine, CLIName: req.CLIName, CLIVersion: req.CLIVersion,
		OS: req.OS, Arch: req.Arch, Hostname: req.Hostname, Workdir: req.Workdir,
		Capabilities: req.Capabilities, Metadata: req.Metadata,
	})
	if err != nil {
		slog.Warn("worker 能力上报失败", "worker", u.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存能力失败"})
		return
	}
	_ = s.store.WorkerHeartbeat(r.Context(), u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated_at": cap.UpdatedAt})
}

// llmProxyTimeout 单次上游模型调用的默认墙钟上限（agent 步长在 worker 侧另有控制）。
const llmProxyTimeout = 5 * time.Minute

// llmProxyBodyLimit 请求体上限：agent 对话记录会随步数增长，给足余量。
const llmProxyBodyLimit = 4 << 20

// llmProxyConcurrency 同时在途的上游模型调用上限（多 worker 同跑内置智能体时排队）。
const llmProxyConcurrency = 8

func (s *Server) llmSemaphore() chan struct{} {
	s.llmMu.Lock()
	defer s.llmMu.Unlock()
	if s.llmSem == nil {
		s.llmSem = make(chan struct{}, llmProxyConcurrency)
	}
	return s.llmSem
}

// handleWorkerLLM 内置智能体的模型管道：worker 固定发送 OpenAI 风格的
// messages/tools；中枢按配置转发到 OpenAI 兼容或 Claude/Anthropic 兼容端点，并把响应
// 统一转回 worker 认识的 OpenAI 风格。API key 只留在中枢。
func (s *Server) handleWorkerLLM(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	model := s.runtimeLLMModel(r.Context())
	if strings.TrimSpace(s.llm.BaseURL) == "" || strings.TrimSpace(model) == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "中枢未配置 API 模型，内置智能体不可用"})
		return
	}
	// 限并发排队：worker 客户端超时富余（6 分钟），排队优于打爆上游。
	sem := s.llmSemaphore()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-r.Context().Done():
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, llmProxyBodyLimit)
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体须是 JSON 对象"})
		return
	}
	body["model"] = model  // 服务端钉死，防 worker 指定任意模型
	body["stream"] = false // 管道不透传流式
	s.applyWorkerLLMBudget(body)

	var out []byte
	var status int
	var err error
	switch s.llmProvider() {
	case config.ProviderClaude:
		status, out, err = s.callWorkerLLMClaude(r.Context(), model, body)
	case config.ProviderOpenAI:
		status, out, err = s.callWorkerLLMOpenAI(r.Context(), model, body)
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "中枢未配置可用模型 provider，内置智能体不可用"})
		return
	}
	if err != nil {
		slog.Warn("worker llm 管道上游失败", "worker", u.ID, "provider", s.llmProvider(), "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status != http.StatusOK {
		slog.Warn("worker llm 管道上游非 200", "worker", u.ID, "provider", s.llmProvider(), "status", status)
	}
	if len(out) > llmProxyBodyLimit*2 {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "上游响应超出大小上限"})
		return
	}
	if status == http.StatusOK {
		// 用量落库含两次目标归因查询，异步执行：不阻塞 worker 拿响应（热路径），
		// 失败不阻断业务。用独立 context，响应已返回后仍能完成。
		recordWorkerLLMUsageAsync(s.store, u.ID, model, bytes.Clone(out))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func recordWorkerLLMUsageAsync(st *store.Store, userID int64, model string, out []byte) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("worker LLM 用量记录 panic 已恢复", "worker", userID, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		recordWorkerLLMUsage(ctx, st, userID, model, out)
	}()
}

func (s *Server) callWorkerLLMOpenAI(ctx context.Context, model string, body map[string]any) (int, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("请求体无法序列化")
	}
	ctx, cancel := context.WithTimeout(ctx, s.llmTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.llm.BaseURL, "/")+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return 0, nil, fmt.Errorf("构造上游请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	if s.llm.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.llm.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("上游模型服务不可达")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, llmProxyBodyLimit*2+1))
	if err != nil {
		return 0, nil, fmt.Errorf("读取上游响应失败")
	}
	return resp.StatusCode, out, nil
}

func (s *Server) callWorkerLLMClaude(ctx context.Context, model string, body map[string]any) (int, []byte, error) {
	reqBody, err := openAIWorkerBodyToClaude(body, model, s.llmMaxTokens())
	if err != nil {
		return 0, nil, err
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("请求体无法序列化")
	}
	ctx, cancel := context.WithTimeout(ctx, s.llmTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.llm.BaseURL, "/")+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return 0, nil, fmt.Errorf("构造上游请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if s.llm.APIKey != "" {
		req.Header.Set("X-API-Key", s.llm.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("上游模型服务不可达")
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, llmProxyBodyLimit*2+1))
	if err != nil {
		return 0, nil, fmt.Errorf("读取上游响应失败")
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, out, nil
	}
	converted, err := claudeWorkerRespToOpenAI(out)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, converted, nil
}

func (s *Server) llmProvider() string {
	p := strings.TrimSpace(s.llm.Provider)
	if p == "" {
		return config.ProviderOpenAI
	}
	return p
}

func (s *Server) llmTimeout() time.Duration {
	if s.llm.TimeoutMS <= 0 {
		return llmProxyTimeout
	}
	return time.Duration(s.llm.TimeoutMS) * time.Millisecond
}

func (s *Server) llmMaxTokens() int {
	budget, _ := s.llmOutputBudget()
	return budget
}

func (s *Server) llmOutputBudget() (int, string) {
	if s.llm.MaxCompletionTokens > 0 {
		return s.llm.MaxCompletionTokens, "max_completion_tokens"
	}
	if s.llm.MaxTokens > 0 {
		return s.llm.MaxTokens, "max_tokens"
	}
	return 4096, "max_tokens"
}

func (s *Server) applyWorkerLLMBudget(body map[string]any) {
	if body == nil {
		return
	}
	if s.llm.MaxCompletionTokens > 0 {
		body["max_completion_tokens"] = s.llm.MaxCompletionTokens
		delete(body, "max_tokens")
	} else if s.llm.MaxTokens > 0 {
		body["max_tokens"] = s.llm.MaxTokens
	}
	if effort := strings.TrimSpace(s.llm.ReasoningEffort); effort != "" {
		body["reasoning_effort"] = effort
	}
}

func recordWorkerLLMUsage(ctx context.Context, st *store.Store, userID int64, model string, out []byte) {
	if st == nil {
		return
	}
	var meta struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(out, &meta) != nil {
		return
	}
	in := meta.Usage.PromptTokens
	if in == 0 {
		in = meta.Usage.InputTokens
	}
	outTok := meta.Usage.CompletionTokens
	if outTok == 0 {
		outTok = meta.Usage.OutputTokens
	}
	if in == 0 && outTok == 0 {
		return
	}
	// 目标归因（尽力）：经 worker 最近活跃会话的 last_task_id → 任务里程碑 → 目标 解析。
	// worker 可能在非任务上下文下调用模型，或任务未挂里程碑，此时 goal_id 为 nil（记为未归因）。
	// ErrNotFound（无会话/任务已删）属正常的未归因，静默；其他错误记日志便于排查归因缺口。
	var goalID *int64
	if taskID, terr := st.LatestWorkerTaskID(ctx, userID); terr == nil && taskID != nil {
		gid, gerr := st.GoalIDOfTask(ctx, *taskID)
		if gerr == nil {
			goalID = gid
		} else if !errors.Is(gerr, store.ErrNotFound) {
			slog.Warn("worker llm 目标归因失败", "worker", userID, "task", *taskID, "err", gerr)
		}
	} else if terr != nil && !errors.Is(terr, store.ErrNotFound) {
		slog.Warn("worker llm 取最近任务失败", "worker", userID, "err", terr)
	}
	if err := st.RecordAIUsage(ctx, store.AIUsage{
		UserID: userID, Kind: "worker_llm", Model: model,
		InputTokens: in, OutputTokens: outTok, GoalID: goalID,
	}); err != nil {
		slog.Warn("worker llm 用量落库失败", "worker", userID, "err", err)
	}
}

type claudeMessageReq struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    string            `json:"system,omitempty"`
	Messages  []claudeMsgParam  `json:"messages"`
	Tools     []claudeToolParam `json:"tools,omitempty"`
}

type claudeMsgParam struct {
	Role    string          `json:"role"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type claudeToolParam struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func openAIWorkerBodyToClaude(body map[string]any, model string, maxTokens int) (*claudeMessageReq, error) {
	msgVals, ok := body["messages"].([]any)
	if !ok || len(msgVals) == 0 {
		return nil, fmt.Errorf("worker llm 请求缺少 messages")
	}
	out := &claudeMessageReq{Model: model, MaxTokens: maxTokens}
	for _, mv := range msgVals {
		m, ok := mv.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages 里存在非对象元素")
		}
		role := mapString(m, "role")
		content := mapString(m, "content")
		switch role {
		case "system":
			if content != "" {
				if out.System != "" {
					out.System += "\n\n"
				}
				out.System += content
			}
		case "user":
			out.Messages = append(out.Messages, claudeMsgParam{Role: "user",
				Content: []claudeContent{{Type: "text", Text: content}}})
		case "assistant":
			blocks := make([]claudeContent, 0, 1)
			if content != "" {
				blocks = append(blocks, claudeContent{Type: "text", Text: content})
			}
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, cv := range calls {
					tc, err := openAIToolCallToClaude(cv)
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, tc)
				}
			}
			if len(blocks) == 0 {
				blocks = append(blocks, claudeContent{Type: "text", Text: ""})
			}
			out.Messages = append(out.Messages, claudeMsgParam{Role: "assistant", Content: blocks})
		case "tool":
			id := mapString(m, "tool_call_id")
			if id == "" {
				return nil, fmt.Errorf("tool 消息缺少 tool_call_id")
			}
			out.Messages = append(out.Messages, claudeMsgParam{Role: "user",
				Content: []claudeContent{{Type: "tool_result", ToolUseID: id, Content: content}}})
		default:
			return nil, fmt.Errorf("不支持的 worker llm role: %q", role)
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, tv := range tools {
			t, err := openAIToolToClaude(tv)
			if err != nil {
				return nil, err
			}
			out.Tools = append(out.Tools, t)
		}
	}
	return out, nil
}

func openAIToolCallToClaude(v any) (claudeContent, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return claudeContent{}, fmt.Errorf("tool_calls 里存在非对象元素")
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		return claudeContent{}, fmt.Errorf("tool_call 缺少 function")
	}
	args := mapString(fn, "arguments")
	if args == "" {
		args = "{}"
	}
	raw := json.RawMessage(args)
	if !json.Valid(raw) {
		raw = json.RawMessage(`{"_raw":` + strconvQuote(args) + `}`)
	}
	return claudeContent{
		Type:  "tool_use",
		ID:    mapString(m, "id"),
		Name:  mapString(fn, "name"),
		Input: raw,
	}, nil
}

func openAIToolToClaude(v any) (claudeToolParam, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return claudeToolParam{}, fmt.Errorf("tools 里存在非对象元素")
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		return claudeToolParam{}, fmt.Errorf("tool 缺少 function")
	}
	params := fn["parameters"]
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return claudeToolParam{}, fmt.Errorf("tool parameters 无法序列化")
	}
	return claudeToolParam{
		Name:        mapString(fn, "name"),
		Description: mapString(fn, "description"),
		InputSchema: raw,
	}, nil
}

func mapString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func claudeWorkerRespToOpenAI(raw []byte) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("解析 Anthropic 响应失败: %w", err)
	}
	var textParts []string
	var calls []map[string]any
	for _, b := range in.Content {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				textParts = append(textParts, b.Text)
			}
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": args,
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": strings.Join(textParts, "\n")}
	if len(calls) > 0 {
		msg["tool_calls"] = calls
	}
	out := map[string]any{
		"id":    in.ID,
		"model": in.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     in.Usage.InputTokens,
			"completion_tokens": in.Usage.OutputTokens,
			"input_tokens":      in.Usage.InputTokens,
			"output_tokens":     in.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (s *Server) runtimeLLMModel(ctx context.Context) string {
	if s.store == nil {
		return strings.TrimSpace(s.llm.Model)
	}
	model, err := s.store.GetKV(ctx, store.KVAIModel)
	if err != nil {
		slog.Warn("读取 worker LLM 运行时模型失败，使用配置默认模型", "key", store.KVAIModel, "err", err)
		return strings.TrimSpace(s.llm.Model)
	}
	if model = strings.TrimSpace(model); model != "" {
		return model
	}
	return strings.TrimSpace(s.llm.Model)
}

// handleWorkerNext claims a durable execution and injects server-owned context.
// The response also carries the legacy "task" key during one rolling upgrade;
// its id is the run id, so old workers still report against the correct lease.
func (s *Server) handleWorkerNext(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	ctx := r.Context()
	_ = s.store.WorkerHeartbeat(ctx, u.ID)

	run, err := s.store.ClaimNextWorkerRun(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent) // 无活可干
		return
	}
	if err != nil {
		slog.Error("worker 认领执行失败", "worker", u.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "认领失败"})
		return
	}
	delivered := false
	defer func() {
		if delivered {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if rerr := s.store.ReleaseWorkerRunClaim(releaseCtx, run.ID, u.ID, run.ClaimID); rerr != nil && !errors.Is(rerr, store.ErrNotFound) {
			slog.Error("worker 领取执行后交付失败，释放 claim 失败", "worker", u.ID, "run", run.ID, "claim", run.ClaimID, "err", rerr)
		}
	}()
	engine := strings.TrimSpace(r.URL.Query().Get("engine"))
	ws, err := s.store.ClaimWorkerSession(ctx, u.ID, engine, run.ScopeType, run.ScopeKey, run.ScopeTitle, run.ID, run.TaskID)
	if err != nil {
		slog.Error("worker 会话认领失败", "worker", u.ID, "run", run.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "会话认领失败"})
		return
	}
	var task *store.Task
	if run.TaskID != nil {
		task, err = s.store.TaskByID(ctx, *run.TaskID)
		if err != nil {
			slog.Error("worker 执行关联任务读取失败", "worker", u.ID, "run", run.ID, "task", *run.TaskID, "err", err)
			writeJSON(w, http.StatusConflict, map[string]string{"error": "关联任务已失效"})
			return
		}
	}
	// 进化：注入服务端托管的 worker/project 记忆。每个任务仍干净启动 PTY，
	// 长期经验由中枢检索后下发，避免本地记忆分裂、不可审计。
	var lessons []string
	query := run.Title
	if strings.TrimSpace(run.Description) != "" {
		query += " " + run.Description
	}
	if ps, err := s.store.ProfilesBy(ctx, u.ID, u.ID); err == nil {
		for i, p := range ps {
			if i >= workerKnowledgeHits {
				break
			}
			lessons = append(lessons, "我的工作画像："+p.Content)
		}
	}
	if u.OwnerID != nil {
		if ps, err := s.store.ProfilesBy(ctx, u.ID, *u.OwnerID); err == nil {
			for i, p := range ps {
				if i >= workerKnowledgeHits {
					break
				}
				lessons = append(lessons, "监护人对我的工作画像："+p.Content)
			}
		}
	}
	seenKnowledge := make(map[int64]bool)
	appendKnowledge := func(prefix string, items []*store.Knowledge) {
		for _, k := range items {
			if k == nil || k.Kind == store.KnowledgeKindPolicy || seenKnowledge[k.ID] {
				continue
			}
			seenKnowledge[k.ID] = true
			lessons = append(lessons, prefix+k.Title+"："+boundedWorkerText(k.Content, workerContextLineRunes))
		}
	}
	var personal []*store.Knowledge
	var personalErr error
	if s.deps.Knowledge != nil {
		personal, personalErr = s.deps.Knowledge.SearchByAuthor(ctx, u.ID, query, workerKnowledgeHits)
	} else {
		personal, personalErr = s.store.SearchKnowledgeByAuthor(ctx, u.ID, query, workerKnowledgeHits)
	}
	if personalErr == nil {
		appendKnowledge("我的历史经验：", personal)
	}
	if scopeTag := strings.TrimSpace(run.ScopeKey); scopeTag != "" {
		var scoped []*store.Knowledge
		var scopedErr error
		if s.deps.Knowledge != nil {
			scoped, scopedErr = s.deps.Knowledge.SearchByTag(ctx, scopeTag, query, workerKnowledgeHits)
		} else {
			scoped, scopedErr = s.store.SearchKnowledgeByTag(ctx, scopeTag, query, workerKnowledgeHits)
		}
		if scopedErr == nil {
			appendKnowledge("本主题历史经验：", scoped)
		}
	}
	if task != nil {
		projectTag := fmt.Sprintf("project:%d", task.ProjectID)
		var project []*store.Knowledge
		var projectErr error
		if s.deps.Knowledge != nil {
			project, projectErr = s.deps.Knowledge.SearchByTag(ctx, projectTag, query, workerKnowledgeHits)
		} else {
			project, projectErr = s.store.SearchKnowledgeByTag(ctx, projectTag, query, workerKnowledgeHits)
		}
		if projectErr == nil {
			appendKnowledge("本项目历史经验：", project)
		}
	}
	var ks []*store.Knowledge
	if s.deps.Knowledge != nil {
		ks, _ = s.deps.Knowledge.Search(ctx, query, workerKnowledgeHits)
	} else {
		ks, _ = s.store.SearchKnowledge(ctx, query, workerKnowledgeHits)
	}
	appendKnowledge("", ks)
	// 规则注入：常驻规则 + 与任务语义相关的动态规则中适用 worker 场景的，
	// 放在全部经验之前（规则优先于经验）。
	var rules []*store.Knowledge
	if pinned, err := s.store.PinnedRules(ctx); err == nil {
		rules = append(rules, pinned...)
	} else {
		slog.Warn("worker 常驻规则加载失败", "worker", u.ID, "err", err)
	}
	var dynRules []*store.Knowledge
	if s.deps.Knowledge != nil {
		dynRules, _ = s.deps.Knowledge.SearchRules(ctx, query, workerKnowledgeHits)
	} else {
		dynRules, _ = s.store.SearchRules(ctx, query, workerKnowledgeHits)
	}
	rules = append(rules, dynRules...)
	var ruleLines []string
	seenRules := make(map[int64]bool)
	for _, k := range rules {
		if k != nil && !seenRules[k.ID] && knowledge.RuleApplies(k.Tags, "worker", u.ID) {
			seenRules[k.ID] = true
			ruleLines = append(ruleLines, "公司规则（必须遵守）："+k.Title+"："+boundedWorkerText(k.Content, workerContextLineRunes))
		}
	}
	lessons = append(ruleLines, lessons...)
	// Skill 注入：只给摘要与 ID，避免把长流程塞满 worker prompt；worker 如需细节
	// 可通过对话/MCP 的 load_skill 读取完整方法。
	var skillLines []string
	var skills []*store.Knowledge
	if s.deps.Knowledge != nil {
		skills, _ = s.deps.Knowledge.SearchSkills(ctx, query, workerKnowledgeHits)
	} else {
		skills, _ = s.store.SearchSkills(ctx, query, workerKnowledgeHits)
	}
	for _, k := range skills {
		if knowledge.RuleApplies(k.Tags, "worker", u.ID) {
			skillLines = append(skillLines, "相关 skill 摘要："+k.Title+"："+workerSkillSummary(k.Content))
		}
	}
	lessons = append(skillLines, lessons...)
	lessons = boundedWorkerLines(lessons, workerContextLineRunes, workerContextTotalRunes)
	// Linked work receives business progress (including review feedback); direct
	// execution receives its own retry/progress history.
	var history []string
	if task != nil {
		if ps, err := s.store.ProgressOf(ctx, task.ID); err == nil {
			if len(ps) > workerHistoryEntries {
				ps = ps[len(ps)-workerHistoryEntries:]
			}
			for _, pr := range ps {
				history = append(history, pr.Content)
			}
		}
	} else if ps, err := s.store.WorkerRunProgress(ctx, run.ID); err == nil {
		if len(ps) > workerHistoryEntries {
			ps = ps[len(ps)-workerHistoryEntries:]
		}
		for _, pr := range ps {
			history = append(history, pr.Content)
		}
	}
	history = boundedWorkerLines(history, workerHistoryLineRunes, workerHistoryTotalRunes)
	var attachments []workerFileJSON
	seenFiles := map[int64]bool{}
	appendFiles := func(files []store.File, kind, caption string) {
		for _, file := range files {
			if seenFiles[file.ID] {
				continue
			}
			seenFiles[file.ID] = true
			q := url.Values{"run_id": {fmt.Sprint(run.ID)}, "claim_id": {run.ClaimID}}
			attachments = append(attachments, workerFileJSON{
				fileJSON: toFileJSON(file, "/api/worker/files/"+fmt.Sprint(file.ID)+"?"+q.Encode()),
				Kind:     kind, Caption: caption,
			})
		}
	}
	if files, err := s.store.WorkerRunFiles(ctx, run.ID, "input"); err == nil {
		appendFiles(files, "attachment", "执行输入")
	} else {
		slog.Warn("worker 执行输入查询失败", "worker", u.ID, "run", run.ID, "err", err)
	}
	if task != nil {
		if fs, err := s.store.TaskFileAttachments(ctx, task.ID); err == nil {
			appendFiles(fs, "attachment", "任务输入")
		} else {
			slog.Warn("worker 附件查询失败", "worker", u.ID, "run", run.ID, "task", task.ID, "err", err)
		}
		if arts, err := s.store.TaskArtifacts(ctx, task.ID); err == nil {
			for _, artifact := range arts {
				if seenFiles[artifact.File.ID] {
					continue
				}
				seenFiles[artifact.File.ID] = true
				q := url.Values{"run_id": {fmt.Sprint(run.ID)}, "claim_id": {run.ClaimID}}
				attachments = append(attachments, workerFileJSON{
					fileJSON: toFileJSON(artifact.File, "/api/worker/files/"+fmt.Sprint(artifact.File.ID)+"?"+q.Encode()),
					Kind:     "previous_artifact", Caption: artifact.Caption,
				})
			}
		} else {
			slog.Warn("worker 产物查询失败", "worker", u.ID, "run", run.ID, "task", task.ID, "err", err)
		}
	}
	if files, err := s.store.WorkerRunFiles(ctx, run.ID, "artifact"); err == nil {
		appendFiles(files, "previous_artifact", "本执行先前上传的产物")
	} else {
		slog.Warn("worker 执行产物查询失败", "worker", u.ID, "run", run.ID, "err", err)
	}
	command := ""
	commandPTY := false
	if run.Executor == workerproto.ExecutorCommand {
		var input store.WorkerCommandInput
		if err := json.Unmarshal(run.Input, &input); err != nil || strings.TrimSpace(input.Command) == "" {
			slog.Error("worker 命令执行输入损坏", "run", run.ID, "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "执行输入损坏"})
			return
		}
		command, commandPTY = input.Command, input.PTY
	}
	slog.Info("worker 领取执行", "worker", u.ID, "run", run.ID, "task", run.TaskID, "session", ws.ID, "scope", ws.ScopeKey, "knowledge_hits", len(lessons), "history", len(history), "attachments", len(attachments))
	payload := map[string]any{
		"id": run.ID, "task_id": run.TaskID, "executor": run.Executor,
		"title": run.Title, "goal": run.Goal, "description": run.Description,
		"acceptance": run.Acceptance, "command": command, "command_pty": commandPTY,
		"claim_id": run.ClaimID, "attachments": attachments,
		"session": workerSessionJSON{
			ID: ws.ID, Engine: ws.Engine, ScopeType: ws.ScopeType, ScopeKey: ws.ScopeKey,
			Title: ws.Title, Workdir: ws.Workdir, EngineSessionRef: ws.EngineSessionRef,
			Summary: ws.Summary,
		},
	}
	if err := writeJSON(w, http.StatusOK, map[string]any{
		"run": payload, "task": payload, "knowledge": lessons, "history": history,
	}); err != nil {
		slog.Warn("worker 执行响应写回失败，将释放 claim", "worker", u.ID, "run", run.ID, "claim", run.ClaimID, "err", err)
		return
	}
	delivered = true
}

func boundedWorkerText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func boundedWorkerLines(lines []string, perLine, total int) []string {
	if perLine <= 0 || total <= 0 {
		return nil
	}
	out := make([]string, 0, len(lines))
	remaining := total
	for _, line := range lines {
		if remaining <= 0 {
			break
		}
		line = boundedWorkerText(line, min(perLine, remaining))
		if line == "" {
			continue
		}
		out = append(out, line)
		remaining -= len([]rune(line))
	}
	return out
}

// handleWorkerProgress worker 回传执行进度（CLI 屏幕或命令输出的节流片段）。
func (s *Server) resolveWorkerRunID(ctx context.Context, workerID, runID, legacyTaskID int64, claimID string) (int64, error) {
	if runID <= 0 {
		runID = legacyTaskID
	}
	if runID <= 0 || strings.TrimSpace(claimID) == "" {
		return 0, store.ErrNotFound
	}
	return s.store.ResolveWorkerRunID(ctx, runID, workerID, strings.TrimSpace(claimID))
}

func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		RunID   int64  `json:"run_id"`
		TaskID  int64  `json:"task_id"`
		ClaimID string `json:"claim_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil || (req.RunID == 0 && req.TaskID == 0) || strings.TrimSpace(req.ClaimID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id 与 claim_id 必填"})
		return
	}
	runID, err := s.resolveWorkerRunID(r.Context(), u.ID, req.RunID, req.TaskID, req.ClaimID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
		return
	}
	if err := s.store.HeartbeatWorkerRun(r.Context(), runID, u.ID, req.ClaimID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "续租失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

func (s *Server) handleWorkerProgress(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		RunID   int64  `json:"run_id"`
		TaskID  int64  `json:"task_id"`
		ClaimID string `json:"claim_id"`
		Content string `json:"content"`
	}
	if err := decodeJSON(w, r, &req); err != nil || (req.RunID == 0 && req.TaskID == 0) || strings.TrimSpace(req.ClaimID) == "" || strings.TrimSpace(req.Content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id、claim_id 与 content 必填"})
		return
	}
	runID, err := s.resolveWorkerRunID(r.Context(), u.ID, req.RunID, req.TaskID, req.ClaimID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
		return
	}
	req.Content = truncateRunes(textfmt.RedactSecrets(req.Content), 16000)
	if err := s.store.AddWorkerRunProgress(r.Context(), runID, u.ID, req.ClaimID, req.Content); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许记录进度（可能已被改派或重置）"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "记录失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// handleWorkerSession persists native CLI continuity while the exact task claim
// is still active. This closes the power-loss window before submit/fail/input.
func (s *Server) handleWorkerSession(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		RunID            int64  `json:"run_id"`
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		SessionSummary   string `json:"session_summary"`
		EngineSessionRef string `json:"engine_session_ref"`
		Workdir          string `json:"workdir"`
	}
	if err := decodeJSON(w, r, &req); err != nil || (req.RunID == 0 && req.TaskID == 0) || req.WorkerSessionID == 0 ||
		strings.TrimSpace(req.ClaimID) == "" || strings.TrimSpace(req.EngineSessionRef) == "" || strings.TrimSpace(req.Workdir) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id、claim_id、worker_session_id、engine_session_ref 与 workdir 必填"})
		return
	}
	runID, err := s.resolveWorkerRunID(r.Context(), u.ID, req.RunID, req.TaskID, req.ClaimID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
		return
	}
	req.SessionSummary = truncateRunes(textfmt.RedactSecrets(req.SessionSummary), 1200)
	if err := s.store.UpdateWorkerSessionForClaim(r.Context(), req.WorkerSessionID, u.ID, runID,
		req.ClaimID, req.SessionSummary, req.EngineSessionRef, req.Workdir); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务 claim 或 worker 会话已失效"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存 worker 会话失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

func (s *Server) persistFinalizedWorkerSession(ctx context.Context, workerSessionID, workerID, runID int64, claimID, finalizationID, summary, engineRef, workdir string) bool {
	if workerSessionID <= 0 {
		return true
	}
	if err := s.store.UpdateWorkerSessionForFinalization(ctx, workerSessionID, workerID, runID,
		claimID, finalizationID, summary, engineRef, workdir); err != nil {
		slog.Warn("保存已最终化的 worker 会话失败", "worker", workerID, "run", runID, "session", workerSessionID, "err", err)
		return false
	}
	return true
}

func (s *Server) handleWorkerRequestInput(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		RunID            int64  `json:"run_id"`
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		FinalizationID   string `json:"finalization_id"`
		Content          string `json:"content"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		SessionSummary   string `json:"session_summary"`
		EngineSessionRef string `json:"engine_session_ref"`
		Workdir          string `json:"workdir"`
	}
	if err := decodeJSON(w, r, &req); err != nil || (req.RunID == 0 && req.TaskID == 0) || strings.TrimSpace(req.ClaimID) == "" || strings.TrimSpace(req.Content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id、claim_id 与 content 必填"})
		return
	}
	req.Content = truncateRunes(textfmt.RedactSecrets(req.Content), 4000)
	req.SessionSummary = truncateRunes(textfmt.RedactSecrets(req.SessionSummary), 1200)
	finalization := workerFinalization(req.FinalizationID, req.ClaimID, "request_input", struct {
		Content, SessionSummary, EngineSessionRef, Workdir string
		WorkerSessionID                                    int64
	}{req.Content, req.SessionSummary, req.EngineSessionRef, req.Workdir, req.WorkerSessionID})
	ctx := r.Context()
	candidate := req.RunID
	if candidate == 0 {
		candidate = req.TaskID
	}
	runID, err := s.store.ResolveWorkerRunIDForFinalization(ctx, candidate, u.ID, req.ClaimID, finalization.ID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
		return
	}
	run, task, replayed, err := s.store.RequestWorkerRunInput(ctx, runID, u.ID, req.ClaimID, req.Content, finalization)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许请求补充信息（可能已被改派或重置）"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "暂停任务失败"})
		return
	}
	sessionSaved := s.persistFinalizedWorkerSession(ctx, req.WorkerSessionID, u.ID, runID,
		req.ClaimID, finalization.ID, req.SessionSummary, req.EngineSessionRef, req.Workdir)
	if !replayed && s.bus != nil {
		if task != nil && task.AssignerID != u.ID {
			s.bus.EmitRequired("任务需要补充信息", task.AssignerID,
				fmt.Sprintf("AI 员工「%s」执行任务「%s」（任务内部编号 %d）时需要你补充信息：%s",
					u.Name, task.Title, task.ID, truncateRunes(req.Content, 500)))
		} else if task == nil && run.RequestedBy != u.ID {
			s.bus.EmitRequired("Worker 执行需要补充信息", run.RequestedBy,
				fmt.Sprintf("AI 员工「%s」执行「%s」（执行内部编号 %d）时需要你补充信息：%s",
					u.Name, run.Title, run.ID, truncateRunes(req.Content, 500)))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": "1", "replayed": replayed, "session_saved": sessionSaved})
}

// handleWorkerFail records an execution failure, releases the exact claim and
// applies durable retry/backoff. Repeated failures pause for the assigner rather
// than submitting an interrupted run as completed work.
func (s *Server) handleWorkerFail(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		RunID            int64  `json:"run_id"`
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		FinalizationID   string `json:"finalization_id"`
		Error            string `json:"error"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		SessionSummary   string `json:"session_summary"`
		EngineSessionRef string `json:"engine_session_ref"`
		Workdir          string `json:"workdir"`
	}
	if err := decodeJSON(w, r, &req); err != nil || (req.RunID == 0 && req.TaskID == 0) || strings.TrimSpace(req.ClaimID) == "" || strings.TrimSpace(req.Error) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id、claim_id 与 error 必填"})
		return
	}
	req.Error = truncateRunes(textfmt.RedactSecrets(req.Error), 4000)
	req.SessionSummary = truncateRunes(textfmt.RedactSecrets(req.SessionSummary), 1200)
	finalization := workerFinalization(req.FinalizationID, req.ClaimID, "fail", struct {
		Error, SessionSummary, EngineSessionRef, Workdir string
		WorkerSessionID                                  int64
	}{req.Error, req.SessionSummary, req.EngineSessionRef, req.Workdir, req.WorkerSessionID})
	ctx := r.Context()
	candidate := req.RunID
	if candidate == 0 {
		candidate = req.TaskID
	}
	runID, err := s.store.ResolveWorkerRunIDForFinalization(ctx, candidate, u.ID, req.ClaimID, finalization.ID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
		return
	}
	run, task, replayed, err := s.store.FailWorkerRun(ctx, runID, u.ID, req.ClaimID, req.Error, finalization)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前 claim 已失效"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "记录 worker 失败状态失败"})
		return
	}
	sessionSaved := s.persistFinalizedWorkerSession(ctx, req.WorkerSessionID, u.ID, runID,
		req.ClaimID, finalization.ID, req.SessionSummary, req.EngineSessionRef, req.Workdir)
	if !replayed && run.Status == store.WorkerRunAwaitingInput && s.bus != nil {
		if task != nil && task.AssignerID != u.ID {
			s.bus.EmitRequired("Worker 任务连续失败", task.AssignerID,
				fmt.Sprintf("AI 员工「%s」执行任务「%s」（任务内部编号 %d）连续失败，执行已暂停等待处理。最近错误：%s",
					u.Name, task.Title, task.ID, truncateRunes(run.LastError, 500)))
		} else if task == nil && run.RequestedBy != u.ID {
			s.bus.EmitRequired("Worker 执行连续失败", run.RequestedBy,
				fmt.Sprintf("AI 员工「%s」执行「%s」（执行内部编号 %d）连续失败，已暂停等待处理。最近错误：%s",
					u.Name, run.Title, run.ID, truncateRunes(run.LastError, 500)))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": "1", "status": run.Status, "retry_at": run.AvailableAt, "attempts": run.Attempts, "replayed": replayed, "session_saved": sessionSaved,
	})
}

// handleWorkerSubmit finalizes the exact execution lease. Linked work then
// advances its business task; direct runs finish without creating review noise.
func (s *Server) handleWorkerSubmit(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		RunID            int64  `json:"run_id"`
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		FinalizationID   string `json:"finalization_id"`
		Summary          string `json:"summary"`
		Lessons          string `json:"lessons"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		SessionSummary   string `json:"session_summary"`
		EngineSessionRef string `json:"engine_session_ref"`
		Workdir          string `json:"workdir"`
		Outcome          string `json:"outcome"`
		ExitCode         *int   `json:"exit_code"`
		// Deprecated rolling-upgrade input from workers older than 0063.
		LegacyCommandExitCode *int `json:"command_exit_code"`
	}
	if err := decodeJSON(w, r, &req); err != nil || (req.RunID == 0 && req.TaskID == 0) || strings.TrimSpace(req.ClaimID) == "" || strings.TrimSpace(req.Summary) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id、claim_id 与 summary 必填"})
		return
	}
	req.Summary = truncateRunes(textfmt.RedactSecrets(req.Summary), 64000)
	req.Lessons = truncateRunes(textfmt.RedactSecrets(req.Lessons), 20000)
	req.SessionSummary = truncateRunes(textfmt.RedactSecrets(req.SessionSummary), 1200)
	// Older workers did not send outcome. Their submit endpoint still meant that
	// execution finished; an optional legacy exit code refines success/failure.
	outcome, exitCode, validOutcome := resolveWorkerSubmissionOutcome(req.Outcome, req.ExitCode, req.LegacyCommandExitCode)
	if !validOutcome {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "outcome 必须为 succeeded 或 failed"})
		return
	}
	req.ExitCode = exitCode
	finalization := workerFinalization(req.FinalizationID, req.ClaimID, "complete", struct {
		Summary, Lessons, SessionSummary, EngineSessionRef, Workdir, Outcome string
		ExitCode                                                             *int
		WorkerSessionID                                                      int64
	}{req.Summary, req.Lessons, req.SessionSummary, req.EngineSessionRef, req.Workdir, string(outcome), req.ExitCode, req.WorkerSessionID})
	ctx := r.Context()
	candidate := req.RunID
	if candidate == 0 {
		candidate = req.TaskID
	}
	runID, err := s.store.ResolveWorkerRunIDForFinalization(ctx, candidate, u.ID, req.ClaimID, finalization.ID)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "执行 claim 已失效"})
		return
	}
	sessionSummary := strings.TrimSpace(req.SessionSummary)
	if sessionSummary == "" {
		sessionSummary = truncateRunes(req.Summary, 1200)
	}
	// Finalization is atomic against requirement updates/reassignment: those
	// operations cancel this run first, so a stale claim cannot advance the task.
	run, task, chain, replayed, err := s.store.CompleteWorkerRun(ctx, runID, u.ID, req.ClaimID, req.Summary, req.Lessons, outcome, req.ExitCode, finalization)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许提交（可能已被改派或重置）"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "提交失败"})
		return
	}
	sessionSaved := s.persistFinalizedWorkerSession(ctx, req.WorkerSessionID, u.ID, runID,
		req.ClaimID, finalization.ID, sessionSummary, req.EngineSessionRef, req.Workdir)
	if !replayed && task != nil && task.Status == store.TaskAccepted {
		tools.FireReadyDependents(ctx, s.deps, task.ID)
		for _, c := range chain {
			tools.FireReadyDependents(ctx, s.deps, c.ID)
		}
	}
	// 进化：可复用经验回流知识库，供后续同类任务检索。
	if lessons := strings.TrimSpace(req.Lessons); !replayed && run.Executor == workerproto.ExecutorAgent && lessons != "" {
		tags := []string{"worker经验", fmt.Sprintf("worker:%d", u.ID)}
		if task != nil {
			tags = append(tags, fmt.Sprintf("project:%d", task.ProjectID))
		}
		if scope := strings.TrimSpace(run.ScopeKey); scope != "" {
			tags = append(tags, scope)
		}
		if _, err := s.store.CreateKnowledge(ctx, run.Title, lessons, tags, u.ID); err != nil {
			slog.Warn("worker 经验入库失败", "run", run.ID, "task", run.TaskID, "err", err)
		}
	}
	learned := 0
	if !replayed && task != nil {
		learned = s.ingestWorkerLearningCandidates(ctx, u, task, req.Summary)
	}
	if !replayed && s.bus != nil && task != nil && task.AssignerID != u.ID {
		extra := ""
		if learned > 0 {
			extra = fmt.Sprintf(" 已抽取 %d 条学习候选，可用 list_learning_candidates 审核。", learned)
		}
		if task.Status == store.TaskDone {
			s.bus.EmitRequired("任务提交待验收", task.AssignerID,
				fmt.Sprintf("AI 员工「%s」提交了任务「%s」（任务内部编号 %d）待你验收。提交摘要：%s%s",
					u.Name, task.Title, task.ID, truncateRunes(req.Summary, 400), extra))
		} else {
			s.bus.EmitRequired("任务执行结束", task.AssignerID,
				fmt.Sprintf("AI 员工「%s」已完成任务「%s」（任务内部编号 %d），任务已归入历史。执行摘要：%s%s",
					u.Name, task.Title, task.ID, truncateRunes(req.Summary, 400), extra))
		}
	} else if !replayed && s.bus != nil && task == nil && run.RequestedBy != u.ID {
		s.bus.EmitRequired("Worker 执行结束", run.RequestedBy,
			fmt.Sprintf("AI 员工「%s」完成了「%s」（执行内部编号 %d，结果 %s）。摘要：%s",
				u.Name, run.Title, run.ID, outcome, truncateRunes(req.Summary, 500)))
	}
	taskStatus := ""
	if task != nil {
		taskStatus = task.Status
	}
	slog.Info("worker 完成执行", "worker", u.ID, "run", run.ID, "task", run.TaskID, "outcome", outcome, "run_status", run.Status, "task_status", taskStatus, "exit_code", req.ExitCode, "lessons", req.Lessons != "", "replayed", replayed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": "1", "outcome": outcome, "run_status": run.Status, "task_status": taskStatus, "replayed": replayed, "session_saved": sessionSaved})
}

func resolveWorkerSubmissionOutcome(value string, exitCode, legacyExitCode *int) (workerproto.Outcome, *int, bool) {
	if exitCode == nil {
		exitCode = legacyExitCode
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = string(workerproto.OutcomeSucceeded)
		if exitCode != nil && *exitCode != 0 {
			value = string(workerproto.OutcomeFailed)
		}
	}
	outcome, ok := workerproto.ParseOutcome(value)
	return outcome, exitCode, ok
}

func workerSkillSummary(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if v, ok := strings.CutPrefix(line, "摘要："); ok {
			return strings.TrimSpace(v)
		}
	}
	return truncateRunes(content, 240)
}

// materialLearningMarker 引用 tools 包的单一来源，防双方漂移导致学习候选静默丢失。
const materialLearningMarker = tools.MaterialLearningMarker

type workerLearningPayload struct {
	Knowledge []struct {
		Title      string          `json:"title"`
		Content    string          `json:"content"`
		Tags       []string        `json:"tags"`
		Confidence float32         `json:"confidence"`
		Evidence   json.RawMessage `json:"evidence"`
	} `json:"knowledge"`
	Entities []struct {
		EntityType string          `json:"entity_type"`
		Name       string          `json:"name"`
		Content    string          `json:"content"`
		FileID     int64           `json:"file_id"`
		Confidence float32         `json:"confidence"`
		Evidence   json.RawMessage `json:"evidence"`
	} `json:"entities"`
	Rules []struct {
		Title      string          `json:"title"`
		Content    string          `json:"content"`
		Scope      string          `json:"scope"`
		Tags       []string        `json:"tags"`
		Confidence float32         `json:"confidence"`
		Evidence   json.RawMessage `json:"evidence"`
	} `json:"rules"`
	Skills []struct {
		Title       string          `json:"title"`
		Trigger     string          `json:"trigger"`
		Summary     string          `json:"summary"`
		Procedure   string          `json:"procedure"`
		Constraints string          `json:"constraints"`
		Scope       string          `json:"scope"`
		Tags        []string        `json:"tags"`
		Confidence  float32         `json:"confidence"`
		Evidence    json.RawMessage `json:"evidence"`
	} `json:"skills"`
	Questions []struct {
		Title    string          `json:"title"`
		Content  string          `json:"content"`
		Evidence json.RawMessage `json:"evidence"`
	} `json:"questions"`
}

func (s *Server) ingestWorkerLearningCandidates(ctx context.Context, u *store.User, t *store.Task, summary string) int {
	raw := afterLast(summary, materialLearningMarker)
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndex(raw, "}"); i >= 0 {
		raw = raw[:i+1]
	}
	var payload workerLearningPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		slog.Warn("worker 学习候选 JSON 解析失败", "worker", u.ID, "task", t.ID, "err", err)
		return 0
	}
	createdBy := u.ID
	sourceRef := fmt.Sprint(t.ID)
	count := 0
	save := func(in store.LearningCandidateInput) *store.LearningCandidate {
		in.SourceType = "worker_task"
		in.SourceRef = sourceRef
		in.CreatedBy = &createdBy
		in.Tags = append(in.Tags, fmt.Sprintf("worker:%d", u.ID), fmt.Sprintf("project:%d", t.ProjectID))
		c, err := s.store.CreateLearningCandidate(ctx, in)
		if err != nil {
			slog.Warn("保存 worker 学习候选失败", "worker", u.ID, "task", t.ID, "kind", in.Kind, "err", err)
			return nil
		}
		count++
		return c
	}
	for _, k := range payload.Knowledge {
		if strings.TrimSpace(k.Title) == "" || strings.TrimSpace(k.Content) == "" {
			continue
		}
		save(store.LearningCandidateInput{
			Kind: store.LearningKindKnowledge, Scope: "global", Title: k.Title, Content: k.Content,
			Tags: k.Tags, Evidence: k.Evidence, Confidence: k.Confidence,
		})
	}
	for _, e := range payload.Entities {
		if strings.TrimSpace(e.EntityType) == "" || strings.TrimSpace(e.Name) == "" {
			continue
		}
		var fileID *int64
		if e.FileID > 0 {
			id := e.FileID
			fileID = &id
		}
		if _, err := s.store.CreateMaterialEntity(ctx, store.MaterialEntity{
			FileID: fileID, EntityType: strings.TrimSpace(e.EntityType), Name: strings.TrimSpace(e.Name),
			Content: strings.TrimSpace(e.Content), Evidence: e.Evidence, Confidence: e.Confidence,
			CreatedBy: &createdBy,
		}); err != nil {
			slog.Warn("保存材料实体失败", "worker", u.ID, "task", t.ID, "entity", e.Name, "err", err)
		} else {
			count++
		}
	}
	for _, r := range payload.Rules {
		if strings.TrimSpace(r.Title) == "" || strings.TrimSpace(r.Content) == "" {
			continue
		}
		scope := strings.TrimSpace(r.Scope)
		if scope == "" {
			scope = "global"
		}
		save(store.LearningCandidateInput{
			Kind: store.LearningKindRule, Scope: scope, Title: r.Title, Content: r.Content,
			Tags: r.Tags, Evidence: r.Evidence, Confidence: r.Confidence,
		})
	}
	for _, sk := range payload.Skills {
		if strings.TrimSpace(sk.Title) == "" || strings.TrimSpace(sk.Trigger) == "" ||
			strings.TrimSpace(sk.Summary) == "" || strings.TrimSpace(sk.Procedure) == "" {
			continue
		}
		scope := strings.TrimSpace(sk.Scope)
		if scope == "" {
			scope = "global"
		}
		save(store.LearningCandidateInput{
			Kind: store.LearningKindSkill, Scope: scope, Title: sk.Title,
			Content: buildWorkerSkillContent(sk.Trigger, sk.Summary, sk.Procedure, sk.Constraints),
			Tags:    sk.Tags, Evidence: sk.Evidence, Confidence: sk.Confidence,
		})
	}
	for _, q := range payload.Questions {
		if strings.TrimSpace(q.Title) == "" || strings.TrimSpace(q.Content) == "" {
			continue
		}
		save(store.LearningCandidateInput{
			Kind: store.LearningKindSummary, Scope: "global", Title: "待裁决：" + q.Title, Content: q.Content,
			Tags: []string{"待裁决"}, Evidence: q.Evidence,
		})
	}
	if count > 0 {
		slog.Info("worker 学习候选已入库", "worker", u.ID, "task", t.ID, "count", count)
	}
	return count
}

func afterLast(s, marker string) string {
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return ""
	}
	return s[i+len(marker):]
}

func buildWorkerSkillContent(trigger, summary, procedure, constraints string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "触发条件：%s\n", strings.TrimSpace(trigger))
	fmt.Fprintf(&b, "摘要：%s\n", strings.TrimSpace(summary))
	fmt.Fprintf(&b, "执行方法：\n%s\n", strings.TrimSpace(procedure))
	if c := strings.TrimSpace(constraints); c != "" {
		fmt.Fprintf(&b, "限制与禁忌：\n%s\n", c)
	}
	return strings.TrimSpace(b.String())
}

// mcpServer 对外 MCP：按 token 换用户，暴露其权限内的工具集。
func (s *Server) mcpServer(r *http.Request) *mcp.Server {
	u := s.authenticate(r)
	if u == nil {
		return nil
	}
	return mcpbridge.NewServer("nbco", "1", tools.StripApprovalRequired(tools.ForUserContext(r.Context(), s.deps, u, nil)))
}

// Serve 启动 HTTP/HTTPS 服务并随 ctx 关停。certFile/keyFile 为空时走明文 HTTP。
func (s *Server) Serve(ctx context.Context, addr, certFile, keyFile string) error {
	if strings.TrimSpace(s.fileStorePath) != "" {
		go s.runFileGC(ctx)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 ReadTimeout：它覆盖「读完整个请求体」，与 200MB 上传/产物回传
		// （可长达分钟级）直接冲突；慢速头攻击由 ReadHeaderTimeout 挡。
		// 同样不设全局 WriteTimeout：Go 从读完请求头就开始计时，30 分钟文件
		// 传输或长 AI 轮次会在真正写响应前耗尽它。各长操作由自身 context 限时。
		IdleTimeout: 2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() {
		if certFile != "" && keyFile != "" {
			errCh <- srv.ListenAndServeTLS(certFile, keyFile)
			return
		}
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Package httpapi 是 HTTP 入口：内嵌 Web 页 + REST 对话/任务接口 + 对外 MCP 端点 + CLI 回连端点。
// 认证只走 Authorization: Bearer <api token>（不支持查询参数，避免 token 进访问日志）。
package httpapi

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
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
)

// Channel HTTP 渠道标识（Web 页与 REST 共用同一会话）。
const Channel = "api"

const maxJSONBodyBytes = 1 << 20

var Version = "dev"

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
}

// New 创建 HTTP 入口。
func New(s *store.Store, orch *chat.Orchestrator, deps tools.Deps, bus *events.Bus, llm LLMConfig, fileStorePath, downloadPath string) *Server {
	return &Server{store: s, orch: orch, deps: deps, bus: bus, llm: llm,
		llmSem: make(chan struct{}, llmProxyConcurrency), fileStorePath: fileStorePath, downloadPath: downloadPath}
}

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(mustWebAssetFS()))))
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
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /downloads/worker/{name}", s.handleWorkerDownloadBinary)
	mux.HandleFunc("POST /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/me/tasks", s.handleMyTasks)
	mux.HandleFunc("GET /api/me/review", s.handleReview)
	mux.HandleFunc("GET /api/me/assigned", s.handleAssigned)
	mux.HandleFunc("GET /api/schedules", s.handleSchedules)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/admin/workers", s.handleAdminWorkers)
	mux.HandleFunc("GET /api/admin/task-queue", s.handleAdminTaskQueue)
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
	mux.HandleFunc("POST /api/worker/progress", s.handleWorkerProgress)
	mux.HandleFunc("POST /api/worker/request-input", s.handleWorkerRequestInput)
	mux.HandleFunc("POST /api/worker/submit", s.handleWorkerSubmit)
	mux.HandleFunc("GET /api/worker/files/{id}", s.handleWorkerDownloadFile)
	mux.HandleFunc("POST /api/worker/artifacts", s.handleWorkerArtifact)
	mux.HandleFunc("GET /api/worker/ws", s.handleWorkerWS)
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(s.mcpServer, nil))
	return mux
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
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") {
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
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasksJSON(ts, names)})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    Version,
		"go":         runtime.Version(),
		"migrations": migrations,
		"workers": map[string]any{
			"hub_configured": s.deps.Workers != nil,
		},
		"engine": s.engineHealth(),
	})
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
	writeJSON(w, http.StatusOK, map[string]any{"result": out})
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
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasksJSON(ts, names)})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"stats": map[string]int64{
			"open": stats.Open, "overdue": stats.Overdue,
			"awaiting": stats.Awaiting, "done_week": stats.DoneSince,
		},
		"projects": pjs,
		"overdue":  tasksJSON(overdue, names),
	})
}

// taskJSON 任务的对外表示（带人名，前端免二次查询）。
type taskJSON struct {
	ID           int64      `json:"id"`
	ProjectID    int64      `json:"project_id"`
	Title        string     `json:"title"`
	Status       string     `json:"status"`
	Priority     string     `json:"priority"`
	Deadline     *time.Time `json:"deadline,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	AssignerID   int64      `json:"assigner_id"`
	AssignerName string     `json:"assigner_name"`
	AssigneeID   int64      `json:"assignee_id"`
	AssigneeName string     `json:"assignee_name"`
	NudgeCount   int64      `json:"nudge_count,omitempty"`
	WorkerClaim  string     `json:"worker_claim_id,omitempty"`
}

func tasksJSON(ts []*store.Task, names map[int64]string) []taskJSON {
	out := make([]taskJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskJSON{
			ID: t.ID, ProjectID: t.ProjectID, Title: t.Title, Status: t.Status,
			Priority: t.Priority, Deadline: t.Deadline, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
			AssignerID: t.AssignerID, AssignerName: names[t.AssignerID],
			AssigneeID: t.AssigneeID, AssigneeName: names[t.AssigneeID],
			NudgeCount: t.NudgeCount, WorkerClaim: t.WorkerClaimID,
		})
	}
	return out
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
			Mode: sc.Mode, Kind: sc.Kind, Message: sc.Message, FireAt: sc.FireAt,
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
	workerKnowledgeHits  = 4
	workerHistoryEntries = 10 // 领取任务时随带的最近过程记录条数（返工时含打回理由）
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
		outCopy := out
		go recordWorkerLLMUsage(context.Background(), s.store, u.ID, model, outCopy)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(out)
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

// handleWorkerNext 认领下一个待办任务；顺带注入相关历史经验（越干越准）。
func (s *Server) handleWorkerNext(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	ctx := r.Context()
	_ = s.store.WorkerHeartbeat(ctx, u.ID)

	t, err := s.store.ClaimNextTask(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent) // 无活可干
		return
	}
	if err != nil {
		slog.Error("worker 认领任务失败", "worker", u.ID, "err", err)
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
		if rerr := s.store.ReleaseWorkerTaskClaim(releaseCtx, t.ID, u.ID, t.WorkerClaimID); rerr != nil && !errors.Is(rerr, store.ErrNotFound) {
			slog.Error("worker 领取任务后交付失败，释放 claim 失败", "worker", u.ID, "task", t.ID, "claim", t.WorkerClaimID, "err", rerr)
		}
	}()
	engine := strings.TrimSpace(r.URL.Query().Get("engine"))
	scopeType, scopeKey, scopeTitle := s.workerSessionScope(ctx, t)
	ws, err := s.store.ClaimWorkerSession(ctx, u.ID, engine, scopeType, scopeKey, scopeTitle, t.ID)
	if err != nil {
		slog.Error("worker 会话认领失败", "worker", u.ID, "task", t.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "会话认领失败"})
		return
	}
	// 进化：注入服务端托管的 worker/project 记忆。每个任务仍干净启动 PTY，
	// 长期经验由中枢检索后下发，避免本地记忆分裂、不可审计。
	var lessons []string
	query := t.Title
	if strings.TrimSpace(t.Description) != "" {
		query += " " + t.Description
	}
	if ps, err := s.store.ProfilesBy(ctx, u.ID, u.ID); err == nil {
		for _, p := range ps {
			lessons = append(lessons, "我的工作画像："+p.Content)
		}
	}
	if u.OwnerID != nil {
		if ps, err := s.store.ProfilesBy(ctx, u.ID, *u.OwnerID); err == nil {
			for _, p := range ps {
				lessons = append(lessons, "监护人对我的工作画像："+p.Content)
			}
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
		for _, k := range personal {
			if k.Kind == store.KnowledgeKindPolicy {
				continue // 规则走下方专门通道，不混进经验
			}
			lessons = append(lessons, "我的历史经验："+k.Title+"："+k.Content)
		}
	}
	projectTag := fmt.Sprintf("project:%d", t.ProjectID)
	var project []*store.Knowledge
	var projectErr error
	if s.deps.Knowledge != nil {
		project, projectErr = s.deps.Knowledge.SearchByTag(ctx, projectTag, query, workerKnowledgeHits)
	} else {
		project, projectErr = s.store.SearchKnowledgeByTag(ctx, projectTag, query, workerKnowledgeHits)
	}
	if projectErr == nil {
		for _, k := range project {
			if k.Kind == store.KnowledgeKindPolicy {
				continue
			}
			lessons = append(lessons, "本项目历史经验："+k.Title+"："+k.Content)
		}
	}
	var ks []*store.Knowledge
	if s.deps.Knowledge != nil {
		ks, _ = s.deps.Knowledge.Search(ctx, query, workerKnowledgeHits)
	} else {
		ks, _ = s.store.SearchKnowledge(ctx, query, workerKnowledgeHits)
	}
	for _, k := range ks {
		if k.Kind == store.KnowledgeKindPolicy {
			continue
		}
		lessons = append(lessons, k.Title+"："+k.Content)
	}
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
	for _, k := range rules {
		if knowledge.RuleApplies(k.Tags, "worker", u.ID) {
			ruleLines = append(ruleLines, "公司规则（必须遵守）："+k.Title+"："+k.Content)
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
	// 返工闭环：带上任务已有的过程记录（含验收打回理由），worker 按它改。
	var history []string
	if ps, err := s.store.ProgressOf(ctx, t.ID); err == nil {
		if len(ps) > workerHistoryEntries {
			ps = ps[len(ps)-workerHistoryEntries:]
		}
		for _, pr := range ps {
			history = append(history, pr.Content)
		}
	}
	var attachments []workerFileJSON
	if fs, err := s.store.TaskFileAttachments(ctx, t.ID); err == nil {
		for _, f := range fs {
			q := url.Values{"task_id": {fmt.Sprint(t.ID)}, "claim_id": {t.WorkerClaimID}}
			attachments = append(attachments, workerFileJSON{
				fileJSON: toFileJSON(f, "/api/worker/files/"+fmt.Sprint(f.ID)+"?"+q.Encode()),
				Kind:     "attachment",
			})
		}
	} else {
		slog.Warn("worker 附件查询失败", "worker", u.ID, "task", t.ID, "err", err)
	}
	if arts, err := s.store.TaskArtifacts(ctx, t.ID); err == nil {
		for _, a := range arts {
			q := url.Values{"task_id": {fmt.Sprint(t.ID)}, "claim_id": {t.WorkerClaimID}}
			attachments = append(attachments, workerFileJSON{
				fileJSON: toFileJSON(a.File, "/api/worker/files/"+fmt.Sprint(a.File.ID)+"?"+q.Encode()),
				Kind:     "previous_artifact",
				Caption:  a.Caption,
			})
		}
	} else {
		slog.Warn("worker 产物查询失败", "worker", u.ID, "task", t.ID, "err", err)
	}
	slog.Info("worker 领取任务", "worker", u.ID, "task", t.ID, "session", ws.ID, "scope", ws.ScopeKey, "knowledge_hits", len(lessons), "history", len(history), "attachments", len(attachments))
	if err := writeJSON(w, http.StatusOK, map[string]any{
		"task": map[string]any{
			"id": t.ID, "title": t.Title, "goal": t.Goal,
			"description": t.Description, "acceptance": t.Acceptance, "command": t.WorkerCommand, "command_pty": t.WorkerCommandPTY, "claim_id": t.WorkerClaimID,
			"attachments": attachments,
			"session": workerSessionJSON{
				ID: ws.ID, Engine: ws.Engine, ScopeType: ws.ScopeType, ScopeKey: ws.ScopeKey,
				Title: ws.Title, Workdir: ws.Workdir, EngineSessionRef: ws.EngineSessionRef,
				Summary: ws.Summary,
			},
		},
		"knowledge": lessons,
		"history":   history,
	}); err != nil {
		slog.Warn("worker 任务响应写回失败，将释放 claim", "worker", u.ID, "task", t.ID, "claim", t.WorkerClaimID, "err", err)
		return
	}
	delivered = true
}

func (s *Server) workerSessionScope(ctx context.Context, t *store.Task) (scopeType, scopeKey, title string) {
	projectName := ""
	if pj, err := s.store.ProjectByID(ctx, t.ProjectID); err == nil {
		projectName = pj.Name
	}
	text := strings.ToLower(strings.Join([]string{projectName, t.Title, t.Goal, t.Description, t.WorkerCommand}, "\n"))
	if looksLikeNBCOCodeTask(text) {
		return "repo", "repo:nbco", "NBCO codebase / deployment"
	}
	switch strings.ToLower(strings.TrimSpace(projectName)) {
	case "company intelligence inbox":
		return "materials", "materials:company-intelligence", "Company material analysis"
	case "worker commands":
		return "ops", "ops:worker-commands", "Worker command tasks"
	}
	if t.ProjectID > 0 {
		name := strings.TrimSpace(projectName)
		if name == "" {
			name = fmt.Sprintf("Project %d", t.ProjectID)
		}
		return "project", fmt.Sprintf("project:%d", t.ProjectID), name
	}
	return "task", fmt.Sprintf("task:%d", t.ID), "One-off task"
}

func looksLikeNBCOCodeTask(text string) bool {
	if strings.Contains(text, "repo:nbco") || strings.Contains(text, "repository:nbco") {
		return true
	}
	if !strings.Contains(text, "nbco") {
		return false
	}
	for _, kw := range []string{
		"代码", "功能", "修复", "bug", "部署", "升级", "commit", "push", "测试", "go test",
		"codex", "claude", "repo", "repository", "代码库", "仓库",
	} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// handleWorkerProgress worker 回传执行进度（CLI 屏幕或命令输出的节流片段）。
func (s *Server) handleWorkerProgress(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		TaskID  int64  `json:"task_id"`
		ClaimID string `json:"claim_id"`
		Content string `json:"content"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id 与 content 必填"})
		return
	}
	if err := s.store.AddWorkerProgress(r.Context(), req.TaskID, u.ID, req.ClaimID, req.Content); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许记录进度（可能已被改派或重置）"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "记录失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

func (s *Server) handleWorkerRequestInput(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		TaskID  int64  `json:"task_id"`
		ClaimID string `json:"claim_id"`
		Content string `json:"content"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.TaskID == 0 || strings.TrimSpace(req.Content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id 与 content 必填"})
		return
	}
	ctx := r.Context()
	content := "❓ 需要补充信息：" + strings.TrimSpace(req.Content)
	if err := s.store.AddWorkerProgress(ctx, req.TaskID, u.ID, req.ClaimID, content); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许请求补充信息（可能已被改派或重置）"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "记录失败"})
		return
	}
	t, err := s.store.TaskByID(ctx, req.TaskID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询任务失败"})
		return
	}
	if t.AssignerID != u.ID && s.bus != nil {
		s.bus.Emit("任务需要补充信息", t.AssignerID,
			fmt.Sprintf("AI 员工「%s」执行任务「%s」（任务内部编号 %d）时需要你补充信息：%s",
				u.Name, t.Title, t.ID, truncateRunes(req.Content, 500)))
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// handleWorkerSubmit worker 提交完成：进入验收流；可复用经验回流知识库（进化闭环）。
func (s *Server) handleWorkerSubmit(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		TaskID           int64  `json:"task_id"`
		ClaimID          string `json:"claim_id"`
		Summary          string `json:"summary"`
		Lessons          string `json:"lessons"`
		WorkerSessionID  int64  `json:"worker_session_id"`
		SessionSummary   string `json:"session_summary"`
		EngineSessionRef string `json:"engine_session_ref"`
		Workdir          string `json:"workdir"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.TaskID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id 必填"})
		return
	}
	ctx := r.Context()
	// 原子提交：要求任务仍是本 worker 手上的 in_progress。若此刻分配者刚把它
	// 改需求重置为 pending，提交落空（ErrNotFound），旧交付不会被当成完成。
	t, chain, err := s.store.SubmitWorkerTask(ctx, req.TaskID, u.ID, req.ClaimID, req.Summary)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许提交（可能已被改派或重置）"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "提交失败"})
		return
	}
	// 依赖编排：自派任务提交即 accepted（含级联），可能让下游任务全部前置就绪。
	if t.Status == store.TaskAccepted {
		tools.FireReadyDependents(ctx, s.deps, t.ID)
		for _, c := range chain {
			tools.FireReadyDependents(ctx, s.deps, c.ID)
		}
	}
	// 进化：可复用经验回流知识库，供后续同类任务检索。
	if lessons := strings.TrimSpace(req.Lessons); lessons != "" {
		tags := []string{"worker经验", fmt.Sprintf("worker:%d", u.ID), fmt.Sprintf("project:%d", t.ProjectID)}
		if _, err := s.store.CreateKnowledge(ctx, t.Title, lessons, tags, u.ID); err != nil {
			slog.Warn("worker 经验入库失败", "task", t.ID, "err", err)
		}
	}
	if req.WorkerSessionID > 0 {
		sessionSummary := strings.TrimSpace(req.SessionSummary)
		if sessionSummary == "" {
			sessionSummary = truncateRunes(req.Summary, 1200)
		}
		if err := s.store.UpdateWorkerSession(ctx, req.WorkerSessionID, u.ID, t.ID, sessionSummary, req.EngineSessionRef, req.Workdir); err != nil {
			slog.Warn("worker 会话更新失败", "worker", u.ID, "task", t.ID, "session", req.WorkerSessionID, "err", err)
		}
	}
	learned := s.ingestWorkerLearningCandidates(ctx, u, t, req.Summary)
	// 提交事件交派活人的 AI 分析：AI 可先看交付摘要，通知里直接给验收建议。
	if t.AssignerID != u.ID {
		extra := ""
		if learned > 0 {
			extra = fmt.Sprintf(" 已抽取 %d 条学习候选，可用 list_learning_candidates 审核。", learned)
		}
		s.bus.Emit("任务提交待验收", t.AssignerID,
			fmt.Sprintf("AI 员工「%s」提交了任务「%s」（任务内部编号 %d）待你验收。提交摘要：%s%s",
				u.Name, t.Title, t.ID, truncateRunes(req.Summary, 400), extra))
	}
	slog.Info("worker 提交任务", "worker", u.ID, "task", t.ID, "lessons", req.Lessons != "")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
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
	return mcpbridge.NewServer("nbco", "1", tools.StripApprovalRequired(tools.ForUser(s.deps, u, nil)))
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
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  2 * time.Minute,
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

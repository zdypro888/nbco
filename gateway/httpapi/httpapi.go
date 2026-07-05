// Package httpapi 是 HTTP 入口：内嵌 Web 页 + REST 对话/任务接口 + 对外 MCP 端点 + CLI 回连端点。
// 认证只走 Authorization: Bearer <api token>（不支持查询参数，避免 token 进访问日志）。
package httpapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/mcpbridge"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

// Channel HTTP 渠道标识（Web 页与 REST 共用同一会话）。
const Channel = "api"

const maxJSONBodyBytes = 1 << 20

//go:embed web/index.html
var indexHTML []byte

// Server HTTP 入口。
type Server struct {
	store         *store.Store
	orch          *chat.Orchestrator
	deps          tools.Deps
	fileStorePath string
	downloadPath  string
}

// New 创建 HTTP 入口。
func New(s *store.Store, orch *chat.Orchestrator, deps tools.Deps, fileStorePath, downloadPath string) *Server {
	return &Server{store: s, orch: orch, deps: deps, fileStorePath: fileStorePath, downloadPath: downloadPath}
}

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /downloads/worker/{name}", s.handleWorkerDownloadBinary)
	mux.HandleFunc("POST /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/me/tasks", s.handleMyTasks)
	mux.HandleFunc("GET /api/me/review", s.handleReview)
	mux.HandleFunc("GET /api/me/assigned", s.handleAssigned)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("POST /api/files", s.handleUploadFile)
	mux.HandleFunc("GET /api/files/{id}", s.handleDownloadFile)
	mux.HandleFunc("POST /api/tasks/{id}/attachments", s.handleAttachFile)
	// AI 员工（worker client）接口。任务队列走 HTTP（DB 为准），WS 做实时增强。
	mux.HandleFunc("GET /api/worker/next", s.handleWorkerNext)
	mux.HandleFunc("POST /api/worker/progress", s.handleWorkerProgress)
	mux.HandleFunc("POST /api/worker/submit", s.handleWorkerSubmit)
	mux.HandleFunc("GET /api/worker/files/{id}", s.handleWorkerDownloadFile)
	mux.HandleFunc("POST /api/worker/artifacts", s.handleWorkerArtifact)
	mux.HandleFunc("GET /api/worker/ws", s.handleWorkerWS)
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(s.mcpServer, nil))
	return mux
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

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
	AssignerID   int64      `json:"assigner_id"`
	AssignerName string     `json:"assigner_name"`
	AssigneeID   int64      `json:"assignee_id"`
	AssigneeName string     `json:"assignee_name"`
	NudgeCount   int64      `json:"nudge_count,omitempty"`
}

func tasksJSON(ts []*store.Task, names map[int64]string) []taskJSON {
	out := make([]taskJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskJSON{
			ID: t.ID, ProjectID: t.ProjectID, Title: t.Title, Status: t.Status,
			Priority: t.Priority, Deadline: t.Deadline,
			AssignerID: t.AssignerID, AssignerName: names[t.AssignerID],
			AssigneeID: t.AssigneeID, AssigneeName: names[t.AssigneeID],
			NudgeCount: t.NudgeCount,
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
	if ks, err := s.store.SearchKnowledgeByAuthor(ctx, u.ID, query, workerKnowledgeHits); err == nil {
		for _, k := range ks {
			lessons = append(lessons, "我的历史经验："+k.Title+"："+k.Content)
		}
	}
	if ks, err := s.store.SearchKnowledgeByTag(ctx, fmt.Sprintf("project:%d", t.ProjectID), query, workerKnowledgeHits); err == nil {
		for _, k := range ks {
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
		lessons = append(lessons, k.Title+"："+k.Content)
	}
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
	slog.Info("worker 领取任务", "worker", u.ID, "task", t.ID, "knowledge_hits", len(lessons), "history", len(history), "attachments", len(attachments))
	writeJSON(w, http.StatusOK, map[string]any{
		"task": map[string]any{
			"id": t.ID, "title": t.Title, "goal": t.Goal,
			"description": t.Description, "acceptance": t.Acceptance, "command": t.WorkerCommand, "command_pty": t.WorkerCommandPTY, "claim_id": t.WorkerClaimID,
			"attachments": attachments,
		},
		"knowledge": lessons,
		"history":   history,
	})
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

// handleWorkerSubmit worker 提交完成：进入验收流；可复用经验回流知识库（进化闭环）。
func (s *Server) handleWorkerSubmit(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	var req struct {
		TaskID  int64  `json:"task_id"`
		ClaimID string `json:"claim_id"`
		Summary string `json:"summary"`
		Lessons string `json:"lessons"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.TaskID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task_id 必填"})
		return
	}
	ctx := r.Context()
	// 原子提交：要求任务仍是本 worker 手上的 in_progress。若此刻分配者刚把它
	// 改需求重置为 pending，提交落空（ErrNotFound），旧交付不会被当成完成。
	t, _, err := s.store.SubmitWorkerTask(ctx, req.TaskID, u.ID, req.ClaimID, req.Summary)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "任务当前状态不允许提交（可能已被改派或重置）"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "提交失败"})
		return
	}
	// 进化：可复用经验回流知识库，供后续同类任务检索。
	if lessons := strings.TrimSpace(req.Lessons); lessons != "" {
		tags := []string{"worker经验", fmt.Sprintf("worker:%d", u.ID), fmt.Sprintf("project:%d", t.ProjectID)}
		if _, err := s.store.CreateKnowledge(ctx, t.Title, lessons, tags, u.ID); err != nil {
			slog.Warn("worker 经验入库失败", "task", t.ID, "err", err)
		}
	}
	// 通知派活人来验收。
	if t.AssignerID != u.ID && s.deps.Notifier != nil {
		_ = s.deps.Notifier.Send(ctx, t.AssignerID,
			fmt.Sprintf("📥 AI 员工 %s 提交了任务「%s」（#%d），等你验收。", u.Name, t.Title, t.ID))
	}
	slog.Info("worker 提交任务", "worker", u.ID, "task", t.ID, "lessons", req.Lessons != "")
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}

// mcpServer 对外 MCP：按 token 换用户，暴露其权限内的工具集。
func (s *Server) mcpServer(r *http.Request) *mcp.Server {
	u := s.authenticate(r)
	if u == nil {
		return nil
	}
	return mcpbridge.NewServer("nbco", "1", tools.ForUser(s.deps, u, nil))
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
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
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

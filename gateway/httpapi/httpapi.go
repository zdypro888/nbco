// Package httpapi 是 HTTP 入口：内嵌 Web 页 + REST 对话/任务接口 + 对外 MCP 端点 + CLI 回连端点。
// 认证只走 Authorization: Bearer <api token>（不支持查询参数，避免 token 进访问日志）。
package httpapi

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/events"
	"github.com/zdypro888/nbco/knowledge"
	"github.com/zdypro888/nbco/mcpbridge"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

// Channel HTTP 渠道标识（Web 页与 REST 共用同一会话）。
const Channel = "api"

const maxJSONBodyBytes = 1 << 20

//go:embed web/index.html
var indexHTML []byte

// LLMConfig worker 内置智能体的模型管道配置（/api/worker/llm 透传代理）。
// 中枢只做管道：model 服务端钉死、API key 不出中枢、内容不解析。
type LLMConfig struct {
	BaseURL string // OpenAI 兼容网关地址；空 = 管道关闭（worker 内置智能体不可用）
	APIKey  string
	Model   string
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
	mux.HandleFunc("POST /api/worker/bind", s.handleWorkerBind)
	mux.HandleFunc("POST /api/worker/llm", s.handleWorkerLLM)
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

// truncateRunes 按字符截断（事件详情里带交付摘要时防超长，不切坏多字节字符）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
			fmt.Sprintf("AI 员工「%s」（#%d）刚在工作机完成绑定，已可领取任务。", u.Name, u.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "worker_id": u.ID, "worker_name": u.Name})
}

// llmProxyTimeout 单次上游模型调用的墙钟上限（agent 步长在 worker 侧另有控制）。
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

// handleWorkerLLM 内置智能体的模型管道：把 worker 发来的 OpenAI 格式请求透传
// 到中枢配置的模型服务。中枢只做三件事：worker 认证、钉死 model、带上服务端
// API key——内容不解析、不改写（stream 除外），业务智能全在 worker 的 agent 循环里。
func (s *Server) handleWorkerLLM(w http.ResponseWriter, r *http.Request) {
	u := s.requireWorker(w, r)
	if u == nil {
		return
	}
	if strings.TrimSpace(s.llm.BaseURL) == "" || strings.TrimSpace(s.llm.Model) == "" {
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
	body["model"] = s.llm.Model // 服务端钉死，防 worker 指定任意模型
	body["stream"] = false      // 管道不透传流式
	buf, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体无法序列化"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), llmProxyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(s.llm.BaseURL, "/")+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "构造上游请求失败"})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.llm.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.llm.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Warn("worker llm 管道上游失败", "worker", u.ID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "上游模型服务不可达"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("worker llm 管道上游非 200", "worker", u.ID, "status", resp.StatusCode)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
	// 提交事件交派活人的 AI 分析：AI 可先看交付摘要，通知里直接给验收建议。
	if t.AssignerID != u.ID {
		s.bus.Emit("任务提交待验收", t.AssignerID,
			fmt.Sprintf("AI 员工「%s」提交了任务「%s」（#%d）待你验收。提交摘要：%s",
				u.Name, t.Title, t.ID, truncateRunes(req.Summary, 400)))
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

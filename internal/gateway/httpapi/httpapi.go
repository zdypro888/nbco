// Package httpapi 是 HTTP 入口：REST 对话接口 + 对外 MCP 端点 + claudecli 回连端点。
// 认证只走 Authorization: Bearer <api token>（不支持查询参数，避免 token 进访问日志）。
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/zdypro888/nbco/internal/chat"
	"github.com/zdypro888/nbco/internal/mcpbridge"
	"github.com/zdypro888/nbco/internal/store"
	"github.com/zdypro888/nbco/internal/tools"
)

// Channel HTTP 渠道标识。
const Channel = "api"

// Server HTTP 入口。
type Server struct {
	store *store.Store
	orch  *chat.Orchestrator
	deps  tools.Deps
	// CLIHandler claudecli 引擎的回连 MCP handler；eino 引擎下为 nil。
	CLIHandler http.Handler
}

// New 创建 HTTP 入口。
func New(s *store.Store, orch *chat.Orchestrator, deps tools.Deps) *Server {
	return &Server{store: s, orch: orch, deps: deps}
}

// Handler 组装路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /api/chat", s.handleChat)
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(s.mcpServer, nil))
	if s.CLIHandler != nil {
		mux.Handle("/mcp/cli", s.CLIHandler)
	}
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

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	u := s.authenticate(r)
	if u == nil {
		http.Error(w, `{"error":"未认证"}`, http.StatusUnauthorized)
		return
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		http.Error(w, `{"error":"message 必填"}`, http.StatusBadRequest)
		return
	}
	// 对话可能耗时数分钟，脱离请求超时限制交给引擎自身控制。
	answer, err := s.orch.HandleMessage(r.Context(), u, Channel, req.Message)
	if err != nil {
		slog.Error("API 对话失败", "user", u.ID, "err", err)
		http.Error(w, `{"error":"对话处理失败"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"reply": answer})
}

// mcpServer 对外 MCP：按 token 换用户，暴露其权限内的工具集。
func (s *Server) mcpServer(r *http.Request) *mcp.Server {
	u := s.authenticate(r)
	if u == nil {
		return nil
	}
	return mcpbridge.NewServer("nbco", "1", tools.ForUser(s.deps, u, nil))
}

// Serve 启动 HTTP 服务并随 ctx 关停。
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	ihtml "github.com/zdypro888/ihtml"
	"github.com/zdypro888/ihtml/sqlstore"

	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

// EnableIHTML mounts ihtml as a library inside nbco. It deliberately imports
// only ihtml's core and sqlstore packages: the standalone ihtml/chat Agent is
// not constructed, so model configuration and Agent lifecycle remain owned by
// nbco's single Orchestrator/Eino engine.
func (s *Server) EnableIHTML() error {
	if s == nil || s.store == nil || s.orch == nil {
		return errors.New("启用 ihtml 需要可用的 store 和 orchestrator")
	}
	if s.ihtmlHandler != nil {
		return nil
	}
	tickets, err := newIHTMLTicketManager()
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(s.store.Pool())
	uiStore, err := sqlstore.New(db, sqlstore.Postgres)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("初始化 ihtml PostgreSQL store: %w", err)
	}
	// Schema creation belongs to nbco migration 0060. Probe it here so a partial
	// deployment fails before opening HTTP traffic instead of on the first user
	// write.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	var versionTable string
	if err := db.QueryRowContext(probeCtx, `SELECT COALESCE(to_regclass('public.ihtml_user_version')::text, '')`).Scan(&versionTable); err != nil || versionTable == "" {
		_ = db.Close()
		if err == nil {
			err = errors.New("migration 0060 is not applied")
		}
		return fmt.Errorf("检查 ihtml 数据表: %w", err)
	}

	s.ihtmlTickets = tickets
	backend := newIHTMLChatBackend(s.orch, s.store, s.llm.Provider, s.runtimeLLMModel,
		timeDurationMilliseconds(s.llm.TimeoutMS))
	handler, err := ihtml.NewHandler(uiStore,
		ihtml.WithPageTitle("nbco 动态工作台"),
		ihtml.WithUserResolver(s.resolveIHTMLUser),
		ihtml.WithChatBackend(backend),
		ihtml.WithPageConfig(s.ihtmlPageConfig),
		ihtml.WithAPIs(nbcoIHTMLAPIs()...),
		ihtml.WithAccessPolicyE(s.ihtmlAccessPolicy),
		ihtml.WithAudit(s.auditIHTML),
	)
	if err != nil {
		s.ihtmlTickets = nil
		_ = db.Close()
		return fmt.Errorf("创建 ihtml handler: %w", err)
	}
	s.ihtmlHandler = handler
	s.ihtmlClose = func() error {
		return errors.Join(handler.Close(), db.Close())
	}
	return nil
}

func timeDurationMilliseconds(milliseconds int) time.Duration {
	if milliseconds <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func (s *Server) Close() error {
	if s == nil || s.ihtmlClose == nil {
		return nil
	}
	closeFn := s.ihtmlClose
	s.ihtmlClose = nil
	return closeFn()
}

func (s *Server) ihtmlPageConfig(ctxRequest *http.Request, user string) map[string]any {
	userID, err := strconv.ParseInt(strings.TrimSpace(user), 10, 64)
	if err != nil {
		return map[string]any{}
	}
	u, err := s.store.UserByID(ctxRequest.Context(), userID)
	if err != nil {
		return map[string]any{"user_id": userID}
	}
	return map[string]any{
		"user_id": u.ID, "user_name": u.Name, "is_superadmin": u.IsSuperadmin,
		"nbco_api_base": "/api/",
	}
}

func (s *Server) ihtmlAccessPolicy(r *http.Request, user string) (ihtml.Access, error) {
	userID, err := strconv.ParseInt(strings.TrimSpace(user), 10, 64)
	if err != nil || userID <= 0 {
		return ihtml.Access{}, errors.New("invalid ihtml user")
	}
	u, err := s.store.UserByID(r.Context(), userID)
	if err != nil || u.Status != store.UserActive {
		return ihtml.Access{}, errors.New("ihtml user is unavailable")
	}
	return ihtml.Access{}, nil
}

func (s *Server) auditIHTML(ctx context.Context, event ihtml.AuditEvent) {
	userID, err := strconv.ParseInt(strings.TrimSpace(event.User), 10, 64)
	if err != nil || userID <= 0 {
		return
	}
	args, _ := json.Marshal(map[string]any{
		"action": event.Action, "by": event.By, "note": event.Note, "detail": event.Detail,
	})
	args = []byte(textfmt.RedactSecrets(string(args)))
	result := "ihtml workspace mutation committed"
	if err := s.store.Audit(ctx, userID, nil, "ihtml."+event.Action, args, result, true); err != nil {
		slog.Warn("ihtml 审计写入失败", "user", userID, "action", event.Action, "err", err)
	}
}

func nbcoIHTMLAPIs() []ihtml.APISpec {
	return []ihtml.APISpec{
		{Name: "nbco_overview", Title: "运营总览", Method: "GET", Path: "/api/overview", Description: "当前用户可见的运营概览。"},
		{Name: "nbco_me", Title: "当前身份", Method: "GET", Path: "/api/me", Description: "当前用户的稳定内部 ID、名称和权限摘要。"},
		{Name: "nbco_my_tasks", Title: "我的任务", Method: "GET", Path: "/api/me/tasks", Description: "当前用户的待办任务。"},
		{Name: "nbco_my_review", Title: "待我验收", Method: "GET", Path: "/api/me/review", Description: "当前用户待验收的任务。"},
		{Name: "nbco_my_assigned", Title: "我分配的任务", Method: "GET", Path: "/api/me/assigned", Description: "当前用户分配给他人的任务。"},
		{Name: "nbco_schedules", Title: "定时自动化", Method: "GET", Path: "/api/schedules", Description: "当前用户可见的定时任务。"},
		{Name: "nbco_files", Title: "文件中心", Method: "GET", Path: "/api/files", Description: "当前用户可见的文件元数据。"},
		{Name: "nbco_workers", Title: "Worker 状态", Method: "GET", Path: "/api/admin/workers", Description: "有权限时返回 Worker 在线和能力状态。"},
		{Name: "nbco_task_queue", Title: "任务队列", Method: "GET", Path: "/api/admin/task-queue", Description: "有权限时返回系统任务队列。"},
		{Name: "nbco_learning", Title: "学习候选", Method: "GET", Path: "/api/admin/learning", Description: "有权限时返回待治理的学习候选。"},
		{Name: "nbco_decisions", Title: "决策队列", Method: "GET", Path: "/api/admin/decisions", Description: "有权限时返回待处理决策。"},
		{Name: "nbco_approvals", Title: "待确认操作", Method: "GET", Path: "/api/admin/approvals", Description: "有权限时返回待用户确认的高影响操作。"},
		{Name: "nbco_action_turns", Title: "动作轮次", Method: "GET", Path: "/api/admin/action-turns", Description: "有权限时返回最近 Agent 执行轮次。"},
		{Name: "nbco_capabilities", Title: "能力目录", Method: "GET", Path: "/api/admin/capabilities", Description: "有权限时返回当前可用工具和权限边界。"},
		{Name: "nbco_workflows", Title: "工作流", Method: "GET", Path: "/api/admin/workflows", Description: "有权限时返回可用工作流。"},
		{Name: "nbco_ai_settings", Title: "AI 运行配置", Method: "GET", Path: "/api/admin/ai-settings", Description: "超级管理员可见的当前模型与运行配置。"},
		{Name: "nbco_operations", Title: "系统运行状态", Method: "GET", Path: "/api/admin/ops", Description: "超级管理员可见的服务、索引和运行状态。"},
	}
}

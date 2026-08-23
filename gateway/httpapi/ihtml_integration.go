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

	"github.com/zdypro888/nbco/chat"
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
	backend := newIHTMLChatBackend(s.orch, s.store, s.llm.Provider, s.brandName(), s.runtimeLLMModel, s.deps.SubcallAI,
		timeDurationMilliseconds(s.llm.TimeoutMS))
	handler, err := ihtml.NewHandler(uiStore,
		ihtml.WithPageTitle(s.brandName()+" 动态工作台"),
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
	apis := nbcoIHTMLAPIs()
	s.orch.SetTurnExtensionProvider(func(_ context.Context, u *store.User, channel string) (*chat.TurnExtension, error) {
		// The embedded ihtml chat already supplies browser context and the same
		// scoped tools explicitly. Other private channels receive the capability
		// lazily through the shared Orchestrator.
		if channel == ihtmlChatChannel {
			return nil, nil
		}
		scoped, err := ihtml.ScopeService(handler, strconv.FormatInt(u.ID, 10))
		if err != nil {
			return nil, fmt.Errorf("绑定 ihtml 用户作用域: %w", err)
		}
		return &chat.TurnExtension{
			System: crossChannelIHTMLSystem(),
			Tools: ihtmlAgentTools(scoped, ihtmlAgentToolOptions{
				APIs: apis, PublicBaseURL: s.deps.PublicBaseURL,
				ReviewPage: newIHTMLPageReviewer(s.deps.SubcallAI, u),
			}),
		}, nil
	})
	s.ihtmlClose = func() error {
		s.orch.SetTurnExtensionProvider(nil)
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
		{Name: "nbco_users", Title: "成员目录", Method: "GET", Path: "/api/users", Description: "权限感知的成员目录。支持 q、status(active|disabled)、kind(human|worker)、limit(1..100)、offset 查询参数。响应为 {users:[{user_id,name,status,is_superadmin,is_worker,owner_id,worker_last_seen,info,created_at}],limit,offset,next_offset}；员工自定义字段位于 info 对象中，并已按当前身份裁剪。必须使用稳定 user_id 标识成员，使用 is_worker 区分真人与 Worker。"},
		{Name: "nbco_data_sources", Title: "数据源目录", Method: "GET", Path: "/api/data/sources", Description: "列出当前身份可访问的权限感知数据源、字段、稳定实体 ID 字段和说明。"},
		{Name: "nbco_data", Title: "通用数据读取", Method: "GET", Path: "/api/data/{source}", Description: "读取一个权限感知数据源。source 使用目录中的稳定名称；支持重复 q 做词法筛选、在目录声明 stable_id_field 时用重复 id 回读稳定实体ID、filter.<field>=value 精确筛选、limit(1..100)、offset(0..10000)。响应为 {source,rows,count,limit,offset,next_offset,page_full}；page_full 只表示本页达到上限，不虚构后续必有数据。权限在数据库读取层执行，页面不得缓存或推断不可见字段。"},
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

func crossChannelIHTMLSystem() string {
	return "当前用户拥有一个可持久化的 ihtml 动态工作台。用户要求网页、表格视图、仪表盘或可视化操作界面时，使用 ui_* 工具直接实现；" +
		"需要实时业务数据时先读取 ui_list_host_apis；GET 使用 ihtml.http(path, options) 或 ihtml.http.get(path, options)，其他请求使用对应方法。" +
		"已有宿主数据必须由页面通过已登记 API 按稳定 ID 加载，不要把大批业务记录手工复制进源码；派生数据量较大或需要独立更新时，用 ui_put_data 保存结构化 JSON，页面通过 ihtml.kv.get(key) 读取；创建或整体更新页面用 ui_publish_page 原子发布。" +
		"ui_publish_page 会在写入前执行同模型设计审核：design_review.verdict=revise 表示没有提交，须按 issues 修正后重试；unavailable 表示只缺少设计审核，不得声称已经过视觉验收。" +
		"发布后用 ui_inspect_page 核对，并把工具返回的 workspace_url 原样交给用户；不要自行拼接地址，也不要用通用工作台首页代替具体页面。" +
		"\n[ihtml 页面设计与实现契约]\n" + ihtml.PageAuthoringGuide
}

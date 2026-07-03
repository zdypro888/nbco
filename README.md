# nbco — AI 公司运营中枢

让几十人规模、没有职业中层的公司，靠 AI 运转起来。AI 是每个员工的直属经理 + 老板的参谋部；IM、Web 都只是它的接口。规划见 [PLAN.md](PLAN.md)。

Go 单二进制：Telegram 网关、HTTP API/MCP、AI 引擎、定时调度跑在一个进程里；进程完全无状态，所有运行状态（用户、权限、任务、会话、绑定 Key、定时任务、审计）落 PostgreSQL，随时可杀可重启。

> Python 原型已移至 `legacy/`，仅作参考。

## 架构

```
┌─ 入口层（皆可换）────────────────────────────┐
│  gateway/telegram    gateway/httpapi(REST+MCP) │
├─ 编排层 ─────────────────────────────────────┤
│  chat（会话落库·系统提示·引擎调度）           │
│  sched（DB 驱动定时·每日汇总）                │
├─ AI 引擎（可换）─────────────────────────────┤
│  ai.Engine 接口                               │
│   ├─ einoengine：eino ADK 直调 API（claude/openai）│
│   └─ claudecli：claude CLI headless + MCP 回连 │
├─ 领域层 ─────────────────────────────────────┤
│  tools（工具即权限边界·全量审计）             │
│  perm（双维度权限纯逻辑·单测覆盖）            │
├─ 存储层 ─────────────────────────────────────┤
│  store（pgx·内嵌迁移）→ PostgreSQL           │
└──────────────────────────────────────────────┘
```

核心设计：

- **自有 Tool 抽象**（`internal/ai.Tool`：名称 + JSON Schema + handler），不绑任何框架。eino、claude CLI（经 MCP 回连）、对外 MCP、HTTP API 都是同一套工具的薄适配。
- **双引擎**：`eino` 引擎直调模型 API（客户自带 key 的产品路径）；`claudecli` 引擎驱动本机 claude CLI（订阅额度路径），工具经一次性 token 的 MCP 回连暴露，天然按用户隔离。
- **工具即权限边界**：每个工具 handler 内部做权限校验（超管专属工具只组装给超管），每次调用写审计日志。

## 配置

复制 `nbco.json.example` 为 `nbco.json`：

| 字段 | 说明 |
|------|------|
| `telegram_token` | Bot token |
| `superadmins` | Telegram 用户 ID 列表，首次发消息自动开通 |
| `postgres_dsn` | PostgreSQL 连接串（首次启动自动建表） |
| `listen` | HTTP 监听地址，默认 `127.0.0.1:8900` |
| `timezone` | IANA 时区，默认 `Asia/Shanghai` |
| `daily_summary_hour` | 每日待办推送小时（0-23），-1 关闭 |
| `ai.engine` | `eino`（直调 API）或 `claudecli`（驱动 claude CLI） |
| `ai.provider` | eino 引擎：`claude` 或 `openai`（兼容网关） |

## 构建与运行

```bash
go build -o nbco ./cmd/nbco
./nbco -config nbco.json
```

## HTTP API

认证：`Authorization: Bearer <token>`（在 TG 里让 AI 调 `generate_api_token` 生成）。

- `POST /api/chat` `{"message":"..."}` → `{"reply":"..."}` — 与 TG 同一编排器，独立会话
- `/mcp` — 对外 MCP 端点（Streamable HTTP），暴露该用户权限内的全部工具
- `GET /healthz`

## 权限体系

**主动权限**（存在操作者身上：我能对谁做什么）：`write_profile` / `view_self_intro` / `manage_perm` / `generate_key` / `send_msg` / `create_project` / `edit_info`，目标为用户 ID 或 `_all`。

**被动权限**（存在被操作者身上：谁能对我做什么）：`view_profile:<作者ID>` / `view_profile:_all`。

规则：超管旁路；非超管只能转授自己拥有且范围不超过自己的权限；派任务时执行人继承分配者的 `view_self_intro` 范围。判定逻辑在 `internal/perm`（纯函数，有单测）。

## 测试

```bash
go test ./...
go vet ./...
```

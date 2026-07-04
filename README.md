# nbco — AI 公司运营中枢

让几十人规模、没有职业中层的公司，靠 AI 运转起来。AI 是每个员工的直属经理 + 老板的参谋部；IM、Web 都只是它的接口。规划见 [PLAN.md](PLAN.md)。

Go 单二进制：Telegram 网关、HTTP API/MCP、AI 引擎、定时调度跑在一个进程里；进程完全无状态，所有运行状态（用户、权限、任务、会话、绑定 Key、定时任务、审计）落 PostgreSQL，随时可杀可重启。

> Python 原型已移至 `legacy/`，仅作参考。

## 架构

```
┌─ 入口层（皆可换）────────────────────────────┐
│  gateway/telegram  gateway/httpapi(Web+REST+MCP)│
├─ 编排层 ─────────────────────────────────────┤
│  chat（会话落库·系统提示·引擎调度）           │
│  sched（DB 驱动定时·截止提醒·每日汇总/日报）  │
├─ AI 引擎（可换）─────────────────────────────┤
│  ai.Engine 接口                               │
│   └─ einoengine：eino ADK 直调 API（claude/openai）│
├─ 领域层 ─────────────────────────────────────┤
│  tools（工具即权限边界·全量审计）             │
│  perm（双维度权限纯逻辑·单测覆盖）            │
├─ 存储层 ─────────────────────────────────────┤
│  store（pgx·内嵌迁移）→ PostgreSQL           │
└──────────────────────────────────────────────┘
```

核心设计：

- **自有 Tool 抽象**（`internal/ai.Tool`：名称 + JSON Schema + handler），不绑任何框架。eino、对外 MCP、HTTP API 都是同一套工具的薄适配。
- **中枢只走 API 引擎**：`eino` 引擎直调模型 API（客户自带 key 的产品路径）。本机 CLI 只允许由 `nbco-worker` 通过交互式 PTY 驱动，严禁 `claude -p` / `codex exec` 这类 headless 入口。
- **工具即权限边界**：每个工具 handler 内部做权限校验（超管专属工具只组装给超管），每次调用写审计日志。
- **分渠道排版**：系统提示按会话渠道注入格式指引——Telegram 用其 HTML 子集（粗体/代码/引用）+ emoji，网关先按 HTML 发送、格式非法自动降级纯文本；Web/API 输出纯文本。

## 配置

复制 `nbco.json.example` 为 `nbco.json`：

| 字段 | 说明 |
|------|------|
| `telegram_token` | Bot token |
| `superadmins` | Telegram 用户 ID 列表（可留空：全新系统里第一个对 bot 发 `/superadmin` 的人自动成为超管） |
| `postgres_dsn` | PostgreSQL 连接串（首次启动自动建表） |
| `listen` | HTTP 监听地址，默认 `127.0.0.1:8900` |
| `log_level` | `debug` / `info` / `warn` / `error`，默认 `info`（debug 会记录消息与工具调用全文） |
| `public_base_url` | 保留给外部回调集成，通常留空 |
| `timezone` | IANA 时区，默认 `Asia/Shanghai` |
| `daily_summary_hour` | 每日待办推送小时（0-23），-1 关闭 |
| `mcp_servers` | 外接 MCP 工具服务列表（`name`/`url`/`headers`），可选 |
| `ai.engine` | 仅支持 `eino`（直调 API） |
| `ai.provider` | eino 引擎：`claude` 或 `openai`（兼容网关） |
| `ai.api_key` / `ai.model` | eino 引擎必填 |

## 构建与运行

```bash
go build -o nbco ./cmd/nbco
./nbco -config nbco.json
```

Docker（中枢服务；AI 员工 worker 仍建议装在真实工作机上）：

```bash
cp nbco.json.example nbco.json   # postgres_dsn 指向 postgres 服务名，listen 用 0.0.0.0:8900
docker compose up -d
```

## Web 入口与 HTTP API

浏览器打开 `http://<listen>/` 即是 Web 入口（内嵌单页，无需部署前端）：粘贴 API Token 登录，
可对话（与 REST 同一会话）、看我的待办/待验收/我分配的任务；超管多一个全景页（统计+项目+过期点名）。

认证一律 `Authorization: Bearer <token>`（在 TG 里让 AI 调 `generate_api_token` 生成）：

- `POST /api/chat` `{"message":"..."}` → `{"reply":"..."}` — 与 TG 同一编排器，独立会话
- `GET /api/me` — 当前用户
- `GET /api/me/tasks` / `GET /api/me/review` / `GET /api/me/assigned` — 待办 / 待我验收 / 我分配的
- `GET /api/overview` — 全局统计+项目+过期任务（超管）
- `/mcp` — 对外 MCP 端点（Streamable HTTP），暴露该用户权限内的全部工具
- `GET /healthz`

## 任务流转（验收状态机）

```
pending → in_progress → done（提交待验收）→ accepted（验收通过，终态）
                          ↑        │
                          └────────┘ reject_task 打回（理由入进度记录）
```

- 自派任务（分配者=执行人）提交即 `accepted`，免自我验收
- 拆分的任务：子任务**全部验收通过**时父任务自动转入待验收，逐级向上；父任务也是自派的则直达 `accepted`
- 验收工具：`get_review_queue` / `accept_task` / `reject_task`（限分配者与超管）

## AI 员工 / Worker

`nbco-worker` 装在工作机上，把一台机器变成可派活的 AI 员工。worker 本质是一个特殊用户，复用任务、进度、验收、催办、画像与审计机制。

```bash
go build -o nbco-worker ./cmd/nbco-worker
nbco-worker bind http://127.0.0.1:8900 <create_worker 返回的一次性令牌>
nbco-worker run [-engine claude|codex] [-bin /path/to/cli]
```

执行规则：worker 只能启动 `claude` / `codex` 的**交互式 PTY**，像人在终端里粘贴任务一样输入，多行任务用 bracketed paste 投递；严禁 `claude -p` / `codex exec` 等 headless 入口。

## 主动运营（AI 主动，人被动）

调度器每 30 秒扫库，发送标记落库（原子认领），重启不重发。两级主动性——模板消息（确定性）与 **AI 轮次**（调度器把系统指令注入用户会话跑一轮引擎，产出个性化内容后推送）：

- **定时提醒**：用户让 AI 设置的单次/循环提醒（`schedule_once` / `schedule_repeating`）
- **临近截止**：任务截止前 24 小时提醒执行人；分配者改截止时间后重新生效
- **过期通知**：截止时间一过，通知执行人与分配者（各一次）
- **AI 催办**：过期任务每 48 小时无动静（无进度更新），AI 核实状态后向执行人发出个性化催办；有汇报就不打扰
- **催办升级**：同一任务累计催办 2 次仍无进度，通知分配者介入（调整/改期/改派）
- **每日待办**：`daily_summary_hour` 时刻给每个有待办的人推清单
- **老板日报**：同一时刻给超管推全局概览（进行中/过期/待验收/近24小时验收 + 过期任务点名）
- **AI 周报**：每周一同一时刻，AI 调 `company_overview` 等工具核实数据后，给每位超管写叙事周报
- **月度人员盘点**：每月 1 号，AI 以超管身份基于任务履历更新成员画像草稿，并推送盘点摘要

## 角色 / Skill

内置十一个开箱即用的工作模式（迁移种子，可改可删）：**CEO参谋、产品经理、开发工程师、测试工程师、前端工程师、运营经理、市场营销、销售顾问、财务顾问、HR招聘、UI设计师**。
角色定义遵循"身份 + 工作流程 SOP + 交付标准 + 质量红线"的结构（借鉴 Agency-Agents 方法论），并绑定系统工具（验收、负载查询、知识沉淀、提醒）——角色即工作流，不是人设扮演。
系统提示注入角色清单，AI 匹配场景时主动建议切换；`activate_role` 切换、`list_roles` 查看；超管可 `create_role` 增补。

## 知识与画像（越用越值钱）

- **知识库**：`save_knowledge` / `search_knowledge` 等工具全员可用；系统提示要求 AI 主动沉淀有复用价值的结论、回答公司事实前先检索
- **履历统计**：`get_user_stats` 输出某人的当前负载、验收通过数、按时率——派任务前的参考，也是画像的数据原料

## 权限体系

**主动权限**（存在操作者身上：我能对谁做什么）：`write_profile` / `view_self_intro` / `manage_perm` / `generate_key` / `send_msg` / `create_project` / `edit_info`，目标为用户 ID 或 `_all`。

**被动权限**（存在被操作者身上：谁能对我做什么）：`view_profile:<作者ID>` / `view_profile:_all`。

规则：超管旁路；非超管只能转授自己拥有且范围不超过自己的权限；派任务时执行人继承分配者的 `view_self_intro` 范围。判定逻辑在 `internal/perm`（纯函数，有单测）。

## 测试

```bash
go test ./...    # 纯单测；store 集成测试默认跳过
go vet ./...

# 跑 store 集成测试（需要 PostgreSQL，CI 自动跑）：
NBCO_TEST_PG_DSN='postgres://nbco:nbco@127.0.0.1:5432/nbco_test?sslmode=disable' go test ./internal/store/
```

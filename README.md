# nbco — AI 公司运营中枢

让几十人规模、没有职业中层的公司，靠 AI 运转起来。AI 是每个员工的直属经理 + 老板的参谋部；IM、Web 都只是它的接口。规划见 [PLAN.md](PLAN.md)，能力地图见 [docs/system-map.md](docs/system-map.md)。

Go 单二进制：Telegram 网关、HTTP API/MCP、AI 引擎、定时调度跑在一个进程里；进程完全无状态，所有业务事实（用户、权限、任务、会话、真人员工一次性邀请、Worker Access Token、定时任务、审计）落 PostgreSQL。Qdrant 只保存可从 PostgreSQL 重建的语义索引，任一进程都可随时重启。

## 架构

```
┌─ 入口层（皆可换）────────────────────────────┐
│  gateway/telegram  gateway/httpapi(Web+REST+MCP+ihtml)│
├─ 编排层 ─────────────────────────────────────┤
│  chat（会话落库·系统提示·引擎调度）       │
│  sched（DB 驱动定时·截止提醒·每日汇总/日报）│
├─ AI 引擎（可换）─────────────────────────────┤
│  ai.Engine 接口                               │
│   └─ einoengine：DeepAgent + OneShot·原生 session/skill/tool search│
├─ 领域层 ─────────────────────────────────────┤
│  tools（工具即权限边界·全量审计）         │
│  perm（双维度权限纯逻辑·单测覆盖）        │
├─ 存储层 ─────────────────────────────────────┤
│  store（pgx·内嵌迁移）→ PostgreSQL（事实源）│
│  semantic/vectorstore → Qdrant（可重建语义索引）│
└──────────────────────────────────────────────┘
```

核心设计：

- **自有 Tool 抽象**（`ai.Tool`：名称 + JSON Schema + handler），不绑任何框架。eino、对外 MCP、HTTP API 都是同一套工具的薄适配。
- **中枢只走 API 引擎**：`eino` 引擎直调模型 API（客户自带 key 的产品路径）。本机 CLI 只允许由 `nbco-worker` 通过交互式 PTY 驱动，严禁 `claude -p` / `codex exec` 这类 headless 入口。
- **Eino 双执行模式**：主对话和可能产生动作的系统轮次只经过一个 `DeepAgent`，由其原生循环负责规划、工具选择、skill 加载、连续执行与最终回答；`tool_search` 延迟发现权限内业务工具，summarization 管理长上下文，patchtoolcalls 修复中断会话的悬空工具调用。nbco 不在 Agent 前再跑意图分类或工具路由，也不在 Agent 后重写正常答复。Memory Miner、摘要压缩和受控内部分析使用普通 `ChatModelAgent` 的 `OneShot` 模式，严格禁止工具和 skill，只做一次生成。Deep 会话事件和 interrupt/cancel checkpoint 持久化到 PostgreSQL，服务重启后继续使用同一 agent 上下文；OneShot 仅用于独立的结构化分析和终端输出截断修复，不参与业务决策。
- **工具即权限边界**：每个工具 handler 内部做权限校验（超管专属工具只组装给超管），每次调用写审计日志。
- **分渠道排版**：系统提示按会话渠道注入格式指引——Telegram 用其 HTML 子集（粗体/代码/引用）+ emoji，网关先按 HTML 发送、格式非法自动降级纯文本；Web/API 输出纯文本。
- **[ihtml](https://github.com/zdypro888/ihtml) 动态工作台**：`/ui/` 以固定提交版本作为 Go 库挂进控制中心，Item、页面、KV 和修订与 nbco 共用 PostgreSQL，并按稳定内部用户 ID 隔离。ihtml 不再创建第二套模型或 Agent；它通过 `chat.TurnExtension` 把当前用户作用域的 `ui_*` 能力接入同一个 Orchestrator/Eino DeepAgent，继续复用模型切换、会话、权限、知识、skill、审计和上下文压缩。控制中心每次响应签发 CSP nonce，动态脚本无需放开全站 `unsafe-inline`。

## 配置

复制 `nbco.json.example` 为 `nbco.json`：

| 字段 | 说明 |
|------|------|
| `brand_name` | 当前实例面向用户的显示名称，默认 `nbco`；Web、Telegram 和 Agent 身份会使用它，内部协议标识保持不变 |
| `telegram_token` | Bot token；可留空，留空则不启动 Telegram 网关，HTTP/API/MCP/worker 仍可用 |
| `telegram_api_url` | 可选的自建 `telegram-bot-api` 基地址；为空使用 Telegram 云端。云端只能下载不超过 20MB 的文件，本机服务可接收更大的 Telegram 文件 |
| `superadmins` | Telegram 用户 ID 列表（启用 Telegram 时可留空：全新系统里第一个对 bot 发 `/superadmin` 的人自动成为超管） |
| `postgres_dsn` | PostgreSQL 连接串（首次启动自动建表） |
| `qdrant.url` | Qdrant gRPC 地址，例如 `http://127.0.0.1:6334`；空值禁用并回退 PostgreSQL 旧向量/词法路径 |
| `qdrant.api_key` | 自托管 Qdrant API Key；仅回环网络可留空 |
| `qdrant.collection_prefix` | collection 前缀，默认 `nbco_semantic`；模型、维度与实际输出指纹的哈希自动追加 |
| `qdrant.sync_interval_seconds` | 项目、任务、文件元数据、画像等结构化数据与 Qdrant 增量同步间隔，默认 120 秒；完整删除对账每 6 小时执行 |
| `qdrant.sync_timeout_seconds` | 单轮完整对账最长时间，默认 3600 秒；慢速本地 embedding 或大数据集可调高 |
| `ai.embed_revision` | 可选的 embedding 运行策略版本；上下文长度、池化方式等改变但模型名不变时更新它，强制使用新 collection |
| `listen` | HTTP 监听地址，默认 `127.0.0.1:8900` |
| `log_level` | `debug` / `info` / `warn` / `error`，默认 `info`（debug 只记录消息长度与短哈希，不记录对话/工具明文） |
| `file_store_path` | 文件存储目录，默认 `files`；相对路径按进程工作目录解释。上传后始终异步提取正文并保存 PostgreSQL 分块；启用 Qdrant 后再异步建立向量索引 |
| `worker_download_path` | `nbco-worker` 多平台发行物目录，默认 `downloads`；服务端通过 `/downloads/worker/...` 提供下载 |
| `public_base_url` | 外部可访问的 HTTPS 基地址；Telegram Mini App 文件中心等入口需要配置 |
| `timezone` | IANA 时区，默认 `Asia/Shanghai` |
| `daily_summary_hour` | 每日待办推送小时（0-23），-1 关闭 |
| `sched_ai_concurrency` | 调度器同时进行的 AI 轮次上限（催办/周报/定时 AI 推送），默认 4；防「全员问候」几百轮齐发打爆后端 |
| `mcp_servers` | 外接 MCP 工具服务列表（`name`/`url`/`headers`/`required_action`）；默认仅超管可用，可选 |
| `ai.engine` | 仅支持 `eino`（直调 API） |
| `ai.provider` | eino 引擎：`claude` 或 `openai`（兼容网关） |
| `ai.api_key` / `ai.model` | eino 引擎必填 |
| `ai.timeout_ms` | 单次模型 API 请求超时，默认 `300000`（5 分钟）；长上下文/慢模型可调大 |
| `ai.turn_timeout_ms` | 一整轮对话总时限（含排队、路由、工具与重试），默认 `600000`（10 分钟），最大 30 分钟 |
| `ai.max_turns` | Eino DeepAgent 单轮模型生成生命周期上限，默认 `64`；不是工具调用次数限制，同一工具可按任务需要重复或并行调用 |
| `ai.summarize_after_tokens` / `ai.summarize_after_messages` | Eino 原生上下文摘要触发阈值，任一达到即压缩 agent 上下文；默认 `24000` tokens / `80` messages，不删除产品聊天或审计记录 |
| `ai.stream_reasoning` | 是否在流式回复阶段展示模型推理内容，默认 `false`；超管可通过对话修改，运行时设置优先于配置文件默认值 |
| `ai.embed_model` | 语义检索的 embedding 模型（可选）；空=知识检索走词法。指向 OpenAI 兼容 embeddings 端点 |
| `ai.embed_base_url` / `ai.embed_api_key` | embedding 端点地址/密钥；仅 `ai.provider=openai` 时空值才回退 `ai.base_url` / `ai.api_key`，Claude/Anthropic 兼容主模型必须显式配置 |
| `ai.stt_model` | 语音转写模型（可选，如本地 whisper）；空=TG 语音提示改用文字。指向 OpenAI 兼容 /audio/transcriptions 端点 |
| `ai.stt_base_url` / `ai.stt_api_key` | 转写端点地址/密钥；仅 `ai.provider=openai` 时空值才回退 `ai.base_url` / `ai.api_key`，Claude/Anthropic 兼容主模型必须显式配置 |

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

## 生产反向代理与 TLS

生产环境建议只让反向代理监听公网 `80/443`，nbco 监听回环地址上的纯 HTTP。以 Caddy 为例：

```caddyfile
nbco.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8900
}
```

对应的 nbco 配置应满足：

- `listen` 使用 `127.0.0.1:<内部端口>`，不要直接暴露到公网。
- `public_base_url` 使用外部完整地址，例如 `https://nbco.example.com`。
- `tls_cert_file` / `tls_key_file` 留空，由 Caddy 终止 TLS、自动申请和续期证书。
- 自建 `telegram-bot-api` 必须使用另一个回环端口，并让 `telegram_api_url` 指向它；不能与 nbco 的 `listen` 重合。
- Caddy 的 `reverse_proxy` 原生支持 Worker WebSocket 和流式 HTTP，不需要单独配置升级头。

DNS 必须先指向服务器，并确保公网 `80/443` 可达，Caddy 才能完成 ACME 验证和自动续期。后端端口只绑定回环地址，因此不需要单独配置证书或开放防火墙。

## Web 入口与 HTTP API

浏览器打开 `http://<listen>/` 即是 Web 入口（内嵌单页，无需部署前端）：粘贴 Access Token 登录，
可对话（与 REST 同一会话）、看我的待办/待验收/我分配的任务和决策队列，并在“动态工作台”中让同一个 nbco Agent 按需生成可持久、可回滚的网页；超管还可以看全景、AI 员工能力、学习候选治理与运维状态。

普通浏览器和 API 使用 `Authorization: Bearer <token>`；从 Bot 按钮打开的 Telegram Mini App 使用 Telegram 签名自动登录，不在 URL 中传凭据。全新系统且没有 Telegram 时，先调一次：

```bash
curl -X POST http://<listen>/api/bootstrap \
  -H 'Content-Type: application/json' \
  -d '{"name":"老板"}'
```

该接口仅在系统没有活跃超管时可用，会返回首任超管和首个 API token；已有超管后再调用会返回 `409`。已有账号可在 TG/Web 对话里让 AI 调 `generate_api_token` 重新生成自己的 token。

## 凭证区别

| 凭证 | 给谁用 | 怎么生成 | 生命周期 | 用途 |
| --- | --- | --- | --- | --- |
| 真人员工一次性邀请 | 真人员工 | `invite_employee` | 默认 24 小时有效，只能用一次 | 员工首次在 Telegram 通过邀请链接或邀请码绑定身份 |
| 用户 Access Token | 真人员工或超管 | Telegram 私聊 `/token new` → `/token confirm`，或 AI 工具 `generate_api_token` | 常驻，重新生成会替换旧 token | HTTP API / Web / MCP / 控制中心认证 |
| Worker 绑定码（`wbc_` 前缀） | `nbco-worker` 客户端 | `create_worker` / `issue_worker_bind_code` | 24 小时有效，只能用一次 | 工作机 `bind/bootstrap` 时兑换 Worker Access Token |
| Worker Access Token | `nbco-worker` 客户端 | 绑定码兑换时由服务端签发 | 常驻，新绑定码被兑换或吊销 worker 时失效 | worker 轮询任务、回传进度、上传产物 |

这几种不要混用：邀请和绑定码是一次性的进门票，Access Token 是持续认证凭证。Worker Access Token 只在工作机兑换绑定码那一刻出现，不进入对话与会话历史。多个 worker 必须各自 `create_worker`；同一个 worker 的凭据放到多台机器上，服务端仍把它们视为同一个 AI worker（且后兑换的绑定码会替换旧 token）。

邀请可以带姓名和角色，例如“生成一个给 CEO 的邀请链接”会生成一次性 Telegram deep link，并在对方绑定后把 `role=CEO` 写入用户信息。

常用接口：

- `POST /api/chat` `{"message":"..."}` → `{"reply":"..."}` — 与 TG 同一编排器，独立会话
- `GET /api/me` — 当前用户
- `GET /api/search?q=...` — 按当前用户的数据权限搜索任务、文件、项目和成员
- `GET /api/me/tasks` / `GET /api/me/review` / `GET /api/me/assigned` — 待办 / 待我验收 / 我分配的
- `GET /api/overview` — 全局统计+项目+过期任务（超管）
- `GET /api/admin/workers` / `/api/admin/learning` / `/api/admin/decisions` / `/api/admin/ops` / `/api/admin/capabilities` — 运营控制中心数据源
- `GET /api/admin/evals` / `POST /api/admin/evals/run` — 查看并运行无生产副作用的 Eino 行为回归案例（超管）
- `POST /api/files`（multipart `file`，最大 200MB）/ `GET /api/files/{id}` — 上传/下载文件（按权限校验）；上传会进入显式材料生命周期，等待指令而不自动分析
- `POST /api/tasks/{id}/attachments` `{"file_id":123,"caption":"..."}` — 把文件挂到任务
- `GET /version` — 当前服务版本与 Go 版本（部署脚本会把 git SHA 写进版本号）
- `/mcp` — 对外 MCP 端点（Streamable HTTP），暴露该用户权限内的全部工具
- `GET /healthz` — 进程与数据库存活检查
- `GET /readyz` — 数据库及已配置外部网关就绪检查（部署切流应使用此接口）

## 任务流转（验收状态机）

```
pending → in_progress → done（提交待验收）→ accepted（验收通过，终态）
                          ↑        │
                          └────────┘ reject_task 打回（理由入进度记录）
```

- 每个任务持久化 `completion_policy`：普通任务默认 `self_accept_on_success`；确定性执行可声明 `auto_accept_on_success`；必须验收的任务可声明 `review_required`
- 显式 `reviewer` 始终优先于完成策略；失败结果始终进入 `done` 等待处理，不会自动标成成功
- 拆分的任务：子任务**全部验收通过**时，父任务按自己的 `completion_policy` 转入待验收或直接归档，并逐级向上
- 验收工具：`get_review_queue` / `accept_task` / `reject_task`（限分配者与超管）
- **依赖编排（流水线）**：`assign_task` 可带 `depends_on`（只能指向已存在任务，天然无环）；前置全部 `accepted` 之前 worker 领不到该任务，验收通过时自动唤醒就绪的下游 worker 并发事件给派活人的 AI——「开发→测试→审查」接力不再人肉盯
- **一个交付物一个任务 ID**：`assignee_id` 是唯一责任人；同一产出涉及多人时用 `collaborator_ids`（可执行/提交）、`reviewer_ids`（可验收/打回）、`watcher_ids`（只读/接收通知）。等价未终态任务在事务内自动合并，只有确实需要彼此独立产出时才用 `allow_parallel=true`
- **合并保留审计**：重复任务用 `cancel_assigned_task` 指向保留任务，不物理抹除；旧责任人、参与者、附件和下游依赖原子迁移，旧任务进入历史并记录取消原因与替代任务
- **智能派工**：`assign_task` 不填 `assignee_id` 时自动派给最合适的 AI 员工（在办最少 → 在线优先 → 通过率高），回复里说明选人理由
- **两段式审批**：破坏性工具（停用用户、吊销 worker、删项目/角色/字段、底层写库兜底）首次调用只登记待确认动作，AI 须向用户复述并获明确同意后以相同参数再次调用才执行（10 分钟时效、参数哈希匹配、全渠道生效）——防单轮冲动执行与提示注入一击即中

## AI 员工 / Worker

`nbco-worker` 装在工作机上，把一台机器变成可派活的 AI 员工。worker 本质是一个特殊用户，复用任务、进度、验收、催办、画像与审计机制。
worker 是独立工作代理，不依赖 Telegram；Telegram、Web、HTTP API、MCP 都只是给中枢创建任务和查看结果的入口。文件与产物闭环规划见 [docs/worker-roadmap.md](docs/worker-roadmap.md)。
每个 worker 必须各自 `create_worker` 拿一次性绑定码（`wbc_` 前缀），工作机 `bind/bootstrap` 时用它兑换自己的 Worker Access Token；服务端用 access token 反查 worker 用户 ID 来区分身份。不要把同一个 worker 的凭据复制给多台机器，那会被视为同一个 AI worker，多进程只是在抢同一身份的任务。绑定码过期或换机重绑用 `issue_worker_bind_code` 补发。

worker 可直接从中枢下载发行二进制：

```bash
curl -fsSL -o nbco-worker <PUBLIC_BASE_URL>/downloads/worker/nbco-worker-darwin-arm64
chmod +x nbco-worker
./nbco-worker bootstrap -install-service=true <PUBLIC_BASE_URL> <create_worker 返回的绑定码>
```

可下载文件：

- `nbco-worker-darwin-arm64`
- `nbco-worker-linux-amd64`
- `nbco-worker-linux-arm64`
- `nbco-worker-windows-amd64.exe`
- 对应 `.sha256` 校验文件

`bootstrap` 会完成 `bind`、写配置、安装并启动系统服务。服务化支持：

- macOS：LaunchAgent
- Linux：`systemd --user`
- Windows：任务计划（登录时启动）

服务管理：

```bash
./nbco-worker service-status
./nbco-worker status
./nbco-worker doctor
./nbco-worker workspace
./nbco-worker once
./nbco-worker logs
./nbco-worker install-service [-engine claude|codex] [-bin /path/to/cli]
./nbco-worker uninstall-service
```

手动模式仍可用：

```bash
./nbco-worker bind <PUBLIC_BASE_URL> <create_worker 返回的绑定码>
./nbco-worker run [-engine claude|codex] [-bin /path/to/cli]
```

`bind/bootstrap` 用绑定码兑换 Worker Access Token（也兼容直接传已有 token），校验其必须属于 worker，并把换来的 token 与 worker ID/名字写入 `~/.nbco-worker.json`；`run` 启动时也会打印当前上线身份。
worker 上线和单次执行前会向中枢上报能力（OS/Arch、引擎、CLI 版本、可用能力如 code/go/python/pdf/xlsx/images），`list_workers`、Web AI员工页和自动派工都会使用这些信号；任务里出现代码、PDF、Excel、图片等线索时会优先派给匹配能力的 worker，再看负载、在线状态和历史通过数。

### 沙箱化部署（推荐）

worker 的模型 CLI 带完整 shell，安全边界必须在部署侧：[deploy/worker-sandbox/](deploy/worker-sandbox/) 提供容器化模板——每个 worker 独立容器、低权限用户、独立卷、一枚绑定码，宿主机密（其他 worker 的 token、SSH 私钥）不在其可达范围。数据库备份模板见 [deploy/backup/](deploy/backup/)（全部公司状态只在 PG，务必启用）。

### 内置智能体（没有 claude/codex 也能干活）

工作机上未安装 claude/codex 时，worker 启动自动回退 **内置智能体**（也可显式 `-engine builtin`）：中枢模型当大脑、本机 shell 当手脚——worker 通过中枢的 `/api/worker/llm` 透传管道调模型（model 由服务端钉死、API key 不出中枢），在主题 workspace 里以 `run_command` 小步执行并自我验证，完成后 `task_done` 提交。进度回传、产物上传、验收流与 CLI 模式完全一致。能力弱于 claude/codex，但运维/脚本/数据/构建类任务足够用；装好 CLI 重启即自动恢复 PTY 模式。

同一台机器跑多个 worker 时，每个 worker 用独立配置文件：

```bash
./nbco-worker bootstrap -config ~/.config/nbco/workers/frontend.json -name nbco-worker-frontend <PUBLIC_BASE_URL> <frontend-worker-bind-code>
./nbco-worker bootstrap -config ~/.config/nbco/workers/reviewer.json -name nbco-worker-reviewer <PUBLIC_BASE_URL> <reviewer-worker-bind-code>

./nbco-worker run -config ~/.config/nbco/workers/frontend.json -engine claude
./nbco-worker run -config ~/.config/nbco/workers/reviewer.json -engine codex
```

也可以用环境变量 `NBCO_WORKER_CONFIG` 指向配置文件。

> **隔离建议（安全边界在部署侧）**：worker 用 `--dangerously-skip-permissions` 跑 CLI，模型有完整 shell，能读到 worker 账号可读的一切（包括自身 Worker Access Token）。产物上传做了纵深加固（拒软/硬链接、非常规文件），但那不是安全边界——真正的隔离靠部署：**每个 worker 跑在独立容器 / 低权限账号里**，把宿主机密（别的 Worker Access Token、SSH 私钥等）挡在其可达范围外。

执行规则：需要观察输出并连续判断的工作走 `delegate_worker_agent`，worker 启动 `claude` / `codex` 时必须用**交互式 PTY**，同一稳定 `scope_key` 自动恢复对应工作目录与原生 CLI session；严禁 `claude -p` / `codex exec` 等 headless 入口。只有无需 Agent 判断的原子命令才走 `run_worker_command`，默认用 stdout/stderr pipe 执行，命令确实依赖终端行为时才显式 `pty=true`。进度、完成汇报与产物统一回传；这不是常驻远程 shell。AI CLI 驱动手法（借鉴 [aibridge](https://github.com/zdypro888/aibridge)）：

- **vt10x 屏幕仿真**：PTY 字节流喂进内存终端仿真器，一切检测读渲染后的屏幕，不在原始流上扒 ANSI
- **两步投递**：多行任务用 bracketed paste 包住、停顿后单发回车（防 TUI 的 paste 防抖吞掉提交）
- **忙碌感知等待**：屏幕先动后稳判定回合结束；"esc to interrupt" 可见期间永不判闲也永不判卡，长任务不设硬超时
- **启动对话框自动应答**：Bypass Permissions 确认（选 Yes）、目录 trust 确认
- **收尾解析防回显**：从最后一个哨兵块回溯、跳过任务原文的回显；没按格式收尾会补提醒，仍失败则以屏幕摘录提交
- **任务级 Agent 循环**：CLI 结束一轮对话不等于任务完成；未提交结果时自动继续，有新进展就不限制交互轮数，仅连续无进展或任务总时限才停止
- **进度即屏幕**：定期回传屏幕快照作为任务进度

**深执行引擎可插拔（前瞻·买管道留业务）**：worker 驱动的不限于 claude/codex——`~/.nbco-worker.json` 里配 `bin` + `args` + `busy_pattern`，就能把任意**交互式** harness（如 swarm 编排器 ruflo/claude-flow 的交互 REPL）挂成一个引擎，无需改代码。重活可交给更强的 swarm 深啃，nbco 只管派活/验收的业务闭环。仍守 PTY 交互铁律，只是换掉「启动哪个 CLI、怎么判完成」。

⚠️ **`busy_pattern` 最好只匹配「工作中才出现、空闲即消失」的瞬态状态行**（内置 `esc to interrupt` 就是这种），用来在屏幕短暂静止时不误判完成。完成检测已对常驻误配做了兜底：`BusyStable`（默认 2min）在**去噪后**（滴答计时/心跳/token 计数/spinner 归一）屏幕连续无实质变化时判完成，所以即便 `busy_pattern` 误命中常驻横幅、屏上有动画元素，也不会卡到 taskTimeout。例：`{"engine":"ruflo","bin":"ruflo","args":["chat"],"busy_pattern":"(?i)esc to interrupt|thinking…"}`（按目标 harness 真正的忙碌行填）。

### 实时通道（WebSocket）

**数据库是唯一任务队列**：派活=建任务、领活=原子认领（`FOR UPDATE SKIP LOCKED`），worker 离线不丢活、进程重启无状态可恢复。`GET /api/worker/ws`（Bearer 认证）之上只跑三类消息，是纯增强件：

- **唤醒 `wake`**：派活/委派审核/打回返工时推给在线 worker，秒级领活（离线则轮询兜底，10 秒）
- **取消 `cancel`**：分配者删除任务时，正在执行的 worker 立即终止 CLI 会话
- **心跳 `ping/pong`**：30 秒一跳；`list_workers` 显示「🔗 在线（实时连接）」为真实时状态

**打回即返工**：验收打回 worker 的任务会自动回到待领取并推唤醒，worker 重新领到时任务自带过程记录（含打回理由），按理由整改后重新提交——审核-返工闭环全自动。

### 文件与产物

worker 领取任务时，服务端会把任务附件下载到主题 workspace 下的 `.nbco-task/current/attachments/`；返工历史产物进入 `.nbco-task/current/previous_artifacts/`。prompt 会明确告诉 CLI 每个文件的相对路径。worker 若需要交付文件，把文件放进 `.nbco-task/current/artifacts/`，提交前会自动上传为任务产物，验收人可在任务详情里看到文件 ID 与名称。

返工时会保持工作连续性：上一轮已经上传的产物会作为只读输入重新下发到 `.nbco-task/current/previous_artifacts/`，同时最近过程记录会包含验收打回理由。worker 需要对照打回理由和上一轮产物继续修改，而不是从零重做。

worker 不把所有任务塞进同一个 Claude/Codex 长会话；每个任务仍是新的 PTY 进程，避免窗口衰减和隐私串任务。但 nbco 会在任务之上维护 **worker 主题会话**：按 `worker + engine + scope` 复用 workspace 与会话摘要。比如 nbco 代码/部署任务命中 `repo:nbco`，公司资料整理命中 `materials:company-intelligence`，普通项目任务命中 `project:<id>`。如果工作机配置了 `session_workspaces`，可把某个 scope 固定到真实目录：

```json
{
  "session_workspaces": {
    "repo:nbco": "/root/src/nbco"
  }
}
```

原生 CLI session 只在工作目录和 **engine runtime fingerprint** 同时一致时恢复。fingerprint 自动覆盖引擎、CLI 版本、启动参数、Codex/Claude 配置文件与相关环境变量；模型、provider、凭据来源或 CLI 版本变化后会开启新的原生 session，但仍保留相同 scope 的 workspace、摘要和中枢历史。自定义 harness 可用 `session_runtime_files` / `session_runtime_env` 把额外配置纳入指纹；服务端只保存 SHA-256，不接收文件内容或环境变量值。

长期记忆由中枢托管并在领活时注入：worker 自我画像、监护人画像、该 worker 自己沉淀的经验、当前项目经验、主题会话摘要和全局相关知识。worker 本地配置只保存认证、引擎参数与可选 workspace 映射，不维护不可审计的独立记忆文件。

生产升级这类高风险连续流程要反过来保证“一次尝试一个执行上下文”：不要把同一次 nbco 升级拆给多个 worker、多个 agent 或多条零散 worker 任务。优先用一个 command task 跑完整升级入口；如果需要 AI CLI 介入，也必须在一个 worker 任务的一次交互式 PTY session 里完成更新、测试、部署、健康检查与回滚判断。升级结束后该 session 仍按任务边界销毁，不跨任务保留。

### 分层铁律：中枢调度，worker 深干

中枢（eino 直调 API）是**调度管理层**：派活、跟进、催办、汇总，不在对话里做深度工作。要深度解决问题（写代码、审代码、深度调研）就派任务给 AI 员工，worker 驱动本机 claude/codex 完成。

**审核委派**：任务提交待验收后，分配者让中枢调 `delegate_review`——服务端把任务要素、执行过程、完成汇报打包成审核简报，作为高优先级任务派给审核角色（推荐 AI 员工，实地读代码、跑测试、逐条对照验收标准），结论（「建议通过」/「建议打回：理由」）随完成汇报回流，分配者只做最终拍板。

## 群聊（Telegram）

bot 可拉进群，交互按场景收敛（命令菜单按作用域注册：私聊菜单 /start /new，群菜单 /listen /new，互不串场）：

- **默认只应答点名**：@bot 或回复 bot 的消息才触发 AI，以**发言人的权限**跑**群共享会话**（历史带【发言人】署名），未绑定成员会被引导私聊绑定——群里绝不做绑定流程
- **/listen 群监听**（超管）：开启后 bot 旁听群讨论记入群会话上下文（不插话），再被 @ 时能接住前文；再次 /listen 关闭。旁听普通消息需在 @BotFather `/setprivacy` 选 Disable
- **群消息事实查询**：`list_telegram_group_messages` 按公司时区读取系统真实保存的群消息，跨 `/new` 会话重置并支持稳定游标分页；回答“今天群里说了什么”时不再拿群配置代替消息内容
- **事件监控**：`set_telegram_group_monitor` 只分析开启后的新消息。消息按时间窗口批处理，由 AI 判断是否值得提醒；分析批次有持久租约、失败退避和重启恢复，不依赖关键词表
- **每日摘要**：`set_telegram_group_digest` 是独立的持久自动化，必须明确发送时刻；到点读取当天群消息再生成摘要。它与 `/listen`、事件监控互不修改开关，也不冒充业务任务
- **/new**（超管）：重置本群会话
- 系统提示注入群纪律：回复全员可见，涉及隐私（画像/Token/私人任务）引导私聊，不主动插话

## 会话与上下文压缩

`chat_messages` 是唯一的跨轮对话事实源；`action_turns` 是与回复同事务提交的执行证据。每个交互轮次先在 `conversation_turns` 原子登记用户输入，再创建独立的 Eino managed session：Eino 原生 DeepAgent 在该轮内部持久记录模型消息、工具调用、结果和 checkpoint，完成后一次性提交助手消息、动作证据、用量和 Memory Miner 队列。下一轮从滚动摘要、近期聊天和限量执行事实重新建立 Eino session，不依赖上一轮可能回滚或缺失的引擎事件。

`conversation_turns` 同时记录 Agent 执行状态与渠道交付状态。Telegram 消息 ID 和 HTTP `Idempotency-Key` 只保存哈希，用于抑制重放；结果提交与外部发送分开确认，因此“Agent 已完成但 Telegram 发送失败”可被准确审计。进程启动会关闭超过轮次截止时间的失联 claim，但不会自动重放可能已经产生外部副作用的操作。私聊、群聊和 ihtml 共用这套生命周期；群 `/listen` 的旁听消息仍直接进入共享聊天事实，随后由同一产品层摘要和历史重放处理。

## 主动运营（AI 主动，人被动）

调度器每 30 秒扫库，发送标记落库（原子认领），重启不重发。两级主动性——模板消息（确定性）与 **AI 轮次**（调度器把系统指令注入用户会话跑一轮引擎，产出个性化内容后推送）。

**资源模型**：每次 tick 只做几条**命中部分索引**的原子认领查询（`WHERE fire_at <= now()` / `deadline` / kv 日期），成本随「到期数」而非「任务总数」增长——库里堆多少未来任务都不扫。重活（AI 轮次、逐人推送）**在认领后派发到限并发协程池异步执行**，既不阻塞 30 秒节拍（截止提醒照常及时），又用 `sched_ai_concurrency`（默认 4）护住模型网关：全员 AI 问候不会几百轮齐发，而是限并发滚动完成。模板推送另走 16 并发池。

- **定时提醒**：用户让 AI 设置的单次/循环提醒（`schedule_once` / `schedule_repeating`）
- **动态运营推送（`schedule_once_push` / `schedule_recurring_push`）**：单次日期与明确周期使用不同工具，避免把“周一做一次”误建成“每周一”。规则由目标（某人/全体）× 时间 × 模式组成；`ai` 在每次触发时结合实时数据生成内容，`message` 原文投递。业务政策仍完全存于数据，不硬编码作息；给他人/全体设置需要 `send_msg` 权限
- **临近截止**：任务截止前 24 小时提醒执行人；分配者改截止时间后重新生效
- **过期通知**：截止时间一过，通知执行人与分配者（各一次）
- **AI 催办**：过期任务每 48 小时无动静（无进度更新），AI 核实状态后向执行人发出个性化催办；有汇报就不打扰
- **催办升级**：同一任务累计催办 2 次仍无进度，通知分配者介入（调整/改期/改派）
- **每日待办**：`daily_summary_hour` 时刻给每个有待办的人推清单
- **老板日报**：同一时刻给超管推全局概览（进行中/过期/待验收/近24小时验收 + 过期任务点名）
- **AI 周报**：每周一同一时刻，AI 调 `company_overview` 等工具核实数据后，给每位超管写叙事周报
- **权限感知通用读取**：`query_data` 让 AI 自行发现并查询成员、身份、画像、权限、项目、任务、文件、日程、知识、目标、活动与审计等数据；数据库在检索前按调用者做行级/字段级裁剪，超管另有只读 SQL 兜底
- **月度人员盘点**：每月 1 号，AI 以超管身份基于任务履历更新成员画像草稿，并推送盘点摘要。所有周期批处理在首次运行时冻结当期成员快照；后续 tick 只重试该快照中的失败项，当期新增成员进入下个周期，避免开放重试窗口变成持续吸收新数据的常驻任务

### 系统事件总线（事件 → AI 决策）

领域事件不硬编码「通知谁、说什么」：员工通过邀请加入、AI 员工绑定上线、worker 提交任务待验收等事件统一进 `events` 总线，以事件相关人（邀请人 / 监护人 / 派活人）的身份跑一轮 AI——AI 结合该用户的会话上下文、行为规则与工具，自行决定通知措辞、要不要顺手行动（建任务/设提醒/记档案），不值得打扰就按约定词静默跳过。通知落在用户自己的会话里，接着对话就能直接处理（如「验收通过」）。AI 轮次失败时降级为事件原文推送；事件持久化并有限重试，最终失败保留在运行账本中。新事件源只需一行 `bus.Emit(类型, 相关人, 详情)`。

## 角色 / Skill

内置十一个开箱即用的工作模式（迁移种子，可改可删）：**CEO参谋、产品经理、开发工程师、测试工程师、前端工程师、运营经理、市场营销、销售顾问、财务顾问、HR招聘、UI设计师**。
角色定义遵循"身份 + 工作流程 SOP + 交付标准 + 质量红线"的结构（借鉴 Agency-Agents 方法论），并绑定系统工具（验收、负载查询、知识沉淀、提醒）——角色即工作流，不是人设扮演。
系统提示注入角色清单，AI 匹配场景时主动建议切换；`activate_role` 切换、`list_roles` 查看；超管可 `create_role` 增补。

## 知识与画像（越用越值钱）

- **知识库**：`save_knowledge` / `search_knowledge` 等工具全员可用；系统提示要求 AI 主动沉淀有复用价值的结论、回答公司事实前先检索
- **行为规则（Policy Memory）**：超管对 AI 提出持久要求时，Memory Miner 先抽取、再由独立治理子调用判断发布/待审/拒绝；冲突候选不会并排自动生效。少数 `pinned` 底线规则每轮常驻，其余规则按当前输入语义召回并校验作用域。知识、规则和 Skill 都支持 `set_knowledge_active` 可逆归档；归档后立即退出提示注入、搜索和 Qdrant 对账，原文、版本与审计仍保留。
- **情景记忆（Episodic Memory）**：每条有效非空聊天消息都建立 embedding，`search_history` 作为常驻只读能力交给 DeepAgent，是否检索、检索什么由 Agent 根据当前目标决定。授权聊天原文、单轮 Eino 执行日志和 Worker 主题会话在 PostgreSQL 中保持原值，保证 Agent/PTY 恢复时不丢参数和凭据；embedding、Memory Miner、学习候选和审计摘要使用独立的脱敏投影。旧 AI 答复不会被自动当成当前事实回灌；短确认仍会索引并携带相邻上下文供显式检索；历史控制文案或截断碎片只标记为不可回放，原始行仍保留审计。
- **知识代谢**：每月 2 号 AI 对当期冻结的学习候选快照做一次治理——合并重复、归档过期、点名冲突条目待裁决（冲突不擅自定夺）。批次可跨 tick 重试，但快照成员和最终汇报在本期保持稳定；月中新候选进入下期
- **成本计量**：每轮对话、压缩轮、worker 内置智能体的 token 用量全部落 `ai_usage` 表；超管用 `ai_usage_stats` 看今日/7天/30天总量与按人排行——每个 AI 员工花多少钱，账算得清
- **统一语义检索**（可选）：同时配置 `ai.embed_model` 与 `qdrant.url` 后，生效知识/规则/Skill、会话中可用于上下文的全部非空聊天、用户画像、项目、任务、文件元数据与正文分块、日程、决策和资料实体统一进入 Qdrant。`query_data(source="*")` 在这些来源间做语义与词法混合召回。Qdrant 只存向量、内容哈希、类型和稳定实体 ID，不复制正文；命中后必须回 PostgreSQL 按当前身份复核行与字段权限。所有 embedding 输入与路由 payload 都在统一边界脱敏。启动和周期对账按内容哈希只补缺失/变更记录，并清理已删除或归档实体；模型名、维度或固定探针的实际输出指纹变化时自动使用新的物理 collection，避免供应方同名换模后混用不兼容向量
- **文件正文索引**：上传请求只负责可靠落盘，后台持久队列再提取文本并按重叠窗口分块；PostgreSQL 正文提取与 Qdrant 向量写入分别记录状态和重试。TXT/CSV/JSON/源码和 DOCX/XLSX/PPTX/ODF 使用确定性提取；PDF 优先读取文本层，无文本层时受控 OCR，图片使用 `tesseract`（命令不可用时文件名和元数据仍可搜索，安装新提取器后会自动重试）。该流程限制 Office 解压规模与 OCR 页数，只建立搜索索引，不执行文件内容，也不会自动发布成知识、规则或 Skill
- **履历统计**：`get_user_stats` 输出某人的当前负载、验收通过数、按时率——派任务前的参考，也是画像的数据原料

## 脚本工具（让 nbco 长出新工具）

Prompt Skill 负责“什么时候想起某个流程、按什么步骤做”；脚本工具负责“把稳定的小计算/转换/格式化固化成可调用 tool”。超管可以用 `create_script_tool` / `test_script_tool` / `enable_script_tool` 创建 Starlark 脚本工具，启用后它会像内置工具一样进入 tool 列表，继续走权限裁剪、群聊高危过滤、参数归一和审计日志。Agent 工具调用没有项目自建的总数、单工具或重复参数次数配额。

脚本工具第一版只支持内嵌 **Starlark**：脚本必须定义 `run(args)`，`args` 是 JSON 对象，返回字符串、列表或字典。运行时无文件、无 shell、无网络、无数据库直连，并带执行步数和超时限制；适合值日表字段转换、报表格式化、规则计算、文本规范化这类可重复小工具。复杂 Python/Excel/PDF/爬虫/命令行工作交给 worker；Go 仍用于核心系统能力，不做主进程动态 Go 插件。

## 自主学习候选（Learning Pipeline）

nbco 不把每次模型归纳都直接混进不可见的系统提示，而是把可长期复用的结论沉淀成可治理资产：

- **学习候选**：`learning_candidates` 记录自动归纳出的 knowledge / rule / skill / script / profile / summary，带来源、证据、置信度、状态、审核人和权威类别。`memory_class` 将资产分为可长期复用的 `durable`、应回写业务表的 `canonical`、仅供历史检索的 `transient` 与待人工判断的 `unclassified`
- **对话学习**：Memory Miner 只消费用户真实输入和已验证工具结果，不消费 Gateway 为执行而补充的文件路径、控制指令或模型答复。它从证据中抽取规则、skill、知识，再由独立治理子调用复核权威类别和是否过度泛化；只有双方一致判定为 `durable` 的候选才可自动进入长期资产。员工档案、任务、项目、日程等 `canonical` 主数据必须由对应领域工具维护，短期事件留在聊天与活动索引中
- **worker 学习**：worker 完成资料分析任务时，可在汇报末尾输出 `NBCO_LEARNING_CANDIDATES_JSON:`，nbco 会解析为学习候选
- **治理评分**：`score_learning_candidates` 会给候选计算 `value_score`，并只在同一 `memory_class` 内判断重复/冲突；调度器月度治理和 Web 学习页会自动触发一次轻量评分
- **审核发布**：超管用 `list_learning_candidates` 查看，用 `approve_learning_candidate` 发布 `durable` 知识/规则/Skill，用 `classify_learning_candidate` 把误入长期资产的主数据或短期事件可逆归档。状态机禁止已拒绝候选被重新发布，也禁止未分类候选绕过权威判断
- **版本回滚**：知识/规则/Skill 更新前会写入 `knowledge_versions`；误改后用 `list_knowledge_versions` / `rollback_knowledge` 恢复
- **资料实体库**：worker 资料分析可同时输出客户、项目、合同、制度、联系人等结构化实体，入 `material_entities`，再由 `list_material_entities` 检索
- **对话回归用例**：`create_eval_case` / `list_eval_cases` 保存格式、隐私、工具纪律等红线用例，作为后续自动评测入口

这层是“智能学习”的治理面：长期规则、执行方法、公司事实可以越来越多；业务检索层只召回有权限的少量 skill 元数据，再由 Eino skill middleware 让主 Agent 判断是否加载完整内容，不靠无限拉长系统提示，也不额外运行一轮 Skill Router。

## 公司资料分析与 worker 归属

普通 worker 默认仍是最小权限白名单，只能干活、汇报、沉淀知识。超管可以把可信工作机上的 worker 设置为 **admin worker**：

- `set_worker_admin(worker_id, true)`：将指定 worker 提升为系统级 worker，工具能力等同超管；用于 nbco 自升级、资料入库、维护任务
- `set_worker_admin(worker_id, false)`：撤销系统级能力，回到普通 worker 最小权限
- worker 仍然绑定 `owner_id` 监护人：普通用户只能管理自己名下 worker；超管可管理全部。admin worker 的设置权只给超管
- `analyze_company_materials`：把 `/api/files` 上传得到的系统文件 ID 派给**发起人名下的 worker**，创建 “Company Intelligence Inbox” 任务；worker 读取 PDF/XLSX/TXT/图片后输出结构化学习候选，nbco 解析入 `learning_candidates`
- 自动选择严格按发起人归属：谁安排资料分析，就调用谁名下 worker；超管默认也用自己名下 worker，只有显式指定 `worker_id` 时才会调别的 worker

### 文件输入缓冲

Telegram 私聊收到 PDF/XLSX/TXT/图片/视频等文件时，nbco 会先下载到统一文件库，生成系统 `file_id`：

- 纯文件消息没有文字说明时，只暂存文件并等待用户下一步指令，不主动分析
- 文件消息带说明文字时，本轮上下文会包含刚上传的系统 `file_id`
- 后续用户说“这几个文件/刚才那两个附件”时，每轮系统提示会注入最近 24 小时的上传文件摘要；AI 也可用 `list_recent_files` 精确查看队列
- 需要读取文件内容、抽表格、识别图片或跨文件归纳时，AI 再调用 `analyze_company_materials`，由发起人名下 worker 下载任务附件并处理
- 简单文字事实不派 worker，直接走 `save_knowledge` / 信息字段 / 任务工具
- `send_file` 可把 nbco 文件库里的文件发送回 Telegram 用户；发给自己只需能访问该文件，发给别人需要 `send_msg` 权限。worker 产物、整理后的 XLSX/PDF/TXT 可通过这个工具交付给用户

## 脚本 SDK

Starlark 脚本工具默认仍是受限纯逻辑运行时，但可以通过两个受控 builtin 使用 nbco 能力：

- `nbco_tool(name, args={...})`：调用当前用户有权使用的 nbco tool，所有权限裁剪、目标校验、审计记录照常生效；脚本不能递归调用自己
- `nbco_ai(prompt)`：发起一个无工具、短时限的 AI 子调用，适合模糊分类、轻量总结、字段判断；复杂执行仍应调用 worker 或正式工具

权限继承调用者：谁调用脚本，脚本里的 `nbco_tool` / `nbco_ai` 就以谁的身份运行。脚本没有数据库、文件系统、网络、shell 直通能力；要访问系统状态必须走 tool。

## 权限体系

**主动权限**（存在操作者身上：我能做什么、作用于谁）：`write_profile` / `view_self_intro` / `manage_perm` / `send_msg` / `create_project` / `edit_info` / `manage_mandatory_schedule` 使用稳定用户 ID 或 `_all`；`manage_telegram_group` 使用稳定 `group_ref` 或 `_all`；`generate_key`（兼容别名 `invite_employee`）/ `manage_worker` 属于系统或自有资源能力，目标使用 `_all`，具体 Worker 归属在执行时重新校验。`list_permission_actions` 可读取当前完整能力词表和作用域，不需要从角色名称猜权限。

**被动权限**（存在被操作者身上：谁能对我做什么）：`view_profile:<作者ID>` / `view_profile:_all`。

规则：角色/Skill 只决定 AI 的工作方式，不携带权限；权限是以 `granted_by → user_id + action + target` 表示的能力委托边。超管建立授权根；非超管必须同时拥有对“被管理人”的 `manage_perm` 和准备转授的能力，且目标范围不能放大、不能管理超级管理员。同一能力可以来自多个独立上级；读取和执行只认可从当前有效超管根可追溯的委托链。超管可撤全部来源，普通管理者只能撤自己或其管理链下级签发的来源，不能覆盖同级或更高层授权；撤销上游后会永久清理失去全部来源的下游授权，仍有另一条有效来源则保留。停用身份只暂停整条链，重新启用后恢复；明确撤权、Admin Worker 降级或吊销才永久清理失效链。授权、撤权和身份状态切换使用同一数据库图锁，工具层先验检查不能被并发变更绕过。

例如：超管可授 CEO `manage_perm:_all` 与 `generate_key:_all`，CEO 再把邀请能力授给下属；两个组长 A/B 如果没有彼此的 `manage_perm`，即使都能邀请员工，也不能互相改权限。任务分配不会再隐式产生永久员工资料权限；任务、附件、参与者和 Worker claim 使用任务级访问关系，长期能力必须显式授权。已创建的业务对象保留审计身份，但每次后续变更仍按当前权限检查：例如失去 `manage_mandatory_schedule` 后不能继续修改仍为 mandatory 的日程，但可以把它降为 optional 或取消。

### 权限 → 工具矩阵（装配期裁剪）

工具集在组装时就按权限裁剪（`tools.toolPerm` 是工具到能力的事实来源，`perm.ActiveActionDefinitions` 是能力词表的事实来源）：没有对应权限的用户**看不到**该工具，而不是调用时才被拒；handler 和存储层仍保留目标级、所有权和委托链校验（有能力 ≠ 对任意目标都行）。`list_capabilities` 还会返回当前能力的 `granted_targets`，供 Agent 规划时理解真实边界。

| 所需权限 | 解锁的工具 |
|----------|-----------|
| （人人可用） | 自我视角全部：我的任务/项目/验收、清单、进度、画像自助、知识库、角色、提醒、Access Token、list_workers、转授自有权限 |
| `create_project` | `create_project` / `assign_task` / `delegate_review`（派活三件套） |
| `generate_key` | `invite_employee` |
| `send_msg` | `send_message` |
| `edit_info` | `update_user_info` |
| `write_profile` | `save_infos_on_user` |
| `manage_perm` | `grant_passive_perm` / `revoke_passive_perm` / `view_user_perms` |
| `manage_worker` | `create_worker` / `issue_worker_bind_code` / `delegate_worker_agent` / `run_worker_command` / `revoke_worker`（非超管仅限自己名下的 worker，创建者即监护人） |
| `manage_mandatory_schedule` | 允许在 `schedule_once_push` / `schedule_recurring_push` / `update_schedule` 中，对授权范围内的接收者设置 `recipient_policy=mandatory`；仍需相应 `send_msg` 权限 |
| 超管 | `company_overview`、信息字段管理、用户启停、角色管理、行为规则 `save_rule` / `list_rules` / `set_rule_pinned`、成本统计 `ai_usage_stats`、底层兜底 `low_level_db_query` / `low_level_db_exec`、Telegram 群控制 |

**普通 worker 机器账号**只拿白名单最小集（干活与沉淀知识：我的任务、进度、清单、知识库），即使其令牌访问 `/api/chat`、`/mcp` 也无法越权。显式设为 admin 的 Worker 是受审计的系统执行身份，仍只有其 owner 或超管能向它派发；每次执行保留 `requested_by`、任务/文件范围和 Worker 身份，不能靠改名或角色提示词获得权限。

## 测试

```bash
go test ./...    # 纯单测；store 集成测试默认跳过
go vet ./...

# 跑 store 集成测试（需要 PostgreSQL，CI 自动跑）：
NBCO_TEST_PG_DSN='postgres://nbco:nbco@127.0.0.1:5432/nbco_test?sslmode=disable' go test ./store/
```

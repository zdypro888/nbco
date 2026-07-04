# nbco — AI 公司运营中枢

让几十人规模、没有职业中层的公司，靠 AI 运转起来。AI 是每个员工的直属经理 + 老板的参谋部；IM、Web 都只是它的接口。规划见 [PLAN.md](PLAN.md)。

Go 单二进制：Telegram 网关、HTTP API/MCP、AI 引擎、定时调度跑在一个进程里；进程完全无状态，所有运行状态（用户、权限、任务、会话、绑定 Key、定时任务、审计）落 PostgreSQL，随时可杀可重启。

## 架构

```
┌─ 入口层（皆可换）────────────────────────────┐
│  gateway/telegram  gateway/httpapi(Web+REST+MCP)│
├─ 编排层 ─────────────────────────────────────┤
│  chat（会话落库·系统提示·引擎调度）       │
│  sched（DB 驱动定时·截止提醒·每日汇总/日报）│
├─ AI 引擎（可换）─────────────────────────────┤
│  ai.Engine 接口                               │
│   └─ einoengine：eino ADK 直调 API（claude/openai）│
├─ 领域层 ─────────────────────────────────────┤
│  tools（工具即权限边界·全量审计）         │
│  perm（双维度权限纯逻辑·单测覆盖）        │
├─ 存储层 ─────────────────────────────────────┤
│  store（pgx·内嵌迁移）→ PostgreSQL       │
└──────────────────────────────────────────────┘
```

核心设计：

- **自有 Tool 抽象**（`ai.Tool`：名称 + JSON Schema + handler），不绑任何框架。eino、对外 MCP、HTTP API 都是同一套工具的薄适配。
- **中枢只走 API 引擎**：`eino` 引擎直调模型 API（客户自带 key 的产品路径）。本机 CLI 只允许由 `nbco-worker` 通过交互式 PTY 驱动，严禁 `claude -p` / `codex exec` 这类 headless 入口。
- **工具即权限边界**：每个工具 handler 内部做权限校验（超管专属工具只组装给超管），每次调用写审计日志。
- **分渠道排版**：系统提示按会话渠道注入格式指引——Telegram 用其 HTML 子集（粗体/代码/引用）+ emoji，网关先按 HTML 发送、格式非法自动降级纯文本；Web/API 输出纯文本。

## 配置

复制 `nbco.json.example` 为 `nbco.json`：

| 字段 | 说明 |
|------|------|
| `telegram_token` | Bot token；可留空，留空则不启动 Telegram 网关，HTTP/API/MCP/worker 仍可用 |
| `superadmins` | Telegram 用户 ID 列表（启用 Telegram 时可留空：全新系统里第一个对 bot 发 `/superadmin` 的人自动成为超管） |
| `postgres_dsn` | PostgreSQL 连接串（首次启动自动建表） |
| `listen` | HTTP 监听地址，默认 `127.0.0.1:8900` |
| `log_level` | `debug` / `info` / `warn` / `error`，默认 `info`（debug 会记录消息与工具调用全文） |
| `file_store_path` | 文件存储目录，默认 `files`；相对路径按进程工作目录解释 |
| `public_base_url` | 保留给外部回调集成，通常留空 |
| `timezone` | IANA 时区，默认 `Asia/Shanghai` |
| `daily_summary_hour` | 每日待办推送小时（0-23），-1 关闭 |
| `sched_ai_concurrency` | 调度器同时进行的 AI 轮次上限（催办/周报/定时 AI 推送），默认 4；防「全员问候」几百轮齐发打爆后端 |
| `mcp_servers` | 外接 MCP 工具服务列表（`name`/`url`/`headers`），可选 |
| `ai.engine` | 仅支持 `eino`（直调 API） |
| `ai.provider` | eino 引擎：`claude` 或 `openai`（兼容网关） |
| `ai.api_key` / `ai.model` | eino 引擎必填 |
| `ai.stream_reasoning` | 是否在流式回复阶段展示模型推理内容，默认 `false`；超管可通过对话修改，运行时设置优先于配置文件默认值 |
| `ai.embed_model` | 语义检索的 embedding 模型（可选）；空=知识检索走词法。指向 OpenAI 兼容 embeddings 端点 |
| `ai.embed_base_url` / `ai.embed_api_key` | embedding 端点地址/密钥（空则回退 `ai.base_url` / `ai.api_key`） |

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

认证一律 `Authorization: Bearer <token>`。全新系统且没有 Telegram 时，先调一次：

```bash
curl -X POST http://<listen>/api/bootstrap \
  -H 'Content-Type: application/json' \
  -d '{"name":"老板"}'
```

该接口仅在系统没有活跃超管时可用，会返回首任超管和首个 API token；已有超管后再调用会返回 `409`。已有账号可在 TG/Web 对话里让 AI 调 `generate_api_token` 重新生成自己的 token。

常用接口：

- `POST /api/chat` `{"message":"..."}` → `{"reply":"..."}` — 与 TG 同一编排器，独立会话
- `GET /api/me` — 当前用户
- `GET /api/me/tasks` / `GET /api/me/review` / `GET /api/me/assigned` — 待办 / 待我验收 / 我分配的
- `GET /api/overview` — 全局统计+项目+过期任务（超管）
- `POST /api/files`（multipart `file`，最大 200MB）/ `GET /api/files/{id}` — 上传/下载文件（按权限校验）
- `POST /api/tasks/{id}/attachments` `{"file_id":123,"caption":"..."}` — 把文件挂到任务
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
worker 是独立工作代理，不依赖 Telegram；Telegram、Web、HTTP API、MCP 都只是给中枢创建任务和查看结果的入口。文件与产物闭环规划见 [docs/worker-roadmap.md](docs/worker-roadmap.md)。

本机 LaunchAgent 部署用统一脚本，避免 repo 内二进制/配置与实际运行路径漂移：

```bash
scripts/deploy-local.sh
```

脚本会构建 `nbco` / `nbco-worker`，同步到 `~/.local/bin`，复制 `nbco.json` 到 `~/Library/Application Support/nbco/`，重启 `com.zdypro.nbco` 并检查 `/healthz`。

```bash
~/.local/bin/nbco-worker bind http://127.0.0.1:8900 <create_worker 返回的一次性令牌>
~/.local/bin/nbco-worker run [-engine claude|codex] [-bin /path/to/cli]
```

> **隔离建议（安全边界在部署侧）**：worker 用 `--dangerously-skip-permissions` 跑 CLI，模型有完整 shell，能读到 worker 账号可读的一切（包括自身接入令牌）。产物上传做了纵深加固（拒软/硬链接、非常规文件），但那不是安全边界——真正的隔离靠部署：**每个 worker 跑在独立容器 / 低权限账号里**，把宿主机密（别的 worker 令牌、SSH 私钥等）挡在其可达范围外。

执行规则：worker 只能启动 `claude` / `codex` 的**交互式 PTY**，像人在终端里操作一样干活；严禁 `claude -p` / `codex exec` 等 headless 入口。驱动手法（借鉴 [aibridge](https://github.com/zdypro888/aibridge)）：

- **vt10x 屏幕仿真**：PTY 字节流喂进内存终端仿真器，一切检测读渲染后的屏幕，不在原始流上扒 ANSI
- **两步投递**：多行任务用 bracketed paste 包住、停顿后单发回车（防 TUI 的 paste 防抖吞掉提交）
- **忙碌感知等待**：屏幕先动后稳判定回合结束；"esc to interrupt" 可见期间永不判闲也永不判卡，长任务不设硬超时
- **启动对话框自动应答**：Bypass Permissions 确认（选 Yes）、目录 trust 确认
- **收尾解析防回显**：从最后一个哨兵块回溯、跳过任务原文的回显；没按格式收尾会补提醒，仍失败则以屏幕摘录提交
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

worker 领取任务时，服务端会把任务附件下发到工作目录的 `attachments/`；prompt 会明确告诉 CLI 附件位置。worker 若需要交付文件，把文件放进 `artifacts/`，提交前会自动上传为任务产物，验收人可在任务详情里看到文件 ID 与名称。

### 分层铁律：中枢调度，worker 深干

中枢（eino 直调 API）是**调度管理层**：派活、跟进、催办、汇总，不在对话里做深度工作。要深度解决问题（写代码、审代码、深度调研）就派任务给 AI 员工，worker 驱动本机 claude/codex 完成。

**审核委派**：任务提交待验收后，分配者让中枢调 `delegate_review`——服务端把任务要素、执行过程、完成汇报打包成审核简报，作为高优先级任务派给审核角色（推荐 AI 员工，实地读代码、跑测试、逐条对照验收标准），结论（「建议通过」/「建议打回：理由」）随完成汇报回流，分配者只做最终拍板。

## 群聊（Telegram）

bot 可拉进群，交互按场景收敛（命令菜单按作用域注册：私聊菜单 /start /new，群菜单 /listen /new，互不串场）：

- **默认只应答点名**：@bot 或回复 bot 的消息才触发 AI，以**发言人的权限**跑**群共享会话**（历史带【发言人】署名），未绑定成员会被引导私聊绑定——群里绝不做绑定流程
- **/listen 群监听**（超管）：开启后 bot 旁听群讨论记入群会话上下文（不插话），再被 @ 时能接住前文；再次 /listen 关闭。旁听普通消息需在 @BotFather `/setprivacy` 选 Disable
- **/new**（超管）：重置本群会话
- 系统提示注入群纪律：回复全员可见，涉及隐私（画像/Token/私人任务）引导私聊，不主动插话

## 会话上下文压缩（eino 的"auto-compact"）

eino 直连 API 没有 CLI 那种自动压缩，中枢自建**滚动摘要**：未折叠消息达到阈值（30 条或 16KB）时，后台把「除最近 12 条外」的消息连同既有摘要压缩成新摘要存进会话（`summary` / `summary_upto`）；每轮重放 = 摘要 + 位点后消息。早期的决定、承诺不再随硬截断（40 条上限）静默丢失。压缩轮次无工具、异步执行，不增加用户等待；失败下轮重试。

## 主动运营（AI 主动，人被动）

调度器每 30 秒扫库，发送标记落库（原子认领），重启不重发。两级主动性——模板消息（确定性）与 **AI 轮次**（调度器把系统指令注入用户会话跑一轮引擎，产出个性化内容后推送）。

**资源模型**：每次 tick 只做几条**命中部分索引**的原子认领查询（`WHERE fire_at <= now()` / `deadline` / kv 日期），成本随「到期数」而非「任务总数」增长——库里堆多少未来任务都不扫。重活（AI 轮次、逐人推送）**在认领后派发到限并发协程池异步执行**，既不阻塞 30 秒节拍（截止提醒照常及时），又用 `sched_ai_concurrency`（默认 4）护住模型网关：全员 AI 问候不会几百轮齐发，而是限并发滚动完成。模板推送另走 16 并发池。

- **定时提醒**：用户让 AI 设置的单次/循环提醒（`schedule_once` / `schedule_repeating`）
- **动态运营节奏（`schedule_push`）**：管理者一句话（如"我们10点上班6:30下班，上下班问候一下大家"），AI 自己落成定时规则：目标（某人/全体）× 时间（每天 HH:MM，可限工作日）× 模式（`ai`=每次触发为每位目标现场跑一轮 AI，结合其当天待办等真实数据生成个性化内容；`message`=原文投递）。**代码里没有任何"作息"概念**——政策全在数据行里，说句话就能改；给他人/全体设置需要 `send_msg` 权限
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
- **语义检索**（可选）：配 `ai.embed_model`（指向任意 OpenAI 兼容 embeddings 端点，如自建本地 embedding 服务；`embed_base_url`/`embed_api_key` 空则回退主引擎的）后，知识检索走「语义（cosine）+ 词法」混合召回，措辞不同也能命中；worker 领活时也据任务标题+描述语义召回相关经验。存知识时自动向量化，启动时后台回填存量。**未配则优雅回退到改进版词法检索**（多词打分 + 标签 + 近因），零外部依赖。向量存 `real[]`，nbco 规模下应用层暴力 cosine 足够，无需 pgvector 扩展
- **履历统计**：`get_user_stats` 输出某人的当前负载、验收通过数、按时率——派任务前的参考，也是画像的数据原料

## 权限体系

**主动权限**（存在操作者身上：我能对谁做什么）：`write_profile` / `view_self_intro` / `manage_perm` / `generate_key` / `send_msg` / `create_project` / `edit_info`，目标为用户 ID 或 `_all`。

**被动权限**（存在被操作者身上：谁能对我做什么）：`view_profile:<作者ID>` / `view_profile:_all`。

规则：超管旁路；非超管只能转授自己拥有且范围不超过自己的权限；派任务时执行人继承分配者的 `view_self_intro` 范围。判定逻辑在 `perm`（纯函数，有单测）。

### 权限 → 工具矩阵（装配期裁剪）

工具集在组装时就按权限裁剪（`tools.toolPerm` 注册表是单一事实来源）：没有对应权限的用户**看不到**该工具，而不是调用时才被拒；handler 内仍保留目标级校验（有能力 ≠ 对任意目标都行），双层防御。

| 所需权限 | 解锁的工具 |
|----------|-----------|
| （人人可用） | 自我视角全部：我的任务/项目/验收、清单、进度、画像自助、知识库、角色、提醒、API Token、list_workers、转授自有权限 |
| `create_project` | `create_project` / `assign_task` / `delegate_review`（派活三件套） |
| `generate_key` | `generate_key` |
| `send_msg` | `send_message` |
| `edit_info` | `update_user_info` |
| `write_profile` | `save_infos_on_user` |
| `manage_perm` | `grant_passive_perm` / `revoke_passive_perm` / `view_user_perms` |
| 超管 | `company_overview`、信息字段管理、用户启停、角色管理、`create_worker` / `revoke_worker` |

**worker 机器账号**只拿白名单最小集（干活与沉淀知识：我的任务、进度、清单、知识库），即使其令牌访问 `/api/chat`、`/mcp` 也无法越权。

## 测试

```bash
go test ./...    # 纯单测；store 集成测试默认跳过
go vet ./...

# 跑 store 集成测试（需要 PostgreSQL，CI 自动跑）：
NBCO_TEST_PG_DSN='postgres://nbco:nbco@127.0.0.1:5432/nbco_test?sslmode=disable' go test ./store/
```

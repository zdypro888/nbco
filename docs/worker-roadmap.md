# Worker Roadmap

`nbco-worker` 是安装在工作机上的 AI 员工客户端。它是独立能力，不依赖 Telegram：Telegram、Web、HTTP API、MCP 都只是给中枢创建任务和查看结果的入口；worker 只依赖 nbco 的 HTTP/WS worker API。

## 定位

- 中枢 `nbco`：任务、权限、审计、文件、通知、调度、知识与画像的唯一事实源。
- `nbco-worker`：受控工作代理，领取被分配给自己的任务，在本机工作目录内用交互式 PTY 驱动 `claude` / `codex` 完成深度工作。
- 真实员工：可以不安装 worker，照常通过 Telegram/Web/API 接任务、汇报、验收。
- AI 员工：worker 用户是一种特殊员工账号，复用同一套任务流转、进度、验收、返工、催办和审计机制。

## 已有能力

- `bind` 写入本机配置，Worker 接入 Token 与服务端 worker 用户绑定；它不同于真人员工一次性邀请。
- `run` 后通过 `/api/worker/next` 原子认领任务，数据库是唯一任务队列。
- WebSocket 增强：`wake` 秒级领活、`cancel` 终止当前任务、`ping/pong` 在线状态。
- 只用交互式 PTY 驱动 CLI，禁止 `claude -p` / `codex exec`。
- vt10x 屏幕仿真、两步投递、忙碌感知等待、屏幕快照进度、结构化收尾。
- 显式命令任务：超管用 `run_worker_command` 派发命令，worker 在任务工作目录里默认用 stdout/stderr pipe 执行系统 shell；需要终端行为时可显式 `pty=true`，输出回传进度并进入验收。
- 验收打回后自动重新领取，带上历史过程和打回理由返工。
- HTTP/API 文件上传下载、任务附件挂载、worker 附件下载到 `attachments/`、worker 产物从 `artifacts/` 上传。
- Telegram 网关可选；未配置 `telegram_token` 时，HTTP/API/MCP/worker 仍可运行。

## 核心缺口

1. Telegram 文件适配还没打通：Bot API 文件下载、自动挂任务、发回产物还待实现。
2. Web UI 还没有文件上传/下载控件；HTTP API 已有。
3. worker 缺少本机可观测命令：状态、当前任务、工作目录、最近日志、配置检查。
4. 安装分发还粗糙：目前依赖手工构建/复制，没有平台包、校验和、升级策略。
5. workspace 边界还不够产品化：当前默认 `~/nbco-work/task-<id>`，需要显式配置、展示和检查。

## 文件链路

目标链路：

```
用户/系统上传文件
  -> nbco 文件存储
  -> 绑定到会话/任务/进度/结果
  -> worker 领取任务时下载到工作目录
  -> CLI 在 PTY 中处理本地文件
  -> worker 上传产物
  -> nbco 通知验收人，可通过 Telegram/Web 下载
```

服务端需要新增统一文件模型：

- `files`：文件元数据，含来源、原文件名、MIME、大小、sha256、存储路径、创建人。
- `task_attachments`：任务附件关系，引用 `files.id`，保留 caption、来源消息。
- `task_artifacts`：worker 完成交付物，引用 `files.id`，属于某次进度或提交。

接口建议：

- `POST /api/files`：Web/API 上传文件。
- `GET /api/files/{id}`：下载文件，按权限校验。
- `POST /api/tasks/{id}/attachments`：把文件挂到任务。
- `GET /api/worker/files/{id}?task_id=...&claim_id=...`：worker 用当前 claim 下载被授权任务文件。
- `POST /api/worker/artifacts`：worker 上传产物并绑定当前 claim。
- `/api/worker/next`：返回附件列表和下载 URL。

Telegram 入口只做适配：

- 收到 document/photo/video 后调用 Bot API `GetFile` 下载到 nbco 文件存储。
- 若消息上下文能定位任务，自动挂附件；否则把文件作为会话文件，让 AI 决定挂到哪个任务或创建任务。
- 通知验收时支持发送 artifact 链接；需要时再补 `sendDocument`。

worker 侧：

- 领取任务后创建：
  - `attachments/`：服务端下发文件。
  - `artifacts/`：worker 要回传的产物。
  - `task.md`：任务、验收标准、附件清单、历史记录。
- prompt 明确告诉 CLI：所有输入文件在 `attachments/`，交付物放到 `artifacts/`。
- 完成前扫描 `artifacts/`，上传新增文件，summary 中列出产物。

## 独立运行

worker 脱离 Telegram 应满足：

- 管理员可通过 Web/API/MCP 创建 worker、生成绑定 token、分配任务。
- worker 只通过 HTTP/WS 与 nbco 通信，不调用 Telegram。
- 任务、附件、进度、产物、验收都可以通过 Web/API 完成。
- Telegram 只是通知和对话入口之一；关掉 Telegram 后，worker 队列仍继续跑。

## 真实员工与 AI 员工协同

- 同一个任务系统，不分两套流程。
- 真实员工可以把任务分给 AI 员工，也可以把自己的任务拆一部分给 AI 员工。
- AI 员工提交后仍进验收队列，由分配者或委派审核人拍板。
- 真实员工可以上传补充文件、打回理由和验收意见，worker 返工时自动带上。

## 非木马边界

worker 是明示安装、明示绑定、明示运行的工作代理：

- 不做隐蔽安装，不伪装系统进程。
- 不静默自更新，不从 Telegram 下发可执行文件后自动运行。
- 不扫全盘；默认只在配置的 workspace 与当前 task 目录内工作。
- 不绕过 nbco 审计；任务、进度、取消、产物上传都落库。
- 不开放常驻任意远程 shell；执行入口是任务队列里的显式任务：`claude` / `codex` 交互式 PTY，或超管创建的 `run_worker_command` 一次性命令任务（默认 pipe，可选 PTY，同样走审计、验收）。
- 不把用户本机密钥、环境变量、任意文件主动上传；只有 worker 产物目录和显式附件参与传输。

## 实施顺序

1. [x] 文件模型与本地存储：`files` 表、存储目录、sha256、权限校验、下载接口。
2. [x] 任务附件下发：`/api/worker/next` 返回附件，worker 下载到 `attachments/`。
3. [x] worker 产物上传：`artifacts/` 扫描、上传接口、验收通知展示。
4. [x] 显式命令任务：`run_worker_command` → `worker_command` → worker pipe/可选 PTY 执行并回传。
5. [ ] Web UI 文件入口：网页上传/下载，脱离 Telegram 完整可用。
6. [ ] Telegram 文件适配：Bot API 下载上传文件、必要时发 artifact。
7. [ ] worker 运维命令：`status`、`doctor`、`logs`、`workspace`、`once`。
8. [ ] 分发安装：release 二进制、校验和、平台安装脚本；仍由用户显式执行。

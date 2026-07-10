# nbco System Map

本文档是 nbco 的能力地图。它不替代 README，而是把当前系统按稳定业务域组织起来，避免功能继续散落成一堆工具。

## 北极星

nbco 是 AI 公司运营中枢：入口可换，运行状态和组织记忆沉淀在中枢。目标是让老板只下目标和做决策，员工只对话和交付，AI 主动完成拆解、派发、催办、总结、学习和治理。

## 七个业务域

### 1. People：人员、身份、画像、权限

职责：

- 管理真人员工、AI worker、超级管理员。
- 管理 Telegram 身份绑定、HTTP/MCP Access Token、真人一次性邀请、worker 绑定码。
- 管理基本信息字段、自我介绍、他人评价、画像可见性。
- 管理资料收集活动，分别跟踪字段齐全度、通知覆盖和完成进度。
- 管理主动权限和被动权限，支持上级授权和转授权。
- 管理组织组/项目组/部门成员关系。

关键工具：

- `list_users`
- `get_user_info`
- `update_user_info`
- `bulk_update_user_info`
- `create_data_collection_campaign`
- `list_data_collection_campaigns`
- `get_data_collection_campaign`
- `send_data_collection_reminder`
- `invite_employee`
- `grant_active_perm`
- `revoke_active_perm`
- `grant_passive_perm`
- `revoke_passive_perm`
- `view_user_perms`
- `create_org_group`
- `add_org_group_member`

边界：

- 不做 HR 薪酬/考勤系统。
- 不向用户展示内部 user_id、Telegram ID、token hash 等系统细节。
- 内部 ID/ref 是模型工作内存和工具参数，不是能力限制；最终展示由 `textfmt.SanitizeVisibleReply` 与渠道格式化层清理。
- 真人邀请、用户 Access Token、worker 绑定码、Worker Access Token 必须严格区分。
- 资料收集活动是专项追踪基线；没有活动不能证明员工没有自行更新，实际发生记录以 `audit_log` 为准。
- `pending` 只表示字段仍缺失，`notified` 只表示消息已投递。资料更新会刷新完成率，但重复提醒和主动汇报必须由显式提醒或定时规则驱动。

动作闭环：

- 主 agent 直接基于可见工具执行，不再先跑额外规划模型，也不把“候选工具”塞回主 prompt 当硬约束。
- 编排器不根据自然语言措辞裁决或改写业务回复；模型协议错误和输出截断可以恢复，但“是否完成”不能靠关键词门控。
- `action_turns` 是非阻断的事后观测账本：按用户输入和实际工具调用记录摘要、候选工具、工具成功数、证据 JSON 和 outcome；它不参与当前回复决策。
- `audit_log` 是所有工具调用的底层事实流水，超级管理员可通过 `list_system_activity` 按人员、会话、工具、时间和文字查询，不依赖是否创建了专项任务或活动。

### 2. Work：目标、项目、任务、验收、决策

职责：

- 把战略目标拆成里程碑，再拆成项目任务。
- 支持任务树、依赖、拆分、改派、进度、清单、验收和打回。
- 汇总老板/负责人决策队列：待验收、过期、阻塞、孤儿任务。
- 通过事件总线让任务变化触发 AI 分析和通知。

关键工具：

- `create_goal`
- `add_milestone`
- `decompose_milestone`
- `create_project`
- `assign_task`
- `split_my_task`
- `reassign_task`
- `accept_task`
- `reject_task`
- `delegate_review`
- `refresh_decision_queue`
- `list_decision_queue`
- `company_overview`

边界：

- 单一明确执行项直接派任务。
- 复杂、多依赖、多人并行的工作先拆分。
- 已提交任务需要深度核查时委派审核，不在普通对话里“脑补验收”。

### 3. Workers：AI 员工和工作机执行

职责：

- 把可信工作机变成 AI worker。
- worker 通过 HTTP/WS 领取任务、回传进度、上传产物。
- CLI 自动执行只能用交互式 PTY；显式命令任务默认用 pipe，需要终端行为才用 PTY。
- 支持 worker 能力上报、主题 workspace、任务附件、返工产物回注。
- 支持 admin worker 执行系统级维护和资料入库任务。

关键工具：

- `list_workers`
- `create_worker`
- `issue_worker_bind_code`
- `run_worker_command`
- `revoke_worker`
- `set_worker_admin`
- `analyze_company_materials`

标准工作流：

- `material_intake`：资料分析入库。
- `nbco_upgrade`：单 worker 执行升级脚本、测试、部署、健康检查和回滚判断。

边界：

- worker 是明示安装、明示绑定、明示运行的工作代理，不做隐蔽远控。
- 不把所有任务塞进同一个 Claude/Codex 长会话；由中枢维护主题 workspace 和摘要。
- 生产升级必须保持在一个 worker 任务里，不拆成多个并发任务。

### 4. Memory：知识、规则、skill、历史、学习治理

职责：

- 管理事实知识、行为规则、可复用执行方法、历史对话检索。
- 从 worker lessons、聊天、资料分析中生成学习候选。
- 对候选做审核、去重、冲突评分、发布和回滚。
- 把相关 rule/skill/knowledge 按需注入系统提示，避免提示词无限膨胀。
- 同一轮知识、规则、skill 和历史检索复用查询向量；embedding 短时故障会限流并回退词法检索。

关键工具：

- `save_knowledge`
- `search_knowledge`
- `search_history`
- `save_rule`
- `list_rules`
- `set_rule_pinned`
- `save_skill`
- `search_skills`
- `load_skill`
- `propose_learning_candidate`
- `list_learning_candidates`
- `approve_learning_candidate`
- `reject_learning_candidate`
- `score_learning_candidates`
- `rollback_knowledge`

边界：

- fact/rule/skill/profile 不混用。
- 普通员工和 worker 的复用经验先进候选队列。
- 超管明确提出“以后/默认/记住/永远不要”这类行为约束时，走 rule。

### 5. Comms：Telegram、群、通知、文件

职责：

- Telegram 私聊和群聊入口。
- 群状态、群监听、成员可见信息、自动邀请、群消息管理。
- 文件上传、下载、任务附件、worker 产物、Telegram 文件收发。
- 定时提醒、循环提醒、AI 推送。

关键工具：

- `list_telegram_groups`
- `get_telegram_group`
- `list_telegram_group_members`
- `resolve_telegram_group_members`
- `set_telegram_group_listen`
- `set_telegram_group_auto_invite`
- `send_telegram_group_message`
- `edit_telegram_group_message`
- `delete_telegram_group_message`
- `pin_telegram_group_message`
- `list_recent_files`
- `send_file`
- `schedule_push`

边界：

- 群聊默认只响应点名；监听只记上下文，不主动插话。
- 群共享会话禁用 token、权限、worker 命令、规则修改等敏感工具。
- Telegram 格式只使用 Telegram HTML 子集；非法 HTML 自动降级纯文本。

### 6. Automation：工作流、脚本、MCP

职责：

- 把稳定流程固化成 workflow。
- 把纯计算/格式转换逻辑固化成 Starlark 脚本工具。
- 接入外部 MCP 工具，并统一走权限、审计、输出截断。

关键工具：

- `list_workflows`
- `start_workflow`
- `list_script_tools`
- `create_script_tool`
- `test_script_tool`
- `enable_script_tool`
- 外接 MCP tools

边界：

- workflow 编排业务过程，仍然创建标准任务或调用标准工具，不绕过审计。
- 脚本工具只做无文件、无网络、无 shell 的纯逻辑。
- 需要 shell、文件系统、PDF/XLSX、爬虫或长流程时派给 worker。

### 7. Ops：部署、健康、模型、成本、回归

职责：

- 管理 AI 模型设置、stream_reasoning、用量统计。
- 暴露 `/healthz`、`/version`、管理页运维状态。
- 监控 AI 引擎连续失败并告警超管。
- 管理部署升级脚本和 worker 发行物。
- 管理对话 eval case，逐步形成回归测试体系。

关键工具：

- `list_capabilities`
- `get_ai_settings`
- `set_ai_settings`
- `ai_usage_stats`
- `list_eval_cases`
- `create_eval_case`

边界：

- 中枢只用 API 引擎；CLI 自动执行只在 worker 里通过交互式 PTY。
- 部署脚本必须环境中立，部署环境差异放到配置或服务器本地环境变量。
- 健康检查必须能反映 DB 可用性，不能死 200。

## 能力注册中心

代码中的能力注册中心位于 `tools/capabilities.go`。它从真实工具集生成能力目录，并标注：

- `domain`：七个业务域之一。
- `required_action`：需要的主动权限或 `superadmin`。
- `risk`：`normal` / `sensitive` / `approval` / `admin`。
- `available`：当前用户/入口是否可用。
- `worker_allowed`：worker 机器账号是否可用。
- `group_allowed`：群共享会话是否可用。

AI 或管理员可用 `list_capabilities` 查询，而不是依赖手写 README 猜工具。

## 工作流模板

代码中的工作流模板位于 `tools/workflows.go`。

首批已实现：

- `material_intake`
  - 输入：`file_ids`、`instruction`、可选 `worker_id/title`。
  - 行为：创建 Company Intelligence Inbox 任务，挂载文件，派发起人名下 worker 分析。
  - 输出：学习候选和结构化实体进入后续审核。

- `nbco_upgrade`
  - 输入：可选 `worker_id/ref/repo_dir/title`，必须 `confirm=true`。
  - 行为：选择 admin worker，创建一个显式命令任务，执行 `scripts/upgrade-nbco.sh`。
  - 输出：测试、构建、部署、healthz 和回滚状态写入任务完成汇报。

新增 workflow 的规则：

1. 先判断它是否是可重复、可命名、可验收的流程。
2. workflow 只编排，不能绕过权限、审计、任务状态机。
3. 高风险 workflow 必须有显式确认参数或两段式审批。
4. workflow 文案和参数必须能被 `list_workflows` 清楚展示。

## 下一步系统化方向

优先级从高到低：

1. Web 管理台补齐文件上传/下载、worker 任务/日志、知识/规则/skill、群管理、eval run。
2. 对话 eval runner 真正执行 `conversation_eval_cases`，覆盖 Telegram 格式、隐私、工具选择和群权限。
3. Learning Controller：定期从聊天、worker lessons、资料分析中归纳候选，自动去重/冲突打分。
4. 权限模板：CEO、项目负责人、人事、财务、AI worker 监护人等角色化授权包。
5. Ops 中心：worker 离线、队列堆积、Telegram 失败、AI 成本异常、模型健康集中展示。

const root = document.querySelector("#root");
const brandName = String(root?.dataset.brandName || "nbco").trim() || "nbco";
const brandMark = Array.from(brandName)[0]?.toUpperCase() || "N";
const telegramWebApp = window.Telegram && window.Telegram.WebApp;
const tg = telegramWebApp && String(telegramWebApp.initData || "").trim() ? telegramWebApp : null;
const initialURLParams = new URLSearchParams(window.location.search);
const requestedView = initialURLParams.get("view");
const requestedWorkspacePage = normalizeWorkspacePage(initialURLParams.get("workspace_page"));
const knownViews = new Set(["command", "files", "tasks", "people", "workers", "learning", "workspace", "model", "ops", "chat"]);

const state = {
  me: null,
  route: knownViews.has(requestedView) ? requestedView : "command",
  workspacePage: requestedWorkspacePage,
  loading: false,
  notice: "",
  files: [],
  fileIntakes: [],
  materials: [],
  selectedFileIDs: new Set(),
  tasks: { todo: [], assigned: [], review: [] },
  taskQueue: [],
  taskReview: [],
  taskHistory: [],
  workerRuns: [],
  workerRunHistory: [],
  schedules: [],
  workers: [],
	users: [],
	learning: { candidates: [], asset_usage_30d: {}, asset_effectiveness_30d: [] },
	evals: { cases: [], runs: [], stats: {} },
  workflows: [],
  capabilities: [],
  decisions: [],
  approvals: [],
  actionTurns: [],
  ops: null,
  ai: null,
  selected: null,
  logs: [],
  chat: [
    { role: "sys", text: "控制中心和 Telegram 使用同一个中枢能力。复杂操作优先走队列和工作流，临时问题再用对话。" },
  ],
  actionResult: "",
  actionOK: true,
	search: null,
};

let memoryToken = "";
const storage = {
  get token() {
    try { return localStorage.getItem("nbco_token") || memoryToken; }
    catch (_) { return memoryToken; }
  },
  set token(value) {
    memoryToken = value || "";
    try {
      if (value) localStorage.setItem("nbco_token", value);
      else localStorage.removeItem("nbco_token");
    } catch (_) {
      // Some embedded webviews disable persistent storage; memory auth still works.
    }
  },
};
let useAccessToken = !tg;
let ihtmlWorkspace = null;
let ihtmlTicket = { userID: "", token: "", expiresAt: 0 };
let ihtmlTicketPromise = { userID: "", value: null };
let ihtmlMountGeneration = 0;

function finitePixels(value) {
  const number = Number(value);
  return Number.isFinite(number) && number > 0 ? number : 0;
}

function syncViewportMetrics() {
  const visualHeight = finitePixels(window.visualViewport?.height);
  const windowHeight = finitePixels(window.innerHeight);
  const telegramHeight = finitePixels(tg?.viewportHeight);
  const currentHeight = telegramHeight || visualHeight || windowHeight;
  const stableHeight = finitePixels(tg?.viewportStableHeight) || currentHeight;
  if (currentHeight) document.documentElement.style.setProperty("--app-viewport-height", `${currentHeight}px`);
  if (stableHeight) document.documentElement.style.setProperty("--app-stable-viewport-height", `${stableHeight}px`);

  if (!tg) return;
  const safe = tg.safeAreaInset || {};
  const contentSafe = tg.contentSafeAreaInset || {};
  for (const side of ["top", "right", "bottom", "left"]) {
    const inset = Math.max(finitePixels(safe[side]), finitePixels(contentSafe[side]));
    document.body.style.setProperty(`--app-safe-${side}`, `${inset}px`);
  }
}

syncViewportMetrics();
window.addEventListener("resize", syncViewportMetrics, { passive: true });
window.addEventListener("orientationchange", syncViewportMetrics, { passive: true });
window.visualViewport?.addEventListener("resize", syncViewportMetrics, { passive: true });

if (tg) {
  document.body.classList.add("tg-mini");
  for (const event of ["viewportChanged", "safeAreaChanged", "contentSafeAreaChanged"]) {
    tg.onEvent(event, syncViewportMetrics);
  }
  tg.ready();
  tg.expand();
  syncViewportMetrics();
}

function esc(value) {
  const span = document.createElement("span");
  span.textContent = value ?? "";
  return span.innerHTML;
}

function fmtTime(value) {
  if (!value) return "";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return "";
  return `${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")} ${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

function fmtClock() {
  const d = new Date();
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}:${String(d.getSeconds()).padStart(2, "0")}`;
}

function fmtBytes(n) {
  n = Number(n || 0);
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(1)} GB`;
}

function parseIDs(value) {
  return String(value || "")
    .split(/[,\s，、]+/)
    .map(x => Number(x.trim()))
    .filter(x => Number.isInteger(x) && x > 0);
}

function normalizeWorkspacePage(value) {
  const page = String(value || "").trim();
  return /^[a-z0-9][a-z0-9_-]{0,63}$/.test(page) ? page : "";
}

function syncBrowserLocation() {
  const next = new URL(window.location.href);
  next.searchParams.set("view", state.route);
  if (state.route === "workspace" && state.workspacePage) {
    next.searchParams.set("workspace_page", state.workspacePage);
  } else {
    next.searchParams.delete("workspace_page");
  }
  window.history.replaceState(window.history.state, "", next);
}

function truncate(value, max) {
  const s = String(value || "");
  return s.length > max ? s.slice(0, max - 1) + "…" : s;
}

function icon(name) {
  return `<i class="ti ti-${name}" aria-hidden="true"></i>`;
}

function addLog(source, level, message, status = "", duration = "") {
  state.logs.unshift({ time: fmtClock(), source, level, message, status, duration });
  state.logs = state.logs.slice(0, 80);
  renderLogsOnly();
}

function newRequestID() {
  if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID();
  const bytes = new Uint8Array(16);
  globalThis.crypto?.getRandomValues?.(bytes);
  const random = bytes.some(Boolean)
    ? Array.from(bytes, value => value.toString(16).padStart(2, "0")).join("")
    : `${Date.now().toString(36)}${Math.random().toString(36).slice(2)}`;
  return `web-${random}`;
}

async function api(path, opts = {}) {
  const started = performance.now();
  const headers = { ...(opts.headers || {}) };
	const method = String(opts.method || "GET").toUpperCase();
  if (tg && !useAccessToken) headers["X-Telegram-Init-Data"] = tg.initData;
  else headers.Authorization = "Bearer " + storage.token;
	if (method !== "GET" && method !== "HEAD" && !headers["Idempotency-Key"]) {
		headers["Idempotency-Key"] = newRequestID();
	}
  if (opts.body && !(opts.body instanceof FormData) && !Object.prototype.hasOwnProperty.call(headers, "Content-Type")) {
    headers["Content-Type"] = "application/json";
  }
  let res;
  try {
    res = await fetch(path, { ...opts, headers });
  } catch (err) {
    if (method === "GET" || method === "HEAD" || !headers["Idempotency-Key"]) throw err;
    res = await fetch(path, { ...opts, headers });
  }
  const duration = `${Math.max(1, Math.round(performance.now() - started))}ms`;
  const level = res.ok ? "INFO" : "WARN";
  addLog("http", level, `${method} ${path}`, String(res.status), duration);
  if (res.status === 401) {
    storage.token = "";
    state.me = null;
    renderLogin("登录已失效，请重新输入 Token。");
    throw new Error("未认证");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
  return data;
}

function renderLogin(error = "") {
  disposeIHTMLWorkspace();
  resetIHTMLAuth();
  const hint = tg
    ? "Telegram 自动登录未成功，也可以使用你的 Access Token。"
    : "使用你的 Access Token 登录。";
  root.innerHTML = `
    <main class="login">
      <h1>${esc(brandName)}</h1>
      <p>AI 运营控制中心。${hint}</p>
      <input id="loginToken" type="password" autocomplete="off" placeholder="Access Token">
      <button class="btn primary" style="width:100%;margin-top:10px" data-action="login">进入控制中心</button>
      <div class="error">${esc(error)}</div>
    </main>`;
  document.querySelector("#loginToken")?.focus();
}

function navItems() {
  const base = [
	["command", "layout-dashboard", "运营总览"],
	["tasks", "checkbox", "工作中心"],
	["files", "file-upload", "文件中心"],
	["people", "users", "人员"],
  ];
  if (state.me?.is_superadmin || canStartWorkflow()) {
	base.push(["workers", "robot", "Worker 管理"]);
  }
  if (state.me?.is_superadmin) {
	base.push(["learning", "school", "学习与评测"]);
	base.push(["model", "brain", "模型管理"]);
	base.push(["ops", "shield-check", "系统运维"]);
  }
	base.push(["workspace", "apps", "动态工作台"]);
	base.push(["chat", "messages", "对话"]);
  return base;
}

function renderApp() {
  disposeIHTMLWorkspace();
  if (!state.me) {
    renderLogin();
    return;
  }
  const activeWorkers = state.workers.filter(w => w.online).length;
  const engineFails = Number(state.ops?.engine?.consecutive_fails || 0);
  const currentModel = state.ai?.current_model || state.ai?.default_model || "未读取";
  const statusText = state.me?.is_superadmin ? (engineFails ? "AI 异常" : "系统正常") : "控制台在线";
  root.innerHTML = `
    <div class="app route-${esc(state.route)}">
      <aside class="sidebar">
        <div class="brand"><span class="brand-mark">${esc(brandMark)}</span><span>${esc(brandName)}</span></div>
        <nav class="nav">
          ${navItems().map(([route, ico, label]) => `
            <button data-route="${route}" class="${state.route === route ? "active" : ""}">
              ${icon(ico)}<span>${label}</span>
            </button>`).join("")}
        </nav>
        <div class="user-box">
          <div class="user-row">
            <div class="avatar">${esc((state.me.name || brandMark).slice(0, 1).toUpperCase())}</div>
            <div>
              <div class="user-name">${esc(state.me.name)}</div>
              <div class="user-role">${state.me.is_superadmin ? "超级管理员" : "成员"}</div>
            </div>
          </div>
        </div>
      </aside>
		<nav class="mobile-nav" aria-label="控制中心导航">
			${navItems().map(([route, ico, label]) => `<button title="${esc(label)}" aria-label="${esc(label)}" data-route="${route}" class="${state.route === route ? "active" : ""}">${icon(ico)}<span>${esc(label)}</span></button>`).join("")}
		</nav>
      <header class="topbar">
        <div class="command-actions">
          <button class="btn primary" data-action="focus-chat">${icon("plus")}新建命令</button>
          <button class="btn" data-action="go-files">${icon("upload")}传文件</button>
          ${state.me?.is_superadmin ? `<button class="btn" data-action="select-risk" data-risk="model">${icon("switch-2")}改模型</button>
          <button class="btn danger" data-action="select-risk" data-risk="upgrade">${icon("rocket")}升级</button>` : ""}
        </div>
        <div class="command-search">
          ${icon("search")}
			<input id="globalSearch" value="${esc(state.search?.query || "")}" placeholder="搜索任务 / 员工 / 文件 / 项目">
        </div>
        <div class="status-strip">
          <div class="status-meta">
            <span class="status-item"><span class="dot ${engineFails ? "red" : "green"}"></span>${statusText}</span>
            <span class="status-item">${tg ? "Mini App 模式：开" : "Mini App 模式：关"}</span>
            <span class="status-item">Worker ${activeWorkers}/${state.workers.length}</span>
            ${state.me?.is_superadmin ? `<span class="status-item status-model" title="${esc(currentModel)}"><span>模型</span><span class="status-model-name">${esc(shortModel(currentModel))}</span></span>` : ""}
          </div>
          <div class="status-actions">
            <button class="btn subtle" data-action="refresh">${icon("refresh")}刷新</button>
            <button class="btn subtle" data-action="logout">${icon("logout")}退出</button>
          </div>
        </div>
      </header>
      <main class="main-shell">
        <section class="content" id="content"></section>
        <aside class="inspector" id="inspector"></aside>
      </main>
      <section class="log-panel">
        <div class="log-head">
          <strong>实时日志</strong>
          <span class="pill green">连接正常</span>
          <span class="spacer"></span>
          <button class="btn subtle" data-action="clear-logs">${icon("trash")}清空</button>
        </div>
        <div class="log-table-wrap" id="logs"></div>
      </section>
    </div>`;
  renderContent();
  renderInspector();
  renderLogsOnly();
}

function shortModel(model) {
  if (!model) return "未设置";
  return model.length > 24 ? model.slice(0, 23) + "…" : model;
}

function renderContent() {
  const el = document.querySelector("#content");
  if (!el) return;
  if (state.loading) {
    el.innerHTML = `<div class="surface"><div class="empty">加载中…</div></div>`;
    return;
  }
	if (state.search?.query) {
		el.innerHTML = renderSearchResults();
		if (state.notice) {
			el.insertAdjacentHTML("afterbegin", `<div class="result bad page-notice">${esc(state.notice)}</div>`);
		}
		return;
	}
  switch (state.route) {
  case "files":
    el.innerHTML = renderFilesRoute();
    break;
  case "tasks":
    el.innerHTML = renderTasksRoute();
    break;
  case "workers":
    el.innerHTML = renderWorkersRoute();
    break;
	case "people":
		el.innerHTML = renderPeopleRoute();
		break;
	case "learning":
		el.innerHTML = renderLearningRoute();
		break;
  case "model":
    el.innerHTML = renderModelRoute();
    break;
  case "ops":
    el.innerHTML = renderOpsRoute();
    break;
  case "workspace":
    el.innerHTML = renderWorkspaceRoute();
    scheduleIHTMLWorkspaceMount();
    break;
  case "chat":
    el.innerHTML = renderChatRoute();
    setTimeout(() => {
      const chatLog = document.querySelector("#chatLog");
      if (chatLog) chatLog.scrollTop = chatLog.scrollHeight;
    }, 0);
    break;
  default:
    el.innerHTML = renderCommandRoute();
    break;
  }
  if (state.notice) {
    el.insertAdjacentHTML("afterbegin", `<div class="result bad page-notice">${esc(state.notice)}</div>`);
  }
}

function renderSearchResults() {
	const resources = state.search?.resources || [];
	const users = state.search?.users || [];
	const resourceRows = resources.length
		? `<table class="data-table"><thead><tr><th>类型</th><th>ID</th><th>名称</th><th>状态</th><th>创建时间</th></tr></thead><tbody>${resources.map(item => `<tr><td>${esc({ task: "任务", file: "文件", project: "项目" }[item.kind] || item.kind)}</td><td>#${item.id}</td><td class="td-title"><div class="title-strong">${esc(item.name)}</div></td><td>${statusPill(item.state)}</td><td>${fmtTime(item.created_at)}</td></tr>`).join("")}</tbody></table>`
		: `<div class="empty">没有匹配的任务、文件或项目。</div>`;
	const userRows = users.length
		? `<table class="data-table"><thead><tr><th>员工 ID</th><th>姓名</th><th>类型</th><th>状态</th></tr></thead><tbody>${users.map(user => `<tr><td>#${esc(user.user_id)}</td><td class="td-title"><div class="title-strong">${esc(user.name)}</div></td><td>${user.is_worker ? "Worker" : "员工"}</td><td>${statusPill(user.status)}</td></tr>`).join("")}</tbody></table>`
		: `<div class="empty">没有匹配的可见成员。</div>`;
	return `<section class="surface section"><div class="section-head"><h2>搜索：${esc(state.search.query)}</h2><span class="count">${resources.length + users.length}</span><span class="spacer"></span><button class="btn" data-action="clear-search">${icon("x")}清除</button></div>${resourceRows}</section>
		<section class="surface section"><div class="section-head"><h2>成员</h2><span class="count">${users.length}</span></div>${userRows}</section>`;
}

function renderCommandRoute() {
	const materialCases = state.me?.is_superadmin ? (state.ops?.materials?.active || state.materials) : state.materials;
	const pendingMaterials = materialCases.length;
  const queuedTasks = taskQueueSource().filter(t => ["pending", "in_progress", "awaiting_input"].includes(t.status));
  const workerRuns = state.me?.is_superadmin ? state.workerRuns : [];
  const reviewTasks = state.me?.is_superadmin ? state.taskReview : state.tasks.review;
  const taskCounts = countTaskStatuses(queuedTasks);
  const claimedRuns = workerRuns.filter(run => run.status === "claimed").length;
  const waitingRuns = workerRuns.filter(run => run.status === "awaiting_input" || run.status === "retry_wait").length;
  const activeSchedules = (state.schedules || []).filter(s => s.status === "active");
  const approvals = state.approvals || [];
  const riskCount = state.me?.is_superadmin ? approvals.length : 0;
  const decisions = state.decisions.length;
  const activeWorkers = state.workers.filter(w => w.online).length;
	const health = state.ops?.product_health || {};
	const exceptionCount = [health.delivery_failures_24h, health.notification_failures_24h,
		health.notification_uncertain, health.external_action_failures_24h, health.external_action_uncertain,
		health.domain_outbox_failures_24h, health.domain_outbox_backlog,
		health.telegram_inbound_failures_24h, health.telegram_inbound_backlog,
		health.telegram_delivery_failures_24h, health.telegram_delivery_uncertain,
		health.worker_llm_failures_24h, health.worker_llm_uncertain, health.tool_failures_24h,
		health.action_failures_24h, health.conversation_failures_24h, health.worker_needs_input,
		health.worker_retrying, health.learning_conflicts, state.ops?.evals?.failing_cases]
		.filter(value => Number(value || 0) > 0).length;
  const materialActions = canStartWorkflow()
		? `<button class="btn" data-action="batch-select-files">${icon("checks")}选中待处理文件</button>
			<button class="btn primary" data-action="start-material-from-selection">${icon("settings")}配置分析</button>`
		: `<button class="btn" data-action="batch-select-files">${icon("checks")}选中待处理文件</button>`;
  const riskActions = state.me?.is_superadmin
    ? `<button class="btn" data-action="select-risk" data-risk="model">${icon("switch-2")}模型</button>
      <button class="btn danger" data-action="select-risk" data-risk="upgrade">${icon("rocket")}升级</button>`
    : "";
  return `
    <div class="metrics">
		${metric(exceptionCount ? "alert-triangle" : "circle-check", "异常类别", exceptionCount, exceptionCount ? "需要优先处理" : "当前没有已知异常")}
		${metric("folder-up", "待处理材料", pendingMaterials, `${Number(state.ops?.materials?.stats?.needs_input || 0)} 个需要补充`)}
      ${metric("checkbox", "业务任务", queuedTasks.length, `${taskCounts.pending} 待处理 · ${taskCounts.in_progress} 进行中 · ${taskCounts.awaiting_input} 待补充`)}
      ${metric("player-play", "Worker 执行", workerRuns.length, `${claimedRuns} 正在执行 · ${waitingRuns} 重试或待输入`)}
      ${metric("clipboard-check", "待验收", reviewTasks.length, "已停止执行，等待确认结果")}
      ${metric("robot", "Worker 可用", activeWorkers, `总数 ${state.workers.length}`)}
      ${metric("calendar-time", "定时自动化", activeSchedules.length, `${state.schedules.length} 条可见规则`)}
      ${metric("alert-triangle", "需要确认", riskCount + decisions, `${decisions} 个决策项`)}
    </div>
		${state.me?.is_superadmin ? queueSection("运营异常", exceptionCount, renderProductExceptions(), `<button class="btn" data-route="ops">${icon("activity")}查看运维</button>`) : ""}
		${queueSection("待处理材料", pendingMaterials, renderMaterialRows(materialCases), materialActions)}
    ${queueSection("业务任务", queuedTasks.length, renderTaskRows(queuedTasks.slice(0, 12)), `
      <button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
    `)}
    ${state.me?.is_superadmin ? queueSection("Worker 执行队列", workerRuns.length, renderWorkerRunRows(workerRuns.slice(0, 12)), `
      <button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
    `) : ""}
    ${queueSection("待验收", reviewTasks.length, renderTaskRows(reviewTasks.slice(0, 12)), "")}
    ${queueSection("定时自动化", activeSchedules.length, renderScheduleRows(activeSchedules.slice(0, 8)), `
      <button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
    `)}
		${queueSection("待确认高风险操作", riskCount, renderRiskRows(approvals), riskActions)}
		${state.me?.is_superadmin ? queueSection("近期工作证据", (state.ops?.recent_evidence || []).length, renderWorkEvidenceRows(), "") : ""}
  `;
}

function metric(ico, label, value, note) {
  return `
    <div class="metric">
      <div class="metric-label">${icon(ico)}${esc(label)}</div>
      <div class="metric-value">${esc(value)}</div>
      <div class="metric-note">${esc(note)}</div>
    </div>`;
}

function queueSection(title, count, rows, actions) {
  return `
    <section class="surface section">
      <div class="section-head">
        <h2>${esc(title)}</h2>
        <span class="count">${count}</span>
        <span class="spacer"></span>
        <div class="toolbar">${actions || ""}</div>
      </div>
      ${rows}
    </section>`;
}

function renderMaterialRows(items = state.materials) {
  if (!items.length) return `<div class="empty">当前没有待处理材料。</div>`;
  return `
    <table class="data-table">
      <thead><tr><th></th><th>ID</th><th>材料</th><th>提交人</th><th>文件</th><th>提交时间</th><th>状态</th><th>关联工作</th></tr></thead>
		<tbody>${items.slice(0, 12).map(item => {
        const files = item.files || [];
        const first = files[0];
				const canQueue = item.source !== "workflow" && Number(item.owner_id) === Number(state.me?.id);
				const selectKind = item.task_id ? "task" : (first && canQueue ? "file" : "");
				const selectID = item.task_id || first?.id;
				const selected = selectKind && state.selected?.kind === selectKind && Number(state.selected.id) === Number(selectID);
				const checked = canQueue && files.length > 0 && files.every(f => state.selectedFileIDs.has(Number(f.id)));
				return `<tr class="${selectKind ? "selectable" : ""} ${selected ? "selected" : ""}" ${selectKind ? `data-select-kind="${selectKind}" data-id="${selectID}"` : ""}>
					<td><input type="checkbox" ${canQueue ? `data-action="toggle-material" data-id="${item.id}"` : "disabled"} ${checked ? "checked" : ""}></td>
          <td>MAT-${item.id}</td>
          <td class="td-title"><div class="title-strong">${esc(item.title || files.map(f => f.original_name).join("、") || "未命名材料")}</div><div class="subline">${esc(item.instruction || "等待处理指令")}</div></td>
          <td>${esc(item.owner_name || state.me?.name || "")}</td>
          <td>${files.length} 个<div class="subline">${esc(files.slice(0, 2).map(f => f.original_name).join("、"))}</div></td>
          <td>${fmtTime(item.created_at)}</td>
          <td>${materialStatusPill(item.status)}</td>
          <td>${item.task_id ? `TSK-${esc(item.task_id)}` : "未派发"}</td>
        </tr>`;
      }).join("")}</tbody>
    </table>`;
}

function renderProductExceptions() {
	const health = state.ops?.product_health || {};
	const evals = state.ops?.evals || {};
	const rows = [
		["对话执行失败", health.conversation_failures_24h, "近24小时", "conversation"],
		["动作工具失败", health.action_failures_24h, "近24小时", "action"],
		["底层工具错误", health.tool_failures_24h, "近24小时", "tool"],
		["定时投递失败", health.delivery_failures_24h, "近24小时", "schedule"],
		["外部通知失败", health.notification_failures_24h, "近24小时", "notification"],
		["外部通知结果不确定", health.notification_uncertain, "当前", "notification"],
		["渠道直达动作失败", health.external_action_failures_24h, "近24小时", "transport"],
		["渠道直达动作中断", health.external_action_uncertain, "当前", "transport"],
		["领域事件投递失败", health.domain_outbox_failures_24h, "近24小时", "outbox"],
		["领域事件投递积压", health.domain_outbox_backlog, "当前", "outbox"],
		["Telegram 入站失败", health.telegram_inbound_failures_24h, "近24小时", "telegram"],
		["Telegram 入站积压", health.telegram_inbound_backlog, "当前", "telegram"],
		["Telegram 分片失败", health.telegram_delivery_failures_24h, "近24小时", "telegram"],
		["Telegram 分片不确定", health.telegram_delivery_uncertain, "当前", "telegram"],
		["Worker 模型调用失败", health.worker_llm_failures_24h, "近24小时", "worker"],
		["Worker 模型调用不确定", health.worker_llm_uncertain, "当前", "worker"],
		["Worker 待补充", health.worker_needs_input, "当前", "worker"],
		["Worker 等待重试", health.worker_retrying, "当前", "worker"],
		["学习候选冲突", health.learning_conflicts, "当前", "learning"],
		["模型评测未通过", evals.failing_cases, "最近一次", "eval"],
	].filter(row => Number(row[1] || 0) > 0);
	if (!rows.length) return `<div class="empty">当前没有已知运营异常。</div>`;
	return `<table class="data-table"><thead><tr><th>异常</th><th>数量</th><th>窗口</th><th>来源</th></tr></thead>
		<tbody>${rows.map(row => `<tr><td class="td-title"><div class="title-strong">${esc(row[0])}</div></td><td><span class="pill red">${Number(row[1])}</span></td><td>${esc(row[2])}</td><td>${esc(row[3])}</td></tr>`).join("")}</tbody></table>`;
}

function renderWorkEvidenceRows() {
	const items = state.ops?.recent_evidence || [];
	if (!items.length) return `<div class="empty">近7天没有可展示的工作证据。</div>`;
	return `<table class="data-table"><thead><tr><th>时间</th><th>类型</th><th>项目 / 人员</th><th>有来源的事实</th><th>状态</th></tr></thead>
		<tbody>${items.map(item => `<tr><td>${fmtTime(item.event_at)}</td><td>${esc(item.kind)}</td><td>${esc(item.project_name || item.actor_name || item.title || "")}</td><td class="td-title">${esc(truncate(item.content, 220))}</td><td>${materialStatusPill(item.status)}</td></tr>`).join("")}</tbody></table>`;
}

function renderFileLibraryRows() {
  if (!state.files.length) return `<div class="empty">文件库为空。</div>`;
  return `<table class="data-table">
    <thead><tr><th></th><th>ID</th><th>文件</th><th>类型</th><th>大小</th><th>上传时间</th><th></th></tr></thead>
    <tbody>${state.files.map(file => `<tr class="selectable" data-select-kind="file" data-id="${file.id}">
      <td><input type="checkbox" data-action="toggle-file" data-id="${file.id}" ${state.selectedFileIDs.has(Number(file.id)) ? "checked" : ""}></td>
      <td>FILE-${file.id}</td><td class="td-title"><div class="title-strong">${esc(file.original_name)}</div></td>
      <td>${esc(typeLabel(file))}</td><td>${fmtBytes(file.size_bytes)}</td><td>${fmtTime(file.created_at)}</td>
      <td><button class="icon-btn" title="下载" data-action="download-file" data-id="${file.id}">${icon("download")}</button></td>
    </tr>`).join("")}</tbody></table>`;
}

function materialStatusPill(status) {
  const labels = {
    received: ["amber", "待指令"], queued: ["blue", "已排队"], processing: ["blue", "处理中"],
    needs_input: ["red", "需补充"], completed: ["green", "已完成"], ignored: ["", "已忽略"],
		observed: ["blue", "已观察"], active: ["amber", "需跟进"], resolved: ["green", "已闭合"],
		superseded: ["", "已替代"],
  };
  const [tone, label] = labels[status] || ["", status || "未知"];
  return `<span class="pill ${tone}">${esc(label)}</span>`;
}

function renderTaskRows(tasks = taskQueueSource().slice(0, 10)) {
  if (!tasks.length) return `<div class="empty">当前没有任务。</div>`;
  return `
    <table class="data-table">
      <thead><tr><th></th><th>ID</th><th>任务</th><th>责任人</th><th>发起人</th><th>创建时间</th><th>进度</th><th>状态</th><th>截止 / 更新</th></tr></thead>
      <tbody>${tasks.map(t => {
        const progress = taskProgress(t.status);
        const selected = state.selected?.kind === "task" && Number(state.selected.id) === Number(t.id);
        return `<tr class="selectable ${selected ? "selected" : ""}" data-select-kind="task" data-id="${t.id}">
          <td><input type="checkbox"></td>
          <td>TSK-${t.id}</td>
		  <td class="td-title"><div class="title-strong">${esc(t.title)}</div><div class="subline">优先级 ${esc(t.priority || "normal")}${taskWorkerState(t)}</div></td>
          <td>${taskOwnerCell(t)}</td>
          <td>${esc(t.assigner_name || "")}</td>
          <td>${fmtTime(t.created_at) || "未知"}</td>
          <td><div class="progress"><span style="width:${progress}%"></span></div></td>
          <td>${statusPill(t.status)}</td>
          <td>${t.deadline ? fmtTime(t.deadline) : `更新于 ${fmtAge(t.updated_at)}`}</td>
        </tr>`;
      }).join("")}</tbody>
    </table>`;
}

function renderWorkerRunRows(runs = state.workerRuns.slice(0, 10)) {
  if (!runs.length) return `<div class="empty">当前没有 Worker 执行。</div>`;
  return `
    <table class="data-table">
      <thead><tr><th>ID</th><th>执行目标</th><th>Worker</th><th>发起人</th><th>执行器</th><th>尝试 / 失败</th><th>状态</th><th>最近更新</th></tr></thead>
      <tbody>${runs.map(run => `<tr class="selectable" data-select-kind="workerRun" data-id="${run.id}">
        <td>RUN-${run.id}</td>
        <td class="td-title"><div class="title-strong">${esc(run.title)}</div><div class="subline">${run.task_id ? `业务任务 TSK-${esc(run.task_id)}` : "独立执行"}${run.scope_title || run.scope_key ? ` · ${esc(run.scope_title || run.scope_key)}` : ""}</div></td>
        <td>${esc(run.worker_name || `#${run.worker_id}`)}</td>
        <td>${esc(run.requested_by_name || `#${run.requested_by}`)}</td>
        <td>${esc(run.executor || "agent")}<div class="subline">${esc(workerEvidenceScopeText(run.evidence_scope))}</div></td>
        <td>${esc(run.attempts || 0)} / ${esc(run.failures || 0)}</td>
        <td>${statusPill(run.status)}</td>
        <td>${fmtAge(run.updated_at)}</td>
      </tr>`).join("")}</tbody>
    </table>`;
}

function renderScheduleRows(items = state.schedules || []) {
  if (!items.length) return `<div class="empty">暂无定时自动化。</div>`;
  return `
    <table class="data-table">
      <thead><tr><th>ID</th><th>规则</th><th>目标</th><th>创建人</th><th>接收策略</th><th>模式</th><th>状态</th><th>下次触发</th><th>上次触发</th></tr></thead>
      <tbody>${items.map(s => `<tr class="selectable" data-select-kind="schedule" data-id="${s.id}">
        <td>SCH-${s.id}</td>
        <td class="td-title"><div class="title-strong">${esc(scheduleTitle(s))}</div><div class="subline">${esc(scheduleSubtitle(s))}</div></td>
        <td>${esc(scheduleTarget(s))}</td>
        <td>${esc(s.creator_name || "")}</td>
        <td>${s.recipient_policy === "mandatory" ? '<span class="pill red">强制接收</span>' : '<span class="pill">可退订</span>'}</td>
        <td>${esc(s.mode || "message")}</td>
        <td>${statusPill(s.status)}</td>
        <td>${fmtTime(s.fire_at) || ""}</td>
        <td>${fmtTime(s.last_fired) || "未触发"}</td>
      </tr>`).join("")}</tbody>
    </table>`;
}

function renderRiskRows(approvals) {
  if (!state.me?.is_superadmin) return `<div class="empty">高风险操作只对超级管理员开放。</div>`;
  if (!approvals?.length) {
    return `<div class="empty">暂无待确认高风险操作。模型切换和系统升级请使用顶部快捷入口。</div>`;
  }
  return `
    <table class="data-table">
      <thead><tr><th></th><th>ID</th><th>工具</th><th>发起人</th><th>会话</th><th>风险等级</th><th>状态</th><th>过期时间</th></tr></thead>
      <tbody>${approvals.map(a => {
        return `<tr>
          <td><input type="checkbox"></td><td>APPROVAL-${esc(a.id)}</td><td>${esc(a.tool)}</td><td>${esc(a.user_name || a.user_id)}</td>
          <td>${esc(a.session_id || "")}</td><td>${priorityPill("高")}</td><td><span class="pill red">待确认</span></td><td>${fmtTime(a.expires_at)}</td>
        </tr>`;
      }).join("")}</tbody>
    </table>`;
}

function renderFilesRoute() {
  return `
    <section class="surface section">
      <div class="section-head"><h2>文件中心</h2><span class="count">${state.files.length}</span></div>
      <div style="padding:12px">
        <div class="file-upload">
          <input class="input" id="fileInput" type="file" multiple>
          <button class="btn primary" data-action="upload-files">${icon("upload")}上传</button>
          ${canStartWorkflow() ? `<button class="btn" data-action="start-material-from-selection">${icon("send")}派 Worker 分析</button>` : ""}
        </div>
        <div class="result ${state.actionResult ? (state.actionOK ? "ok" : "bad") : ""}">${esc(state.actionResult || "文件上传后只进入待处理材料池，不会自动分析。")}</div>
      </div>
      ${renderFailedFileIntakes()}
      ${renderFileLibraryRows()}
    </section>`;
}

function renderFailedFileIntakes() {
  if (!state.fileIntakes.length) return "";
  return `<div class="result bad"><strong>未进入文件库</strong><br>${state.fileIntakes.slice(0, 8).map(x =>
    `${esc(x.original_name)}（${fmtBytes(x.size_bytes)}）：${esc(x.error_message || x.status)}`
  ).join("<br>")}</div>`;
}

function renderTasksRoute() {
	const assignedActive = state.tasks.assigned.filter(t => ["pending", "in_progress", "awaiting_input"].includes(t.status));
	const assignedHistory = state.tasks.assigned.filter(t => ["accepted", "split", "cancelled"].includes(t.status));
	if (state.me?.is_superadmin) {
		return `${queueSection("业务任务队列", state.taskQueue.length, taskTable(state.taskQueue), `
			<button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
		`)}
		${queueSection("全局待验收", state.taskReview.length, taskTable(state.taskReview), "")}
		${queueSection("业务任务历史", state.taskHistory.length, taskTable(state.taskHistory), "")}
		${queueSection("Worker 执行队列", state.workerRuns.length, renderWorkerRunRows(state.workerRuns), "")}
		${queueSection("Worker 执行历史", state.workerRunHistory.length, renderWorkerRunRows(state.workerRunHistory), "")}
		${queueSection("定时自动化", state.schedules.length, renderScheduleRows(state.schedules), `
			<button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
		`)}`;
	}
	const schedules = queueSection("定时自动化", state.schedules.length, renderScheduleRows(state.schedules), "");
  return `
		${schedules}
    ${queueSection("我的待办", state.tasks.todo.length, taskTable(state.tasks.todo), "")}
    ${queueSection("待验收", state.tasks.review.length, taskTable(state.tasks.review), "")}
		${queueSection("我分配的活跃任务", assignedActive.length, taskTable(assignedActive), "")}
		${queueSection("我分配的历史", assignedHistory.length, taskTable(assignedHistory), "")}`;
}

function taskTable(tasks) {
  if (!tasks.length) return `<div class="empty">（空）</div>`;
  return `
    <table class="data-table">
      <thead><tr><th>ID</th><th>标题</th><th>状态</th><th>责任人与参与者</th><th>发起人</th><th>最近更新</th><th>截止</th></tr></thead>
      <tbody>${tasks.map(t => `<tr class="selectable" data-select-kind="task" data-id="${t.id}">
			<td>#${t.id}</td><td class="td-title"><div class="title-strong">${esc(t.title)}</div>${taskWorkerState(t, true)}</td><td>${statusPill(t.status)}</td>
        <td>${taskOwnerCell(t)}</td><td>${esc(t.assigner_name || "")}</td><td>${fmtAge(t.updated_at)}</td><td>${fmtTime(t.deadline) || "未设定"}</td>
      </tr>`).join("")}</tbody>
    </table>`;
}

function renderWorkersRoute() {
  if (!state.me?.is_superadmin && !canStartWorkflow()) return `<div class="surface"><div class="empty">需要 AI 员工管理权限。</div></div>`;
  if (!state.workers.length) return `<div class="surface"><div class="empty">暂无 AI Worker。</div></div>`;
  return `
    <section class="surface section">
      <div class="section-head"><h2>Worker 管理</h2><span class="count">${state.workers.length}</span></div>
      <table class="data-table">
        <thead><tr><th>ID</th><th>名称</th><th>状态</th><th>Admin</th><th>引擎</th><th>能力</th><th>最后在线</th></tr></thead>
        <tbody>${state.workers.map(w => {
          const c = w.capability || {};
          return `<tr class="selectable" data-select-kind="worker" data-id="${w.id}">
            <td>#${w.id}</td><td>${esc(w.name)}</td><td>${w.online ? '<span class="pill green">在线</span>' : '<span class="pill amber">离线</span>'}</td>
            <td>${w.admin ? '<span class="pill red">admin</span>' : '<span class="pill blue">普通</span>'}</td>
            <td>${esc(c.Engine || c.engine || "")}</td><td>${esc((c.Capabilities || c.capabilities || []).join(" / "))}</td><td>${fmtTime(w.last_seen) || ""}</td>
          </tr>`;
        }).join("")}</tbody>
      </table>
    </section>`;
}

function renderPeopleRoute() {
	if (!state.users.length) return `<div class="surface"><div class="empty">成员目录为空。</div></div>`;
	return `<section class="surface section">
		<div class="section-head"><h2>成员目录</h2><span class="count">${state.users.length}</span></div>
		<table class="data-table"><thead><tr><th>员工 ID</th><th>姓名</th><th>类型</th><th>状态</th><th>职位 / 组别</th><th>创建时间</th></tr></thead>
		<tbody>${state.users.map(user => {
			const info = user.info || {};
			const details = [info.position || info.role || "", info.group || info.department || ""].filter(Boolean).join(" · ");
			return `<tr><td>#${esc(user.user_id)}</td><td class="td-title"><div class="title-strong">${esc(user.name)}</div>${user.is_superadmin ? '<div class="subline">超级管理员</div>' : ""}</td><td>${user.is_worker ? '<span class="pill blue">Worker</span>' : '<span class="pill">员工</span>'}</td><td>${statusPill(user.status)}</td><td>${esc(details || "未完善")}</td><td>${fmtTime(user.created_at)}</td></tr>`;
		}).join("")}</tbody></table>
	</section>`;
}

function renderLearningRoute() {
	if (!state.me?.is_superadmin) return `<div class="surface"><div class="empty">需要超级管理员权限。</div></div>`;
	const usage = state.learning?.asset_usage_30d || {};
	const stats = state.evals?.stats || {};
	const cases = state.evals?.cases || [];
	const runs = state.evals?.runs || [];
	return `<div class="metrics">
		${metric("bulb", "待治理学习", state.learning?.candidates?.length || 0, "当前待审核候选")}
		${metric("filter", "规则注入", Number(usage.injected || 0), "近30天")}
		${metric("route", "Skill 候选", Number(usage.candidates || 0), `${Number(usage.loaded || 0)} 次实际加载`)}
		${metric("activity", "资产异常归因", Number(usage.partial || 0) + Number(usage.failed || 0), `${Number(usage.action_succeeded || 0)} 次动作成功`)}
		${metric("test-pipe", "评测通过", Number(stats.passing_cases || 0), `${Number(stats.failing_cases || 0)} 失败 · ${Number(stats.enabled_cases || 0)} 启用`)}
	</div>
	${queueSection("模型行为评测", cases.length, renderEvalCases(cases), `<button class="btn primary" data-action="run-evals">${icon("player-play")}运行全部启用案例</button>`)}
	${queueSection("最近评测结果", runs.length, renderEvalRuns(runs), "")}
	${queueSection("规则与 Skill 使用成效", state.learning?.asset_effectiveness_30d?.length || 0, renderAssetEffectiveness(), "")}
	${queueSection("待治理学习候选", state.learning?.candidates?.length || 0, renderLearningCandidates(), "")}`;
}

function renderEvalCases(cases) {
	if (!cases.length) return `<div class="empty">暂无评测案例。</div>`;
	return `<table class="data-table"><thead><tr><th>ID</th><th>案例</th><th>渠道</th><th>输入</th><th>状态</th><th></th></tr></thead>
		<tbody>${cases.map(item => `<tr><td>EVAL-${item.ID || item.id}</td><td class="td-title"><div class="title-strong">${esc(item.Name || item.name)}</div></td><td>${esc(item.Channel || item.channel)}</td><td>${esc(truncate(item.UserInput || item.user_input, 120))}</td><td>${(item.Enabled ?? item.enabled) ? '<span class="pill green">启用</span>' : '<span class="pill">停用</span>'}</td><td><button class="btn" data-action="run-eval" data-id="${item.ID || item.id}">${icon("player-play")}运行</button></td></tr>`).join("")}</tbody></table>`;
}

function renderEvalRuns(runs) {
	if (!runs.length) return `<div class="empty">还没有评测记录。</div>`;
	return `<table class="data-table"><thead><tr><th>时间</th><th>案例</th><th>结果</th><th>工具轨迹</th><th>耗时</th><th>输出</th></tr></thead>
		<tbody>${runs.map(run => {
			const details = run.details || {};
			return `<tr><td>${fmtTime(run.created_at)}</td><td>${esc(run.case_name || `#${run.case_id || ""}`)}</td><td>${run.status === "passed" ? '<span class="pill green">通过</span>' : '<span class="pill red">未通过</span>'}</td><td>${esc((details.tool_calls || []).join(" → ") || "无")}</td><td>${Number(details.duration_ms || 0)}ms</td><td class="td-title">${esc(truncate((details.failures || []).join("；") || run.output, 180))}</td></tr>`;
		}).join("")}</tbody></table>`;
}

function renderLearningCandidates() {
	const items = state.learning?.candidates || [];
	if (!items.length) return `<div class="empty">当前没有待治理候选。</div>`;
	return `<table class="data-table"><thead><tr><th>ID</th><th>类型</th><th>标题</th><th>作用域</th><th>价值</th><th>来源</th></tr></thead>
		<tbody>${items.map(item => `<tr><td>#${item.ID || item.id}</td><td>${esc(item.Kind || item.kind)}</td><td class="td-title"><div class="title-strong">${esc(item.Title || item.title)}</div><div class="subline">${esc(truncate(item.Content || item.content, 160))}</div></td><td>${esc(item.Scope || item.scope)}</td><td>${Number(item.ValueScore ?? item.value_score ?? 0).toFixed(2)}</td><td>${esc(item.SourceType || item.source_type || "")}</td></tr>`).join("")}</tbody></table>`;
}

function renderAssetEffectiveness() {
	const items = state.learning?.asset_effectiveness_30d || [];
	if (!items.length) return `<div class="empty">近30天还没有规则或 Skill 使用记录。</div>`;
	return `<table class="data-table"><thead><tr><th>资产</th><th>类型</th><th>注入</th><th>候选 / 加载</th><th>动作成功</th><th>部分 / 失败</th><th>最近使用</th></tr></thead>
		<tbody>${items.map(item => `<tr><td class="td-title"><div class="title-strong">#${item.knowledge_id} ${esc(item.title)}</div></td><td>${esc(item.kind)}</td><td>${Number(item.injected || 0)}</td><td>${Number(item.candidates || 0)} / ${Number(item.loaded || 0)}</td><td>${Number(item.action_succeeded || 0)}</td><td>${Number(item.partial || 0)} / ${Number(item.failed || 0)}</td><td>${fmtTime(item.last_used_at)}</td></tr>`).join("")}</tbody></table>`;
}

function renderModelRoute() {
  if (!state.me?.is_superadmin) return `<div class="surface"><div class="empty">需要超级管理员权限。</div></div>`;
  const models = state.ai?.loaded_models || [];
  return `
    <div class="two-col">
      <section class="surface section">
        <div class="section-head"><h2>模型切换</h2><span class="pill blue">纯命令 API</span></div>
        <div class="inspector-body">
          ${modelForm()}
        </div>
      </section>
      <section class="surface section">
        <div class="section-head"><h2>能力域</h2><span class="count">${state.capabilities.length}</span></div>
        <table class="data-table">
          <thead><tr><th>领域</th><th>可用</th><th>总数</th></tr></thead>
          <tbody>${capabilityDomainRows()}</tbody>
        </table>
      </section>
    </div>
    <section class="surface section">
      <div class="section-head"><h2>已加载模型</h2><span class="count">${models.length}</span></div>
      ${models.length ? `<table class="data-table"><tbody>${models.map(m => `<tr><td>${esc(m)}</td><td><button class="btn" data-action="choose-model" data-model="${esc(m)}">${icon("check")}选择</button></td></tr>`).join("")}</tbody></table>` : `<div class="empty">${esc(state.ai?.loaded_models_error || "暂无已加载模型")}</div>`}
    </section>`;
}

function renderOpsRoute() {
  if (!state.me?.is_superadmin) return `<div class="surface"><div class="empty">需要超级管理员权限。</div></div>`;
	const health = state.ops?.product_health || {};
	const evals = state.ops?.evals || {};
	return `
		<div class="metrics">
			${metric("message-circle", "群聊工作证据", Number(state.ops?.work_evidence?.observed_messages || 0), "近7天")}
			${metric("folder-up", "待处理材料", Number(state.ops?.materials?.stats?.received || 0) + Number(state.ops?.materials?.stats?.queued || 0) + Number(state.ops?.materials?.stats?.processing || 0) + Number(state.ops?.materials?.stats?.needs_input || 0), `${Number(state.ops?.materials?.stats?.needs_input || 0)} 需补充`)}
			${metric("alert-triangle", "异常类别", [health.tool_failures_24h, health.action_failures_24h, health.conversation_failures_24h, health.delivery_failures_24h, health.notification_failures_24h, health.notification_uncertain, health.external_action_failures_24h, health.external_action_uncertain, health.domain_outbox_failures_24h, health.domain_outbox_backlog, health.telegram_inbound_failures_24h, health.telegram_inbound_backlog, health.telegram_delivery_failures_24h, health.telegram_delivery_uncertain, health.worker_llm_failures_24h, health.worker_llm_uncertain].filter(value => Number(value || 0) > 0).length, "执行 / 投递 / 渠道")}
			${metric("test-pipe", "评测健康", Number(evals.passing_cases || 0), `${Number(evals.failing_cases || 0)} 未通过`)}
		</div>
    <div class="two-col">
      <section class="surface section">
        <div class="section-head"><h2>生产升级</h2><span class="pill red">高风险</span></div>
        <div class="inspector-body">${upgradeForm()}</div>
      </section>
      <section class="surface section">
        <div class="section-head"><h2>运维状态</h2></div>
        ${opsTable()}
      </section>
		</div>
		${maintenanceSection()}
		${queueSection("能力成熟度与使用", state.capabilities.length, renderCapabilityRows(), "")}`;
}

function maintenanceSection() {
	const lifecycle = state.ops?.maintenance || {};
	const jobs = lifecycle.jobs || [];
	if (lifecycle.error) return `<section class="surface section"><div class="section-head"><h2>数据生命周期</h2><span class="pill red">状态不可用</span></div><div class="empty">${esc(lifecycle.error)}</div></section>`;
	const toolbar = lifecycle.enabled
		? `<button class="btn" data-action="inspect-maintenance">${icon("scan")}仅检查</button>
		   <button class="btn danger" data-action="apply-maintenance">${icon("recycle")}立即维护</button>`
		: `<span class="pill amber">未启用</span>`;
	const rows = jobs.length ? `<table class="data-table"><thead><tr><th>维护项</th><th>数据类别</th><th>状态</th><th>上次结果</th><th>下次运行</th></tr></thead>
		<tbody>${jobs.map(job => {
			const report = job.last_report || {};
			const status = job.last_status === "succeeded" ? '<span class="pill green">正常</span>'
				: job.last_status === "failed" ? '<span class="pill red">失败</span>'
				: job.last_status === "running" ? '<span class="pill blue">运行中</span>' : '<span class="pill">未运行</span>';
			return `<tr><td class="td-title"><div class="title-strong">${esc(job.name)}</div><div class="subline">${esc(job.description)}</div></td><td>${esc(job.class)}</td><td>${status}${job.last_error ? `<div class="subline">${esc(truncate(job.last_error, 120))}</div>` : ""}</td><td>${Number(report.reclaimed || 0)} 条 · ${fmtBytes(report.bytes || 0)}</td><td>${fmtTime(job.next_run_at)}</td></tr>`;
		}).join("")}</tbody></table>` : `<div class="empty">维护任务尚未完成首次注册。</div>`;
	return `<section class="surface section"><div class="section-head"><h2>数据生命周期</h2><span class="count">${jobs.length}</span><span class="spacer"></span>${toolbar}</div>${rows}</section>`;
}

function renderCapabilityRows() {
	if (!state.capabilities.length) return `<div class="empty">能力目录为空。</div>`;
	return `<table class="data-table"><thead><tr><th>能力</th><th>领域</th><th>成熟度</th><th>效果</th><th>近30天调用</th><th>失败</th><th>最后使用</th></tr></thead>
		<tbody>${state.capabilities.map(item => `<tr><td class="td-title"><div class="title-strong">${esc(item.name)}</div><div class="subline">${esc(truncate(item.description, 100))}</div></td><td>${esc(item.domain)}</td><td>${item.maturity === "stable" ? '<span class="pill green">稳定</span>' : '<span class="pill amber">实验</span>'}</td><td>${esc(item.effect)}</td><td>${Number(item.usage_30d || 0)}</td><td>${Number(item.failures_30d || 0)}</td><td>${fmtTime(item.last_used_at) || "未使用"}</td></tr>`).join("")}</tbody></table>`;
}

function renderChatRoute() {
  return `
    <section class="surface chat-layout">
      <div class="chat-log" id="chatLog">
        ${state.chat.map(m => `<div class="msg ${m.role}">${esc(m.text)}</div>`).join("")}
      </div>
      <form class="chat-form" data-form="chat">
        <textarea class="textarea" id="chatInput" placeholder="输入命令或问题，Enter 发送，Shift+Enter 换行"></textarea>
        <button class="btn primary" type="submit">${icon("send")}发送</button>
      </form>
    </section>`;
}

function renderWorkspaceRoute() {
  return `
    <section class="ihtml-workspace-shell">
      <nav class="ihtml-workspace-menu" id="ihtmlWorkspaceMenu" aria-label="动态工作台页面"></nav>
      <div class="ihtml-workspace-host" id="ihtmlWorkspace"></div>
    </section>`;
}

function disposeIHTMLWorkspace() {
  ihtmlMountGeneration++;
  if (!ihtmlWorkspace) return;
  try { ihtmlWorkspace.destroy(); } catch (_) { /* already detached */ }
  ihtmlWorkspace = null;
}

function scheduleIHTMLWorkspaceMount() {
  const generation = ihtmlMountGeneration;
  queueMicrotask(() => mountIHTMLWorkspace(generation));
}

async function getIHTMLTicket() {
  const userID = String(state.me?.id || "");
  if (!userID) throw new Error("工作台用户身份不可用");
  const now = Date.now();
  if (ihtmlTicket.userID === userID && ihtmlTicket.token && ihtmlTicket.expiresAt > now + 30_000) {
    return ihtmlTicket.token;
  }
  if (ihtmlTicketPromise.userID === userID && ihtmlTicketPromise.value) return ihtmlTicketPromise.value;
  const request = api("/api/ihtml/ticket", { method: "POST" })
    .then(data => {
      const expiresAt = Date.parse(data.expires_at || "");
      if (!data.token || !Number.isFinite(expiresAt)) throw new Error("工作台连接票据无效");
      if (String(state.me?.id || "") !== userID) throw new Error("工作台用户身份已变更");
      ihtmlTicket = { userID, token: data.token, expiresAt };
      return data.token;
    })
    .finally(() => {
      if (ihtmlTicketPromise.value === request) ihtmlTicketPromise = { userID: "", value: null };
    });
  ihtmlTicketPromise = { userID, value: request };
  return request;
}

function resetIHTMLAuth() {
  ihtmlTicket = { userID: "", token: "", expiresAt: 0 };
  ihtmlTicketPromise = { userID: "", value: null };
}

function mountIHTMLWorkspace(generation) {
  if (generation !== ihtmlMountGeneration || state.route !== "workspace") return;
  const host = document.querySelector("#ihtmlWorkspace");
  const menu = document.querySelector("#ihtmlWorkspaceMenu");
  if (!host || !menu || !window.ihtml || typeof window.ihtml.mount !== "function") {
    if (host) host.textContent = "动态工作台内核未加载。";
    return;
  }
  try {
    if (generation !== ihtmlMountGeneration) return;
    ihtmlWorkspace = window.ihtml.mount(host, {
      base: new URL("/ui/", window.location.origin).toString(),
      auth: getIHTMLTicket,
      connectionAuth: getIHTMLTicket,
      theme: "inherit",
      routing: "memory",
      page: state.workspacePage,
      menu,
      chatEntry: true,
      storageKey: `nbco-dynamic-workspace:${state.me.id}`,
      scriptNonce: root.dataset.cspNonce || "",
      onAuthError: resetIHTMLAuth,
      onChange: navState => {
        const page = normalizeWorkspacePage(navState?.page);
        if (page === state.workspacePage) return;
        state.workspacePage = page;
        syncBrowserLocation();
      },
    });
  } catch (err) {
    host.textContent = `动态工作台启动失败：${err.message}`;
    addLog("ihtml", "ERROR", err.message);
  }
}

function renderInspector() {
  const el = document.querySelector("#inspector");
  if (!el) return;
	if (state.search?.query) {
		el.innerHTML = inspectorFrame("搜索结果", "search", `<dl class="kv"><dt>工作对象</dt><dd>${state.search.resources?.length || 0}</dd><dt>可见成员</dt><dd>${state.search.users?.length || 0}</dd></dl>`);
		return;
	}
  if (state.route === "chat") {
    el.innerHTML = inspectorFrame("对话上下文", "messages", `<div class="result">临时问题走对话；文件分析、升级、模型切换等高影响动作建议走命令队列。</div>`);
    return;
  }
  if (state.route === "workspace") {
    el.replaceChildren();
    return;
  }
	if (state.route === "people") {
		const humans = state.users.filter(user => !user.is_worker).length;
		const workers = state.users.filter(user => user.is_worker).length;
		el.innerHTML = inspectorFrame("成员概况", "users", `<dl class="kv"><dt>可见成员</dt><dd>${state.users.length}</dd><dt>真实员工</dt><dd>${humans}</dd><dt>AI Worker</dt><dd>${workers}</dd><dt>稳定标识</dt><dd>员工 ID</dd></dl>`);
		return;
	}
	if (state.route === "learning") {
		const usage = state.learning?.asset_usage_30d || {};
		const stats = state.evals?.stats || {};
		el.innerHTML = inspectorFrame("学习健康", "school", `<dl class="kv"><dt>待治理候选</dt><dd>${state.learning?.candidates?.length || 0}</dd><dt>实际加载 Skill</dt><dd>${Number(usage.loaded || 0)}</dd><dt>部分 / 失败归因</dt><dd>${Number(usage.partial || 0)} / ${Number(usage.failed || 0)}</dd><dt>评测通过</dt><dd>${Number(stats.passing_cases || 0)} / ${Number(stats.enabled_cases || 0)}</dd></dl>`);
		return;
	}
	if (state.route === "model") {
		el.innerHTML = inspectorFrame("模型与推理设置", "brain", modelForm());
		return;
	}
	if (state.route === "ops") {
		const health = state.ops?.product_health || {};
		el.innerHTML = inspectorFrame("当前异常", "activity", `<dl class="kv"><dt>对话失败</dt><dd>${Number(health.conversation_failures_24h || 0)}</dd><dt>Agent 动作失败</dt><dd>${Number(health.action_failures_24h || 0)}</dd><dt>定时投递失败</dt><dd>${Number(health.delivery_failures_24h || 0)}</dd><dt>外部通知失败 / 不确定</dt><dd>${Number(health.notification_failures_24h || 0)} / ${Number(health.notification_uncertain || 0)}</dd><dt>渠道动作失败 / 中断</dt><dd>${Number(health.external_action_failures_24h || 0)} / ${Number(health.external_action_uncertain || 0)}</dd><dt>领域事件失败 / 积压</dt><dd>${Number(health.domain_outbox_failures_24h || 0)} / ${Number(health.domain_outbox_backlog || 0)}</dd><dt>Telegram 入站失败 / 积压</dt><dd>${Number(health.telegram_inbound_failures_24h || 0)} / ${Number(health.telegram_inbound_backlog || 0)}</dd><dt>Telegram 分片失败 / 不确定</dt><dd>${Number(health.telegram_delivery_failures_24h || 0)} / ${Number(health.telegram_delivery_uncertain || 0)}</dd><dt>Worker 模型失败 / 不确定</dt><dd>${Number(health.worker_llm_failures_24h || 0)} / ${Number(health.worker_llm_uncertain || 0)}</dd><dt>Worker 待输入</dt><dd>${Number(health.worker_needs_input || 0)}</dd></dl>`);
		return;
	}
  const selected = selectedItem();
  if (!selected) {
    el.innerHTML = inspectorFrame("上下文检查器", "clipboard-list", `<div class="empty">选择左侧队列中的一行。</div>`);
    return;
  }
  if (selected.kind === "file") {
    el.innerHTML = inspectorFrame("材料详情", "file-description", fileInspector(selected.item));
    return;
  }
  if (selected.kind === "task") {
    el.innerHTML = inspectorFrame("任务详情", "checkbox", taskInspector(selected.item));
    return;
  }
  if (selected.kind === "workerRun") {
    el.innerHTML = inspectorFrame("Worker 执行详情", "player-play", workerRunInspector(selected.item));
    return;
  }
  if (selected.kind === "schedule") {
    el.innerHTML = inspectorFrame("定时自动化", "calendar-time", scheduleInspector(selected.item));
    return;
  }
  if (selected.kind === "worker") {
    el.innerHTML = inspectorFrame("Worker 详情", "robot", workerInspector(selected.item));
    return;
  }
  if (selected.kind === "risk" && selected.id === "upgrade") {
    if (!state.me?.is_superadmin) {
      el.innerHTML = inspectorFrame("高风险操作", "shield-lock", `<div class="empty">需要超级管理员权限。</div>`);
      return;
    }
    el.innerHTML = inspectorFrame("系统升级", "rocket", upgradeForm());
    return;
  }
  if (selected.kind === "risk") {
    if (!state.me?.is_superadmin) {
      el.innerHTML = inspectorFrame("高风险操作", "shield-lock", `<div class="empty">需要超级管理员权限。</div>`);
      return;
    }
    el.innerHTML = inspectorFrame("模型与推理设置", "brain", modelForm());
    return;
  }
  el.innerHTML = inspectorFrame("上下文检查器", "clipboard-list", `<div class="empty">暂无详情。</div>`);
}

function inspectorFrame(title, ico, body) {
  return `
    <div class="inspector-head">${icon(ico)}<h2>${esc(title)}</h2></div>
    <div class="inspector-body">${body}</div>`;
}

function fileInspector(file) {
  const fileIDs = selectedFileList();
  if (!fileIDs.includes(Number(file.id))) fileIDs.unshift(Number(file.id));
  const details = `
    <dl class="kv">
      <dt>文件</dt><dd>${esc(file.original_name)}</dd>
      <dt>类型</dt><dd>${esc(file.mime_type || typeLabel(file))}</dd>
      <dt>大小</dt><dd>${fmtBytes(file.size_bytes)}</dd>
      <dt>上传时间</dt><dd>${fmtTime(file.created_at)}</dd>
      <dt>文件 ID</dt><dd>#${file.id}</dd>
    </dl>`;
  if (!canStartWorkflow()) {
    return `${details}
      <div class="form">
        <button class="btn" data-action="download-file" data-id="${file.id}">${icon("download")}下载原文件</button>
        <div class="result">文件已进入材料池。具备 AI 员工管理权限后，可以派 worker 做深度分析。</div>
      </div>`;
  }
  return `
    ${details}
    <div class="form">
      <div class="field"><label>分析文件 ID</label><input class="input" id="materialFileIDs" value="${esc(fileIDs.join(","))}"></div>
      <div class="field"><label>整理目标</label><textarea class="textarea" id="materialInstruction" placeholder="例如：提炼公司基本信息、制度、联系人、项目背景和待确认问题"></textarea></div>
      <div class="form-row">
        <div class="field"><label>Worker</label><select class="select" id="materialWorker">${workerOptions(false)}</select></div>
        <div class="field"><label>任务标题</label><input class="input" id="materialTitle" value="整理公司资料"></div>
      </div>
      <button class="btn primary" data-action="start-material">${icon("send")}启动资料分析</button>
      <button class="btn" data-action="download-file" data-id="${file.id}">${icon("download")}下载原文件</button>
      ${actionResult()}
    </div>`;
}

function taskInspector(task) {
  const participants = taskParticipantText(task);
  const me = Number(state.me?.id || 0);
  const canReview = task.status === "done" && (state.me?.is_superadmin || Number(task.assigner_id) === me ||
    (task.participants || []).some(p => Number(p.user_id) === me && p.role === "reviewer"));
  const canManage = ["pending", "in_progress", "awaiting_input", "done"].includes(task.status) &&
    (state.me?.is_superadmin || Number(task.assigner_id) === me);
  const actions = canReview || canManage ? `
		<div class="form">
		  <div class="field"><label>评语 / 原因</label><textarea class="textarea" id="taskActionReason" placeholder="验收评语可留空；打回或取消必须填写原因"></textarea></div>
		  ${canManage ? `<div class="field"><label>替代任务 ID（仅合并重复任务时填写）</label><input class="input" id="taskReplacementID" inputmode="numeric" placeholder="例如 42"></div>` : ""}
		  <div class="form-row">
		    ${canReview ? `<button class="btn primary" data-action="accept-task" data-id="${task.id}">${icon("check")}验收通过</button>
		    <button class="btn" data-action="reject-task" data-id="${task.id}">${icon("arrow-back-up")}打回</button>` : ""}
		    ${canManage ? `<button class="btn danger" data-action="cancel-task" data-id="${task.id}">${icon("x")}取消 / 合并</button>` : ""}
		  </div>
		</div>` : "";
  return `
    <dl class="kv">
      <dt>任务</dt><dd>#${task.id} ${esc(task.title)}</dd>
      <dt>状态</dt><dd>${statusPill(task.status)}</dd>
	  <dt>需求版本</dt><dd>v${esc(task.revision || 1)}</dd>
      <dt>责任人</dt><dd>${esc(task.assignee_name || "")}</dd>
      <dt>参与者</dt><dd>${esc(participants || "无额外参与者")}</dd>
      <dt>发起人</dt><dd>${esc(task.assigner_name || "")}</dd>
      <dt>优先级</dt><dd>${esc(task.priority || "normal")}</dd>
      <dt>创建</dt><dd>${fmtTime(task.created_at) || ""}</dd>
      <dt>更新</dt><dd>${fmtTime(task.updated_at) || ""}（${fmtAge(task.updated_at)}）</dd>
      <dt>截止</dt><dd>${fmtTime(task.deadline) || "未设定"}</dd>
		${task.submitted_by ? `<dt>最近提交</dt><dd>${esc(task.submitted_by_name || `用户 #${task.submitted_by}`)}${task.submitted_at ? ` · ${fmtTime(task.submitted_at)}` : ""}</dd>` : ""}
		${task.cancel_reason ? `<dt>取消原因</dt><dd>${esc(task.cancel_reason)}</dd>` : ""}
		${task.superseded_by ? `<dt>替代任务</dt><dd>#${esc(task.superseded_by)}</dd>` : ""}
		<dt>催办次数</dt><dd>${esc(task.nudge_count || 0)}</dd>
		${task.execution ? `<dt>最近执行</dt><dd>RUN-${esc(task.execution.id)} · ${statusPill(task.execution.status)} · ${esc(task.execution.attempts || 0)} 次尝试</dd>` : ""}
	  </dl>
	  ${task.goal ? `<div class="result"><strong>目标</strong><br>${esc(task.goal)}</div>` : ""}
	  ${task.description ? `<div class="result"><strong>描述</strong><br>${esc(task.description)}</div>` : ""}
	  ${task.acceptance ? `<div class="result"><strong>验收标准</strong><br>${esc(task.acceptance)}</div>` : ""}
	  ${task.latest_progress ? `<div class="result"><strong>最新进度 / 结果${task.latest_progress_at ? ` · ${fmtTime(task.latest_progress_at)}` : ""}</strong><br>${esc(task.latest_progress)}</div>` : ""}
	  ${task.execution?.last_error ? `<div class="result bad">${esc(task.execution.last_error)}</div>` : ""}
	  ${actions}
	  ${state.actionResult ? actionResult() : ""}
	  `;
}

function taskWorkerState(task, block = false) {
	const run = task?.execution;
	if (!run) return "";
	const parts = [`RUN-${run.id}`, statusText(run.status)];
	if (run.failures) parts.push(`失败 ${run.failures} 次`);
	const text = parts.join(" · ");
	return block ? `<div class="subline">${esc(text)}</div>` : ` · ${esc(text)}`;
}

function workerRunInspector(run) {
  if (!run) return `<div class="empty">执行记录不存在或已刷新。</div>`;
  return `<dl class="kv">
    <dt>执行</dt><dd>RUN-${esc(run.id)} ${esc(run.title)}</dd>
    <dt>状态</dt><dd>${statusPill(run.status)}</dd>
    <dt>业务任务</dt><dd>${run.task_id ? `TSK-${esc(run.task_id)} · v${esc(run.task_revision || 1)}` : "独立执行"}</dd>
    <dt>Worker</dt><dd>${esc(run.worker_name || `#${run.worker_id}`)}</dd>
    <dt>发起人</dt><dd>${esc(run.requested_by_name || `#${run.requested_by}`)}</dd>
    <dt>执行器</dt><dd>${esc(run.executor || "agent")}</dd>
    <dt>证据层级</dt><dd>${esc(workerEvidenceScopeText(run.evidence_scope))}</dd>
    <dt>执行层结果</dt><dd>${esc(run.outcome || "尚未结束")}${run.exit_code !== null && run.exit_code !== undefined ? ` · exit ${esc(run.exit_code)}` : ""}</dd>
    <dt>上下文</dt><dd>${esc(run.scope_title || run.scope_key || "默认")}</dd>
    <dt>尝试 / 失败</dt><dd>${esc(run.attempts || 0)} / ${esc(run.failures || 0)}</dd>
    <dt>创建</dt><dd>${fmtTime(run.created_at)}</dd>
    <dt>更新</dt><dd>${fmtTime(run.updated_at)}</dd>
  </dl>
  ${run.last_error ? `<div class="result bad">${esc(run.last_error)}</div>` : ""}
  ${run.summary ? `<div class="result"><strong>执行摘要</strong><br>${esc(run.summary)}</div>` : ""}`;
}

function workerEvidenceScopeText(scope) {
  if (scope === "process_execution") return "进程执行证据，不等于业务验收";
  if (scope === "agent_submission") return "Agent 提交证据，不等于独立验收";
  return "执行记录";
}

function scheduleInspector(s) {
  return `
    <dl class="kv">
      <dt>规则</dt><dd>#${s.id} ${esc(scheduleTitle(s))}</dd>
      <dt>状态</dt><dd>${statusPill(s.status)}</dd>
      <dt>目标</dt><dd>${esc(scheduleTarget(s))}</dd>
      <dt>创建人</dt><dd>${esc(s.creator_name || "")}</dd>
      <dt>类型</dt><dd>${esc(s.kind)}</dd>
      <dt>模式</dt><dd>${esc(s.mode || "message")}</dd>
      <dt>下次触发</dt><dd>${fmtTime(s.fire_at) || ""}</dd>
      <dt>上次触发</dt><dd>${fmtTime(s.last_fired) || "未触发"}</dd>
      <dt>创建时间</dt><dd>${fmtTime(s.created_at) || ""}</dd>
    </dl>
    <div class="result">${esc(scheduleSubtitle(s))}</div>`;
}

function workerInspector(worker) {
  const c = worker.capability || {};
  return `
    <dl class="kv">
      <dt>Worker</dt><dd>#${worker.id} ${esc(worker.name)}</dd>
      <dt>状态</dt><dd>${worker.online ? '<span class="pill green">在线</span>' : '<span class="pill amber">离线</span>'}</dd>
      <dt>Admin</dt><dd>${worker.admin ? "是" : "否"}</dd>
      <dt>引擎</dt><dd>${esc(c.Engine || c.engine || "")}</dd>
      <dt>工作目录</dt><dd>${esc(c.Workdir || c.workdir || "")}</dd>
      <dt>能力</dt><dd>${esc((c.Capabilities || c.capabilities || []).join(" / "))}</dd>
    </dl>`;
}

function upgradeForm() {
  return `
    <div class="form">
      <div class="form-row">
        <div class="field"><label>Git ref</label><input class="input" id="upgradeRef" value="origin/main"></div>
        <div class="field"><label>Admin Worker</label><select class="select" id="upgradeWorker">${workerOptions(true)}</select></div>
      </div>
      <div class="field"><label>源码目录</label><input class="input" id="upgradeRepo" placeholder="默认使用 NBCO_REPO_DIR 或 $HOME/src/nbco"></div>
      <div class="field"><label>任务标题</label><input class="input" id="upgradeTitle" placeholder="升级系统到 origin/main"></div>
      <label class="checkline"><input type="checkbox" id="upgradeConfirm">确认执行生产升级工作流</label>
      <button class="btn danger" data-action="start-upgrade">${icon("rocket")}创建升级任务</button>
      ${actionResult("升级会创建一个 worker 命令任务，在同一个任务内完成测试、构建、重启、健康检查和回滚。")}
    </div>`;
}

function modelForm() {
  const models = state.ai?.loaded_models || [];
  const current = state.ai?.runtime_model || "";
  return `
    <div class="form">
      <dl class="kv">
        <dt>当前模型</dt><dd>${esc(state.ai?.current_model || "未读取")}</dd>
        <dt>默认模型</dt><dd>${esc(state.ai?.default_model || "未配置")}</dd>
        <dt>模型来源</dt><dd>${state.ai?.runtime_model ? "运行时设置" : "配置文件默认"}</dd>
      </dl>
      <div class="field">
        <label>已加载模型</label>
        <select class="select" id="modelSelect">
          <option value="">恢复默认模型</option>
          ${models.map(m => `<option value="${esc(m)}" ${m === current ? "selected" : ""}>${esc(m)}</option>`).join("")}
        </select>
      </div>
      <label class="checkline"><input type="checkbox" id="streamReasoning" ${state.ai?.stream_reasoning ? "checked" : ""}>展示模型推理过程</label>
      <button class="btn primary" data-action="apply-model">${icon("device-floppy")}保存设置</button>
      ${actionResult(state.ai?.loaded_models_error ? `已加载模型读取失败：${state.ai.loaded_models_error}` : "模型切换只允许选择当前已加载模型。")}
    </div>`;
}

function actionResult(fallback = "等待操作。") {
  if (!state.actionResult) return `<div class="result">${esc(fallback)}</div>`;
  return `<div class="result ${state.actionOK ? "ok" : "bad"}">${esc(state.actionResult)}</div>`;
}

function workerOptions(onlyAdmin) {
  const workers = state.workers.filter(w => !onlyAdmin || w.admin);
  return `<option value="">自动选择</option>` + workers.map(w => `<option value="${w.id}">#${w.id} ${esc(w.name)} · ${w.online ? "在线" : "离线"}${w.admin ? " · admin" : ""}</option>`).join("");
}

function opsTable() {
  const ops = state.ops || {};
  const engine = ops.engine || {};
  const eino = ops.eino_runtime || {};
  const semantic = ops.semantic_index || {};
  const files = ops.file_index || {};
  const messages = ops.message_index || {};
  const engineFails = Number(engine.consecutive_fails || 0);
  return `
    <table class="data-table">
      <tbody>
        <tr><th>版本</th><td>${esc(ops.version || "")}</td></tr>
        <tr><th>Go</th><td>${esc(ops.go || "")}</td></tr>
        <tr><th>AI 引擎</th><td>${engine.configured ? (engineFails ? `<span class="pill red">连续失败 ${engineFails}</span>` : `<span class="pill green">正常</span>`) : `<span class="pill amber">未配置</span>`}</td></tr>
        <tr><th>最近错误</th><td>${esc(engine.last_error || "无")}</td></tr>
        <tr><th>Eino 会话</th><td>${Number(eino.sessions || 0)} 个 · ${Number(eino.events || 0)} 条事件 · ${fmtBytes(eino.storage_bytes)}</td></tr>
        <tr><th>待恢复检查点</th><td>${Number(eino.checkpoints || 0)}${eino.last_event_at ? ` · 最近事件 ${fmtTime(eino.last_event_at)}` : ""}</td></tr>
        <tr><th>语义索引</th><td>${semantic.configured ? (semantic.available ? `<span class="pill green">正常</span>` : `<span class="pill red">不可用</span>`) : `<span class="pill amber">未配置</span>`}${semantic.last_sync_at ? ` · 对账 ${fmtTime(semantic.last_sync_at)} · ${Number(semantic.last_sync_docs || 0)} 条` : ""}</td></tr>
        <tr><th>聊天索引</th><td>${messages.configured ? `${Number(messages.indexed || 0)}/${Number(messages.total || 0)} 已索引 · ${Number(messages.pending || 0)} 待补齐` : `<span class="pill amber">未配置向量索引</span>`}</td></tr>
        <tr><th>文件正文索引</th><td>${Number(files.indexed || 0)}/${Number(files.total || 0)} 已索引 · ${Number(files.chunks || 0)} 分块 · ${Number(files.pending || 0)} 待处理 · ${Number(files.processing || 0)} 处理中</td></tr>
        <tr><th>文件向量索引</th><td>${files.vector_configured ? `${Number(files.vector_indexed || 0)} 已索引 · ${Number(files.vector_pending || 0)} 待处理 · ${Number(files.vector_processing || 0)} 处理中 · ${Number(files.vector_failed || 0)} 失败` : `<span class="pill amber">未配置向量索引</span>`}</td></tr>
        <tr><th>文件索引异常</th><td>${Number(files.failed || 0)} 失败 · ${Number(files.unsupported || 0)} 无提取器 · ${Number(files.empty || 0)} 无文本 · ${Number(files.truncated || 0)} 截断</td></tr>
        <tr><th>迁移</th><td>${esc((ops.migrations || []).join(", "))}</td></tr>
      </tbody>
    </table>`;
}

function capabilityDomainRows() {
  const map = new Map();
  for (const c of state.capabilities) {
    const domain = c.domain || "unknown";
    const row = map.get(domain) || { total: 0, available: 0 };
    row.total++;
    if (c.available) row.available++;
    map.set(domain, row);
  }
  return [...map.entries()].sort().map(([domain, row]) => `<tr><td>${esc(domain)}</td><td>${row.available}</td><td>${row.total}</td></tr>`).join("");
}

function renderLogsOnly() {
  const el = document.querySelector("#logs");
  if (!el) return;
  const actionRows = state.actionTurns.map(t => ({
    time: fmtTime(t.created_at),
    source: "action",
    level: actionLogLevel(t),
    message: actionLogMessage(t),
    status: actionOutcomeLabel(t),
    duration: `${Number(t.success_tool_count || 0)}/${Number(t.tool_count || 0)} returned`,
  }));
  const rows = [...actionRows, ...state.logs];
  if (!rows.length) {
    el.innerHTML = `<div class="empty">暂无日志。</div>`;
    return;
  }
  el.innerHTML = `
    <table class="log-table">
      <thead><tr><th>时间</th><th>来源</th><th>级别</th><th>消息</th><th>状态</th><th>工具/耗时</th></tr></thead>
      <tbody>${rows.map(l => `<tr><td>${l.time}</td><td>${esc(l.source)}</td><td>${logPill(l.level)}</td><td>${esc(l.message)}</td><td>${esc(l.status)}</td><td>${esc(l.duration)}</td></tr>`).join("")}</tbody>
    </table>`;
}

function actionLogLevel(t) {
  const outcome = String(t.outcome || "");
  if (outcome === "action_tool_returned" || outcome === "read_tool_returned" || outcome === "evidence_ok") return "INFO";
  if (outcome === "tool_handler_error") return "ERROR";
  if (outcome.includes("blocked") || outcome.includes("without_success")) return "WARN";
  return "DEBUG";
}

function actionOutcomeLabel(t) {
  const labels = {
    action_tool_returned: "动作工具已返回",
    read_tool_returned: "只读工具已返回",
    answered_without_tool: "未调用工具",
    tool_handler_error: "工具错误",
    pending_approval: "待确认",
    evidence_ok: "历史：曾判定已执行",
    planned_without_tool: "未执行",
    tool_attempted_without_success_evidence: "工具失败",
    blocked_action_evidence: "历史版本拦截",
    blocked_no_tool_completion: "历史版本拦截",
    no_result: "无结果",
  };
  return labels[t.outcome] || t.outcome || "";
}

function actionLogMessage(t) {
  const intent = t.intent ? `「${t.intent}」` : "动作";
  const input = truncate(t.user_text_excerpt || "", 70);
  const reply = truncate(t.reply_excerpt || "", 90);
  const tools = (t.expected_tools || []).join(", ");
  const actualTools = actionToolEvidenceLabel(t);
  const toolHint = actualTools ? `｜工具：${actualTools}` : (tools ? `｜计划工具：${tools}` : "");
  return `${intent}｜${input}${reply ? ` → ${reply}` : ""}${toolHint}`;
}

function actionToolEvidenceLabel(t) {
  const ev = t.evidence && Array.isArray(t.evidence.tool_evidence) ? t.evidence.tool_evidence : [];
  if (!ev.length) return "";
  return ev.slice(0, 4).map(x => `${x.tool || "tool"}:${(x.handler_returned ?? x.ok) ? "returned" : "handler_error"}`).join(", ");
}

function selectedItem() {
  const selected = state.selected;
  if (!selected) return null;
  if (selected.kind === "file") return { ...selected, item: state.files.find(f => Number(f.id) === Number(selected.id)) };
  if (selected.kind === "task") return { ...selected, item: allTaskSource().find(t => Number(t.id) === Number(selected.id)) };
  if (selected.kind === "workerRun") return { ...selected, item: [...state.workerRuns, ...state.workerRunHistory].find(run => Number(run.id) === Number(selected.id)) };
  if (selected.kind === "schedule") return { ...selected, item: state.schedules.find(s => Number(s.id) === Number(selected.id)) };
  if (selected.kind === "worker") return { ...selected, item: state.workers.find(w => Number(w.id) === Number(selected.id)) };
  if (selected.kind === "risk") return selected;
  return null;
}

function mergedTasks() {
  const seen = new Set();
  const out = [];
  for (const list of [state.tasks.todo, state.tasks.review, state.tasks.assigned]) {
    for (const t of list || []) {
      if (seen.has(Number(t.id))) continue;
      seen.add(Number(t.id));
      out.push(t);
    }
  }
  return out.sort((a, b) => Number(b.id) - Number(a.id));
}

function taskQueueSource() {
	if (state.me?.is_superadmin) return state.taskQueue;
  return mergedTasks();
}

function allTaskSource() {
  const seen = new Set();
  const out = [];
  for (const task of [...taskQueueSource(), ...(state.taskReview || []), ...(state.taskHistory || []), ...mergedTasks()]) {
    if (!task || seen.has(Number(task.id))) continue;
    seen.add(Number(task.id));
    out.push(task);
  }
  return out;
}

function countTaskStatuses(tasks) {
  const counts = { pending: 0, in_progress: 0, awaiting_input: 0, done: 0 };
  for (const task of tasks || []) {
    if (Object.prototype.hasOwnProperty.call(counts, task.status)) counts[task.status]++;
  }
  return counts;
}

function taskParticipantText(task) {
  const labels = { collaborator: "协作", reviewer: "验收", watcher: "关注" };
  const groups = new Map();
  for (const participant of task?.participants || []) {
    const role = labels[participant.role] || "参与";
    const names = groups.get(role) || [];
    names.push(participant.user_name || `用户 #${participant.user_id}`);
    groups.set(role, names);
  }
  return [...groups.entries()].map(([role, names]) => `${role}：${names.join("、")}`).join("；");
}

function taskOwnerCell(task) {
  const participants = taskParticipantText(task);
  return `<div class="title-strong">${esc(task.assignee_name || "未指定")}</div>${participants ? `<div class="subline">${esc(participants)}</div>` : ""}`;
}

function fmtAge(value) {
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "未知";
  const seconds = Math.max(0, Math.floor((Date.now() - at.getTime()) / 1000));
  if (seconds < 60) return "刚刚";
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`;
  return `${Math.floor(seconds / 86400)} 天前`;
}

function scheduleTitle(s) {
  if (s.title) return s.title;
  if (s.kind === "daily") {
    const days = s.weekdays ? ` 周${s.weekdays}` : " 每天";
    return `${s.daily_at || fmtTime(s.fire_at)}${days}`;
  }
  if (s.kind === "repeat") return `每 ${s.interval_s || 0} 秒`;
  return `单次 ${fmtTime(s.fire_at)}`;
}

function scheduleSubtitle(s) {
  if (s.title) {
    const days = s.weekdays ? `周${s.weekdays}` : "每天";
    if (s.kind === "daily") return `${days} ${s.daily_at || fmtTime(s.fire_at)}`;
    if (s.kind === "repeat") return `每 ${s.interval_s || 0} 秒`;
    return `单次 ${fmtTime(s.fire_at)}`;
  }
  return truncate(s.message || "", 72);
}

function scheduleTarget(s) {
  if (s.target === "_all") return "全体成员";
  if (s.target === "self") return s.receiver_name || "自己";
  return s.receiver_name || s.target || "";
}

function selectedFileList() {
  return [...state.selectedFileIDs].sort((a, b) => a - b);
}

function filePriority(file) {
  const size = Number(file.size_bytes || 0);
  if (size > 20 * 1024 * 1024) return "高";
  if (/\.(pdf|xlsx?|csv|docx?|zip)$/i.test(file.original_name || "")) return "中";
  return "低";
}

function typeLabel(file) {
  const name = String(file.original_name || "").toLowerCase();
  if (name.endsWith(".pdf")) return "文档";
  if (/\.(xlsx?|csv)$/.test(name)) return "表格";
  if (/\.(png|jpe?g|webp|heic)$/.test(name)) return "图片";
  if (/\.(zip|rar|7z)$/.test(name)) return "压缩包";
  return "文件";
}

function priorityPill(value) {
  const v = String(value || "");
  if (v === "高" || v === "high") return `<span class="pill red">高</span>`;
  if (v === "中" || v === "normal") return `<span class="pill amber">中</span>`;
  if (v === "低" || v === "low") return `<span class="pill green">低</span>`;
  return `<span class="pill blue">${esc(v || "普通")}</span>`;
}

function statusPill(status) {
  const map = {
    active: ["green", "活跃"],
    cancelled: ["amber", "已取消"],
    pending: ["amber", "待处理"],
    in_progress: ["blue", "运行中"],
    awaiting_input: ["amber", "待补充"],
    done: ["teal", "待验收"],
    accepted: ["green", "已完成"],
    split: ["blue", "已拆分"],
    queued: ["amber", "排队中"],
    claimed: ["blue", "执行中"],
    retry_wait: ["amber", "等待重试"],
    completed: ["green", "执行完成"],
  };
  const [cls, label] = map[status] || ["blue", status || "未知"];
  return `<span class="pill ${cls}">${esc(label)}</span>`;
}

function statusText(status) {
  const labels = {
    pending: "待处理", in_progress: "运行中", awaiting_input: "待补充", done: "待验收",
    accepted: "已完成", split: "已拆分", cancelled: "已取消", queued: "排队中",
    claimed: "执行中", retry_wait: "等待重试", completed: "执行完成",
  };
  return labels[status] || status || "未知";
}

function logPill(level) {
  const cls = level === "ERROR" ? "red" : level === "WARN" ? "amber" : "green";
  return `<span class="pill ${cls}">${esc(level)}</span>`;
}

function taskProgress(status) {
  switch (status) {
  case "accepted":
    return 100;
  case "done":
    return 86;
  case "in_progress":
    return 58;
  case "awaiting_input":
    return 44;
  case "split":
    return 42;
  default:
    return 18;
  }
}

async function enter() {
  try {
    state.me = await api("/api/me");
    addLog("auth", "INFO", `登录 ${state.me.name}`);
    renderApp();
    await loadRoute(state.route);
  } catch (err) {
    if (storage.token || tg) renderLogin(err.message);
  }
}

async function loadRoute(route) {
  state.route = route;
  syncBrowserLocation();
  state.loading = true;
  state.notice = "";
  renderApp();
  try {
    if (route === "chat" || route === "workspace") {
      state.loading = false;
      renderApp();
      return;
    }
    if (route === "files") await settleLoads("文件中心", [loadFiles(), loadAdminData(["workers", "capabilities"])]);
    else if (route === "tasks") await settleLoads("任务中心", [loadTasks(), loadSchedules("all"), loadAdminData(["taskQueue", "workerRuns"])]);
		else if (route === "people") await loadPeople();
    else if (route === "workers") await loadAdminData(["workers"]);
		else if (route === "learning") await loadAdminData(["learning", "evals"]);
    else if (route === "model") await loadAdminData(["ai", "capabilities"]);
		else if (route === "ops") await loadAdminData(["ops", "ai", "workers", "actionTurns", "capabilities"]);
    else await loadCommandData();
  } catch (err) {
    state.notice = err.message;
    addLog("ui", "ERROR", err.message);
  } finally {
    state.loading = false;
    ensureSelection();
    renderApp();
  }
}

async function loadCommandData() {
  await settleLoads("控制中心", [loadFiles(), loadTasks(), loadSchedules("all"), loadAdminData(["taskQueue", "workerRuns", "workers", "workflows", "capabilities", "decisions", "approvals", "actionTurns", "ops", "ai"])]);
}

async function settleLoads(label, jobs) {
  const results = await Promise.allSettled(jobs);
  const failures = results.filter(x => x.status === "rejected").map(x => x.reason);
  if (!failures.length) return results;
  const first = failures[0] instanceof Error ? failures[0].message : String(failures[0]);
  const suffix = failures.length > 1 ? `（另有 ${failures.length - 1} 项失败）` : "";
  throw new Error(`${label}部分数据加载失败：${first}${suffix}`);
}

async function loadFiles() {
  const data = await api("/api/files?limit=40&since_hours=720");
  state.files = data.files || [];
  state.fileIntakes = data.intakes || [];
  state.materials = data.materials || [];
}

async function loadTasks() {
  const [todo, review, assigned] = await Promise.all([
    api("/api/me/tasks"),
    api("/api/me/review"),
    api("/api/me/assigned"),
  ]);
  state.tasks.todo = todo.tasks || [];
  state.tasks.review = review.tasks || [];
  state.tasks.assigned = assigned.tasks || [];
}

async function loadSchedules(status = "active") {
  const data = await api(`/api/schedules?status=${encodeURIComponent(status)}`);
  state.schedules = data.schedules || [];
}

async function loadPeople() {
	const data = await api("/api/users?limit=100");
	state.users = data.users || [];
}

async function loadAdminData(parts) {
  const jobs = [];
	if (state.me?.is_superadmin && parts.includes("taskQueue")) {
		jobs.push(api("/api/admin/task-queue?scope=queue").then(d => { state.taskQueue = d.tasks || []; }));
		jobs.push(api("/api/admin/task-queue?scope=review").then(d => { state.taskReview = d.tasks || []; }));
		jobs.push(api("/api/admin/task-queue?scope=history").then(d => { state.taskHistory = d.tasks || []; }));
	}
	if (state.me?.is_superadmin && parts.includes("workerRuns")) {
		jobs.push(api("/api/admin/worker-runs?scope=queue").then(d => { state.workerRuns = d.runs || []; }));
		jobs.push(api("/api/admin/worker-runs?scope=history").then(d => { state.workerRunHistory = d.runs || []; }));
	}
  if (parts.includes("workers")) jobs.push(api("/api/admin/workers").then(d => { state.workers = d.workers || []; }));
  if (parts.includes("workflows")) jobs.push(api("/api/admin/workflows").then(d => { state.workflows = d.workflows || []; }));
  if (parts.includes("capabilities")) jobs.push(api("/api/admin/capabilities").then(d => { state.capabilities = d.capabilities || []; }));
  if (parts.includes("decisions")) jobs.push(api("/api/admin/decisions").then(d => { state.decisions = d.decisions || []; }));
  if (state.me?.is_superadmin && parts.includes("approvals")) jobs.push(api("/api/admin/approvals").then(d => { state.approvals = d.approvals || []; }));
  if (parts.includes("actionTurns")) jobs.push(api(`/api/admin/action-turns${state.me?.is_superadmin ? "?scope=all" : ""}`).then(d => { state.actionTurns = d.turns || []; }));
  if (state.me?.is_superadmin && parts.includes("ops")) jobs.push(api("/api/admin/ops").then(d => { state.ops = d; }));
  if (state.me?.is_superadmin && parts.includes("ai")) jobs.push(api("/api/admin/ai-settings").then(d => { state.ai = d; }));
	if (state.me?.is_superadmin && parts.includes("learning")) jobs.push(api("/api/admin/learning").then(d => { state.learning = d; }));
	if (state.me?.is_superadmin && parts.includes("evals")) jobs.push(api("/api/admin/evals").then(d => { state.evals = d; }));
  await settleLoads("管理数据", jobs);
}

async function runEvals(caseID = 0) {
	try {
		setResult(caseID ? `正在运行评测 EVAL-${caseID}…` : "正在运行全部启用评测…");
		renderApp();
		const data = await api("/api/admin/evals/run", { method: "POST", body: JSON.stringify({ case_id: Number(caseID || 0) }) });
		const passed = (data.runs || []).filter(run => run.status === "passed").length;
		setResult(`评测完成：${passed}/${(data.runs || []).length} 通过。`, passed === (data.runs || []).length);
		await loadAdminData(["learning", "evals", "ops"]);
	} catch (err) {
		setResult(err.message, false);
	}
	renderApp();
}

async function runMaintenance(mode) {
	if (mode === "apply" && !window.confirm("立即执行所有到期数据维护？业务事实与审计事实不会被自动删除。")) return;
	try {
		setResult(mode === "apply" ? "正在执行数据维护…" : "正在检查可回收数据…");
		renderApp();
		const data = await api("/api/admin/maintenance/run", { method: "POST", body: JSON.stringify({ mode }) });
		const runs = data.runs || [];
		const inspected = runs.reduce((sum, run) => sum + Number(run.report?.inspected || 0), 0);
		const reclaimed = runs.reduce((sum, run) => sum + Number(run.report?.reclaimed || 0), 0);
		setResult(mode === "apply" ? `维护完成：回收 ${reclaimed} 项。` : `检查完成：发现 ${inspected} 项可回收数据。`);
		await loadAdminData(["ops"]);
	} catch (err) {
		setResult(err.message, false);
	}
	renderApp();
}

async function performGlobalSearch(query) {
	query = String(query || "").trim();
	if (!query) {
		state.search = null;
		state.notice = "";
		renderApp();
		return;
	}
	state.notice = "";
	state.loading = true;
	renderApp();
	try {
		const data = await api(`/api/search?q=${encodeURIComponent(query)}`);
		state.search = { query: data.query || query, resources: data.resources || [], users: data.users || [] };
	} catch (err) {
		state.notice = err.message;
		state.search = { query, resources: [], users: [] };
	} finally {
		state.loading = false;
		renderApp();
	}
}

function canStartWorkflow() {
  return !!state.me?.is_superadmin || state.capabilities.some(c => c.name === "start_workflow" && c.available);
}

function ensureSelection() {
  const selected = selectedItem();
	const allowed = {
		command: new Set(["file", "task", "workerRun", "schedule", "worker", "risk"]),
		files: new Set(["file"]), tasks: new Set(["task", "schedule"]),
		workers: new Set(["worker", "workerRun"]),
	};
	const allowedKinds = allowed[state.route];
	if (allowedKinds && ((selected?.item && allowedKinds.has(selected.kind)) || (selected?.kind === "risk" && allowedKinds.has("risk")))) return;
	state.selected = null;
	if (state.route === "files" && state.files.length) {
    state.selected = { kind: "file", id: state.files[0].id };
	} else if (state.route === "tasks" && taskQueueSource().length) {
    state.selected = { kind: "task", id: taskQueueSource()[0].id };
	} else if (state.route === "tasks" && state.taskReview.length) {
    state.selected = { kind: "task", id: state.taskReview[0].id };
	} else if (state.route === "tasks" && state.schedules.length) {
    state.selected = { kind: "schedule", id: state.schedules[0].id };
	} else if (state.route === "workers" && state.workers.length) {
		state.selected = { kind: "worker", id: state.workers[0].id };
	} else if (state.route === "command" && state.files.length) {
		state.selected = { kind: "file", id: state.files[0].id };
	} else if (state.route === "command" && taskQueueSource().length) {
		state.selected = { kind: "task", id: taskQueueSource()[0].id };
	} else if (state.route === "command" && state.me?.is_superadmin) {
    state.selected = { kind: "risk", id: "model" };
  }
}

function setResult(text, ok = true) {
  state.actionResult = text;
  state.actionOK = ok;
}

async function invokeTool(name, args) {
  return api(`/api/tools/${encodeURIComponent(name)}`, {
    method: "POST",
    body: JSON.stringify(args || {}),
  });
}

async function runTaskAction(action, taskID) {
  const reason = document.querySelector("#taskActionReason")?.value.trim() || "";
  const replacement = Number(document.querySelector("#taskReplacementID")?.value || 0);
  let name;
  let args;
  if (action === "accept-task") {
    name = "accept_task";
    args = { task_id: Number(taskID), comment: reason };
  } else if (action === "reject-task") {
    if (!reason) {
      setResult("打回必须填写原因。", false);
      renderApp();
      return;
    }
    name = "reject_task";
    args = { task_id: Number(taskID), reason };
  } else if (action === "cancel-task") {
    if (!reason) {
      setResult("取消或合并任务必须填写原因。", false);
      renderApp();
      return;
    }
    name = "cancel_assigned_task";
    args = { task_id: Number(taskID), reason };
    if (Number.isInteger(replacement) && replacement > 0) args.superseded_by = replacement;
  } else {
    return;
  }
  try {
    const data = await invokeTool(name, args);
    const ok = data.status !== "rejected";
    setResult(data.result || (ok ? "操作已完成。" : "操作未执行。"), ok);
    addLog("task", ok ? "INFO" : "WARN", `${name} #${taskID}`);
    await loadRoute(state.route);
  } catch (err) {
    setResult(err.message, false);
    renderApp();
  }
}

async function uploadFiles() {
  const input = document.querySelector("#fileInput");
  if (!input?.files?.length) {
    setResult("请选择文件。", false);
    renderApp();
    return;
  }
  const uploaded = [];
  const failures = [];
  const batchRef = typeof crypto.randomUUID === "function" ? crypto.randomUUID() : `${Date.now()}-${Math.random()}`;
  for (const file of input.files) {
    try {
      const fd = new FormData();
      fd.append("file", file);
      fd.append("batch_ref", batchRef);
      const data = await api("/api/files", { method: "POST", body: fd });
      if (data.file?.id) {
        uploaded.push(Number(data.file.id));
        state.selectedFileIDs.add(Number(data.file.id));
      }
    } catch (err) {
      failures.push(`${file.name}: ${err.message}`);
    }
  }
  try {
    await loadFiles();
  } catch (err) {
    failures.push(`刷新文件列表: ${err.message}`);
  }
  if (failures.length) {
    const prefix = uploaded.length ? `已上传 ${uploaded.length} 个文件；` : "";
    setResult(`${prefix}${failures.join("；")}`, false);
  } else {
    setResult(`已上传 ${uploaded.length} 个文件：${uploaded.map(id => "#" + id).join("、")}`);
  }
  if (uploaded.length) state.selected = { kind: "file", id: uploaded[0] };
  renderApp();
}

async function downloadFile(id) {
  const file = state.files.find(f => Number(f.id) === Number(id));
  const headers = {};
  if (tg && !useAccessToken) headers["X-Telegram-Init-Data"] = tg.initData;
  else headers.Authorization = "Bearer " + storage.token;
  const res = await fetch(`/api/files/${id}`, { headers });
  addLog("http", res.ok ? "INFO" : "WARN", `GET /api/files/${id}`, String(res.status));
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    setResult(data.error || "下载失败", false);
    renderApp();
    return;
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = file?.original_name || `file-${id}`;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

async function startMaterial() {
  if (!canStartWorkflow()) {
    setResult("需要 AI 员工管理权限。", false);
    renderApp();
    return;
  }
  const ids = parseIDs(document.querySelector("#materialFileIDs")?.value || selectedFileList().join(","));
  const instruction = document.querySelector("#materialInstruction")?.value.trim() || "";
  const workerID = Number(document.querySelector("#materialWorker")?.value || 0);
  const title = document.querySelector("#materialTitle")?.value.trim() || "";
  if (!ids.length || !instruction) {
    setResult("文件 ID 和整理目标必填。", false);
    renderApp();
    return;
  }
  try {
    const args = { file_ids: ids, instruction, worker_id: workerID || undefined, title: title || undefined };
    const data = await api("/api/admin/workflows/start", { method: "POST", body: JSON.stringify({ name: "material_intake", args }) });
    setResult(data.result || "已启动资料分析。");
    addLog("workflow", "INFO", "material_intake 已启动");
    await loadTasks();
  } catch (err) {
    setResult(err.message, false);
  }
  renderApp();
}

async function startUpgrade() {
  if (!state.me?.is_superadmin) {
    setResult("需要超级管理员权限。", false);
    renderApp();
    return;
  }
  if (!document.querySelector("#upgradeConfirm")?.checked) {
    setResult("请先勾选确认。", false);
    renderApp();
    return;
  }
  try {
    const args = {
      ref: document.querySelector("#upgradeRef")?.value.trim() || "origin/main",
      repo_dir: document.querySelector("#upgradeRepo")?.value.trim() || undefined,
      worker_id: Number(document.querySelector("#upgradeWorker")?.value || 0) || undefined,
      title: document.querySelector("#upgradeTitle")?.value.trim() || undefined,
      confirm: true,
    };
    const data = await api("/api/admin/workflows/start", { method: "POST", body: JSON.stringify({ name: "nbco_upgrade", args }) });
    setResult(data.result || "已创建升级任务。");
    addLog("workflow", "WARN", "系统升级工作流已创建");
    await loadTasks();
  } catch (err) {
    setResult(err.message, false);
  }
  renderApp();
}

async function applyModel() {
  if (!state.me?.is_superadmin) {
    setResult("需要超级管理员权限。", false);
    renderApp();
    return;
  }
  try {
    const model = document.querySelector("#modelSelect")?.value || "";
    const streamReasoning = !!document.querySelector("#streamReasoning")?.checked;
    const data = await api("/api/admin/ai-settings", {
      method: "POST",
      body: JSON.stringify({ model, stream_reasoning: streamReasoning }),
    });
    state.ai = data;
    setResult(model ? `已切换模型：${model}` : "已恢复默认模型。");
    addLog("ai", "WARN", model ? `切换模型 ${model}` : "恢复默认模型");
  } catch (err) {
    setResult(err.message, false);
  }
  renderApp();
}

async function sendChat() {
  const input = document.querySelector("#chatInput");
  const text = input?.value.trim();
  if (!text) return;
  state.chat.push({ role: "me", text });
  if (input) input.value = "";
  renderApp();
  try {
    state.chat.push({ role: "sys", text: "正在处理…" });
    renderApp();
    const data = await api("/api/chat", { method: "POST", body: JSON.stringify({ message: text }) });
    state.chat = state.chat.filter(m => !(m.role === "sys" && m.text === "正在处理…"));
    state.chat.push({ role: "ai", text: data.reply || "" });
  } catch (err) {
    state.chat = state.chat.filter(m => !(m.role === "sys" && m.text === "正在处理…"));
    state.chat.push({ role: "sys", text: `出错了：${err.message}` });
  }
  renderApp();
}

function routeFromAction(action) {
  switch (action) {
  case "go-files":
    return "files";
  case "focus-chat":
    return "chat";
  default:
    return "";
  }
}

document.addEventListener("click", async event => {
  const routeBtn = event.target.closest("[data-route]");
  if (routeBtn) {
    state.actionResult = "";
		state.search = null;
    await loadRoute(routeBtn.dataset.route);
    return;
  }
  const row = event.target.closest("[data-select-kind]");
  if (row && !event.target.closest("button") && event.target.type !== "checkbox") {
    state.selected = { kind: row.dataset.selectKind, id: row.dataset.id };
    state.actionResult = "";
    renderApp();
    return;
  }
  const btn = event.target.closest("[data-action]");
  if (!btn) return;
  const action = btn.dataset.action;
  if (action === "login") {
    storage.token = document.querySelector("#loginToken")?.value.trim() || "";
    useAccessToken = true;
    await enter();
  } else if (action === "logout") {
    disposeIHTMLWorkspace();
    resetIHTMLAuth();
    storage.token = "";
    useAccessToken = !tg;
    state.me = null;
    renderLogin();
  } else if (action === "refresh") {
    await loadRoute(state.route);
  } else if (action === "clear-logs") {
    state.logs = [];
    renderLogsOnly();
	} else if (action === "clear-search") {
		state.search = null;
		renderApp();
  } else if (routeFromAction(action)) {
    await loadRoute(routeFromAction(action));
  } else if (action === "select-risk") {
    if (!state.me?.is_superadmin) return;
    state.selected = { kind: "risk", id: btn.dataset.risk };
    if (state.route !== "command") state.route = "command";
    syncBrowserLocation();
    renderApp();
  } else if (action === "toggle-file") {
    const id = Number(btn.dataset.id);
    btn.checked ? state.selectedFileIDs.add(id) : state.selectedFileIDs.delete(id);
  } else if (action === "toggle-material") {
		const item = [...state.materials, ...(state.ops?.materials?.active || [])].find(x => Number(x.id) === Number(btn.dataset.id));
    for (const file of item?.files || []) {
      if (btn.checked) state.selectedFileIDs.add(Number(file.id));
      else state.selectedFileIDs.delete(Number(file.id));
    }
  } else if (action === "batch-select-files") {
		state.materials.filter(item => item.source !== "workflow" && Number(item.owner_id) === Number(state.me?.id))
			.slice(0, 10).flatMap(item => item.files || []).forEach(f => state.selectedFileIDs.add(Number(f.id)));
    renderApp();
  } else if (action === "start-material-from-selection") {
    if (!canStartWorkflow()) {
      setResult("需要 AI 员工管理权限。", false);
      renderApp();
      return;
    }
    const ids = selectedFileList();
    if (!ids.length && state.files[0]) state.selectedFileIDs.add(Number(state.files[0].id));
    state.selected = { kind: "file", id: selectedFileList()[0] || state.files[0]?.id };
    renderApp();
  } else if (action === "upload-files") {
    await uploadFiles();
  } else if (action === "download-file") {
    await downloadFile(btn.dataset.id);
  } else if (action === "start-material") {
    await startMaterial();
  } else if (["accept-task", "reject-task", "cancel-task"].includes(action)) {
    await runTaskAction(action, btn.dataset.id);
  } else if (action === "start-upgrade") {
    await startUpgrade();
  } else if (action === "choose-model") {
    const select = document.querySelector("#modelSelect");
    if (select) select.value = btn.dataset.model;
  } else if (action === "apply-model") {
    await applyModel();
	} else if (action === "run-evals") {
		await runEvals();
	} else if (action === "run-eval") {
		await runEvals(btn.dataset.id);
	} else if (action === "inspect-maintenance") {
		await runMaintenance("inspect");
	} else if (action === "apply-maintenance") {
		await runMaintenance("apply");
  }
});

document.addEventListener("submit", async event => {
  if (event.target.matches('[data-form="chat"]')) {
    event.preventDefault();
    await sendChat();
  }
});

document.addEventListener("keydown", event => {
  if (event.target?.id === "loginToken" && event.key === "Enter") {
    document.querySelector('[data-action="login"]')?.click();
  }
  if (event.target?.id === "chatInput" && event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    event.target.closest("form")?.requestSubmit();
  }
	if (event.target?.id === "globalSearch" && event.key === "Enter") {
		event.preventDefault();
		performGlobalSearch(event.target.value);
	}
});

if (storage.token || tg) {
  enter();
} else {
  renderLogin();
}

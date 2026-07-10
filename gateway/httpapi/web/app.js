const root = document.querySelector("#root");
const telegramWebApp = window.Telegram && window.Telegram.WebApp;
const tg = telegramWebApp && String(telegramWebApp.initData || "").trim() ? telegramWebApp : null;

const state = {
  me: null,
  route: "command",
  loading: false,
  notice: "",
  files: [],
  selectedFileIDs: new Set(),
  tasks: { todo: [], assigned: [], review: [] },
  taskQueue: [],
  schedules: [],
  workers: [],
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

if (tg) {
  document.body.classList.add("tg-mini");
  tg.ready();
  tg.expand();
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

async function api(path, opts = {}) {
  const started = performance.now();
  const headers = { Authorization: "Bearer " + storage.token, ...(opts.headers || {}) };
  if (opts.body && !(opts.body instanceof FormData) && !Object.prototype.hasOwnProperty.call(headers, "Content-Type")) {
    headers["Content-Type"] = "application/json";
  }
  const res = await fetch(path, { ...opts, headers });
  const duration = `${Math.max(1, Math.round(performance.now() - started))}ms`;
  const level = res.ok ? "INFO" : "WARN";
  addLog("http", level, `${opts.method || "GET"} ${path}`, String(res.status), duration);
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
  root.innerHTML = `
    <main class="login">
      <h1>nbco</h1>
      <p>AI 运营控制中心。使用你的 Access Token 登录。</p>
      <input id="loginToken" type="password" autocomplete="off" placeholder="Access Token">
      <button class="btn primary" style="width:100%;margin-top:10px" data-action="login">进入控制中心</button>
      <div class="error">${esc(error)}</div>
    </main>`;
  document.querySelector("#loginToken")?.focus();
}

function navItems() {
  const base = [
    ["command", "sliders", "命令队列"],
    ["files", "file-upload", "文件中心"],
    ["tasks", "checkbox", "任务队列"],
    ["chat", "messages", "对话"],
  ];
  if (state.me?.is_superadmin || canStartWorkflow()) {
    base.splice(3, 0, ["workers", "robot", "Worker 管理"]);
  }
  if (state.me?.is_superadmin) {
    base.splice(4, 0,
      ["model", "brain", "模型管理"],
      ["ops", "shield-check", "系统升级"],
    );
  }
  return base;
}

function renderApp() {
  if (!state.me) {
    renderLogin();
    return;
  }
  const activeWorkers = state.workers.filter(w => w.online).length;
  const engineFails = Number(state.ops?.engine?.consecutive_fails || 0);
  const currentModel = state.ai?.current_model || state.ai?.default_model || "未读取";
  const statusText = state.me?.is_superadmin ? (engineFails ? "AI 异常" : "系统正常") : "控制台在线";
  root.innerHTML = `
    <div class="app">
      <aside class="sidebar">
        <div class="brand"><span class="brand-mark">n</span><span>nbco</span></div>
        <nav class="nav">
          ${navItems().map(([route, ico, label]) => `
            <button data-route="${route}" class="${state.route === route ? "active" : ""}">
              ${icon(ico)}<span>${label}</span>
            </button>`).join("")}
        </nav>
        <div class="user-box">
          <div class="user-row">
            <div class="avatar">${esc((state.me.name || "n").slice(0, 1).toUpperCase())}</div>
            <div>
              <div class="user-name">${esc(state.me.name)}</div>
              <div class="user-role">${state.me.is_superadmin ? "超级管理员" : "成员"}</div>
            </div>
          </div>
        </div>
      </aside>
      <header class="topbar">
        <div class="command-actions">
          <button class="btn primary" data-action="focus-chat">${icon("plus")}新建命令</button>
          <button class="btn" data-action="go-files">${icon("upload")}传文件</button>
          ${state.me?.is_superadmin ? `<button class="btn" data-action="select-risk" data-risk="model">${icon("switch-2")}改模型</button>
          <button class="btn danger" data-action="select-risk" data-risk="upgrade">${icon("rocket")}升级</button>` : ""}
        </div>
        <div class="command-search">
          ${icon("search")}
          <input id="globalSearch" placeholder="搜索命令 / 任务 / 员工 / 文件 / 日志">
        </div>
        <div class="status-strip">
          <span><span class="dot ${engineFails ? "red" : "green"}"></span>${statusText}</span>
          <span>${tg ? "Mini App 模式：开" : "Mini App 模式：关"}</span>
          <span>Worker ${activeWorkers}/${state.workers.length}</span>
          ${state.me?.is_superadmin ? `<span title="${esc(currentModel)}">模型 ${esc(shortModel(currentModel))}</span>` : ""}
          <button class="btn subtle" data-action="refresh">${icon("refresh")}刷新</button>
          <button class="btn subtle" data-action="logout">${icon("logout")}退出</button>
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
  case "model":
    el.innerHTML = renderModelRoute();
    break;
  case "ops":
    el.innerHTML = renderOpsRoute();
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

function renderCommandRoute() {
  const pendingFiles = state.files.length;
  const allTasks = taskQueueSource();
  const queuedTasks = allTasks.filter(t => ["pending", "in_progress", "done"].includes(t.status));
  const activeSchedules = (state.schedules || []).filter(s => s.status === "active");
  const approvals = state.approvals || [];
  const riskCount = state.me?.is_superadmin ? approvals.length : 0;
  const decisions = state.decisions.length;
  const activeWorkers = state.workers.filter(w => w.online).length;
  const materialActions = canStartWorkflow()
    ? `<button class="btn" data-action="batch-select-files">${icon("checks")}选中最近文件</button>
      <button class="btn primary" data-action="start-material-from-selection">${icon("send")}派 Worker</button>`
    : `<button class="btn" data-action="batch-select-files">${icon("checks")}选中最近文件</button>`;
  const riskActions = state.me?.is_superadmin
    ? `<button class="btn" data-action="select-risk" data-risk="model">${icon("switch-2")}模型</button>
      <button class="btn danger" data-action="select-risk" data-risk="upgrade">${icon("rocket")}升级</button>`
    : "";
  return `
    <div class="metrics">
      ${metric("folder-up", "待处理材料", pendingFiles, `${selectedFileList().length} 个已选`)}
      ${metric("player-play", "任务队列", queuedTasks.length, `${allTasks.length} 个可见任务`)}
      ${metric("robot", "Worker 可用", activeWorkers, `总数 ${state.workers.length}`)}
      ${metric("calendar-time", "定时自动化", activeSchedules.length, `${state.schedules.length} 条可见规则`)}
      ${metric("alert-triangle", "需要确认", riskCount + decisions, `${decisions} 个决策项`)}
    </div>
    ${queueSection("待处理材料", pendingFiles, renderMaterialRows(), materialActions)}
    ${queueSection("任务队列", queuedTasks.length, renderTaskRows(queuedTasks.slice(0, 12)), `
      <button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
    `)}
    ${queueSection("定时自动化", activeSchedules.length, renderScheduleRows(activeSchedules.slice(0, 8)), `
      <button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
    `)}
    ${queueSection("待确认高风险操作", riskCount, renderRiskRows(approvals), riskActions)}
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

function renderMaterialRows() {
  if (!state.files.length) return `<div class="empty">最近没有上传材料。</div>`;
  return `
    <table class="data-table">
      <thead><tr><th></th><th>ID</th><th>类型</th><th>标题</th><th>提交人</th><th>优先级</th><th>提交时间</th><th>状态</th><th>SLA</th></tr></thead>
      <tbody>${state.files.slice(0, 12).map(f => {
        const selected = state.selected?.kind === "file" && Number(state.selected.id) === Number(f.id);
        const checked = state.selectedFileIDs.has(Number(f.id));
        return `<tr class="selectable ${selected ? "selected" : ""}" data-select-kind="file" data-id="${f.id}">
          <td><input type="checkbox" data-action="toggle-file" data-id="${f.id}" ${checked ? "checked" : ""}></td>
          <td>MAT-${f.id}</td>
          <td>${esc(typeLabel(f))}</td>
          <td class="td-title"><div class="title-strong">${esc(f.original_name)}</div><div class="subline">${esc(f.mime_type || "未知类型")} · ${fmtBytes(f.size_bytes)}</div></td>
          <td>${esc(state.me?.name || "我")}</td>
          <td>${priorityPill(filePriority(f))}</td>
          <td>${fmtTime(f.created_at)}</td>
          <td><span class="pill blue">待分类</span></td>
          <td><span class="pill amber">待指令</span></td>
        </tr>`;
      }).join("")}</tbody>
    </table>`;
}

function renderTaskRows(tasks = taskQueueSource().filter(t => t.status !== "accepted").slice(0, 10)) {
  if (!tasks.length) return `<div class="empty">当前没有运行中任务。</div>`;
  return `
    <table class="data-table">
      <thead><tr><th></th><th>ID</th><th>命令</th><th>Worker/员工</th><th>发起人</th><th>开始时间</th><th>进度</th><th>状态</th><th>预计完成</th></tr></thead>
      <tbody>${tasks.map(t => {
        const progress = taskProgress(t.status);
        const selected = state.selected?.kind === "task" && Number(state.selected.id) === Number(t.id);
        return `<tr class="selectable ${selected ? "selected" : ""}" data-select-kind="task" data-id="${t.id}">
          <td><input type="checkbox"></td>
          <td>RUN-${t.id}</td>
          <td class="td-title"><div class="title-strong">${esc(t.title)}</div><div class="subline">优先级 ${esc(t.priority || "normal")}</div></td>
          <td>${esc(t.assignee_name || "")}</td>
          <td>${esc(t.assigner_name || "")}</td>
          <td>${fmtTime(t.created_at) || "未知"}</td>
          <td><div class="progress"><span style="width:${progress}%"></span></div></td>
          <td>${statusPill(t.status)}</td>
          <td>${t.deadline ? fmtTime(t.deadline) : "待评估"}</td>
        </tr>`;
      }).join("")}</tbody>
    </table>`;
}

function renderScheduleRows(items = state.schedules || []) {
  if (!items.length) return `<div class="empty">暂无定时自动化。</div>`;
  return `
    <table class="data-table">
      <thead><tr><th>ID</th><th>规则</th><th>目标</th><th>创建人</th><th>模式</th><th>状态</th><th>下次触发</th><th>上次触发</th></tr></thead>
      <tbody>${items.map(s => `<tr class="selectable" data-select-kind="schedule" data-id="${s.id}">
        <td>SCH-${s.id}</td>
        <td class="td-title"><div class="title-strong">${esc(scheduleTitle(s))}</div><div class="subline">${esc(scheduleSubtitle(s))}</div></td>
        <td>${esc(scheduleTarget(s))}</td>
        <td>${esc(s.creator_name || "")}</td>
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
      ${renderMaterialRows()}
    </section>`;
}

function renderTasksRoute() {
  const global = state.me?.is_superadmin
    ? `${queueSection("全局任务队列", state.taskQueue.length, taskTable(state.taskQueue), `
        <button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
      `)}
      ${queueSection("定时自动化", state.schedules.length, renderScheduleRows(state.schedules), `
        <button class="btn" data-action="refresh">${icon("refresh")}刷新</button>
      `)}`
    : `${queueSection("定时自动化", state.schedules.length, renderScheduleRows(state.schedules), "")}`;
  return `
    ${global}
    ${queueSection("我的待办", state.tasks.todo.length, taskTable(state.tasks.todo), "")}
    ${queueSection("待验收", state.tasks.review.length, taskTable(state.tasks.review), "")}
    ${queueSection("我分配的", state.tasks.assigned.length, taskTable(state.tasks.assigned), "")}`;
}

function taskTable(tasks) {
  if (!tasks.length) return `<div class="empty">（空）</div>`;
  return `
    <table class="data-table">
      <thead><tr><th>ID</th><th>标题</th><th>状态</th><th>执行人</th><th>发起人</th><th>截止</th></tr></thead>
      <tbody>${tasks.map(t => `<tr class="selectable" data-select-kind="task" data-id="${t.id}">
        <td>#${t.id}</td><td class="td-title"><div class="title-strong">${esc(t.title)}</div></td><td>${statusPill(t.status)}</td>
        <td>${esc(t.assignee_name || "")}</td><td>${esc(t.assigner_name || "")}</td><td>${fmtTime(t.deadline) || "未设定"}</td>
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
  return `
    <div class="two-col">
      <section class="surface section">
        <div class="section-head"><h2>生产升级</h2><span class="pill red">高风险</span></div>
        <div class="inspector-body">${upgradeForm()}</div>
      </section>
      <section class="surface section">
        <div class="section-head"><h2>运维状态</h2></div>
        ${opsTable()}
      </section>
    </div>`;
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

function renderInspector() {
  const el = document.querySelector("#inspector");
  if (!el) return;
  if (state.route === "chat") {
    el.innerHTML = inspectorFrame("对话上下文", "messages", `<div class="result">临时问题走对话；文件分析、升级、模型切换等高影响动作建议走命令队列。</div>`);
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
  return `
    <dl class="kv">
      <dt>任务</dt><dd>#${task.id} ${esc(task.title)}</dd>
      <dt>状态</dt><dd>${statusPill(task.status)}</dd>
      <dt>执行人</dt><dd>${esc(task.assignee_name || "")}</dd>
      <dt>发起人</dt><dd>${esc(task.assigner_name || "")}</dd>
      <dt>优先级</dt><dd>${esc(task.priority || "normal")}</dd>
      <dt>创建</dt><dd>${fmtTime(task.created_at) || ""}</dd>
      <dt>更新</dt><dd>${fmtTime(task.updated_at) || ""}</dd>
      <dt>截止</dt><dd>${fmtTime(task.deadline) || "未设定"}</dd>
      <dt>催办次数</dt><dd>${esc(task.nudge_count || 0)}</dd>
    </dl>
    <div class="result">任务验收、打回、拆分等深层动作仍建议在 Telegram/对话中通过工具执行；控制台先负责总览和入口。</div>`;
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
      <div class="field"><label>任务标题</label><input class="input" id="upgradeTitle" placeholder="升级 nbco 到 origin/main"></div>
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
  const engineFails = Number(engine.consecutive_fails || 0);
  return `
    <table class="data-table">
      <tbody>
        <tr><th>版本</th><td>${esc(ops.version || "")}</td></tr>
        <tr><th>Go</th><td>${esc(ops.go || "")}</td></tr>
        <tr><th>AI 引擎</th><td>${engine.configured ? (engineFails ? `<span class="pill red">连续失败 ${engineFails}</span>` : `<span class="pill green">正常</span>`) : `<span class="pill amber">未配置</span>`}</td></tr>
        <tr><th>最近错误</th><td>${esc(engine.last_error || "无")}</td></tr>
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
    duration: `${Number(t.success_tool_count || 0)}/${Number(t.tool_count || 0)} tools`,
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
  if (outcome === "evidence_ok") return "INFO";
  if (outcome.includes("blocked") || outcome.includes("without_success")) return "WARN";
  return "DEBUG";
}

function actionOutcomeLabel(t) {
  const labels = {
    evidence_ok: "已执行",
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
  return ev.slice(0, 4).map(x => `${x.tool || "tool"}:${x.ok ? "ok" : "fail"}`).join(", ");
}

function selectedItem() {
  const selected = state.selected;
  if (!selected) return null;
  if (selected.kind === "file") return { ...selected, item: state.files.find(f => Number(f.id) === Number(selected.id)) };
  if (selected.kind === "task") return { ...selected, item: taskQueueSource().find(t => Number(t.id) === Number(selected.id)) };
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
  if (state.me?.is_superadmin && state.taskQueue.length) return state.taskQueue;
  return mergedTasks();
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
    done: ["teal", "待验收"],
    accepted: ["green", "已完成"],
    split: ["blue", "已拆分"],
  };
  const [cls, label] = map[status] || ["blue", status || "未知"];
  return `<span class="pill ${cls}">${esc(label)}</span>`;
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
    if (storage.token) renderLogin(err.message);
  }
}

async function loadRoute(route) {
  state.route = route;
  state.loading = true;
  state.notice = "";
  renderApp();
  try {
    if (route === "chat") {
      state.loading = false;
      renderApp();
      return;
    }
    if (route === "files") await settleLoads("文件中心", [loadFiles(), loadAdminData(["workers", "capabilities"])]);
    else if (route === "tasks") await settleLoads("任务中心", [loadTasks(), loadSchedules("all"), loadAdminData(["taskQueue"])]);
    else if (route === "workers") await loadAdminData(["workers"]);
    else if (route === "model") await loadAdminData(["ai", "capabilities"]);
    else if (route === "ops") await loadAdminData(["ops", "ai", "workers", "actionTurns"]);
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
  await settleLoads("控制中心", [loadFiles(), loadTasks(), loadSchedules("all"), loadAdminData(["taskQueue", "workers", "workflows", "capabilities", "decisions", "approvals", "actionTurns", "ops", "ai"])]);
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

async function loadAdminData(parts) {
  const jobs = [];
  if (state.me?.is_superadmin && parts.includes("taskQueue")) jobs.push(api("/api/admin/task-queue?scope=all").then(d => { state.taskQueue = d.tasks || []; }));
  if (parts.includes("workers")) jobs.push(api("/api/admin/workers").then(d => { state.workers = d.workers || []; }));
  if (parts.includes("workflows")) jobs.push(api("/api/admin/workflows").then(d => { state.workflows = d.workflows || []; }));
  if (parts.includes("capabilities")) jobs.push(api("/api/admin/capabilities").then(d => { state.capabilities = d.capabilities || []; }));
  if (parts.includes("decisions")) jobs.push(api("/api/admin/decisions").then(d => { state.decisions = d.decisions || []; }));
  if (state.me?.is_superadmin && parts.includes("approvals")) jobs.push(api("/api/admin/approvals").then(d => { state.approvals = d.approvals || []; }));
  if (parts.includes("actionTurns")) jobs.push(api(`/api/admin/action-turns${state.me?.is_superadmin ? "?scope=all" : ""}`).then(d => { state.actionTurns = d.turns || []; }));
  if (state.me?.is_superadmin && parts.includes("ops")) jobs.push(api("/api/admin/ops").then(d => { state.ops = d; }));
  if (state.me?.is_superadmin && parts.includes("ai")) jobs.push(api("/api/admin/ai-settings").then(d => { state.ai = d; }));
  await settleLoads("管理数据", jobs);
}

function canStartWorkflow() {
  return !!state.me?.is_superadmin || state.capabilities.some(c => c.name === "start_workflow" && c.available);
}

function ensureSelection() {
  const selected = selectedItem();
  if (selected?.item || selected?.kind === "risk") return;
  if (state.files.length) {
    state.selected = { kind: "file", id: state.files[0].id };
  } else if (taskQueueSource().length) {
    state.selected = { kind: "task", id: taskQueueSource()[0].id };
  } else if (state.schedules.length) {
    state.selected = { kind: "schedule", id: state.schedules[0].id };
  } else if (state.me?.is_superadmin) {
    state.selected = { kind: "risk", id: "model" };
  } else {
    state.selected = null;
  }
}

function setResult(text, ok = true) {
  state.actionResult = text;
  state.actionOK = ok;
}

async function uploadFiles() {
  const input = document.querySelector("#fileInput");
  if (!input?.files?.length) {
    setResult("请选择文件。", false);
    renderApp();
    return;
  }
  try {
    const uploaded = [];
    for (const file of input.files) {
      const fd = new FormData();
      fd.append("file", file);
      const data = await api("/api/files", { method: "POST", body: fd });
      if (data.file?.id) {
        uploaded.push(Number(data.file.id));
        state.selectedFileIDs.add(Number(data.file.id));
      }
    }
    setResult(`已上传 ${uploaded.length} 个文件：${uploaded.map(id => "#" + id).join("、")}`);
    await loadFiles();
    if (uploaded.length) state.selected = { kind: "file", id: uploaded[0] };
  } catch (err) {
    setResult(err.message, false);
  }
  renderApp();
}

async function downloadFile(id) {
  const file = state.files.find(f => Number(f.id) === Number(id));
  const res = await fetch(`/api/files/${id}`, { headers: { Authorization: "Bearer " + storage.token } });
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
    addLog("workflow", "WARN", "nbco_upgrade 已创建");
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
    await enter();
  } else if (action === "logout") {
    storage.token = "";
    state.me = null;
    renderLogin();
  } else if (action === "refresh") {
    await loadRoute(state.route);
  } else if (action === "clear-logs") {
    state.logs = [];
    renderLogsOnly();
  } else if (routeFromAction(action)) {
    await loadRoute(routeFromAction(action));
  } else if (action === "select-risk") {
    if (!state.me?.is_superadmin) return;
    state.selected = { kind: "risk", id: btn.dataset.risk };
    if (state.route !== "command") state.route = "command";
    renderApp();
  } else if (action === "toggle-file") {
    const id = Number(btn.dataset.id);
    btn.checked ? state.selectedFileIDs.add(id) : state.selectedFileIDs.delete(id);
  } else if (action === "batch-select-files") {
    state.files.slice(0, 10).forEach(f => state.selectedFileIDs.add(Number(f.id)));
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
  } else if (action === "start-upgrade") {
    await startUpgrade();
  } else if (action === "choose-model") {
    const select = document.querySelector("#modelSelect");
    if (select) select.value = btn.dataset.model;
  } else if (action === "apply-model") {
    await applyModel();
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
});

if (storage.token) {
  enter();
} else {
  renderLogin();
}

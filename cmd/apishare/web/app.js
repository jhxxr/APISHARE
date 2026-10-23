/* API Share gateway console. No external UI dependencies. */
const TOKEN_KEY = "apishare_token";
const $ = (id) => document.getElementById(id);
const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
const icon = (name) => '<svg class="icon" aria-hidden="true"><use href="#i-' + name + '"/></svg>';
const modelIcon = (model, family = "") => window.APIShareModelIcons.html(model, family);
const modelLabel = (model, family = "") => window.APIShareModelIcons.labelHTML(model, family);
const fmtUSD = (value) => {
  const amount = Number(value || 0), absolute = Math.abs(amount);
  if (!Number.isFinite(amount)) return "—";
  if (absolute > 0 && absolute < 1e-10) return "$" + amount.toExponential(2);
  const digits = absolute > 0 && absolute < 0.0001 ? Math.min(10, Math.max(4, Math.ceil(-Math.log10(absolute)) + 2)) : 4;
  return "$" + amount.toFixed(digits);
};
const fmtTime = (value) => {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return (value || "").replace("T", " ").slice(0, 19);
  const parts = Object.fromEntries(new Intl.DateTimeFormat("en-US", {
    timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23",
  }).formatToParts(date).map(({ type, value }) => [type, value]));
  return `${parts.year}-${parts.month}-${parts.day} ${parts.hour}:${parts.minute}:${parts.second}`;
};
const number = (value) => Number(value || 0).toLocaleString("zh-CN");
const upstreamFormatNames = { cliproxy: "CLIProxyAPI · 原生协议", "openai-chat": "OpenAI Chat Completions", "openai-responses": "OpenAI Responses", "openai-auto": "OpenAI · 旧配置（跟随客户端）", "opencode-free": "OpenCode 免费模型", opencode: "OpenCode · 旧配置（Zen / Go）", cline: "Cline 免费模型", anthropic: "Anthropic Messages", gemini: "Gemini generateContent" };
const providerURLs = { cliproxy: "http://127.0.0.1:18317", openai: "https://api.openai.com", "opencode-free": "https://opencode.ai/zen", opencode: "https://opencode.ai/zen", cline: "https://api.cline.bot/api/v1", anthropic: "https://api.anthropic.com", gemini: "https://generativelanguage.googleapis.com" };
const openCodeURLs = { zen: providerURLs.opencode, go: "https://opencode.ai/zen/go" };
const isOpenCodeType = (type) => type === "opencode" || type === "opencode-free";
function upstreamFormat(upstream) {
  if (!upstream) return "openai-chat";
  if (upstream.type !== "openai") return upstream.type;
  return { chat: "openai-chat", responses: "openai-responses" }[upstream.openai_protocol] || "openai-auto";
}
function editorUpstreamType() {
  const format = $("editType").value;
  return format.startsWith("openai-") ? "openai" : format;
}
function editorOpenAIProtocol() {
  if ($("editType").value === "cline") return "chat";
  if (isOpenCodeType($("editType").value)) return $("editOpenCodeProtocol").value || "auto";
  return { "openai-chat": "chat", "openai-responses": "responses" }[$("editType").value] || "auto";
}
const tabNames = { dashboard: "仪表盘", keys: "API 密钥", upstreams: "上游服务", models: "模型总览", proxies: "代理配置", logs: "调用日志", announcements: "公告管理", settings: "工作空间设置", deployment: "部署配置" };
const siteTitle = () => document.documentElement.dataset.siteTitle || "API Share";
const tabIcons = { dashboard: "grid", keys: "key", upstreams: "layers", models: "spark", proxies: "link", logs: "activity", announcements: "megaphone", settings: "sliders", deployment: "shield" };
document.body.classList.toggle("sidebar-collapsed", localStorage.getItem("apishare_sidebar_collapsed") === "true");
document.querySelectorAll(".tab").forEach((button) => {
  button.title = tabNames[button.dataset.tab];
  button.setAttribute("aria-label", tabNames[button.dataset.tab]);
});
document.querySelectorAll(".panel > .page-heading").forEach((heading) => {
  const glyph = document.createElement("span");
  glyph.className = "page-heading-icon";
  glyph.setAttribute("aria-hidden", "true");
  glyph.innerHTML = icon(tabIcons[heading.closest(".panel").id.slice(4)] || "grid");
  heading.firstElementChild.prepend(glyph);
});
const validTab = (name) => Object.hasOwn(tabNames, name) || /^upstreams\/[1-9]\d*$/.test(name) || name === "subscriptions";
let token = localStorage.getItem(TOKEN_KEY) || "";
let activeTab = "dashboard";
let logsPage = 1;
let keysCache = [], upsCache = [], logsCache = [];
const weightDrafts = new Map(), weightSaving = new Set();
let actionResolve = null, actionInputType = null;
let settingsReady = false;
const requestVersions = {};
const editProbe = { id: null, models: [], selected: new Set(), initial: "", generation: 0, controller: null, saving: false, closing: false };
const busyContents = new WeakMap();

function setBusy(button, busy, label = "处理中…") {
  if (!button) return;
  if (busy) {
    if (!busyContents.has(button)) busyContents.set(button, button.innerHTML);
    button.innerHTML = icon("refresh") + '<span>' + esc(label) + '</span>';
  } else if (busyContents.has(button)) {
    button.innerHTML = busyContents.get(button);
    busyContents.delete(button);
  }
  button.disabled = busy;
  button.classList.toggle("is-loading", busy);
  button.setAttribute("aria-busy", String(busy));
}
function toast(message, type = "success") {
  const item = document.createElement("div");
  item.className = "toast" + (type === "error" ? " error" : "");
  item.innerHTML = icon(type === "error" ? "info" : "check") + "<span>" + esc(message) + "</span>";
  $("toastRegion").append(item);
  while ($("toastRegion").children.length > 3) $("toastRegion").firstElementChild.remove();
  setTimeout(() => item.remove(), type === "error" ? 6500 : 4000);
}
function reportError(error, prefix = "操作失败") {
  if (error.message !== "unauthorized") toast(prefix + "：" + error.message, "error");
}
function updateConnection(online) {
  $("connectionStatus").classList.toggle("offline", !online);
  $("connectionStatus").lastElementChild.textContent = online ? "网关已连接" : "连接异常";
}
async function api(path, options = {}) {
  let response;
  const formData = typeof FormData !== "undefined" && options.body instanceof FormData;
  try {
    response = await fetch(path, { ...options, headers: { ...(formData ? {} : { "Content-Type": "application/json" }), ...(token ? { Authorization: "Bearer " + token } : {}), ...(options.headers || {}) } });
  } catch (error) {
    if (error.name === "AbortError") throw error;
    updateConnection(false);
    throw new Error("无法连接网关，请检查网络后重试");
  }
  if (response.status === 401) {
    showLogin();
    throw new Error("unauthorized");
  }
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    if (response.status >= 500) updateConnection(false);
    const error = new Error(data.error || "请求失败（" + response.status + "）");
    error.status = response.status;
    error.field = data.field || "";
    error.code = data.code || "";
    error.existingName = data.existing_name || "";
    throw error;
  }
  updateConnection(true);
  return data;
}
function stampRefresh() {
  $("lastRefreshed").textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN", { timeZone: "Asia/Shanghai", hour12: false });
}
function emptyRow(columns, title, description = "", action = "", label = "", glyph = "layers") {
  return '<tr><td colspan="' + columns + '"><div class="table-empty">' + icon(glyph) + "<strong>" + esc(title) + "</strong><p>" + esc(description) + "</p>" + (action ? '<button class="btn ghost" onclick="' + action + '">' + icon("plus") + esc(label) + "</button>" : "") + "</div></td></tr>";
}
function errorRow(table, columns, error) {
  $(table).querySelector("tbody").innerHTML = emptyRow(columns, "加载失败", error.message, "refreshCurrent()", "重新加载", "info");
}
function labelTableCells(id) {
  const table = $(id), headings = [...table.querySelectorAll("th")].map((heading) => heading.textContent);
  table.querySelectorAll("tbody tr").forEach((row) => [...row.cells].forEach((cell, index) => { if (cell.colSpan === 1) cell.dataset.label = headings[index]; }));
}
function statusBadge(status) {
  return '<span class="badge ' + (status >= 200 && status < 300 ? "on" : "error") + '">' + Number(status) + "</span>";
}
async function copyText(value, message) {
  try {
    await copyToClipboard(value);
    toast(message);
  } catch (error) { reportError(error, "复制失败"); }
}
function copyEndpoint() { return copyText(location.origin + "/v1", "已复制 OpenAI 兼容接入地址"); }
function copyNewKey() { return copyText($("newKeyValue").textContent, "完整密钥已复制，请妥善保存"); }
function copyKey(id) {
  const key = keysCache.find((key) => key.id === id);
  if (!key?.key) { toast("密钥不存在，请刷新列表后重试", "error"); return; }
  return copyText(key.key, "完整密钥已复制，请妥善保存");
}
function dismissNewKey() { $("newKeyShow").classList.add("hidden"); $("newKeyValue").textContent = ""; }

/* Authentication and navigation */
function showLogin() {
  cancelCoreLogin();
  closeLogDetails();
  $("logColumnsMenu").hidePopover();
  resetAnnouncementComposer();
  if ($("keyModelsDialog").open) { savingKeyModels = false; closeKeyModels(); }
  if ($("actionDialog").open) resolveAction(false);
  if (editorIsOpen()) finishCloseEditor();
  $("loginView").classList.remove("hidden");
  $("appView").classList.add("hidden");
  document.querySelector(".skip-link").classList.add("hidden");
  dismissNewKey();
  toggleSidebar(false);
  clearDeploymentPasswords();
  $("proxyPassword").value = "";
  $("proxyComposer").classList.add("hidden");
  deploymentConfig = null;
}
function showApp(session = {}) {
  if (session.setup_required) { location.replace("/setup/"); return; }
  $("loginView").classList.add("hidden");
  $("appView").classList.remove("hidden");
  document.querySelector(".skip-link").classList.remove("hidden");
  $("loginError").classList.add("hidden");
  if (sessionStorage.getItem(DEPLOYMENT_NOTICE) === "initial") {
    sessionStorage.removeItem(DEPLOYMENT_NOTICE);
    toast("部署配置已完成，欢迎进入工作空间");
  }
  switchTab(validTab(location.hash.slice(1)) ? location.hash.slice(1) : "dashboard", false);
  if (activeTab !== "keys") loadKeys(true);
  if (activeTab !== "upstreams") loadUpstreams(true);
}
async function doLogin(event) {
  event.preventDefault();
  $("loginError").classList.add("hidden");
  setBusy($("loginSubmit"), true, "正在登录…");
  try {
    const response = await fetch("api/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ password: $("loginPassword").value }) });
    const data = await response.json();
    if (!response.ok) throw new Error(response.status === 401 ? "密码不正确，请重新输入" : (data.error || "登录失败"));
    token = data.token;
    localStorage.setItem(TOKEN_KEY, token);
    $("loginPassword").value = "";
    showApp(data);
  } catch (error) {
    $("loginError").textContent = error.message === "Failed to fetch" ? "无法连接网关，请稍后重试" : error.message;
    $("loginError").classList.remove("hidden");
  } finally { setBusy($("loginSubmit"), false); }
  return false;
}
async function logout() {
  try { await api("api/logout", { method: "POST" }); }
  catch (error) { if (error.message !== "unauthorized") { reportError(error, "退出失败"); return; } }
  localStorage.removeItem(TOKEN_KEY);
  token = "";
  showLogin();
}
function syncSidebar() {
  const mobile = matchMedia("(max-width: 768px)").matches;
  const open = mobile && $("sidebar").classList.contains("is-open");
  $("sidebar").inert = mobile && !open;
  document.querySelector(".workspace-main").inert = open;
  document.querySelector(".skip-link").inert = open;
  $("sidebarOverlay").classList.toggle("hidden", !open);
  document.body.classList.toggle("nav-open", open);
  document.querySelector(".mobile-menu").setAttribute("aria-expanded", String(mobile ? open : !document.body.classList.contains("sidebar-collapsed")));
}
function toggleSidebar(open) {
  if (!matchMedia("(max-width: 768px)").matches) {
    if (typeof open !== "boolean") {
      const collapsed = document.body.classList.toggle("sidebar-collapsed");
      localStorage.setItem("apishare_sidebar_collapsed", String(collapsed));
      // Re-measure the chart when the sidebar's width transition finishes.
      setTimeout(() => { if (activeTab === "dashboard") renderTrendChart(); }, 200);
    }
    $("sidebar").classList.remove("is-open");
    syncSidebar();
    return;
  }
  const next = typeof open === "boolean" ? open : !$("sidebar").classList.contains("is-open");
  const restoreFocus = !next && $("sidebar").contains(document.activeElement) && matchMedia("(max-width: 768px)").matches;
  $("sidebar").classList.toggle("is-open", next);
  syncSidebar();
  if (restoreFocus) document.querySelector(".mobile-menu").focus();
  if (next && matchMedia("(max-width: 768px)").matches) document.querySelector(".tab.active").focus();
}
async function switchTab(name, updateHistory = true) {
  if (name === "subscriptions") name = "upstreams";
  const detailID = /^upstreams\/([1-9]\d*)$/.exec(name)?.[1];
  if (editorIsOpen() && !$("editModal").open && Number(detailID) !== editProbe.id) {
    await closeEdit();
    if (editorIsOpen()) return;
  }
  if (detailID) name = "upstreams";
  if (!validTab(name)) name = "dashboard";
  activeTab = name;
  document.querySelectorAll(".tab").forEach((button) => {
    const selected = button.dataset.tab === name;
    button.classList.toggle("active", selected);
    if (selected) button.setAttribute("aria-current", "page"); else button.removeAttribute("aria-current");
  });
  document.querySelectorAll(".panel").forEach((panel) => panel.classList.toggle("hidden", panel.id !== "tab-" + name));
  $("pageCrumb").textContent = tabNames[name];
  document.title = siteTitle() + " · " + tabNames[name];
  const hash = "#" + name + (detailID ? "/" + detailID : "");
  if (updateHistory && location.hash !== hash) history.pushState(null, "", hash);
  toggleSidebar(false);
  if (name !== "keys") dismissNewKey();
  if (!detailID && !editorIsOpen()) cancelCoreLogin();
  await loadCurrentTab();
  if (detailID && (!editorIsOpen() || editProbe.id !== Number(detailID))) openEditUpstream(Number(detailID));
}
async function loadCurrentTab() {
  if (activeTab === "dashboard") return Promise.all([loadStats(), loadRecent()]);
  if (activeTab === "keys") return loadKeys();
  if (activeTab === "upstreams") return Promise.all([loadUpstreams(), editorIsOpen() && coreUI.poolID ? loadCLIProxy() : Promise.resolve()]);
  if (activeTab === "models") return loadModelOverview();
  if (activeTab === "proxies") return loadProxies();
  if (activeTab === "logs") return loadLogs(logsPage);
  if (activeTab === "announcements") return loadAnnouncements();
  if (activeTab === "settings") return loadSettings();
  if (activeTab === "deployment") return loadDeployment();
}
async function refreshCurrent() {
  setBusy($("refreshButton"), true, "");
  try { await loadCurrentTab(); } finally { setBusy($("refreshButton"), false); }
}
document.querySelectorAll(".tab").forEach((button) => button.addEventListener("click", () => switchTab(button.dataset.tab)));
function followHistory() {
  const name = validTab(location.hash.slice(1)) ? location.hash.slice(1) : "dashboard";
  if (!$("appView").classList.contains("hidden")) switchTab(name, false);
}
window.addEventListener("popstate", followHistory);
window.addEventListener("hashchange", followHistory);

/* Overview: only values returned by the gateway are shown. */
function dateKey(date) {
  const parts = Object.fromEntries(new Intl.DateTimeFormat("en-US", {
    timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
  }).formatToParts(date).map(({ type, value }) => [type, value]));
  return `${parts.year}-${parts.month}-${parts.day}`;
}
function addDays(date, offset) {
  const [year, month, day] = date.split("-").map(Number);
  return new Date(Date.UTC(year, month - 1, day + offset)).toISOString().slice(0, 10);
}
let dashboardCache = null, trendMetric = "cost", trendDimension = "overview";
const rankMetrics = { keys: "cost", models: "cost", upstreams: "cost" };
const rankListIDs = { keys: "rankKeys", models: "rankModels", upstreams: "rankUpstreams" };
const rankPalette = ["#b45d8a", "#c95b3e", "#159a95", "#7d8fa0"];
const trendPalette = ["#159a95", "#d65a37", "#7d8fa0"];

function fmtMoney(value) {
  const amount = Number(value || 0);
  if (!Number.isFinite(amount)) return "—";
  return Math.abs(amount) > 0 && Math.abs(amount) < 0.01 ? "$" + amount.toFixed(4) : "$" + amount.toFixed(2);
}
function fmtCompact(value) {
  const amount = Number(value || 0);
  if (!Number.isFinite(amount)) return "—";
  const absolute = Math.abs(amount);
  if (absolute >= 1e9) return (amount / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
  if (absolute >= 1e6) return (amount / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
  if (absolute >= 1e3) return (amount / 1e3).toFixed(1).replace(/\.0$/, "") + "K";
  return number(Math.round(amount));
}
function fmtWide(value) {
  const amount = Number(value || 0);
  if (!Number.isFinite(amount)) return "—";
  const absolute = Math.abs(amount);
  if (absolute >= 1e8) return (amount / 1e8).toFixed(1).replace(/\.0$/, "") + "亿";
  if (absolute >= 1e4) return (amount / 1e4).toFixed(1).replace(/\.0$/, "") + "万";
  return number(Math.round(amount));
}
function fmtRate(value) {
  const amount = Number(value || 0);
  if (!Number.isFinite(amount)) return "—";
  return amount >= 10 ? number(Math.round(amount)) : amount.toFixed(1).replace(/\.0$/, "");
}
function dashLatency(ms) {
  const value = Number(ms) || 0;
  if (value <= 0) return "—";
  return value < 1000 ? Math.round(value) + " ms" : (value / 1000).toFixed(2) + " s";
}
function dashDelta(value, title = "") {
  if (value === null || value === undefined || !Number.isFinite(Number(value))) return '<span class="dash-delta flat"' + (title ? ' title="' + esc(title) + '"' : "") + '>—</span>';
  const amount = Number(value);
  const className = amount > 0.0001 ? "up" : amount < -0.0001 ? "down" : "flat";
  const text = (amount > 0 ? "+" : "") + amount.toFixed(1) + "%";
  return '<span class="dash-delta ' + className + '"' + (title ? ' title="' + esc(title) + '"' : "") + ">" + text + "</span>";
}
function dashSmoothPath(points) {
  if (!points.length) return "";
  let path = "M" + points[0][0].toFixed(1) + " " + points[0][1].toFixed(1);
  if (points.length === 1) return path;
  for (let i = 0; i < points.length - 1; i++) {
    const previous = points[i - 1] || points[i], current = points[i], next = points[i + 1], after = points[i + 2] || next;
    const c1x = current[0] + (next[0] - previous[0]) / 6, c1y = current[1] + (next[1] - previous[1]) / 6;
    const c2x = next[0] - (after[0] - current[0]) / 6, c2y = next[1] - (after[1] - current[1]) / 6;
    path += "C" + c1x.toFixed(1) + " " + c1y.toFixed(1) + " " + c2x.toFixed(1) + " " + c2y.toFixed(1) + " " + next[0].toFixed(1) + " " + next[1].toFixed(1);
  }
  return path;
}
function dashSparkline(values, id) {
  const data = (Array.isArray(values) && values.length ? values : [0]).map((value) => Math.max(0, Number(value) || 0));
  const max = Math.max(...data, 1e-9);
  const points = data.map((value, index) => [2 + index * 96 / Math.max(1, data.length - 1), 37 - value / max * 34]);
  const line = dashSmoothPath(points);
  const last = points[points.length - 1], first = points[0];
  const area = line + " L" + last[0].toFixed(1) + " 37 L" + first[0].toFixed(1) + " 37 Z";
  return '<svg viewBox="0 0 100 40" preserveAspectRatio="none" aria-hidden="true"><defs><linearGradient id="' + id + '" x1="0" y1="0" x2="0" y2="1"><stop stop-color="currentColor" stop-opacity=".16"/><stop offset="1" stop-color="currentColor" stop-opacity="0"/></linearGradient></defs><path d="' + area + '" fill="url(#' + id + ')"/><path d="' + line + '" fill="none" stroke="currentColor" stroke-width="1.4" vector-effect="non-scaling-stroke"/></svg>';
}
function dashNiceMax(value) {
  if (!(value > 0)) return 1;
  const power = 10 ** Math.floor(Math.log10(value));
  const normalized = value / power;
  const step = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10;
  return step * power;
}
function dashAxisLabel(value) {
  if (trendMetric === "cost") {
    if (value >= 10) return "$" + value.toFixed(0);
    if (value >= 1) return "$" + value.toFixed(1).replace(/\.0$/, "");
    return "$" + value.toFixed(2);
  }
  return fmtCompact(value);
}
async function loadStats() {
  const version = requestVersions.stats = (requestVersions.stats || 0) + 1;
  try {
    const data = await api("api/dashboard");
    if (version !== requestVersions.stats) return;
    dashboardCache = data;
    renderDashboardOverview();
    stampRefresh();
  } catch (error) {
    const chart = $("trendChart");
    if (chart) chart.innerHTML = '<div class="dash-chart-empty">无法加载用量数据<br>点击右上角刷新重试</div>';
    reportError(error, "概览加载失败");
  }
}
function renderDashboardOverview() {
  if (!dashboardCache) return;
  renderDashboardKPI();
  renderDashboardCache();
  renderRankings();
  renderTrendChart();
}
function renderDashboardKPI() {
  const kpi = dashboardCache.kpi;
  $("kpiRpm").textContent = fmtRate(kpi.rpm.value);
  $("kpiRpmDelta").innerHTML = dashDelta(kpi.rpm.change, "最近 5 分钟均值，对比前 5 分钟");
  $("kpiCost").textContent = fmtMoney(kpi.cost.value);
  $("kpiCost").title = fmtUSD(kpi.cost.value);
  $("kpiCostDelta").innerHTML = dashDelta(kpi.cost.change, "较昨日同期");
  $("kpiRequests").textContent = fmtWide(kpi.requests.value);
  $("kpiRequests").title = number(kpi.requests.value) + " 次请求";
  $("kpiRequestsDelta").innerHTML = dashDelta(kpi.requests.change, "较昨日同期");
  $("kpiSuccess").textContent = Number(kpi.success.value || 0).toFixed(2) + "%";
  $("kpiSuccessDelta").innerHTML = dashDelta(kpi.success.change, "较前一个 24 小时");
  $("healthRpm").textContent = fmtRate(kpi.health.rpm);
  $("healthTpm").textContent = fmtCompact(kpi.health.tpm);
  $("healthError").textContent = Number(kpi.health.error_rate || 0).toFixed(2) + "%";
  $("healthP50").textContent = dashLatency(kpi.health.p50_ms);
  $("kpiRpmSpark").innerHTML = dashSparkline(kpi.rpm.spark, "kpiSparkRpm");
  $("kpiCostSpark").innerHTML = dashSparkline(kpi.cost.spark, "kpiSparkCost");
  $("kpiRequestsSpark").innerHTML = dashSparkline(kpi.requests.spark, "kpiSparkRequests");
  const trend = dashboardCache.trend;
  let lastRate = 100;
  const successValues = trend.labels.map((_, index) => {
    if ((trend.calls[index] || 0) > 0) lastRate = Number(trend.success[index] || 0) / Number(trend.calls[index]) * 100;
    return lastRate;
  });
  $("kpiSuccessSpark").innerHTML = dashSparkline(successValues, "kpiSparkSuccess");
}
function renderDashboardCache() {
  const cache = dashboardCache.cache;
  const ratio = Math.max(0, Math.min(100, Number(cache.ratio) || 0));
  $("cacheRatio").textContent = ratio.toFixed(1) + "%";
  const circumference = 2 * Math.PI * 48;
  $("cacheRing").setAttribute("stroke-dasharray", (ratio / 100 * circumference).toFixed(1) + " " + circumference.toFixed(1));
  $("cacheRead").textContent = fmtCompact(cache.read);
  $("cacheInput").textContent = fmtCompact(cache.input);
  $("cacheWrite").textContent = fmtCompact(cache.write);
}
function renderRankings() {
  Object.keys(rankListIDs).forEach((dimension) => {
    const metric = rankMetrics[dimension];
    const list = (dashboardCache.rankings[dimension] || []).slice();
    list.sort((left, right) => (Number(right[metric]) || 0) - (Number(left[metric]) || 0));
    $(rankListIDs[dimension]).innerHTML = list.slice(0, 4).map((item, index) => {
      const current = Number(item[metric]) || 0;
      const previous = Number(metric === "cost" ? item.cost_prev : item.calls_prev) || 0;
      const change = previous > 0 ? (current - previous) / previous * 100 : null;
      const values = metric === "cost" ? item.series_cost : item.series_calls;
      const color = rankPalette[index % rankPalette.length];
      return '<li><span class="dash-rank-num">' + (index + 1) + '</span>'
        + (dimension === "models" ? '' : '<span class="dash-rank-dot" style="background:' + color + '"></span>')
        + '<span class="dash-rank-name" title="' + esc(item.name) + '">' + (dimension === "models" ? modelLabel(item.name) : esc(item.name)) + '</span>'
        + '<span class="dash-rank-spark" style="color:' + color + '">' + dashSparkline(values, "rankSpark-" + dimension + "-" + index) + '</span>'
        + '<span class="dash-rank-value"><strong>' + (metric === "cost" ? fmtMoney(current) : fmtWide(current)) + '</strong><small>' + dashDelta(change) + '</small></span></li>';
    }).join("") || '<li class="dash-rank-empty">暂无可排行的调用记录</li>';
  });
}
function dashboardTrendSeries() {
  const trend = dashboardCache.trend;
  if (trendDimension === "overview") {
    return trendMetric === "cost"
      ? [{ name: "缓存 Token 花费", color: trendPalette[0], values: trend.cached_cost || [] }, { name: "未缓存 Token 花费", color: trendPalette[1], values: trend.uncached_cost || [] }]
      : [{ name: "成功请求", color: trendPalette[0], values: trend.success || [] }, { name: "失败请求", color: trendPalette[1], values: trend.failed || [] }];
  }
  const metric = trendMetric === "cost" ? "cost" : "calls";
  const list = (dashboardCache.rankings[trendDimension] || []).slice();
  list.sort((left, right) => (Number(right[metric]) || 0) - (Number(left[metric]) || 0));
  return list.slice(0, 3).map((item, index) => ({
    name: item.name || "未知",
    color: trendPalette[index] || trendPalette[2],
    values: (metric === "cost" ? item.series_cost : item.series_calls) || [],
  }));
}
function renderTrendChart() {
  if (!dashboardCache) return;
  const chart = $("trendChart");
  if (!chart) return;
  const trend = dashboardCache.trend;
  const labels = trend.labels || [];
  const series = dashboardTrendSeries();
  $("trendLegend").innerHTML = series.map((item) => '<span><i style="background:' + item.color + '"></i>' + esc(item.name) + "</span>").join("");
  const seriesValues = series.map((item) => labels.map((_, index) => Math.max(0, Number(item.values[index]) || 0)));
  const total = seriesValues.reduce((sum, values) => sum + values.reduce((part, value) => part + value, 0), 0);
  $("trendTotal").textContent = trendMetric === "cost" ? fmtMoney(total) : fmtWide(total);
  $("trendTotalUnit").textContent = trendDimension === "overview" ? (trendMetric === "cost" ? "近 24 小时消费" : "近 24 小时请求") : "Top 3 · 近 24 小时";
  $("trendNote").textContent = "最近 24 小时";
  if (!series.length || total <= 0) {
    chart.innerHTML = '<div class="dash-chart-empty">最近 24 小时还没有调用记录<br>连接上游并创建密钥后会显示趋势</div>';
    return;
  }
  const width = Math.max(320, chart.clientWidth || 640);
  const height = 278, left = 52, right = width - 12, top = 12, bottom = 244;
  const maxValue = dashNiceMax(Math.max(...seriesValues.flat()));
  const yAt = (value) => bottom - value / maxValue * (bottom - top);
  const xAt = (index) => left + index * (right - left) / Math.max(1, labels.length - 1);
  let svg = '<svg viewBox="0 0 ' + width + " " + height + '" role="img" aria-label="' + esc("最近 24 小时" + (trendMetric === "cost" ? "消费" : "请求数") + "趋势") + '">';
  svg += "<defs>";
  series.forEach((item, index) => {
    svg += '<linearGradient id="trendFill' + index + '" x1="0" y1="0" x2="0" y2="1"><stop stop-color="' + item.color + '" stop-opacity=".34"/><stop offset="1" stop-color="' + item.color + '" stop-opacity=".05"/></linearGradient>';
  });
  svg += "</defs>";
  for (let index = 0; index <= 3; index++) {
    const value = maxValue * index / 3, lineY = yAt(value);
    svg += '<line class="chart-grid" x1="' + left + '" y1="' + lineY.toFixed(1) + '" x2="' + right + '" y2="' + lineY.toFixed(1) + '"/>'
      + '<text class="chart-axis" x="' + (left - 9) + '" y="' + (lineY + 3).toFixed(1) + '" text-anchor="end">' + dashAxisLabel(value) + "</text>";
  }
  // Areas overlap from their own zero baseline, matching the reference console:
  // only the first series stays fully visible, later ones tint the backdrop.
  const areas = [], lines = [];
  series.forEach((item, index) => {
    const topPoints = seriesValues[index].map((value, point) => [xAt(point), yAt(value)]);
    const topPath = dashSmoothPath(topPoints);
    areas.push('<path d="' + topPath + " L" + right.toFixed(1) + " " + bottom + " L" + left.toFixed(1) + " " + bottom + ' Z" fill="url(#trendFill' + index + ')"/>');
    lines.push('<path d="' + topPath + '" fill="none" stroke="' + item.color + '" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"/>');
  });
  svg += areas.reverse().join("") + lines.reverse().join("");
  svg += '<line class="trend-guide" x1="0" y1="' + top + '" x2="0" y2="' + bottom + '"/>';
  const labelStep = width < 520 ? 4 : width < 760 ? 3 : 2;
  for (let index = labels.length - 1; index >= 0; index -= labelStep) {
    svg += '<text class="chart-axis" x="' + xAt(index).toFixed(1) + '" y="' + (height - 8) + '" text-anchor="middle">' + esc(labels[index]) + "</text>";
  }
  const hitWidth = (right - left) / Math.max(1, labels.length - 1);
  labels.forEach((label, index) => {
    const detail = series.map((item) => item.name + " " + (trendMetric === "cost" ? fmtMoney(item.values[index]) : fmtWide(item.values[index]))).join(" · ");
    const text = label + " · " + detail;
    let hitX = xAt(index) - hitWidth / 2, widthValue = hitWidth;
    if (index === 0) { hitX = left; widthValue = hitWidth / 2; }
    if (index === labels.length - 1) widthValue = hitWidth / 2;
    svg += '<rect class="trend-hit" tabindex="0" role="img" aria-label="' + esc(text) + '" data-label="' + esc(text) + '" data-x="' + xAt(index).toFixed(1) + '" x="' + hitX.toFixed(1) + '" y="' + top + '" width="' + widthValue.toFixed(1) + '" height="' + (bottom - top) + '"></rect>';
  });
  chart.innerHTML = svg + "</svg>";
  const guide = chart.querySelector(".trend-guide");
  const note = $("trendNote");
  chart.querySelectorAll(".trend-hit").forEach((hit) => {
    const show = () => {
      note.textContent = hit.dataset.label;
      guide.setAttribute("x1", hit.dataset.x);
      guide.setAttribute("x2", hit.dataset.x);
      guide.style.opacity = "1";
    };
    hit.onmouseenter = show;
    hit.onfocus = show;
  });
  chart.onmouseleave = () => {
    note.textContent = "最近 24 小时";
    if (guide) guide.style.opacity = "0";
  };
}
function setTrendMetric(metric) {
  if (metric !== "cost" && metric !== "calls") return;
  trendMetric = metric;
  document.querySelectorAll("[data-trend-metric]").forEach((button) => {
    const active = button.dataset.trendMetric === metric;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  });
  if (dashboardCache) renderTrendChart();
}
function setTrendDimension(dimension) {
  if (dimension !== "overview" && !Object.hasOwn(rankListIDs, dimension)) return;
  trendDimension = dimension;
  document.querySelectorAll("[data-trend-dim]").forEach((button) => {
    const active = button.dataset.trendDim === dimension;
    button.classList.toggle("active", active);
    button.setAttribute("aria-selected", String(active));
  });
  if (dashboardCache) renderTrendChart();
}
function setRankMetric(dimension, metric) {
  if (!Object.hasOwn(rankMetrics, dimension) || (metric !== "cost" && metric !== "calls")) return;
  rankMetrics[dimension] = metric;
  document.querySelectorAll('[data-rank-dim="' + dimension + '"]').forEach((button) => {
    const active = button.dataset.rankMetric === metric;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", String(active));
  });
  if (dashboardCache) renderRankings();
}
async function loadRecent() {
  try {
    const result = await api("api/logs?page=1&size=5");
    $("recentTable").querySelector("tbody").innerHTML = result.items.length ? result.items.map((log) =>
      '<tr><td><span class="cell-stack"><strong class="model-symbol-title">' + modelIcon(log.model) + esc(log.model) + '</strong><small>' + esc(log.upstream_name || "未分配上游") + '</small></span></td><td>' + esc(log.key_name) + '</td><td>' + statusBadge(log.status) + '</td><td>' + number(log.prompt_tokens + log.completion_tokens) + '</td><td><code>' + fmtUSD(log.cost_usd) + '</code></td><td class="timestamp">' + esc(fmtTime(log.created_at).slice(5)) + '</td></tr>'
    ).join("") : emptyRow(6, "还没有调用记录", "连接上游并创建密钥后，开始你的第一次调用。", "switchTab('upstreams')", "连接上游", "activity");
  } catch (error) { errorRow("recentTable", 6, error); }
}

/* API keys */
function toggleKeyComposer(open) {
  $("keyComposer").classList.toggle("hidden", !open);
  if (open) { $("keyComposer").scrollIntoView({ block: "nearest", behavior: motionBehavior() }); $("keyName").focus(); }
}
async function loadKeys(quiet = false) {
  const version = requestVersions.keys = (requestVersions.keys || 0) + 1;
  try {
    const result = await api("api/keys");
    if (version !== requestVersions.keys) return;
    keysCache = result || [];
    $("navKeyCount").textContent = keysCache.length;
    $("navKeyCount").setAttribute("aria-label", keysCache.length + " 个密钥");
    renderKeys();
    stampRefresh();
  } catch (error) { errorRow("keysTable", 7, error); if (!quiet) reportError(error, "密钥加载失败"); }
}
function renderKeys() {
  const query = $("keySearch").value.trim().toLowerCase();
  const keys = keysCache.filter((key) => key.name.toLowerCase().includes(query));
  $("keyTotal").textContent = keysCache.length;
  $("keysTable").querySelector("tbody").innerHTML = keys.map((key) => {
    const masked = key.key.length > 16 ? key.key.slice(0, 8) + "••••" + key.key.slice(-4) : "••••••••";
    const modelIDs = parseAllowlist(key.models);
    const modelSummary = '<small class="key-model-summary" title="' + esc(modelIDs.join(", ")) + '">可用模型：' + (modelIDs.length ? modelIDs.length + " 个" : "未绑定，无法调用") + '</small>';
    const keyCell = '<div class="key-value"><code>' + esc(masked) + '</code><button type="button" class="text-btn key-copy" onclick="copyKey(' + key.id + ')" aria-label="复制 ' + esc(key.name) + ' 的完整密钥" title="复制完整密钥">' + icon("copy") + '<span>复制</span></button></div>';
    return '<tr><td class="cell-stack"><strong>' + esc(key.name) + '</strong><small>#' + key.id + ' · ' + esc((key.created_at || "").slice(0, 10)) + '</small>' + modelSummary + '</td><td>' + keyCell + '</td><td class="quota-cell">' + renderKeyQuotaSummary(key) + '</td><td><code>' + key.qps + " / " + key.concurrency + '</code></td><td><span class="badge ' + (key.enabled ? "on" : "off") + '">' + (key.enabled ? "启用" : "停用") + '</span></td><td><div class="row-actions"><button class="text-btn" onclick="openKeyModels(' + key.id + ')" aria-label="设置 ' + esc(key.name) + ' 的模型与额度">模型与额度</button><button class="text-btn" onclick="toggleKey(' + key.id + ", " + !key.enabled + ', this)" aria-label="' + (key.enabled ? "停用 " : "启用 ") + esc(key.name) + '">' + (key.enabled ? "停用" : "启用") + '</button><button class="icon-btn danger" onclick="deleteKey(' + key.id + ')" aria-label="删除 ' + esc(key.name) + '" title="删除密钥">' + icon("trash") + '</button></div></td></tr>';
  }).join("") || emptyRow(7, query ? "没有匹配的密钥" : "创建你的第一个 API 密钥", query ? "试试其他名称。" : "为成员分配独立的访问权限和调用额度。", query ? "" : "toggleKeyComposer(true)", "创建密钥", "key");
  labelTableCells("keysTable");
}
async function createKey(event) {
  event.preventDefault();
  const form = event.currentTarget, button = form.querySelector('[type="submit"]');
  const name = $("keyName").value.trim();
  if (!name) { $("keyName").setCustomValidity("请输入密钥名称"); $("keyName").reportValidity(); return false; }
  if (!validateKeyModels("key")) return false;
  setBusy(button, true, "正在生成…");
  try {
    const key = await api("api/keys", { method: "POST", body: JSON.stringify({ name, ...keyQuotaPayload("key"), qps: Number($("keyQps").value) || 3, concurrency: Number($("keyConc").value) || 5, models: parseAllowlist($("keyModels").value).join(",") }) });
    $("newKeyValue").textContent = key.key;
    $("newKeyShow").classList.remove("hidden");
    form.reset(); resetKeyModelPicker("key"); toggleKeyComposer(false);
    await loadKeys(); toast("密钥已创建");
  } catch (error) { reportError(error); } finally { setBusy(button, false); }
  return false;
}
async function toggleKey(id, enabled, button) {
  setBusy(button, true, "更新中");
  try { await api("api/keys/" + id, { method: "PATCH", body: JSON.stringify({ enabled }) }); await loadKeys(); toast(enabled ? "密钥已启用" : "密钥已停用"); }
  catch (error) { reportError(error); } finally { setBusy(button, false); }
}

async function deleteKey(id) {
  const key = keysCache.find((item) => item.id === id);
  if (!await askAction({ title: "删除 API 密钥？", description: (key ? "“" + key.name + "”" : "该密钥") + "删除后，使用它的请求会被拒绝。此操作无法撤销。", confirm: "删除密钥", danger: true })) return;
  try { await api("api/keys/" + id, { method: "DELETE" }); await loadKeys(); toast("密钥已删除"); }
  catch (error) { reportError(error); }
}

/* Upstream list */
function parseAllowlist(value) { return [...new Set((value || "").split(/[,，\r\n]/).map((model) => model.trim()).filter(Boolean))]; }
function providerIcon(type, baseURL = "") { return window.APIShareModelIcons.providerHTML(type, baseURL); }
function upstreamWeight(item) { return weightDrafts.has(item.id) ? weightDrafts.get(item.id) : Number(item.weight) || 0; }
function weightStepper(item) {
  const weight = upstreamWeight(item), priority = Number(item.priority) || 0;
  return '<div class="weight-line"><span>优先级 ' + priority + '</span><span>权重</span><span class="weight-stepper" role="group" aria-label="调整权重"><button type="button" class="weight-dec" onclick="changeUpstreamWeight(' + item.id + ', -1, this)" aria-label="降低权重"' + (weight <= 0 ? " disabled" : "") + '>' + icon("minus") + '</button><span class="weight-value" aria-live="polite">' + weight + '</span><button type="button" class="weight-inc" onclick="changeUpstreamWeight(' + item.id + ', 1, this)" aria-label="提高权重">' + icon("plus") + '</button></span></div>';
}
function changeUpstreamWeight(id, delta, button) {
  const item = upsCache.find((upstream) => upstream.id === id);
  if (!item) return;
  const current = upstreamWeight(item), weight = Math.max(0, current + delta);
  if (weight === current) return;
  weightDrafts.set(id, weight);
  const stepper = button.closest(".weight-stepper");
  if (stepper) {
    stepper.querySelector(".weight-value").textContent = weight;
    stepper.querySelector(".weight-dec").disabled = weight <= 0;
  }
  flushUpstreamWeight(id);
}
async function flushUpstreamWeight(id) {
  if (weightSaving.has(id)) return;
  weightSaving.add(id);
  try {
    while (weightDrafts.has(id)) {
      const target = weightDrafts.get(id);
      await api("api/upstreams/" + id, { method: "PATCH", body: JSON.stringify({ weight: target }) });
      const item = upsCache.find((upstream) => upstream.id === id);
      if (item) item.weight = target;
      if (weightDrafts.get(id) === target) weightDrafts.delete(id);
    }
  } catch (error) {
    weightDrafts.delete(id);
    reportError(error, "权重保存失败");
    renderUpstreams();
  } finally { weightSaving.delete(id); }
}
async function loadUpstreams(quiet = false) {
  const version = requestVersions.upstreams = (requestVersions.upstreams || 0) + 1;
  try {
    const [result, overview] = await Promise.all([api("api/upstreams"), api("api/upstreams/overview").catch(() => null), loadProxies(true)]);
    if (version !== requestVersions.upstreams) return;
    upsCache = result || [];
    upstreamOverviewCache = overview;
    $("navUpCount").textContent = upsCache.length;
    $("navUpCount").setAttribute("aria-label", upsCache.length + " 个上游");
    renderUpstreams(); stampRefresh();
  } catch (error) {
    upstreamOverviewCache = null;
    renderUpstreamMetrics();
    $("upstreamListCount").textContent = "上游加载失败";
    errorRow("upsTable", 9, error);
    $("upstreamCardsView").innerHTML = '<div class="table-empty"><strong>加载失败</strong><p>' + esc(error.message) + '</p><button class="btn ghost" onclick="loadUpstreams()">重新加载</button></div>';
    if (!quiet) reportError(error, "上游加载失败");
  }
}
async function toggleUpstream(id, enabled, button) {
  button.disabled = true;
  try { await api("api/upstreams/" + id, { method: "PATCH", body: JSON.stringify({ enabled }) }); await loadUpstreams(); toast(enabled ? "上游已启用" : "上游已停用"); }
  catch (error) { reportError(error); } finally { button.disabled = false; }
}
async function deleteUpstream(id) {
  const item = upsCache.find((upstream) => upstream.id === id);
  if (!await askAction({ title: "删除上游服务？", description: (item ? "“" + item.name + "”" : "此上游") + "将不再接收请求。删除后无法恢复，请确认其他连接可以承接调用。", confirm: "删除上游", danger: true })) return;
  try { await api("api/upstreams/" + id, { method: "DELETE" }); await loadUpstreams(); toast("上游已删除"); }
  catch (error) { reportError(error); }
}
async function duplicateUpstream(id) {
  const item = upsCache.find((upstream) => upstream.id === id);
  if (!item) { toast("未找到该上游，请刷新列表后重试", "error"); return; }
  const name = await askAction({
    title: "复制上游服务？",
    description: item.type === "cliproxy" && item.managed_pool ? "复制“" + item.name + "”的模型、调度和默认代理配置，并创建一个独立的空账号池。新池的账号需要单独授权或导入。" : "将“" + (item.name || "未命名上游") + "”的类型、地址、密钥、模型配置、请求头、代理、优先级与权重完整复制为一条新的上游，并沿用原启用状态。",
    input: (item.name || "未命名上游") + " 副本",
    inputType: "text",
    inputLabel: "新上游名称",
    confirm: "复制上游",
  });
  if (name === false) return;
  try {
    const result = await api("api/upstreams/" + id + "/duplicate", { method: "POST", body: JSON.stringify({ name: String(name).trim() }) });
    await loadUpstreams();
    toast("已复制为“" + (result.name || name) + "”");
  } catch (error) { reportError(error, "复制失败"); }
}
async function testUpstream(id, button) {
  setBusy(button, true, "");
  try {
    const result = await api("api/upstreams/" + id + "/test", { method: "POST" });
    if (!result.ok) throw new Error(result.error || "连接失败");
    toast("连接正常 · " + result.latency_ms + " ms · " + result.model_count + " 个模型");
    await loadUpstreams();
  } catch (error) { reportError(error, "连接测试失败"); } finally { setBusy(button, false); }
}

/* One editor for both creation and updates. */
function editorValue() {
  return JSON.stringify(["editName", "editType", "editOpenCodeTier", "editOpenCodeProtocol", "editBase", "editKey", "editProxy", "editHeaderOverrides", "editStructuredOutput", "editPriority", "editWeight", "editModels"].map((id) => $(id).value).concat($("editEnabled").checked, $("editModelInput").value, $("modelMapBatch").value, [modelMappingDirtyValue()], editorUpstreamType() === "cliproxy" ? coreUI.pending.map(item => [item.name, item.proxy_id]) : [], editorUpstreamType() === "cliproxy" && !coreUI.poolID ? coreUI.defaultProxy : 0));
}
function editorIsOpen() { return $("editModal").open || !$("upstreamDetailPage").classList.contains("hidden"); }
function editorDirty() { return editorIsOpen() && editorValue() !== editProbe.initial; }
function updateEditorDirty() {
  const dirty = editorDirty();
  $("editorSaveStatus").classList.toggle("dirty", dirty || editProbe.id === null);
  $("editorSaveStatus").lastElementChild.textContent = editProbe.saving ? "正在保存配置…" : dirty ? "有未保存的修改" : editProbe.id === null ? "新连接尚未保存" : "配置未改动";
}
function openCreateUpstream() { openEditor(null); }
async function openEditUpstream(id) {
  const upstream = upsCache.find((item) => item.id === id);
  if (!upstream) { toast("未找到该上游，请刷新列表后重试", "error"); return; }
  if (editorIsOpen() && editProbe.id !== id) await closeEdit();
  await openEditor(upstream);
}
async function openEditor(upstream) {
  if (editorIsOpen()) return;
  if (!await loadProxies()) return;
  if (editorIsOpen() || $("appView").classList.contains("hidden")) return;
  const creating = !upstream;
  editProbe.id = upstream ? upstream.id : null;
  editProbe.generation++;
  editProbe.saving = false; editProbe.closing = false; editProbe.models = [];
  $("upstreamEditorForm").reset();
  preparePoolEditor(upstream);
  $("editType").disabled = upstream?.type === "cliproxy" && !!upstream.managed_pool;
  $("editType").querySelector('[value="cliproxy"]').disabled = !!upstream && upstream.type !== "cliproxy";
  $("upstreamEditorForm").querySelectorAll(".form-error").forEach((error) => error.classList.add("hidden"));
  const format = upstreamFormat(upstream);
  $("legacyOpenAIFormatOption").hidden = format !== "openai-auto";
  $("legacyOpenAIFormatOption").disabled = format !== "openai-auto";
  $("legacyOpenCodeFormatOption").hidden = format !== "opencode";
  $("legacyOpenCodeFormatOption").disabled = format !== "opencode";
  const values = { editName: upstream?.name || "", editType: format, editOpenCodeProtocol: isOpenCodeType(upstream?.type) ? upstream.openai_protocol || "auto" : "auto", editBase: upstream?.base_url || providerURLs.openai, editKey: "", editHeaderOverrides: upstream?.header_overrides || "", editStructuredOutput: upstream?.structured_output === "json_schema" ? "" : upstream?.structured_output || "", editPriority: upstream ? upstream.priority : 0, editWeight: upstream ? upstream.weight : 1, editModels: upstream?.models || "", editModelMap: upstream?.model_map || "" };
  Object.entries(values).forEach(([id, value]) => { $(id).value = value; $(id).setCustomValidity(""); $(id).removeAttribute("aria-invalid"); });
  $("editOpenCodeTier").value = upstream?.opencode_tier || ((upstream?.base_url || "").match(/\/go(?:\/|$)/) ? "go" : "zen");
  renderProxyOptions(upstream?.proxy_id || 0);
  editProbe.selected = new Set(parseAllowlist($("editModels").value));
  $("editEnabled").checked = creating || !!upstream.enabled;
  $("editEnabledRow").classList.toggle("hidden", creating);
  $("createEnabledNote").classList.toggle("hidden", !creating);
  $("editWeight").min = creating ? "1" : "0";
  $("editKey").required = creating;
  $("editKey").type = "password";
  $("editKey").placeholder = creating ? "输入上游 API 密钥" : "当前密钥：" + (upstream.key_masked || "已保存") + " · 留空保留";
  $("editKeyRequired").classList.toggle("hidden", !creating);
  $("editKeyHint").textContent = creating ? "用于网关访问上游服务。密钥默认隐藏，请妥善保管。" : "留空会保留当前密钥；填写新密钥将替换原密钥。";
  $("revealKeyButton").setAttribute("aria-pressed", "false");
  $("revealKeyButton").setAttribute("aria-label", "显示 API 密钥");
  $("editTitle").textContent = creating ? "新连接" : "连接 #" + upstream.id;
  $("editorHeading").textContent = creating ? "添加上游服务" : upstream.type === "cliproxy" ? upstream.name : "编辑上游服务";
  $("editorSaveLabel").textContent = creating ? "创建连接" : "保存修改";
  $("editProbeArea").classList.add("hidden");
  $("editProbeList").innerHTML = ""; $("editProbeFilter").value = ""; $("editProbeInfo").textContent = "";
  $("editorError").classList.add("hidden"); $("modelMapError").classList.add("hidden");
  resetModelMappingEditor();
  setBusy($("probeEditorButton"), false);
  updateEditorProvider(false); renderModelChips(); validateHeaderOverrides();
  editProbe.initial = editorValue();
  const poolPage = !creating && upstream.type === "cliproxy" && upstream.managed_pool;
  if (poolPage) {
    $("upstreamListPage").classList.add("hidden");
    $("upstreamDetailPage").classList.remove("hidden");
    $("upstreamDetailPage").append($("upstreamEditorForm"));
    $("editorScroll").prepend($("editorPool"));
    document.querySelector(".editor-nav .nav-label").after($("poolEditorNav"));
    if (location.hash !== "#upstreams/" + upstream.id) history.pushState(null, "", "#upstreams/" + upstream.id);
    $("pageCrumb").textContent = "上游服务 / " + upstream.name;
    document.title = siteTitle() + " · " + upstream.name;
    window.scrollTo({top: 0});
  } else {
    $("editModal").showModal();
    document.body.classList.add("modal-open");
  }
  numberEditorSections();
  $("editorScroll").scrollTop = 0;
  activateEditorSection(poolPage ? "editorPool" : "editorBasic");
  updateEditorDirty();
  if (!poolPage) $("editName").focus();
  if (editorUpstreamType() !== "cliproxy") loadClientHeaderVersions();
  if (upstream?.type === "cliproxy" && upstream.managed_pool) loadCLIProxy();
}
function updateEditorProvider(changed = true) {
  const format = $("editType").value, type = editorUpstreamType();
  const freeOpenCode = type === "opencode-free";
  const pool = type === "cliproxy" && (!editProbe.id || !!upsCache.find(item => item.id === editProbe.id)?.managed_pool);
  const keepZenCredential = editProbe.provider === "opencode" && $("editOpenCodeTier").value !== "go";
  if (changed && freeOpenCode && editProbe.provider !== type && !keepZenCredential) $("editKey").value = "";
  const knownURLs = [...Object.values(providerURLs), ...Object.values(openCodeURLs), "https://opencode.ai/inference", "https://opencode.ai/inference/go"];
  if (changed && (editProbe.id === null || freeOpenCode) && knownURLs.includes($("editBase").value.replace(/\/+$/, ""))) $("editBase").value = type === "opencode" ? openCodeURLs[$("editOpenCodeTier").value] : providerURLs[type];
  const iconType = isOpenCodeType(type) ? "opencode" : type;
  $("editorProviderIcon").className = "editor-provider-brand";
  $("editorProviderIcon").innerHTML = providerIcon(iconType, $("editBase").value);
  $("editorProviderIcon").setAttribute("aria-hidden", "true");
  $("opencodeProtocolField").classList.toggle("hidden", !isOpenCodeType(type));
  $("opencodeTierField").classList.toggle("hidden", type !== "opencode");
  $("editorPool").classList.toggle("hidden", !pool);
  $("poolEditorNav").classList.toggle("hidden", !pool);
  $("editorConnection").classList.toggle("hidden", pool);
  document.querySelector('[data-section="editorConnection"]').classList.toggle("hidden", pool);
  $("editorHeaders").classList.toggle("hidden", type === "cliproxy");
  document.querySelector('[data-section="editorHeaders"]').classList.toggle("hidden", type === "cliproxy");
  $("editHeaderOverrides").disabled = type === "cliproxy";
  $("editStructuredOutput").closest(".field").classList.toggle("hidden",type==="cliproxy");
  $("editStructuredOutput").disabled=type==="cliproxy";
  validateHeaderOverrides();
  $("editBase").disabled = pool;
  $("editKey").disabled = pool;
  $("editBase").required = !pool;
  $("editProxyField").classList.toggle("hidden", type === "cliproxy");
  if (type === "cliproxy") $("editProxy").value = "0";
  $("providerHint").textContent = {
    "openai-chat": "上游固定使用 /v1/chat/completions。适用于支持 Chat Completions 的 OpenAI 兼容服务。",
    "openai-responses": "上游固定使用 /v1/responses。请确认上游支持 Responses 接口。",
    "openai-auto": "旧连接沿用原有行为：跟随 OpenAI 客户端接口，其他格式转换为 Chat。可改选上游实际支持的固定格式。",
    "opencode-free": "保留默认地址，点击「获取模型」即可选择 OpenCode 当前免费模型。无需安装 CLI，API Key 可留空；模型可用性与免费额度由 OpenCode 调整。",
    opencode: "旧连接保留原有 Zen / Go 套餐、密钥和协议。Zen 连接切换为免费渠道时保留已有密钥；Go 连接切换时使用匿名凭据，也可填写自己的 Zen API Key。",
    cliproxy: "一个上游就是一个独立账号池。在这里添加账号、授权登录，并为每个账号指定代理。内部连接由系统自动管理。",
    cline: "点击「登录 Cline 账号」完成浏览器授权，即可获取当前免费模型，无需安装 CLI。也可手动填写账号凭据。免费模型与额度由 Cline 调整。",
    anthropic: "上游使用 Anthropic 原生 /v1/messages 接口。",
    gemini: "上游使用 Gemini 原生 generateContent / streamGenerateContent 接口。"
  }[format];
  $("editKeyLabel").textContent = type === "cline" ? "账号 RefreshToken / API Key" : freeOpenCode ? "API 密钥（可选）" : "API 密钥";
  $("editKey").required = editProbe.id === null && !freeOpenCode && !pool;
  $("editKeyRequired").classList.toggle("hidden", !$("editKey").required);
  $("editKey").setCustomValidity("");
  if (freeOpenCode) $("editKey").placeholder = editProbe.id === null ? "留空使用匿名访问，或填写自己的 Zen API Key" : "留空保留已有 Zen 凭据；填写 public 使用匿名访问";
  else if (changed) $("editKey").placeholder = editProbe.id === null ? "输入上游 API 密钥" : "留空保留已保存凭据";
  $("editKeyHint").textContent = type === "cline" ? "浏览器授权后自动填入；也可粘贴 RefreshToken 或单个账号 JSON。普通 API Key 请加 api-key: 前缀。编辑时留空保留。" : freeOpenCode ? "新连接留空默认匿名访问；旧 Zen 连接切换及免费连接编辑时留空保留凭据，填写 public 可切回匿名。填写 Key 后仍只提供免费模型。" : editProbe.id === null ? "用于网关访问上游服务。密钥默认隐藏，请妥善保管。" : "留空会保留当前密钥；填写新密钥将替换原密钥。";
  $("editBase").placeholder = type === "opencode" ? openCodeURLs[$("editOpenCodeTier").value] : providerURLs[type];
  editProbe.provider = type;
  updateClineLoginProvider(changed);
  numberEditorSections();
  if (changed) { clearProbeResults(); updateEditorDirty(); }
  $("probeEditorButton").innerHTML = icon("refresh") + (pool ? " 获取池内模型" : " 获取当前 Key 模型");
}
function updateOpenCodeTier() {
  const known = [...Object.values(openCodeURLs), "https://opencode.ai/inference", "https://opencode.ai/inference/go"];
  if (known.includes($("editBase").value.replace(/\/+$/, ""))) $("editBase").value = openCodeURLs[$("editOpenCodeTier").value];
  clearProbeResults(); updateEditorDirty();
}
function clearProbeResults() {
  editProbe.generation++;
  editProbe.controller?.abort();
  editProbe.controller = null;
  setBusy($("probeEditorButton"), false);
  editProbe.models = [];
  editProbe.selected = new Set(parseAllowlist($("editModels").value));
  refreshModelSuggestions();
  $("editProbeArea").classList.add("hidden");
  $("editProbeInfo").textContent = "";
}
function toggleKeyVisibility() {
  const visible = $("editKey").type === "password";
  $("editKey").type = visible ? "text" : "password";
  $("revealKeyButton").setAttribute("aria-pressed", String(visible));
  $("revealKeyButton").setAttribute("aria-label", visible ? "隐藏 API 密钥" : "显示 API 密钥");
}
function motionBehavior() { return matchMedia("(prefers-reduced-motion: reduce)").matches ? "instant" : "smooth"; }
function numberEditorSections() {
  let index = 0;
  document.querySelectorAll(".editor-nav-item:not(.hidden)").forEach(button => {
    const number = String(++index).padStart(2, "0");
    button.firstElementChild.textContent = number;
    const marker = $(button.dataset.section).querySelector(":scope > .editor-section-heading > span");
    if (marker) marker.textContent = number;
  });
}
function activateEditorSection(id) {
  document.querySelectorAll(".editor-nav-item").forEach((button) => {
    const active = button.dataset.section === id;
    button.classList.toggle("active", active);
    if (active) button.setAttribute("aria-current", "location"); else button.removeAttribute("aria-current");
  });
}
function jumpEditor(id) {
  if (!$("upstreamDetailPage").classList.contains("hidden")) {
    $(id).scrollIntoView({block: "start", behavior: motionBehavior()});
    activateEditorSection(id); return;
  }
  $("editorScroll").scrollTo({ top: $(id).offsetTop - 24, behavior: motionBehavior() });
  activateEditorSection(id);
}
let editorScrollFrame = 0;
$("editorScroll").addEventListener("scroll", () => {
  if (editorScrollFrame) return;
  editorScrollFrame = requestAnimationFrame(() => {
    editorScrollFrame = 0;
    const scroll = $("editorScroll");
    let section = "editorBasic";
    document.querySelectorAll(".editor-section:not(.hidden)").forEach((item) => { if (item.offsetTop <= scroll.scrollTop + 65) section = item.id; });
    if (scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 5) section = "editorRouting";
    activateEditorSection(section);
  });
}, { passive: true });
function finishCloseEditor() {
  closePoolInspector();
  clearTimeout(poolManagement.jobTimer);
  clearTimeout(coreUI.refreshTimer);
  cancelCoreLogin(false);
  cancelClineLogin();
  clearProbeResults();
  editProbe.saving = false;
  $("editKey").value = ""; $("editKey").type = "password";
  $("editModal").close();
  document.querySelector('[data-section="editorBasic"]').after($("poolEditorNav"));
  if (!$("upstreamDetailPage").classList.contains("hidden")) {
    $("editModal").append($("upstreamEditorForm"));
    $("editorBasic").after($("editorPool"));
    $("upstreamDetailPage").classList.add("hidden");
    $("upstreamListPage").classList.remove("hidden");
    $("pageCrumb").textContent = "上游服务";
    document.title = siteTitle() + " · 上游服务";
    if (/^#upstreams\/\d+$/.test(location.hash)) history.replaceState(null, "", "#upstreams");
  }
  coreUI.epoch++; coreUI.pending = []; coreUI.poolID = null; coreUI.credentials = []; coreUI.status = null;
  $("editType").disabled = false;
  syncModalLock();
}
async function closeEdit() {
  if(poolManagement.busy){toast("请等待当前操作完成，或先取消剩余操作","error");return;}
  if (!editorIsOpen() || editProbe.saving || editProbe.closing) return;
  if (editorDirty()) {
    editProbe.closing = true;
    const discard = await askAction({ title: "放弃未保存的修改？", description: "当前配置尚未保存。返回编辑器可继续修改，放弃后将恢复原有配置。", confirm: "放弃修改", danger: true });
    editProbe.closing = false;
    if (!discard) return;
  }
  finishCloseEditor();
}
function renderEditProbeList() {
  refreshModelSuggestions();
  const query = $("editProbeFilter").value.trim().toLowerCase();
  const models = editProbe.models.filter((model) => model.toLowerCase().includes(query));
  $("editProbeList").innerHTML = models.map((model) => '<label><input type="checkbox" data-model="' + esc(model) + '" ' + (editProbe.selected.has(model) ? "checked" : "") + '>' + modelLabel(model) + '</label>').join("") || '<span class="muted">没有匹配的模型</span>';
  $("editProbeCount").textContent = "已选 " + editProbe.selected.size + " · 可用 " + editProbe.models.length;
  $("editProbeList").querySelectorAll("input").forEach((checkbox) => checkbox.addEventListener("change", () => {
    if (checkbox.checked) editProbe.selected.add(checkbox.dataset.model); else editProbe.selected.delete(checkbox.dataset.model);
    $("editProbeCount").textContent = "已选 " + editProbe.selected.size + " · 可用 " + editProbe.models.length;
    syncEditModels();
  }));
}
function selectAllEditProbe(on) {
  editProbe.selected = on ? new Set([...editProbe.selected, ...editProbe.models]) : new Set();
  renderEditProbeList(); syncEditModels();
}
function formatJSON(id) {
  const field = $(id);
  try {
    if (!field.value.trim()) return;
    const parsed = JSON.parse(field.value);
    field.value = JSON.stringify(parsed, null, 2);
    if (id === "editModelMap") { validateModelMap(); updateEditorDirty(); }
    else toast("JSON 已格式化");
  } catch {
    if (id === "editModelMap") validateModelMap();
    else { toast("JSON 格式不正确，请检查引号、逗号与括号", "error"); field.focus(); }
  }
}
async function probeEditUpstream() {
  if (editorUpstreamType() === "cliproxy" && !coreUI.poolID && !await ensureCorePool()) return;
  const base = $("editBase"), key = $("editKey");
  if (!validateHeaderOverrides()) { jumpEditor("editorHeaders"); $("editHeaderOverrides").reportValidity(); return; }
  if (!base.checkValidity()) { jumpEditor("editorConnection"); base.reportValidity(); return; }
  if (key.required && !key.value.trim()) { jumpEditor("editorConnection"); key.reportValidity(); return; }
  clearProbeResults();
  const generation = editProbe.generation;
  editProbe.controller = new AbortController();
  const probeSignature = () => [$("editType").value, $("editOpenCodeTier").value, editorOpenAIProtocol(), base.value, key.value, $("editProxy").value, $("editHeaderOverrides").value].join("\n");
  const signature = probeSignature();
  const body = { type: editorUpstreamType(), openai_protocol: editorOpenAIProtocol(), opencode_tier: editorUpstreamType() === "opencode-free" ? "zen" : $("editOpenCodeTier").value, base_url: base.value.trim(), proxy_id: Number($("editProxy").value), header_overrides: editorHeaderOverrides() };
  if (key.value.trim()) body.api_key = key.value.trim();
  $("editProbeInfo").className = "probe-info";
  $("editProbeInfo").textContent = editorUpstreamType() === "cliproxy" ? "正在读取当前池的可用模型…" : editorUpstreamType() === "opencode-free" ? "正在获取 OpenCode 免费模型…" : key.value.trim() ? "正在使用当前填写的 Key 获取模型…" : "正在使用已保存的 Key 获取模型…";
  setBusy($("probeEditorButton"), true, "获取中…");
  try {
    const path = editProbe.id === null ? "api/upstream-probe" : "api/upstreams/" + editProbe.id + "/probe";
    const result = await api(path, { method: "POST", body: JSON.stringify(body), signal: editProbe.controller.signal });
    if (generation !== editProbe.generation || signature !== probeSignature()) return;
    if (!result.ok) throw new Error(result.error || "上游未返回模型");
    editProbe.models = [...new Set(result.models || [])];
    editProbe.selected = new Set(parseAllowlist($("editModels").value));
    $("editProbeInfo").className = "probe-info success";
    $("editProbeInfo").textContent = editProbe.models.length ? (editorUpstreamType() === "cliproxy" ? "当前账号池返回 " : editorUpstreamType() === "opencode-free" ? "免费目录返回 " : "当前 Key 返回 ") + editProbe.models.length + " 个模型 · " + result.latency_ms + " ms · 勾选后保存为白名单" : "连接成功，但当前配置未返回可用模型。可检查权限或手动填写白名单。";
    $("editProbeArea").classList.toggle("hidden", editProbe.models.length === 0);
    renderEditProbeList();
  } catch (error) {
    if (generation !== editProbe.generation || error.name === "AbortError" || error.message === "unauthorized" || signature !== probeSignature()) return;
    $("editProbeInfo").className = "probe-info error";
    const status = error.message.match(/HTTP (\d{3})/);
    const hint = status && ({ 401: "当前 Key 认证失败，请检查密钥", 403: "当前 Key 没有模型列表访问权限", 404: "未找到模型接口，请检查 Base URL 和接口类型", 407: "代理认证失败，请检查代理用户名和密码", 429: "上游请求过于频繁，请稍后重试" })[status[1]];
    $("editProbeInfo").textContent = "获取失败：" + (hint ? hint + "：" + error.message : error.message) + "。仍可手动填写白名单。";
  } finally { if (generation === editProbe.generation) setBusy($("probeEditorButton"), false); }
}
async function saveEditUpstream(event) {
  event.preventDefault();
  if(poolManagement.busy){toast("请等待账号操作完成后再保存上游","error");return false;}
  if (editProbe.saving) return false;
  if (!validateHeaderOverrides()) { jumpEditor("editorHeaders"); $("editHeaderOverrides").reportValidity(); return false; }
  commitModelInput();
  if ($("modelMapBatch").value.trim() && !applyModelMapBatch()) { jumpEditor("editorModels"); return false; }
  if (!validateModelMap()) {
    jumpEditor("editorModels");
    const invalid = modelEditor.mode === "json" ? $("editModelMap") : $("modelMapRows").querySelector(":invalid");
    invalid?.reportValidity(); invalid?.focus(); return false;
  }
  if (!$("editName").value.trim()) { $("editName").setCustomValidity("请输入上游名称"); jumpEditor("editorBasic"); $("editName").reportValidity(); return false; }
  const creating = editProbe.id === null;
  if (creating && $("editKey").required && !$("editKey").value.trim()) { $("editKey").setCustomValidity("请输入上游 API 密钥"); jumpEditor("editorConnection"); $("editKey").reportValidity(); return false; }
  const pool = editorUpstreamType() === "cliproxy" && ! $("editBase").required;
  if (!pool) {
    try {
      if (!["http:", "https:"].includes(new URL($("editBase").value).protocol)) throw new Error();
    } catch { $("editBase").setCustomValidity("请输入以 http:// 或 https:// 开头的服务地址"); jumpEditor("editorConnection"); $("editBase").reportValidity(); return false; }
  }
  if (!$("upstreamEditorForm").reportValidity()) return false;
  const body = { name: $("editName").value.trim(), type: editorUpstreamType(), base_url: $("editBase").value.trim().replace(/\/+$/, ""), proxy_id: Number($("editProxy").value), priority: Number($("editPriority").value), weight: Number($("editWeight").value), models: parseAllowlist($("editModels").value).join(","), model_map: $("editModelMap").value.trim(), header_overrides: editorHeaderOverrides(), structured_output: editorUpstreamType()==="cliproxy"?"":$("editStructuredOutput").value };
  body.openai_protocol = editorOpenAIProtocol();
  body.opencode_tier = editorUpstreamType() === "opencode-free" ? "zen" : editorUpstreamType() === "opencode" ? $("editOpenCodeTier").value : "";
  if (!creating) body.enabled = $("editEnabled").checked;
  if ($("editKey").value.trim()) body.api_key = $("editKey").value.trim();
  if (pool) {
    delete body.base_url; delete body.api_key;
    body.proxy_id = 0;
    if (creating) body.pool_proxy_id = coreUI.defaultProxy;
  }
  const pending = pool ? [...coreUI.pending] : [];
  const pendingReport = pool ? [...poolManagement.report] : [];
  editProbe.saving = true; updateEditorDirty();
  $("editorError").classList.add("hidden");
  setBusy($("editorSaveButton"), true, creating ? "正在创建…" : "正在保存…");
  try {
    const result = await api(creating ? "api/upstreams" : "api/upstreams/" + editProbe.id, { method: creating ? "POST" : "PATCH", body: JSON.stringify(body) });
    const savedID = result.id || editProbe.id;
    // Restore the button's labeled DOM before the shared form is reopened.
    setBusy($("editorSaveButton"), false);
    finishCloseEditor();
    if (activeTab !== "upstreams") await switchTab("upstreams"); else await loadUpstreams();
    if (pool) {
      await openEditUpstream(savedID);
      if (pending.length||pendingReport.some(item=>item.status==="error")) await completeStagedPoolImports(pending,savedID,pendingReport);
      await loadCLIProxy();
      updateEditorDirty();
    }
    toast(creating ? "上游连接已创建" : "上游配置已保存");
    return savedID;
  } catch (error) {
    if (error.message !== "unauthorized") {
      console.error("Upstream configuration could not be saved", error);
      $("editorError").textContent = "保存失败：" + error.message;
      $("editorError").classList.remove("hidden");
      $("editorError").scrollIntoView({ block: "nearest" });
    }
  } finally { editProbe.saving = false; setBusy($("editorSaveButton"), false); updateEditorDirty(); }
  return false;
}
$("upstreamEditorForm").addEventListener("input", (event) => {
  if (!event.target.closest(".model-map-editor") && !["editModelMap", "editHeaderOverrides"].includes(event.target.id) && event.target.setCustomValidity) event.target.setCustomValidity("");
  if (["editBase", "editKey", "editHeaderOverrides"].includes(event.target.id)) clearProbeResults();
  if (["editBase", "editKey"].includes(event.target.id)) { cancelClineLogin(); updateClineLoginProvider(); }
  updateEditorDirty();
});
$("upstreamEditorForm").addEventListener("change", updateEditorDirty);
$("editProxy").addEventListener("change", () => { cancelClineLogin(); updateClineLoginProvider(); });
$("upstreamEditorForm").addEventListener("invalid", (event) => {
  const section = event.target.closest(".editor-section");
  if (section) jumpEditor(section.id);
}, true);
$("editModal").addEventListener("cancel", (event) => { event.preventDefault(); closeEdit(); });
$("editModal").addEventListener("click", (event) => {
  if (event.target !== $("editModal")) return;
  const rect = $("editModal").getBoundingClientRect();
  if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) closeEdit();
});

/* Log formatting is shared by the table and request detail drawer. */
const fmtDuration = (ms) => ms >= 1000 ? (ms / 1000).toFixed(2) + "s" : Math.round(ms) + "ms";
function logSpeedCell(log) {
  const first = Number(log.first_token_ms) || 0;
  const total = Number(log.duration_ms) || Number(log.latency_ms) || 0;
  const completion = Number(log.completion_tokens) || 0;
  const lines = [];
  if (first > 0) lines.push("首字 " + fmtDuration(first));
  if (total > 0) lines.push("总耗时 " + fmtDuration(total));
  // 单帧应答或 JSON 上游在首字处就开始下发时，首字几乎等于总耗时，
  // 此时相减只剩几毫秒，会算出荒谬的 TPS；这种情况下改用总耗时。
  const generating = first > 0 && total - first >= total / 10 ? total - first : total;
  if (total > 0 && completion > 0 && generating > 0) {
    lines.push("输出 TPS " + (completion / (generating / 1000)).toFixed(1) + " tok/s");
  }
  if (!lines.length) return '<td><span class="muted">—</span></td>';
  const failed = !(log.status >= 200 && log.status < 300);
  return '<td><span class="speed-list' + (failed ? " failed" : "") + '">' + lines.map((line) => "<span>" + esc(line) + "</span>").join("") + '</span></td>';
}

function logUsageDetails(log) {
  const usage = log.billing?.usage;
  if (!usage) return "";
  const rows = [["普通输入", usage.input], ["缓存读取", usage.cache_read], ["缓存创建", usage.cache_write], ["其中 1 小时缓存", usage.cache_write_1h], ["工具提示", usage.tool_prompt], ["正文输出", usage.output], ["推理", usage.reasoning]];
  if (usage.input_audio || usage.output_audio || usage.cache_read_audio) rows.push(["音频输入", usage.input_audio], ["音频缓存读取", usage.cache_read_audio], ["音频输出", usage.output_audio]);
  const estimate = usage.estimated_input || usage.estimated_output ? '<small>部分用量为估算</small>' : "";
  return '<details><summary>用量明细</summary><span class="cell-stack">' + rows.map(([name, count]) => '<small>' + name + '：' + number(count || 0) + '</small>').join("") + estimate + '</span></details>';
}

function logCostDetails(log) {
  const cost = log.billing?.cost;
  if (!cost) return "";
  const rows = [["普通输入", cost.input], ["缓存读取", cost.cache_read], ["缓存创建", cost.cache_write], ["工具提示", cost.tool_prompt], ["正文输出", cost.output], ["推理", cost.reasoning]];
  if (log.billing.usage?.input_audio || log.billing.usage?.output_audio || log.billing.usage?.cache_read_audio) rows.push(["音频输入", cost.input_audio], ["音频缓存读取", cost.cache_read_audio], ["音频输出", cost.output_audio]);
  const billing = log.billing;
  const sources = { override: "手动覆盖", official: "官方定价", builtin: "内置兜底", default: "默认兜底", none: "未匹配价格" };
  const source = '<small>价格来源：' + esc(sources[billing.price_source] || billing.price_source || "未知") + '</small>';
  const model = billing.pricing_model ? '<small>计价模型：' + modelLabel(billing.pricing_model) + '</small>' : "";
  const upstream = billing.upstream_model ? '<small>上游模型：' + modelLabel(billing.upstream_model) + '</small>' : "";
  const pool = billing.quota_pool ? '<small>扣费额度：' + (billing.quota_pool === "independent" ? '独立额度（' + modelLabel(log.model) + '）' : "共享额度") + '</small>' : "";
  const unpriced = billing.price_source === "none" || (billing.price_source === "default" && Object.values(billing.price || {}).every(value => value == null || value === 0));
  const notice = unpriced ? '<small>未匹配模型单价，当前按零价记录，请在设置中配置价格。</small>' : "";
  return '<details><summary>费用明细</summary><span class="cell-stack">' + rows.map(([name, value]) => '<small>' + name + '：' + fmtUSD(value || 0) + '</small>').join("") + source + model + upstream + pool + notice + '<small>按请求当时的价格计算</small></span></details>';
}

/* Settings */
function applySiteTitle(title) {
  title = String(title || "").trim() || "API Share";
  document.documentElement.dataset.siteTitle = title;
  document.querySelectorAll("[data-site-brand-name]").forEach((node) => { node.textContent = title; });
  document.querySelectorAll("[data-site-brand-title]").forEach((node) => node.setAttribute("title", title));
  document.querySelectorAll("[data-site-brand-home]").forEach((node) => node.setAttribute("aria-label", title + " 首页"));
  const suffix = $("appView").classList.contains("hidden") ? "工作空间" : tabNames[activeTab];
  document.title = title + " · " + suffix;
}
function applySiteIcon(url, type = "image/png") {
  const custom = !!url;
  document.querySelectorAll("[data-site-brand-logo]").forEach((image) => {
    image.src = custom ? url : (image.dataset.defaultSrc || "/public/static/brand/apishare-icon-v2.png");
  });
  document.querySelectorAll("[data-site-brand-icon]").forEach((link) => {
    link.setAttribute("href", custom ? url : (link.dataset.defaultHref || "/public/static/brand/apishare-favicon-v2.png"));
    link.setAttribute("type", custom ? (type || "image/png") : "image/png");
  });
  const reset = $("resetSiteIcon");
  if (reset) reset.classList.toggle("hidden", !custom);
}
function applySiteBranding(settings) {
  applySiteTitle(settings.site_title || "");
  applySiteIcon(settings.site_icon_url || "", settings.site_icon_type || "image/png");
}
async function uploadSiteIcon(input) {
  const file = input.files && input.files[0];
  input.value = "";
  if (!file) return;
  if (file.size > 512 * 1024) { toast("网站图标不能超过 512 KB", "error"); return; }
  const allowed = ["image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon", "image/vnd.microsoft.icon"];
  if (file.type && !allowed.includes(file.type.toLowerCase())) { toast("请选择 PNG、JPG、GIF、WebP 或 ICO 图片", "error"); return; }
  const form = new FormData();
  form.append("icon", file);
  input.disabled = true;
  try {
    const result = await api("api/settings/icon", { method: "POST", body: form });
    applySiteIcon(result.site_icon_url || "", result.site_icon_type || "image/png");
    toast("网站图标已更新");
  } catch (error) { reportError(error, "图标上传失败"); } finally { input.disabled = false; }
}
async function resetSiteIcon() {
  if (!(await askAction({ title: "恢复默认网站图标？", description: "当前上传的图标会被删除，浏览器标签与页面 Logo 将恢复为默认图标。", confirm: "恢复默认" }))) return;
  try {
    await api("api/settings/icon", { method: "DELETE" });
    applySiteIcon("", "image/png");
    toast("已恢复默认网站图标");
  } catch (error) { reportError(error, "恢复默认图标失败"); }
}
async function loadSettings() {
  try {
    const settings = await api("api/settings");
    $("setTtl").value = settings.affinity_ttl_min; $("setRetry").value = settings.retry;
    $("setInject").checked = !!settings.inject_stream_usage;
    $("setLogRetention").value = String(settings.log_retention_days ?? 7);
    if (settings.log_retention_detail) $("logRetentionHint").textContent = settings.log_retention_detail;
    $("setAutoPricing").checked = !!settings.auto_pricing_enabled;
    $("setPricingURL").value = settings.auto_pricing_url || "";
    $("setPricing").value = JSON.stringify(settings.pricing, null, 2);
    $("setSiteTitle").value = settings.site_title || "";
    applySiteBranding(settings);
    setPricingError("");
    renderPricingStatus(settings.auto_pricing);
    settingsReady = true; stampRefresh();
  } catch (error) { settingsReady = false; reportError(error, "设置加载失败"); }
}
function renderPricingStatus(info) {
  $("pricingStatus").textContent = !info?.last_update ? "尚未同步官方定价" : "已同步 " + number(info.model_count) + " 个模型 · " + fmtTime(info.last_update);
}
async function refreshPricing(button) {
  setBusy(button, true, "正在同步…");
  try {
    const result = await api("api/settings/pricing/refresh", { method: "POST" });
    if (!result.ok) throw new Error(result.error || "同步失败");
    renderPricingStatus({ model_count: result.model_count, last_update: result.last_update });
    toast("已同步 " + number(result.model_count) + " 个模型的官方定价");
  } catch (error) { reportError(error, "定价同步失败"); } finally { setBusy(button, false); }
}
async function lookupPrice() {
  const model = $("lookupModel").value.trim();
  if (!model) { $("lookupModel").focus(); return; }
  $("lookupResult").textContent = "正在查询…";
  try {
    const result = await api("api/settings/pricing/lookup?model=" + encodeURIComponent(model));
    const sources = { override: "手动覆盖", official: "官方定价", builtin: "内置兜底", default: "default 兜底", none: "未找到" };
    const price = result.price;
    const cacheRead = price.cache_read ?? price.in;
    const cacheWrite = price.cache_write ?? price.in;
    const displayModel = modelLabel(result.model) + (result.pricing_model && result.pricing_model !== result.model ? "（按 " + modelLabel(result.pricing_model) + " 计价）" : "");
    $("lookupResult").innerHTML = displayModel + esc(" · 输入 $" + price.in + " · 输出 $" + price.out + " · 缓存读取 $" + cacheRead + " · 缓存创建 $" + cacheWrite + " · 1 小时缓存创建 $" + (price.cache_write_1h ?? cacheWrite) + " · 推理 $" + (price.reasoning ?? price.out) + " / 1M tokens · 来源：" + (sources[result.source] || result.source));
  } catch (error) { $("lookupResult").textContent = "查询失败：" + error.message; }
}
async function saveSettings(event) {
  event.preventDefault();
  if (!settingsReady) { toast("设置尚未加载，请先刷新页面", "error"); return false; }
  const siteTitleValue = $("setSiteTitle").value.trim();
  if ([...siteTitleValue].length > 40) { toast("网站标题最多 40 个字", "error"); $("setSiteTitle").focus(); return false; }
  let pricing;
  try {
    pricing = JSON.parse($("setPricing").value);
    const validRate = (value) => typeof value === "number" && Number.isFinite(value) && value >= 0;
    if (!pricing || Array.isArray(pricing) || typeof pricing !== "object" || !Object.keys(pricing).length) throw new Error("请填写模型价格 JSON 对象。");
    for (const [model, item] of Object.entries(pricing)) {
      if (!item || Array.isArray(item) || typeof item !== "object" || !validRate(item.in) || !validRate(item.out)) throw new Error(model + "：in 和 out 必须是有限的非负单价。");
      const allowedFields = ["in", "out", "cache_read", "cache_write", "cache_write_1h", "reasoning", "in_audio", "out_audio", "cache_read_audio"];
      if (Object.keys(item).some((field) => !allowedFields.includes(field))) throw new Error(model + "：包含不支持的价格字段，请检查拼写。");
      for (const field of ["cache_read", "cache_write", "cache_write_1h", "reasoning", "in_audio", "out_audio", "cache_read_audio"]) {
        if (item[field] != null && !validRate(item[field])) throw new Error(model + "：" + field + " 必须是有限的非负单价，或留空使用默认单价。");
      }
    }
  } catch (error) { setPricingError(error.message || "价格 JSON 无效。"); $("setPricing").focus(); return false; }
  setPricingError("");
  const button = event.currentTarget.querySelector('[type="submit"]');
  setBusy(button, true, "正在保存…");
  try {
    await api("api/settings", { method: "PUT", body: JSON.stringify({ affinity_ttl_min: Number($("setTtl").value), retry: Number($("setRetry").value), inject_stream_usage: $("setInject").checked, log_retention_days: Number($("setLogRetention").value), auto_pricing_enabled: $("setAutoPricing").checked, auto_pricing_url: $("setPricingURL").value.trim(), site_title: siteTitleValue, pricing }) });
    await loadSettings(); toast("工作空间设置已保存");
  } catch (error) { reportError(error, "保存失败"); } finally { setBusy(button, false); }
  return false;
}

/* Reusable accessible confirmation and quota dialogs. */
function syncModalLock() { document.body.classList.toggle("modal-open", $("editModal").open || $("actionDialog").open || $("keyModelsDialog").open || $("modelTestDialog").open || $("logDetailDialog").open || $("poolInspectorDialog").open); }
function setPricingError(message) {
  $("pricingError").textContent = message;
  $("pricingError").classList.toggle("hidden", !message);
  $("setPricing").setCustomValidity(message);
  $("setPricing").setAttribute("aria-invalid", String(!!message));
}
function askAction(options) {
  if ($("actionDialog").open) resolveAction(false);
  $("actionHeading").textContent = options.title;
  $("actionDescription").textContent = options.description;
  $("actionConfirm").textContent = options.confirm || "确认";
  $("actionConfirm").className = "btn" + (options.danger ? " danger" : "");
  const inputType = options.input === undefined ? null : options.inputType === "text" ? "text" : "number";
  actionInputType = inputType;
  $("actionInputField").classList.toggle("hidden", inputType === null);
  $("actionInput").disabled = inputType === null;
  if (inputType) {
    $("actionInput").type = inputType;
    $("actionInput").required = inputType !== "text";
    $("actionInputLabel").textContent = options.inputLabel || "共享额度 · USD";
    $("actionInput").value = options.input;
  }
  $("actionDialog").showModal(); syncModalLock();
  if (inputType) { $("actionInput").focus(); $("actionInput").select(); }
  else $("actionForm").querySelector('.form-actions [type="button"]').focus();
  return new Promise((resolve) => { actionResolve = resolve; });
}
function resolveAction(confirmed) {
  const resolve = actionResolve; actionResolve = null;
  const hasInput = !$("actionInput").disabled;
  const result = !confirmed ? false : !hasInput ? true : actionInputType === "text" ? $("actionInput").value : Number($("actionInput").value);
  $("actionDialog").close(); syncModalLock();
  actionInputType = null;
  if (resolve) resolve(result);
}
$("actionForm").addEventListener("submit", (event) => { event.preventDefault(); resolveAction(true); });
$("actionDialog").addEventListener("cancel", (event) => { event.preventDefault(); resolveAction(false); });
$("keyName").addEventListener("input", () => $("keyName").setCustomValidity(""));
$("setPricing").addEventListener("input", () => setPricingError(""));
document.querySelector(".skip-link").addEventListener("click", (event) => { event.preventDefault(); $("mainContent").focus(); });
document.addEventListener("invalid", (event) => {
  const field = event.target;
  if (["editModelMap", "setPricing"].includes(field.id) || !field.id || !field.closest(".field")) return;
  const id = field.id + "Error";
  let error = $(id);
  if (!error) {
    error = document.createElement("small"); error.id = id; error.className = "form-error"; error.setAttribute("role", "alert");
    field.closest(".field").append(error);
    field.setAttribute("aria-describedby", [field.getAttribute("aria-describedby"), id].filter(Boolean).join(" "));
  }
  error.textContent = field.validity.valueMissing ? "请填写此项。" : field.validity.rangeUnderflow ? "数值不能小于 " + field.min + "。" : field.validity.rangeOverflow ? "数值不能大于 " + field.max + "。" : field.validity.typeMismatch ? "请输入有效的地址。" : field.validationMessage;
  error.classList.remove("hidden"); field.setAttribute("aria-invalid", "true");
}, true);
document.addEventListener("input", (event) => {
  const field = event.target, error = $(field.id + "Error");
  if (error) { error.classList.add("hidden"); field.removeAttribute("aria-invalid"); }
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && $("sidebar").classList.contains("is-open") && !$("editModal").open) toggleSidebar(false);
    if ((event.ctrlKey || event.metaKey) && event.key === "Enter" && editorIsOpen() && !$("actionDialog").open && !editProbe.saving) { event.preventDefault(); $("upstreamEditorForm").requestSubmit(); }
});
window.addEventListener("beforeunload", (event) => { if (editorDirty() || deploymentDirty() || announcementDirty()) { event.preventDefault(); event.returnValue = ""; } });
let resizeFrame = 0;
window.addEventListener("resize", () => {
  if (resizeFrame) cancelAnimationFrame(resizeFrame);
  resizeFrame = requestAnimationFrame(() => { resizeFrame = 0; syncSidebar(); if (activeTab === "dashboard") renderTrendChart(); });
});
document.querySelectorAll("svg.icon").forEach((svg) => { svg.setAttribute("aria-hidden", "true"); svg.setAttribute("focusable", "false"); });
$("todayLabel").textContent = new Date().toLocaleDateString("zh-CN", { timeZone: "Asia/Shanghai", month: "long", day: "numeric", weekday: "long" });
initDeployment();
initOutboundProxies();
initKeyModels();
initModelEditor();
initModelTest();
initAnnouncements();
initLogs();
syncSidebar();
(async function init() {
  if (!token) { showLogin(); return; }
  try { const session = await api("api/me"); showApp(session); } catch { showLogin(); }
})();

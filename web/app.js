/* API Share gateway console. No external UI dependencies. */
const TOKEN_KEY = "apishare_token";
const $ = (id) => document.getElementById(id);
const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
const icon = (name) => '<svg class="icon" aria-hidden="true"><use href="#i-' + name + '"/></svg>';
const fmtUSD = (value) => "$" + Number(value || 0).toFixed(4);
const fmtTime = (value) => (value || "").replace("T", " ").slice(0, 19);
const number = (value) => Number(value || 0).toLocaleString("zh-CN");
const providerNames = { openai: "OpenAI 兼容", anthropic: "Anthropic", gemini: "Gemini" };
const providerURLs = { openai: "https://api.openai.com", anthropic: "https://api.anthropic.com", gemini: "https://generativelanguage.googleapis.com" };
const tabNames = { dashboard: "概览", keys: "API 密钥", upstreams: "上游服务", logs: "调用日志", settings: "工作空间设置", deployment: "部署配置" };
const validTab = (name) => Object.hasOwn(tabNames, name);
let token = localStorage.getItem(TOKEN_KEY) || "";
let activeTab = "dashboard";
let logsPage = 1;
let keysCache = [], upsCache = [], logsCache = [];
let statsCache = null, chartMetric = "calls", actionResolve = null;
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
  try {
    response = await fetch(path, { ...options, headers: { "Content-Type": "application/json", ...(token ? { Authorization: "Bearer " + token } : {}), ...(options.headers || {}) } });
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
    error.field = data.field || "";
    throw error;
  }
  updateConnection(true);
  return data;
}
function stampRefresh() {
  $("lastRefreshed").textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN", { hour12: false });
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
    if (!navigator.clipboard) throw new Error("当前浏览器不支持复制，请手动选择文本");
    await navigator.clipboard.writeText(value);
    toast(message);
  } catch (error) { reportError(error, "复制失败"); }
}
function copyEndpoint() { return copyText(location.origin + "/v1", "已复制 OpenAI 兼容接入地址"); }
function copyNewKey() { return copyText($("newKeyValue").textContent, "完整密钥已复制，请妥善保存"); }
function dismissNewKey() { $("newKeyShow").classList.add("hidden"); $("newKeyValue").textContent = ""; }

/* Authentication and navigation */
function showLogin() {
  if ($("actionDialog").open) resolveAction(false);
  if ($("editModal").open) finishCloseEditor();
  $("loginView").classList.remove("hidden");
  $("appView").classList.add("hidden");
  document.querySelector(".skip-link").classList.add("hidden");
  dismissNewKey();
  toggleSidebar(false);
  clearDeploymentPasswords();
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
  document.querySelector(".mobile-menu").setAttribute("aria-expanded", String(open));
}
function toggleSidebar(open) {
  const next = typeof open === "boolean" ? open : !$("sidebar").classList.contains("is-open");
  const restoreFocus = !next && $("sidebar").contains(document.activeElement) && matchMedia("(max-width: 768px)").matches;
  $("sidebar").classList.toggle("is-open", next);
  syncSidebar();
  if (restoreFocus) document.querySelector(".mobile-menu").focus();
  if (next && matchMedia("(max-width: 768px)").matches) document.querySelector(".tab.active").focus();
}
async function switchTab(name, updateHistory = true) {
  if (!validTab(name)) name = "dashboard";
  activeTab = name;
  document.querySelectorAll(".tab").forEach((button) => {
    const selected = button.dataset.tab === name;
    button.classList.toggle("active", selected);
    if (selected) button.setAttribute("aria-current", "page"); else button.removeAttribute("aria-current");
  });
  document.querySelectorAll(".panel").forEach((panel) => panel.classList.toggle("hidden", panel.id !== "tab-" + name));
  $("pageCrumb").textContent = tabNames[name];
  document.title = "API Share · " + tabNames[name];
  if (updateHistory && location.hash !== "#" + name) history.pushState(null, "", "#" + name);
  toggleSidebar(false);
  if (name !== "keys") dismissNewKey();
  return loadCurrentTab();
}
async function loadCurrentTab() {
  if (activeTab === "dashboard") return Promise.all([loadStats(), loadRecent()]);
  if (activeTab === "keys") return loadKeys();
  if (activeTab === "upstreams") return loadUpstreams();
  if (activeTab === "logs") return loadLogs(logsPage);
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
  if (name !== activeTab && !$("appView").classList.contains("hidden")) switchTab(name, false);
}
window.addEventListener("popstate", followHistory);
window.addEventListener("hashchange", followHistory);

/* Overview: only values returned by the gateway are shown. */
function dateKey(date) {
  return date.getFullYear() + "-" + String(date.getMonth() + 1).padStart(2, "0") + "-" + String(date.getDate()).padStart(2, "0");
}
async function loadStats() {
  const version = requestVersions.stats = (requestVersions.stats || 0) + 1;
  try {
    const stats = await api("api/stats");
    if (version !== requestVersions.stats) return;
    statsCache = stats;
    $("stCalls").textContent = number(stats.today.calls);
    $("stCost").textContent = fmtUSD(stats.today.cost_usd);
    $("stTokens").textContent = number(stats.today.prompt_tokens + stats.today.completion_tokens);
    $("stKeys").textContent = number(stats.enabled_keys);
    $("stUps").textContent = number(stats.enabled_upstreams);
    renderChart();
    stampRefresh();
  } catch (error) {
    $("dailyChart").innerHTML = '<div class="chart-empty">无法加载用量数据<br>点击右上角刷新重试</div>';
    reportError(error, "概览加载失败");
  }
}
function setChartMetric(metric) {
  chartMetric = metric;
  document.querySelectorAll("[data-chart]").forEach((button) => {
    button.classList.toggle("active", button.dataset.chart === metric);
    button.setAttribute("aria-pressed", String(button.dataset.chart === metric));
  });
  renderChart();
}
function renderChart() {
  if (!statsCache) return;
  const byDate = new Map((statsCache.daily || []).map((day) => [day.date, day]));
  const days = Array.from({ length: 7 }, (_, i) => {
    const date = new Date(); date.setDate(date.getDate() - 6 + i);
    const key = dateKey(date);
    return byDate.get(key) || { date: key, calls: 0, cost_usd: 0 };
  });
  const cost = chartMetric === "cost";
  const values = days.map((day) => Number(cost ? day.cost_usd : day.calls) || 0);
  const total = values.reduce((sum, value) => sum + value, 0);
  $("chartTotal").textContent = cost ? fmtUSD(total) : number(total);
  $("chartUnit").textContent = cost ? "USD · 7 日费用" : "次调用 · 7 日合计";
  $("chartLegend").textContent = cost ? "调用费用（USD）" : "成功调用";
  $("chartRange").textContent = days[0].date.slice(5).replace("-", "/") + " – " + days[6].date.slice(5).replace("-", "/");
  $("chartNote").textContent = total === 0 ? "暂无" + (cost ? "计费用量" : "成功调用") : "聚焦图表查看每日数据";
  const width = Math.max(280, $("dailyChart").clientWidth || 600);
  const height = 220, left = cost ? 49 : 35, right = width - 15, top = 15, bottom = 182;
  const largest = Math.max(...values, cost ? .003 : 3);
  const power = 10 ** Math.floor(Math.log10(largest));
  const maxValue = Math.ceil(largest / power) * power;
  const points = values.map((value, i) => [left + i * (right - left) / 6, bottom - value / maxValue * (bottom - top)]);
  const path = points.map(([x, y], i) => (i ? "L" : "M") + x.toFixed(1) + " " + y.toFixed(1)).join(" ");
  let svg = '<svg viewBox="0 0 ' + width + " " + height + '" role="img" aria-label="' + esc(cost ? "近七日费用趋势" : "近七日成功调用趋势") + '">';
  svg += '<title>' + esc(days.map((day, i) => day.date + "：" + (cost ? fmtUSD(values[i]) : number(values[i]) + " 次")).join("；")) + '</title>';
  for (let i = 0; i < 4; i++) {
    const y = bottom - i / 3 * (bottom - top), value = maxValue * i / 3;
    const label = cost ? "$" + value.toFixed(maxValue < 1 ? 3 : 1) : number(Math.round(value));
    svg += '<line class="chart-grid" x1="' + left + '" y1="' + y + '" x2="' + right + '" y2="' + y + '"/><text class="chart-axis" x="' + (left - 9) + '" y="' + (y + 3) + '" text-anchor="end">' + label + '</text>';
  }
  svg += '<path class="chart-area" d="' + path + " L" + right + " " + bottom + " L" + left + " " + bottom + ' Z"/><path class="chart-line" d="' + path + '"/>';
  points.forEach(([x, y], i) => {
    const label = days[i].date + " · " + (cost ? fmtUSD(values[i]) : number(values[i]) + " 次调用");
    svg += '<circle class="chart-point" cx="' + x + '" cy="' + y + '" r="3"/><text class="chart-axis" x="' + x + '" y="209" text-anchor="middle">' + days[i].date.slice(5).replace("-", "/") + '</text><rect class="chart-hit" tabindex="0" role="img" aria-label="' + esc(label) + '" data-label="' + esc(label) + '" x="' + Math.max(left, x - (right - left) / 14) + '" y="0" width="' + (right - left) / 14 + '" height="190"><title>' + esc(label) + '</title></rect>';
  });
  $("dailyChart").innerHTML = svg + "</svg>";
  $("dailyChart").querySelectorAll(".chart-hit").forEach((point) => {
    ["focus", "mouseenter"].forEach((name) => point.addEventListener(name, () => { $("chartNote").textContent = point.dataset.label; }));
  });
}
async function loadRecent() {
  try {
    const result = await api("api/logs?page=1&size=5");
    $("recentTable").querySelector("tbody").innerHTML = result.items.length ? result.items.map((log) =>
      '<tr><td><span class="cell-stack"><strong>' + esc(log.model) + '</strong><small>' + esc(log.upstream_name || "未分配上游") + '</small></span></td><td>' + esc(log.key_name) + '</td><td>' + statusBadge(log.status) + '</td><td>' + number(log.prompt_tokens + log.completion_tokens) + '</td><td><code>' + fmtUSD(log.cost_usd) + '</code></td><td class="timestamp">' + esc(fmtTime(log.created_at).slice(5)) + '</td></tr>'
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
    const used = key.quota_usd > 0 ? Math.min(100, key.used_usd / key.quota_usd * 100) : 0;
    return '<tr><td class="cell-stack"><strong>' + esc(key.name) + '</strong><small>#' + key.id + ' · ' + esc((key.created_at || "").slice(0, 10)) + '</small></td><td><code>' + esc(masked) + '</code></td><td class="quota-cell"><span>' + fmtUSD(key.used_usd) + ' <span class="muted">/ ' + (key.quota_usd > 0 ? fmtUSD(key.quota_usd) : "不限") + '</span></span>' + (key.quota_usd > 0 ? '<div class="quota-track" role="meter" aria-label="额度使用" aria-valuenow="' + used + '" aria-valuemin="0" aria-valuemax="100"><span style="width:' + used + '%"></span></div>' : "") + '</td><td><code>' + key.qps + " / " + key.concurrency + '</code></td><td><span class="badge ' + (key.enabled ? "on" : "off") + '">' + (key.enabled ? "启用" : "停用") + '</span></td><td><label class="home-share-control"><input type="checkbox" ' + (key.show_on_home ? 'checked ' : '') + 'aria-label="在公开首页汇总 ' + esc(key.name) + ' 的余量和用量" onchange="toggleHomeShare(' + key.id + ', this)"><span>首页展示</span></label></td><td><div class="row-actions"><button class="text-btn" onclick="editQuota(' + key.id + ", " + key.quota_usd + ')" aria-label="修改 ' + esc(key.name) + ' 的额度">额度</button><button class="text-btn" onclick="toggleKey(' + key.id + ", " + !key.enabled + ', this)" aria-label="' + (key.enabled ? "停用 " : "启用 ") + esc(key.name) + '">' + (key.enabled ? "停用" : "启用") + '</button><button class="icon-btn danger" onclick="deleteKey(' + key.id + ')" aria-label="删除 ' + esc(key.name) + '" title="删除密钥">' + icon("trash") + '</button></div></td></tr>';
  }).join("") || emptyRow(7, query ? "没有匹配的密钥" : "创建你的第一个 API 密钥", query ? "试试其他名称。" : "为成员分配独立的访问权限和调用额度。", query ? "" : "toggleKeyComposer(true)", "创建密钥", "key");
  labelTableCells("keysTable");
}
async function createKey(event) {
  event.preventDefault();
  const form = event.currentTarget, button = form.querySelector('[type="submit"]');
  const name = $("keyName").value.trim();
  if (!name) { $("keyName").setCustomValidity("请输入密钥名称"); $("keyName").reportValidity(); return false; }
  setBusy(button, true, "正在生成…");
  try {
    const key = await api("api/keys", { method: "POST", body: JSON.stringify({ name, quota_usd: Number($("keyQuota").value) || 0, qps: Number($("keyQps").value) || 3, concurrency: Number($("keyConc").value) || 5 }) });
    $("newKeyValue").textContent = key.key;
    $("newKeyShow").classList.remove("hidden");
    form.reset(); toggleKeyComposer(false);
    await loadKeys(); toast("密钥已创建");
  } catch (error) { reportError(error); } finally { setBusy(button, false); }
  return false;
}
async function toggleKey(id, enabled, button) {
  setBusy(button, true, "更新中");
  try { await api("api/keys/" + id, { method: "PATCH", body: JSON.stringify({ enabled }) }); await loadKeys(); toast(enabled ? "密钥已启用" : "密钥已停用"); }
  catch (error) { reportError(error); } finally { setBusy(button, false); }
}
async function toggleHomeShare(id, checkbox) {
  const selected = checkbox.checked;
  checkbox.disabled = true;
  try {
    await api("api/keys/" + id, { method: "PATCH", body: JSON.stringify({ show_on_home: selected }) });
    await loadKeys();
    toast(selected ? "已纳入首页合计，密钥信息不会公开" : "已从首页合计中移除");
  } catch (error) { checkbox.checked = !selected; reportError(error, "首页展示设置失败"); }
  finally { checkbox.disabled = false; }
}
async function editQuota(id, current) {
  const value = await askAction({ title: "修改密钥额度", description: "设置总额度（USD）。填入 0 表示不限制额度，已使用的费用会保留。", confirm: "保存额度", input: current });
  if (value === false) return;
  try { await api("api/keys/" + id, { method: "PATCH", body: JSON.stringify({ quota_usd: Number(value) }) }); await loadKeys(); toast("额度已更新"); }
  catch (error) { reportError(error); }
}
async function deleteKey(id) {
  const key = keysCache.find((item) => item.id === id);
  if (!await askAction({ title: "删除 API 密钥？", description: (key ? "“" + key.name + "”" : "该密钥") + "删除后，使用它的请求会被拒绝。此操作无法撤销。", confirm: "删除密钥", danger: true })) return;
  try { await api("api/keys/" + id, { method: "DELETE" }); await loadKeys(); toast("密钥已删除"); }
  catch (error) { reportError(error); }
}

/* Upstream list */
function parseAllowlist(value) { return [...new Set((value || "").split(/[,，\n]/).map((model) => model.trim()).filter(Boolean))]; }
function providerIcon(type) { return '<span class="provider-icon ' + esc(type) + '" aria-hidden="true">' + ({ openai: "O", anthropic: "A", gemini: "G" }[type] || "API") + '</span>'; }
async function loadUpstreams(quiet = false) {
  const version = requestVersions.upstreams = (requestVersions.upstreams || 0) + 1;
  try {
    const result = await api("api/upstreams");
    if (version !== requestVersions.upstreams) return;
    upsCache = result || [];
    $("navUpCount").textContent = upsCache.length;
    $("navUpCount").setAttribute("aria-label", upsCache.length + " 个上游");
    renderUpstreams(); stampRefresh();
  } catch (error) { errorRow("upsTable", 6, error); if (!quiet) reportError(error, "上游加载失败"); }
}
function renderUpstreams() {
  const query = $("upstreamSearch").value.trim().toLowerCase(), type = $("upstreamTypeFilter").value;
  const list = upsCache.filter((item) => (!type || item.type === type) && [item.name, item.base_url, item.models, item.type].join(" ").toLowerCase().includes(query));
  $("upstreamTotal").textContent = upsCache.length;
  $("upstreamHealthy").textContent = upsCache.filter((item) => item.enabled && item.healthy).length;
  $("upstreamModelCount").textContent = new Set(upsCache.flatMap((item) => parseAllowlist(item.models))).size;
  $("upstreamListCount").textContent = list.length + (list.length !== upsCache.length ? " / " + upsCache.length : "");
  $("upsTable").querySelector("tbody").innerHTML = list.map((item) => '<tr><td><div class="provider-cell">' + providerIcon(item.type) + '<span class="cell-stack"><strong>' + esc(item.name || "未命名上游") + '</strong><small>' + esc(providerNames[item.type] || item.type) + ' · #' + item.id + '</small></span></div></td><td class="endpoint-cell"><span>' + esc(item.base_url) + '</span><small>' + esc(item.key_masked) + '</small></td><td class="cell-stack"><strong>' + (parseAllowlist(item.models).length ? parseAllowlist(item.models).length + " 个模型" : "全部模型") + '</strong><small>权重 ' + item.weight + '</small></td><td><span class="badge ' + (item.healthy ? "on" : "warn") + '">' + (item.healthy ? "健康" : "冷却中") + '</span>' + (item.fails ? '<span class="cell-stack"><small>连续失败 ' + item.fails + ' 次</small></span>' : "") + '</td><td><button class="toggle-btn" role="switch" aria-checked="' + !!item.enabled + '" aria-label="' + (item.enabled ? "停用 " : "启用 ") + esc(item.name) + '" onclick="toggleUpstream(' + item.id + ", " + !item.enabled + ', this)"><span></span></button></td><td><div class="row-actions"><button class="text-btn" onclick="openEditUpstream(' + item.id + ')" aria-label="编辑 ' + esc(item.name) + '">' + icon("edit") + '编辑</button><button class="icon-btn" onclick="testUpstream(' + item.id + ', this)" aria-label="测试 ' + esc(item.name) + '" title="测试连接">' + icon("activity") + '</button><button class="icon-btn danger" onclick="deleteUpstream(' + item.id + ')" aria-label="删除 ' + esc(item.name) + '" title="删除上游">' + icon("trash") + '</button></div></td></tr>').join("") || emptyRow(6, query || type ? "没有匹配的上游" : "连接你的第一个上游", query || type ? "调整搜索条件或接口类型。" : "支持 OpenAI 兼容、Anthropic 与 Gemini 接口。", query || type ? "" : "openCreateUpstream()", "添加上游");
  labelTableCells("upsTable");
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
  return JSON.stringify(["editName", "editType", "editBase", "editKey", "editWeight", "editModels", "editModelMap"].map((id) => $(id).value).concat($("editEnabled").checked));
}
function editorDirty() { return $("editModal").open && editorValue() !== editProbe.initial; }
function updateEditorDirty() {
  const dirty = editorDirty();
  $("editorSaveStatus").classList.toggle("dirty", dirty || editProbe.id === null);
  $("editorSaveStatus").lastElementChild.textContent = editProbe.saving ? "正在保存配置…" : dirty ? "有未保存的修改" : editProbe.id === null ? "新连接尚未保存" : "配置未改动";
}
function openCreateUpstream() { openEditor(null); }
function openEditUpstream(id) {
  const upstream = upsCache.find((item) => item.id === id);
  if (!upstream) { toast("未找到该上游，请刷新列表后重试", "error"); return; }
  openEditor(upstream);
}
function openEditor(upstream) {
  if ($("editModal").open) return;
  const creating = !upstream;
  editProbe.id = upstream ? upstream.id : null;
  editProbe.generation++;
  editProbe.saving = false; editProbe.closing = false; editProbe.models = [];
  $("upstreamEditorForm").reset();
  $("upstreamEditorForm").querySelectorAll(".form-error").forEach((error) => error.classList.add("hidden"));
  const values = { editName: upstream?.name || "", editType: upstream?.type || "openai", editBase: upstream?.base_url || providerURLs.openai, editKey: "", editWeight: upstream ? upstream.weight : 1, editModels: upstream?.models || "", editModelMap: upstream?.model_map || "" };
  Object.entries(values).forEach(([id, value]) => { $(id).value = value; $(id).setCustomValidity(""); $(id).removeAttribute("aria-invalid"); });
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
  $("editorHeading").textContent = creating ? "添加上游服务" : "编辑上游服务";
  $("editorSaveLabel").textContent = creating ? "创建连接" : "保存修改";
  $("editProbeArea").classList.add("hidden");
  $("editProbeList").innerHTML = ""; $("editProbeFilter").value = ""; $("editProbeInfo").textContent = "";
  $("editorError").classList.add("hidden"); $("modelMapError").classList.add("hidden");
  $("modelMapDetails").open = !!upstream?.model_map;
  setBusy($("probeEditorButton"), false);
  updateEditorProvider(false); renderModelChips();
  editProbe.initial = editorValue();
  $("editModal").showModal();
  document.body.classList.add("modal-open");
  $("editorScroll").scrollTop = 0;
  activateEditorSection("editorBasic");
  updateEditorDirty();
  $("editName").focus();
}
function updateEditorProvider(changed = true) {
  const type = $("editType").value;
  if (changed && editProbe.id === null && Object.values(providerURLs).includes($("editBase").value)) $("editBase").value = providerURLs[type];
  $("editorProviderIcon").className = "provider-icon " + type;
  $("editorProviderIcon").textContent = { openai: "O", anthropic: "A", gemini: "G" }[type];
  $("editorProviderIcon").setAttribute("aria-hidden", "true");
  $("providerHint").textContent = { openai: "支持 OpenAI、DeepSeek、Grok 等 OpenAI 兼容接口。", anthropic: "使用 Anthropic 原生 Messages 接口，支持 Claude 模型。", gemini: "使用 Google Gemini 原生 generateContent 接口。" }[type];
  $("editBase").placeholder = providerURLs[type];
  $("editModels").placeholder = { openai: "gpt-4o, deepseek-chat", anthropic: "claude-sonnet-4-20250514, claude-opus-4-20250514", gemini: "gemini-2.5-flash, gemini-2.5-pro" }[type];
  if (changed) { clearProbeResults(); updateEditorDirty(); }
}
function clearProbeResults() {
  editProbe.generation++;
  editProbe.controller?.abort();
  editProbe.controller = null;
  setBusy($("probeEditorButton"), false);
  editProbe.models = [];
  editProbe.selected = new Set(parseAllowlist($("editModels").value));
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
function activateEditorSection(id) {
  document.querySelectorAll(".editor-nav-item").forEach((button) => {
    const active = button.dataset.section === id;
    button.classList.toggle("active", active);
    if (active) button.setAttribute("aria-current", "location"); else button.removeAttribute("aria-current");
  });
}
function jumpEditor(id) {
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
    document.querySelectorAll(".editor-section").forEach((item) => { if (item.offsetTop <= scroll.scrollTop + 65) section = item.id; });
    if (scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 5) section = "editorRouting";
    activateEditorSection(section);
  });
}, { passive: true });
function finishCloseEditor() {
  clearProbeResults();
  editProbe.saving = false;
  $("editKey").value = ""; $("editKey").type = "password";
  $("editModal").close();
  syncModalLock();
}
async function closeEdit() {
  if (!$("editModal").open || editProbe.saving || editProbe.closing) return;
  if (editorDirty()) {
    editProbe.closing = true;
    const discard = await askAction({ title: "放弃未保存的修改？", description: "当前配置尚未保存。返回编辑器可继续修改，放弃后将恢复原有配置。", confirm: "放弃修改", danger: true });
    editProbe.closing = false;
    if (!discard) return;
  }
  finishCloseEditor();
}
function renderModelChips() {
  const box = $("selectedModelChips");
  box.replaceChildren();
  parseAllowlist($("editModels").value).forEach((model) => {
    const chip = document.createElement("span"); chip.className = "model-chip";
    const text = document.createElement("span"); text.textContent = model;
    const button = document.createElement("button"); button.type = "button"; button.setAttribute("aria-label", "移除模型 " + model); button.innerHTML = icon("close");
    button.addEventListener("click", () => {
      editProbe.selected.delete(model);
      $("editModels").value = parseAllowlist($("editModels").value).filter((item) => item !== model).join(", ");
      renderModelChips(); renderEditProbeList(); updateEditorDirty();
      $("editModels").focus();
    });
    chip.append(text, button); box.append(chip);
  });
}
function onModelsInput() {
  editProbe.selected = new Set(parseAllowlist($("editModels").value));
  renderModelChips(); renderEditProbeList(); updateEditorDirty();
}
function syncEditModels() {
  $("editModels").value = [...editProbe.selected].join(", ");
  renderModelChips(); updateEditorDirty();
}
function renderEditProbeList() {
  const query = $("editProbeFilter").value.trim().toLowerCase();
  const models = editProbe.models.filter((model) => model.toLowerCase().includes(query));
  $("editProbeList").innerHTML = models.map((model) => '<label><input type="checkbox" data-model="' + esc(model) + '" ' + (editProbe.selected.has(model) ? "checked" : "") + '><span>' + esc(model) + '</span></label>').join("") || '<span class="muted">没有匹配的模型</span>';
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
function validateModelMap() {
  const field = $("editModelMap"), value = field.value.trim();
  let error = "";
  if (value) {
    try {
      const mapping = JSON.parse(value);
      if (!mapping || Array.isArray(mapping) || typeof mapping !== "object" || Object.values(mapping).some((item) => typeof item !== "string")) error = "请填写 JSON 对象，模型名与映射值都必须是字符串。";
    } catch { error = 'JSON 格式不正确，例如：{"gpt-4o": "deepseek-chat"}'; }
  }
  field.setCustomValidity(error);
  field.setAttribute("aria-invalid", String(!!error));
  $("modelMapError").textContent = error;
  $("modelMapError").classList.toggle("hidden", !error);
  return !error;
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
  const base = $("editBase"), key = $("editKey");
  if (!base.checkValidity()) { jumpEditor("editorConnection"); base.reportValidity(); return; }
  if (editProbe.id === null && !key.value.trim()) { jumpEditor("editorConnection"); key.reportValidity(); return; }
  clearProbeResults();
  const generation = editProbe.generation;
  editProbe.controller = new AbortController();
  const signature = [$("editType").value, base.value, key.value].join("\n");
  const body = { type: $("editType").value, base_url: base.value.trim() };
  if (key.value.trim()) body.api_key = key.value.trim();
  $("editProbeInfo").className = "probe-info";
  $("editProbeInfo").textContent = key.value.trim() ? "正在使用当前填写的 Key 获取模型…" : "正在使用已保存的 Key 获取模型…";
  setBusy($("probeEditorButton"), true, "获取中…");
  try {
    const path = editProbe.id === null ? "api/upstream-probe" : "api/upstreams/" + editProbe.id + "/probe";
    const result = await api(path, { method: "POST", body: JSON.stringify(body), signal: editProbe.controller.signal });
    if (generation !== editProbe.generation || signature !== [$("editType").value, base.value, key.value].join("\n")) return;
    if (!result.ok) throw new Error(result.error || "上游未返回模型");
    editProbe.models = [...new Set(result.models || [])];
    editProbe.selected = new Set(parseAllowlist($("editModels").value));
    $("editProbeInfo").className = "probe-info success";
    $("editProbeInfo").textContent = editProbe.models.length ? "当前 Key 返回 " + editProbe.models.length + " 个模型 · " + result.latency_ms + " ms · 勾选后保存为白名单" : "连接成功，但当前 Key 未返回模型。可检查权限或手动填写白名单。";
    $("editProbeArea").classList.toggle("hidden", editProbe.models.length === 0);
    renderEditProbeList();
  } catch (error) {
    if (generation !== editProbe.generation || error.name === "AbortError" || error.message === "unauthorized" || signature !== [$("editType").value, base.value, key.value].join("\n")) return;
    $("editProbeInfo").className = "probe-info error";
    const status = error.message.match(/HTTP (\d{3})/);
    const hint = status && ({ 401: "当前 Key 认证失败，请检查密钥", 403: "当前 Key 没有模型列表访问权限", 404: "未找到模型接口，请检查 Base URL 和接口类型", 429: "上游请求过于频繁，请稍后重试" })[status[1]];
    $("editProbeInfo").textContent = "获取失败：" + (hint || error.message) + "。仍可手动填写白名单。";
  } finally { if (generation === editProbe.generation) setBusy($("probeEditorButton"), false); }
}
async function saveEditUpstream(event) {
  event.preventDefault();
  if (editProbe.saving) return false;
  if (!validateModelMap()) { $("modelMapDetails").open = true; jumpEditor("editorModels"); $("editModelMap").reportValidity(); return false; }
  if (!$("editName").value.trim()) { $("editName").setCustomValidity("请输入上游名称"); jumpEditor("editorBasic"); $("editName").reportValidity(); return false; }
  const creating = editProbe.id === null;
  if (creating && !$("editKey").value.trim()) { $("editKey").setCustomValidity("请输入上游 API 密钥"); jumpEditor("editorConnection"); $("editKey").reportValidity(); return false; }
  try {
    if (!["http:", "https:"].includes(new URL($("editBase").value).protocol)) throw new Error();
  } catch { $("editBase").setCustomValidity("请输入以 http:// 或 https:// 开头的服务地址"); jumpEditor("editorConnection"); $("editBase").reportValidity(); return false; }
  if (!$("upstreamEditorForm").reportValidity()) return false;
  const body = { name: $("editName").value.trim(), type: $("editType").value, base_url: $("editBase").value.trim().replace(/\/+$/, ""), weight: Number($("editWeight").value), models: parseAllowlist($("editModels").value).join(","), model_map: $("editModelMap").value.trim() };
  if (!creating) body.enabled = $("editEnabled").checked;
  if ($("editKey").value.trim()) body.api_key = $("editKey").value.trim();
  editProbe.saving = true; updateEditorDirty();
  $("editorError").classList.add("hidden");
  setBusy($("editorSaveButton"), true, creating ? "正在创建…" : "正在保存…");
  try {
    await api(creating ? "api/upstreams" : "api/upstreams/" + editProbe.id, { method: creating ? "POST" : "PATCH", body: JSON.stringify(body) });
    finishCloseEditor();
    if (activeTab !== "upstreams") await switchTab("upstreams"); else await loadUpstreams();
    toast(creating ? "上游连接已创建" : "上游配置已保存");
  } catch (error) {
    if (error.message !== "unauthorized") {
      $("editorError").textContent = "保存失败：" + error.message;
      $("editorError").classList.remove("hidden");
      $("editorError").scrollIntoView({ block: "nearest" });
    }
  } finally { editProbe.saving = false; setBusy($("editorSaveButton"), false); updateEditorDirty(); }
  return false;
}
$("upstreamEditorForm").addEventListener("input", (event) => {
  if (event.target.id !== "editModelMap" && event.target.setCustomValidity) event.target.setCustomValidity("");
  if (["editBase", "editKey"].includes(event.target.id)) clearProbeResults();
  updateEditorDirty();
});
$("upstreamEditorForm").addEventListener("change", updateEditorDirty);
$("upstreamEditorForm").addEventListener("invalid", (event) => {
  const section = event.target.closest(".editor-section");
  if (event.target.id === "editModelMap") $("modelMapDetails").open = true;
  if (section) jumpEditor(section.id);
}, true);
$("editModal").addEventListener("cancel", (event) => { event.preventDefault(); closeEdit(); });
$("editModal").addEventListener("click", (event) => {
  if (event.target !== $("editModal")) return;
  const rect = $("editModal").getBoundingClientRect();
  if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) closeEdit();
});

/* Logs */
async function loadLogs(page = 1) {
  const version = requestVersions.logs = (requestVersions.logs || 0) + 1;
  $("logPrev").disabled = true; $("logNext").disabled = true;
  try {
    const result = await api("api/logs?page=" + Math.max(1, page) + "&size=50");
    if (version !== requestVersions.logs) return;
    logsPage = result.page; logsCache = result.items || [];
    $("logPage").textContent = result.page;
    $("logTotal").textContent = "共 " + number(result.total) + " 条调用";
    $("logRange").textContent = result.total ? "第 " + ((result.page - 1) * 50 + 1) + "–" + Math.min(result.page * 50, result.total) + " 条，共 " + number(result.total) + " 条" : "暂无调用记录";
    $("logPrev").disabled = result.page <= 1;
    $("logNext").disabled = result.page * 50 >= result.total;
    renderLogs(); stampRefresh();
  } catch (error) { errorRow("logsTable", 8, error); reportError(error, "日志加载失败"); }
}
function renderLogs() {
  const query = $("logSearch").value.trim().toLowerCase();
  const logs = logsCache.filter((log) => [log.model, log.key_name, log.upstream_name, log.error].join(" ").toLowerCase().includes(query));
  $("logsTable").querySelector("tbody").innerHTML = logs.map((log) => '<tr><td class="timestamp">' + esc(fmtTime(log.created_at)) + '</td><td class="cell-stack"><strong>' + esc(log.key_name) + '</strong><small>' + esc(log.upstream_name || "未分配") + '</small></td><td><code>' + esc(log.model) + '</code></td><td>' + statusBadge(log.status) + '</td><td><code>' + number(log.prompt_tokens) + " / " + number(log.completion_tokens) + '</code></td><td><code>' + fmtUSD(log.cost_usd) + '</code></td><td><code>' + number(log.latency_ms) + ' ms</code></td><td class="log-error">' + (log.error ? '<details><summary>查看错误</summary><p>' + esc(log.error) + '</p></details>' : '<span class="muted">—</span>') + '</td></tr>').join("") || emptyRow(8, query ? "本页没有匹配的日志" : "还没有调用记录", query ? "搜索仅作用于当前页，可以翻页继续查找。" : "网关接收到请求后，会在这里记录状态与用量。", "", "", "activity");
}

/* Settings */
async function loadSettings() {
  try {
    const settings = await api("api/settings");
    $("setTtl").value = settings.affinity_ttl_min; $("setRetry").value = settings.retry;
    $("setInject").checked = !!settings.inject_stream_usage;
    $("setAutoPricing").checked = !!settings.auto_pricing_enabled;
    $("setPricingURL").value = settings.auto_pricing_url || "";
    $("setPricing").value = JSON.stringify(settings.pricing, null, 2);
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
    $("lookupResult").textContent = result.model + " · 输入 $" + result.price.in + " / 1M · 输出 $" + result.price.out + " / 1M · 来源：" + (sources[result.source] || result.source);
  } catch (error) { $("lookupResult").textContent = "查询失败：" + error.message; }
}
async function saveSettings(event) {
  event.preventDefault();
  if (!settingsReady) { toast("设置尚未加载，请先刷新页面", "error"); return false; }
  let pricing;
  try {
    pricing = JSON.parse($("setPricing").value);
    if (!pricing || Array.isArray(pricing) || typeof pricing !== "object" || !Object.keys(pricing).length || Object.values(pricing).some((item) => !item || typeof item.in !== "number" || typeof item.out !== "number" || item.in < 0 || item.out < 0)) throw new Error();
  } catch { setPricingError("请填写价格 JSON 对象，每个模型需包含非负的 in 与 out 单价。"); $("setPricing").focus(); return false; }
  setPricingError("");
  const button = event.currentTarget.querySelector('[type="submit"]');
  setBusy(button, true, "正在保存…");
  try {
    await api("api/settings", { method: "PUT", body: JSON.stringify({ affinity_ttl_min: Number($("setTtl").value), retry: Number($("setRetry").value), inject_stream_usage: $("setInject").checked, auto_pricing_enabled: $("setAutoPricing").checked, auto_pricing_url: $("setPricingURL").value.trim(), pricing }) });
    await loadSettings(); toast("工作空间设置已保存");
  } catch (error) { reportError(error, "保存失败"); } finally { setBusy(button, false); }
  return false;
}

/* Reusable accessible confirmation and quota dialogs. */
function syncModalLock() { document.body.classList.toggle("modal-open", $("editModal").open || $("actionDialog").open); }
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
  const hasInput = options.input !== undefined;
  $("actionInputField").classList.toggle("hidden", !hasInput);
  $("actionInput").disabled = !hasInput;
  if (hasInput) $("actionInput").value = options.input;
  $("actionDialog").showModal(); syncModalLock();
  if (hasInput) { $("actionInput").focus(); $("actionInput").select(); }
  else $("actionForm").querySelector('.form-actions [type="button"]').focus();
  return new Promise((resolve) => { actionResolve = resolve; });
}
function resolveAction(confirmed) {
  const resolve = actionResolve; actionResolve = null;
  const result = confirmed && !$("actionInput").disabled ? Number($("actionInput").value) : confirmed;
  $("actionDialog").close(); syncModalLock();
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
  if ((event.ctrlKey || event.metaKey) && event.key === "Enter" && $("editModal").open && !$("actionDialog").open && !editProbe.saving) { event.preventDefault(); $("upstreamEditorForm").requestSubmit(); }
});
window.addEventListener("beforeunload", (event) => { if (editorDirty() || deploymentDirty()) { event.preventDefault(); event.returnValue = ""; } });
let resizeFrame = 0;
window.addEventListener("resize", () => {
  if (resizeFrame) cancelAnimationFrame(resizeFrame);
  resizeFrame = requestAnimationFrame(() => { resizeFrame = 0; syncSidebar(); if (activeTab === "dashboard") renderChart(); });
});
document.querySelectorAll("svg.icon").forEach((svg) => { svg.setAttribute("aria-hidden", "true"); svg.setAttribute("focusable", "false"); });
$("todayLabel").textContent = new Date().toLocaleDateString("zh-CN", { month: "long", day: "numeric", weekday: "long" });
initDeployment();
syncSidebar();
(async function init() {
  if (!token) { showLogin(); return; }
  try { const session = await api("api/me"); showApp(session); } catch { showLogin(); }
})();

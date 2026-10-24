/* Upstream workspace: real usage, health history, filtering and both layouts. */
let upstreamOverviewCache = null;
let upstreamView = localStorage.getItem("apishare_upstream_view") === "cards" ? "cards" : "table";
let upstreamSortDescending = false, upstreamBulkBusy = false;
const upstreamSelection = new Set();
let upstreamMenuTrigger = null;

function upstreamUsageFor(item) {
  return upstreamOverviewCache?.upstreams.find((usage) => usage.upstream_id === item.id);
}
function compactUpstreamNumber(value) {
  return new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(Number(value) || 0);
}
function upstreamCost(value) {
  const amount = Number(value) || 0;
  return amount > 0 && amount < .01 ? fmtUSD(amount) : "$" + amount.toFixed(2);
}
function renderUpstreamMetrics() {
  const overview = upstreamOverviewCache, today = overview?.today, week = overview?.week;
  $("upstreamTotal").textContent = upsCache.length;
  $("upstreamModelCount").textContent = new Set(upsCache.flatMap((item) => parseAllowlist(item.models))).size;
  $("upstreamProxyCount").textContent = proxiesCache.length;
  $("upActiveNote").textContent = "已启用 " + upsCache.filter((item) => item.enabled).length + " / " + upsCache.length + " 个上游";
  $("upTodayCalls").textContent = today ? number(today.calls) : "—";
  $("upTodayCost").textContent = today ? upstreamCost(today.cost_usd) : "—";
  $("upTodayTokens").textContent = today ? compactUpstreamNumber(today.prompt_tokens + today.completion_tokens) : "—";
  $("upSuccessRate").textContent = week?.calls ? (week.successes / week.calls * 100).toFixed(2) + "%" : "—";
  $("upSuccessRate").classList.toggle("is-healthy", !!week?.calls && week.successes / week.calls >= .95);
  $("upSuccessRate").classList.toggle("is-warning", !!week?.calls && week.successes / week.calls < .95);
  $("upFailureNote").textContent = week?.calls ? "近 7 天 " + number(week.calls - week.successes) + " 次失败" : overview ? "近 7 天暂无调用" : "统计暂不可用，请刷新重试";
  $("upCostNote").textContent = today?.successes ? "每千次成功调用 " + upstreamCost(today.cost_usd / today.successes * 1000) : "成功调用费用 · USD";
  $("upFirstToken").textContent = week?.first_token_ms != null ? Math.round(week.first_token_ms) + "ms" : "—";
  $("upTokenNote").textContent = today ? "输入 " + compactUpstreamNumber(today.prompt_tokens) + " · 输出 " + compactUpstreamNumber(today.completion_tokens) : "输入与输出总用量";
  $("upstreamOverviewUpdated").textContent = overview ? "基于保留的调用明细 · 更新于 " + fmtTime(overview.updated_at).slice(11) : "统计暂不可用，配置操作仍可使用";
}
function filteredUpstreams() {
  const query = $("upstreamSearch").value.trim().toLowerCase(), type = $("upstreamTypeFilter").value, status = $("upstreamStatusFilter").value;
  const sort = $("upstreamSort").value;
  const list = upsCache.filter((item) => {
    const matchesStatus = !status || status === "enabled" && item.enabled || status === "disabled" && !item.enabled || status === "healthy" && item.enabled && item.healthy || status === "cooling" && item.enabled && !item.healthy;
    return matchesStatus && (!type || upstreamFormat(item) === type) && [item.name, item.base_url, item.models, item.type, upstreamFormatNames[upstreamFormat(item)]].join(" ").toLowerCase().includes(query);
  });
  const value = (item) => sort === "name" ? item.name || "" : sort === "calls" ? upstreamUsageFor(item)?.today.calls || 0 : sort === "cost" ? upstreamUsageFor(item)?.today.cost_usd || 0 : sort === "weight" ? upstreamWeight(item) : Number(item.priority) || 0;
  return list.sort((a, b) => {
    const order = sort === "name" ? value(a).localeCompare(value(b), "zh-CN") : value(a) - value(b);
    return (upstreamSortDescending ? -order : order) || a.id - b.id;
  });
}
function upstreamIdentity(item) {
  if (item.type === "cliproxy" && item.managed_pool) return '<div class="upstream-identity">' + providerIcon(item.type, item.base_url) + '<div class="upstream-identity-text"><button class="upstream-name" onclick="openEditUpstream(' + item.id + ')" title="打开 ' + esc(item.name) + ' 账号池">' + esc(item.name) + '</button><span class="upstream-key-line">' + (item.pool_accounts || 0) + ' 个账号 <span>· ' + (item.pool_running ? '运行中' : '已停止') + '</span></span><span class="upstream-format">CLIProxyAPI 账号池 · ' + esc(item.pool_proxy_id ? upstreamProxyLabel(item.pool_proxy_id) : "默认连接") + '</span></div></div>';
  const keys = item.api_keys || [];
  const keyLabel = keys.length ? keys.length + " 把密钥 · " + keys.filter(key => key.healthy).length + " 把可用" : item.key_masked || "已保存密钥";
  const proxyLabel = keys.length ? item.key_proxy_mode === "individual" ? "按密钥配置代理" : "统一代理 · " + upstreamProxyLabel(item.proxy_id) : item.proxy_id ? upstreamProxyLabel(item.proxy_id) : "";
  return '<div class="upstream-identity">' + providerIcon(item.type, item.base_url) + '<div class="upstream-identity-text"><button class="upstream-name" onclick="openEditUpstream(' + item.id + ')" title="编辑 ' + esc(item.name) + '">' + esc(item.name || "未命名上游") + '</button><span class="upstream-key-line" title="' + esc(item.base_url) + '">' + esc(item.base_url.replace(/^https?:\/\//, "").replace(/\/$/, "")) + ' <span>· ' + esc(keyLabel) + '</span></span><span class="upstream-format">' + esc(upstreamFormatNames[upstreamFormat(item)] || item.type) + (proxyLabel ? ' · ' + esc(proxyLabel) : '') + '</span></div></div>';
}
function upstreamHealthHTML(item) {
  const usage = upstreamUsageFor(item), buckets = usage?.health || Array.from({length: 20}, () => ({calls: 0, successes: 0}));
  const calls = buckets.reduce((sum, b) => sum + b.calls, 0), successes = buckets.reduce((sum, b) => sum + b.successes, 0);
  const rate = calls ? Math.round(successes / calls * 100) + "%" : "—";
  const status = !item.enabled ? "已停用" : !item.healthy ? "冷却中" : calls ? "健康" : "暂无调用";
  const state = !item.enabled ? "off" : !item.healthy ? "warn" : calls ? "on" : "off";
  const start = upstreamOverviewCache ? new Date(upstreamOverviewCache.health_start).getTime() : 0;
  return '<div class="upstream-health"><div class="upstream-health-line" tabindex="0" role="img" aria-label="最近 200 分钟，' + calls + ' 次调用，' + successes + ' 次成功，成功率 ' + rate + '"><span class="upstream-health-bars">' + buckets.map((b, i) => {
    const tone = !b.calls ? "empty" : b.successes === b.calls ? "good" : b.successes / b.calls >= .8 ? "mixed" : "bad";
    const time = start ? fmtTime(new Date(start + i * 600000).toISOString()).slice(11, 16) + '–' + fmtTime(new Date(start + (i + 1) * 600000).toISOString()).slice(11, 16) + ' · ' : '';
    return '<span class="' + tone + '" title="' + esc(time + (b.calls ? b.successes + '/' + b.calls + ' 次成功' : '暂无调用')) + '"></span>';
  }).join("") + '</span><span class="upstream-health-rate">' + rate + '</span></div><span class="badge ' + state + '">' + status + '</span>' + (item.fails ? '<small class="upstream-failure-count">连续失败 ' + number(item.fails) + ' 次</small>' : '') + ((!item.healthy && item.enabled) ? ' <button class="text-btn upstream-recover" onclick="recoverUpstream(' + item.id + ', this)" aria-label="恢复 ' + esc(item.name) + '">恢复</button>' : '') + '</div>';
}
function upstreamToggle(item) {
  return '<button class="toggle-btn" role="switch" aria-checked="' + !!item.enabled + '" aria-label="' + (item.enabled ? "停用 " : "启用 ") + esc(item.name) + '" onclick="toggleUpstream(' + item.id + ', ' + !item.enabled + ', this)"><span></span></button>';
}
function upstreamModelsHTML(item) {
  const models = parseAllowlist(item.models);
  return '<button class="upstream-model-count" onclick="openUpstreamSection(' + item.id + ',\'editorModels\')" title="' + esc(models.join(", ") || "允许全部模型") + '">' + icon("layers") + (models.length ? models.length + ' 条' : '全部') + '</button>';
}
function upstreamScheduleHTML(item) {
  return '<button class="upstream-schedule" onclick="openUpstreamSection(' + item.id + ',\'editorRouting\')" title="编辑优先级与权重">P' + (Number(item.priority) || 0) + ' · W' + upstreamWeight(item) + '</button>';
}
function upstreamMenuButton(item) {
  return '<button class="icon-btn upstream-more" onclick="showUpstreamMenu(' + item.id + ', this)" aria-label="' + esc(item.name) + '的更多操作" aria-haspopup="true" aria-expanded="false">' + icon("more") + '</button>';
}
function upstreamTodayHTML(usage) {
  return '<span class="upstream-stat-main">' + (usage ? number(usage.today.calls) + ' · ' + compactUpstreamNumber(usage.today.prompt_tokens + usage.today.completion_tokens) : '—') + '</span><small>' + (usage ? upstreamCost(usage.today.cost_usd) : '暂无统计') + '</small>';
}
function upstreamWeekHTML(usage) {
  return '<span class="upstream-stat-main">' + (usage ? upstreamCost(usage.week.cost_usd) : '—') + '</span><small>' + (usage ? number(usage.week.calls) + ' 次' : '暂无统计') + '</small>';
}
function renderUpstreams() {
  const list = filteredUpstreams();
  const existing = new Set(upsCache.map((item) => item.id));
  for (const id of upstreamSelection) if (!existing.has(id)) upstreamSelection.delete(id);
  renderUpstreamMetrics();
  $("upstreamListCount").textContent = list.length + " 个上游" + (list.length !== upsCache.length ? " / 共 " + upsCache.length + " 个" : "");
  $("upsTable").querySelector("tbody").innerHTML = list.map((item) => {
    const usage = upstreamUsageFor(item);
    return '<tr class="' + (!item.enabled ? 'upstream-is-disabled' : !item.healthy ? 'upstream-is-warning' : '') + '"><td class="upstream-select-cell"><input type="checkbox" value="' + item.id + '" aria-label="选择 ' + esc(item.name) + '" ' + (upstreamSelection.has(item.id) ? 'checked ' : '') + 'onchange="selectUpstream(' + item.id + ',this.checked)"></td><td>' + upstreamToggle(item) + '</td><td>' + upstreamIdentity(item) + '</td><td>' + upstreamModelsHTML(item) + '</td><td>' + upstreamHealthHTML(item) + '</td><td class="upstream-usage-cell">' + upstreamTodayHTML(usage) + '</td><td class="upstream-usage-cell">' + upstreamWeekHTML(usage) + '</td><td>' + upstreamScheduleHTML(item) + '</td><td>' + upstreamMenuButton(item) + '</td></tr>';
  }).join("") || emptyRow(9, upsCache.length ? "没有匹配的上游" : "连接你的第一个上游", upsCache.length ? "调整搜索、状态或接口格式后重试。" : "添加一个模型服务，开始统一管理连接与调用。", upsCache.length ? "" : "openCreateUpstream()", "新建上游");
  $("upstreamCardsView").innerHTML = list.map((item) => {
    const usage = upstreamUsageFor(item);
    return '<article class="upstream-item-card"><div class="upstream-card-heading">' + upstreamIdentity(item) + upstreamMenuButton(item) + '</div><div class="upstream-card-health">' + upstreamHealthHTML(item) + upstreamToggle(item) + '</div><div class="upstream-card-usage"><div><span>今日调用 / Tokens</span>' + upstreamTodayHTML(usage) + '</div><div><span>近 7 天用量</span>' + upstreamWeekHTML(usage) + '</div></div><div class="upstream-card-footer">' + upstreamModelsHTML(item) + upstreamScheduleHTML(item) + '</div></article>';
  }).join("") || '<div class="table-empty"><strong>' + (upsCache.length ? '没有匹配的上游' : '连接你的第一个上游') + '</strong><p>' + (upsCache.length ? '调整搜索、状态或接口格式后重试。' : '添加一个模型服务，开始统一管理连接与调用。') + '</p><button class="btn ghost" onclick="' + (upsCache.length ? 'resetUpstreamFilters()' : 'openCreateUpstream()') + '">' + (upsCache.length ? '重置筛选' : '新建上游') + '</button></div>';
  labelTableCells("upsTable");
  applyUpstreamView();
  syncUpstreamSelection();
}
function resetUpstreamFilters() {
  ["upstreamSearch", "upstreamStatusFilter", "upstreamTypeFilter"].forEach((id) => { $(id).value = ""; });
  renderUpstreams();
}
function toggleUpstreamSort() {
  upstreamSortDescending = !upstreamSortDescending;
  $("upstreamSortDirection").setAttribute("aria-label", "当前" + (upstreamSortDescending ? "降序，点击切换为升序" : "升序，点击切换为降序"));
  $("upstreamSortDirection").classList.toggle("is-descending", upstreamSortDescending);
  renderUpstreams();
}
function applyUpstreamView() {
  $("upstreamTableView").classList.toggle("hidden", upstreamView !== "table");
  $("upstreamCardsView").classList.toggle("hidden", upstreamView !== "cards");
  document.querySelectorAll("[data-upstream-view]").forEach((button) => button.setAttribute("aria-pressed", String(button.dataset.upstreamView === upstreamView)));
}
function setUpstreamView(view) {
  upstreamView = view === "cards" ? "cards" : "table";
  localStorage.setItem("apishare_upstream_view", upstreamView);
  if (upstreamView === "cards") clearUpstreamSelection();
  applyUpstreamView();
}
async function openUpstreamSection(id, section) {
  const item = upsCache.find((upstream) => upstream.id === id);
  if (!item) return;
  await openEditor(item);
  if (editorIsOpen()) jumpEditor(section);
}
function selectUpstream(id, selected) {
  if (upstreamBulkBusy) return;
  if (selected) upstreamSelection.add(id); else upstreamSelection.delete(id);
  syncUpstreamSelection();
}
function selectAllUpstreams(selected) {
  if (upstreamBulkBusy) return;
  filteredUpstreams().forEach((item) => { if (selected) upstreamSelection.add(item.id); else upstreamSelection.delete(item.id); });
  syncUpstreamSelection();
}
function clearUpstreamSelection() {
  if (upstreamBulkBusy) return;
  upstreamSelection.clear(); syncUpstreamSelection();
}
function syncUpstreamSelection() {
  const list = filteredUpstreams(), count = list.filter((item) => upstreamSelection.has(item.id)).length;
  $("upstreamSelectAll").checked = !!list.length && count === list.length;
  $("upstreamSelectAll").indeterminate = count > 0 && count < list.length;
  $("upstreamSelectAll").disabled = !list.length || upstreamBulkBusy;
  document.querySelectorAll('#upsTable tbody input[type="checkbox"]').forEach((field) => { field.checked = upstreamSelection.has(Number(field.value)); field.disabled = upstreamBulkBusy; });
  $("upstreamBulkBar").classList.toggle("hidden", upstreamSelection.size === 0);
  $("upstreamSelectedCount").textContent = "已选择 " + upstreamSelection.size + " 个上游";
  $("upstreamBulkBar").querySelectorAll("button").forEach((button) => { button.disabled = upstreamBulkBusy; });
}
async function setBulkUpstreamEnabled(enabled) {
  if (upstreamBulkBusy || !upstreamSelection.size) return;
  const ids = [...upstreamSelection];
  if (!await askAction({ title: "批量" + (enabled ? "启用" : "停用") + "上游？", description: "将修改选中的 " + ids.length + " 个上游。" + (enabled ? "启用后可接收新的模型请求。" : "停用后这些上游将不再接收新的模型请求。"), confirm: enabled ? "批量启用" : "批量停用" })) return;
  upstreamBulkBusy = true; syncUpstreamSelection();
  try {
    const results = await Promise.allSettled(ids.map((id) => api("api/upstreams/" + id, {method: "PATCH", body: JSON.stringify({enabled})})));
    results.forEach((result, index) => { if (result.status === "fulfilled") upstreamSelection.delete(ids[index]); });
    const failed = results.filter((result) => result.status === "rejected").length;
    toast(failed ? "已完成 " + (ids.length - failed) + " 个，" + failed + " 个失败，可重试剩余项" : "已" + (enabled ? "启用" : "停用") + " " + ids.length + " 个上游", failed ? "error" : "success");
    await loadUpstreams();
  } finally { upstreamBulkBusy = false; syncUpstreamSelection(); }
}
function showUpstreamMenu(id, button) {
  const menu = $("upstreamActionMenu"), item = upsCache.find((upstream) => upstream.id === id);
  if (!item) return;
  const same = upstreamMenuTrigger === button && menu.matches(":popover-open");
  menu.hidePopover();
  if (same) return;
  upstreamMenuTrigger = button;
  button.setAttribute("aria-expanded", "true");
  menu.innerHTML = '<button onclick="runUpstreamMenuAction(\'edit\',' + id + ')">' + icon("edit") + '编辑上游</button><button onclick="runUpstreamMenuAction(\'duplicate\',' + id + ')">' + icon("copy") + '复制上游</button><button onclick="runUpstreamMenuAction(\'model\',' + id + ')">' + icon("spark") + '测试模型</button><button onclick="runUpstreamMenuAction(\'test\',' + id + ')">' + icon("activity") + '测试连接</button><hr><button class="danger" onclick="runUpstreamMenuAction(\'delete\',' + id + ')">' + icon("trash") + '删除上游</button>';
  menu.showPopover();
  const rect = button.getBoundingClientRect();
  menu.style.left = Math.max(8, Math.min(innerWidth - menu.offsetWidth - 8, rect.right - menu.offsetWidth)) + "px";
  menu.style.top = Math.max(8, Math.min(innerHeight - menu.offsetHeight - 8, rect.bottom + 6)) + "px";
  menu.querySelector("button").focus();
}
function runUpstreamMenuAction(action, id) {
  $("upstreamActionMenu").hidePopover();
  if (action === "edit") openEditUpstream(id);
  if (action === "duplicate") duplicateUpstream(id);
  if (action === "model") openModelTest(id);
  if (action === "delete") deleteUpstream(id);
  if (action === "test") {
    const button = upstreamMenuTrigger;
    testUpstream(id, button);
  }
}
document.getElementById("upstreamActionMenu").addEventListener("beforetoggle", (event) => {
  if (event.newState === "closed" && upstreamMenuTrigger) upstreamMenuTrigger.setAttribute("aria-expanded", "false");
});
async function recoverUpstream(id, button) {
  setBusy(button, true, "恢复中");
  try {
    await api("api/upstreams/" + id + "/recover", { method: "POST" });
    await loadUpstreams();
    toast("上游已恢复");
  } catch (error) { reportError(error, "恢复失败"); } finally { setBusy(button, false); }
}

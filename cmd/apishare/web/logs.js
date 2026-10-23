/* Request records: server-wide filters, configurable columns and detail drawer. */
const logColumnDefinitions = [
  ["time", "时间", 100], ["model", "模型", 170], ["upstream", "上游", 155], ["key", "密钥", 115], ["client", "客户端", 160],
  ["tokens", "Tokens", 135], ["performance", "性能", 145], ["cost", "费用", 100], ["status", "状态", 65],
];
let visibleLogColumns = new Set(logColumnDefinitions.map(([id]) => id));
try {
  const saved = JSON.parse(localStorage.getItem("apishare_log_columns"));
  if (Array.isArray(saved)) visibleLogColumns = new Set(saved.filter((id) => logColumnDefinitions.some(([name]) => name === id)).concat("time", "status"));
} catch { /* Ignore a stale or malformed display preference. */ }
let logPeriod = "today", logSearchTimer = null, logAbortController = null;
const logFeatures = new Set();
let logDetailRecord = null, logDetailTab = "overview";

function logDateISO(value, end = false) {
  if (!value) return "";
  return value.slice(0, 16) + (end ? ":59+08:00" : ":00+08:00");
}
function logInputTime(date) { return fmtTime(date.toISOString()).slice(0, 16).replace(" ", "T"); }
function updateLogPeriods() {
  document.querySelectorAll("[data-log-period]").forEach((button) => button.setAttribute("aria-pressed", String(button.dataset.logPeriod === logPeriod)));
}
function setLogPeriod(period, reload = true) {
  logPeriod = period;
  const today = dateKey(new Date());
  let first = today, last = today;
  const [year, month, day] = today.split("-").map(Number);
  if (period === "week") {
    const weekday = new Date(today + "T12:00:00+08:00").getUTCDay();
    first = addDays(today, -((weekday + 6) % 7)); last = addDays(first, 6);
  }
  if (period === "month") {
    first = `${year}-${String(month).padStart(2, "0")}-01`;
    last = new Date(Date.UTC(year, month, 0)).toISOString().slice(0, 10);
  }
  $("logStart").value = first + "T00:00";
  $("logEnd").value = last + "T23:59";
  $("logDateError").classList.add("hidden");
  updateLogPeriods();
  if (reload) loadLogs(1);
}
function shiftLogPeriod(direction) {
  if (!$("logStart").value || !$("logEnd").value) { setLogPeriod("today"); return; }
  const start = new Date(logDateISO($("logStart").value)), end = new Date(logDateISO($("logEnd").value));
  if (start > end) { changeLogDates(); return; }
  if (logPeriod === "month") {
    const [year, month] = $("logStart").value.split("T")[0].split("-").map(Number);
    const first = new Date(Date.UTC(year, month - 1 + direction, 1));
    const last = new Date(Date.UTC(year, month + direction, 0));
    $("logStart").value = first.toISOString().slice(0, 10) + "T00:00";
    $("logEnd").value = last.toISOString().slice(0, 10) + "T23:59";
  } else {
    const duration = end.getTime() - start.getTime() + 60000;
    $("logStart").value = logInputTime(new Date(start.getTime() + duration * direction));
    $("logEnd").value = logInputTime(new Date(end.getTime() + duration * direction));
  }
  updateLogPeriods(); loadLogs(1);
}
function changeLogDates() { logPeriod = "custom"; updateLogPeriods(); loadLogs(1); }
function logQueryParams(page = 1) {
  const params = new URLSearchParams({page: String(Math.max(1, page)), size: $("logPageSize").value});
  const start = logDateISO($("logStart").value), end = logDateISO($("logEnd").value, true);
  if (start && end && new Date(start) > new Date(end)) throw new Error("开始时间不能晚于结束时间");
  if (start) params.set("start", start);
  if (end) params.set("end", end);
  for (const [field, id] of [["q", "logSearch"], ["status", "logStatusFilter"], ["key_id", "logKeyFilter"], ["upstream_id", "logUpstreamFilter"], ["model", "logModelFilter"], ["client_id", "logClientFilter"], ["client_version", "logClientVersionFilter"]]) {
    const value = $(id).value.trim(); if (value) params.set(field, value);
  }
  if (logFeatures.size) params.set("features", [...logFeatures].join(","));
  return params;
}
async function loadLogs(page = 1) {
  clearTimeout(logSearchTimer);
  let params;
  try { params = logQueryParams(page); }
  catch (error) {
    requestVersions.logs = (requestVersions.logs || 0) + 1;
    if (logAbortController) logAbortController.abort();
    $("logsTable").setAttribute("aria-busy", "false");
    $("logDateError").textContent = error.message; $("logDateError").classList.remove("hidden"); return;
  }
  $("logDateError").classList.add("hidden");
  const version = requestVersions.logs = (requestVersions.logs || 0) + 1;
  if (logAbortController) logAbortController.abort();
  logAbortController = new AbortController();
  $("logsTable").setAttribute("aria-busy", "true");
  loadClientOptions().catch((error) => reportError(error, "客户端选项加载失败"));
  $("logPrev").disabled = true; $("logNext").disabled = true;
  try {
    const result = await api("api/logs?" + params, {signal: logAbortController.signal});
    if (version !== requestVersions.logs) return;
    logsPage = result.page; logsCache = result.items || [];
    $("logPage").textContent = result.page;
    $("logTotal").textContent = number(result.total) + " 条记录";
    $("logRange").textContent = result.total ? "第 " + ((result.page - 1) * result.size + 1) + "–" + Math.min(result.page * result.size, result.total) + " 条，共 " + number(result.total) + " 条" : "暂无调用记录";
    $("logPrev").disabled = result.page <= 1;
    $("logNext").disabled = result.page * result.size >= result.total;
    const summary = result.summary;
    $("logQuerySummary").textContent = summary ? "当前筛选：成功 " + number(summary.successes) + " / " + number(summary.calls) + " 次 · " + number(summary.prompt_tokens + summary.completion_tokens) + " Tokens · " + fmtUSD(summary.cost_usd) : "";
    $("logUpdated").textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN", {timeZone: "Asia/Shanghai", hour12: false});
    renderLogs(); stampRefresh();
  } catch (error) {
    if (error.name === "AbortError" || version !== requestVersions.logs) return;
    logsCache = [];
    $("logTotal").textContent = "加载失败"; $("logRange").textContent = "请刷新后重试"; $("logQuerySummary").textContent = "";
    errorRow("logsTable", visibleLogColumns.size, error); reportError(error, "日志加载失败");
  } finally { if (version === requestVersions.logs) $("logsTable").setAttribute("aria-busy", "false"); }
}
function scheduleLogSearch() {
  // Invalidate pending results immediately; debounce only the next request.
  requestVersions.logs = (requestVersions.logs || 0) + 1;
  if (logAbortController) logAbortController.abort();
  clearTimeout(logSearchTimer); logSearchTimer = setTimeout(() => loadLogs(1), 300);
}
async function refreshLogs() {
  setBusy($("logRefreshButton"), true, "刷新中…");
  try { await loadLogs(logsPage); } finally { setBusy($("logRefreshButton"), false); }
}
function toggleLogFeature(feature) {
  if (logFeatures.has(feature)) logFeatures.delete(feature); else logFeatures.add(feature);
  document.querySelectorAll("[data-log-feature]").forEach((button) => button.setAttribute("aria-pressed", String(logFeatures.has(button.dataset.logFeature))));
  loadLogs(1);
}
function toggleLogFilters() {
  const panel = $("logAdvancedFilters"), opening = panel.classList.contains("hidden");
  for (const [id, items, label] of [["logKeyFilter", keysCache, "全部密钥"], ["logUpstreamFilter", upsCache, "全部上游"]]) {
    const field = $(id), selected = field.value;
    field.replaceChildren(new Option(label, ""));
    items.forEach((item) => field.add(new Option(item.name || "#" + item.id, String(item.id))));
    if (selected && !items.some((item) => String(item.id) === selected)) field.add(new Option("已删除配置 #" + selected, selected));
    field.value = selected;
  }
  panel.classList.toggle("hidden", !opening); $("logFilterButton").setAttribute("aria-expanded", String(opening));
  if (opening) $("logStatusFilter").focus();
}
function resetLogFilters() {
  for (const id of ["logSearch", "logStatusFilter", "logKeyFilter", "logUpstreamFilter", "logModelFilter", "logClientFilter", "logClientVersionFilter"]) $(id).value = "";
  logFeatures.clear();
  document.querySelectorAll("[data-log-feature]").forEach((button) => button.setAttribute("aria-pressed", "false"));
  loadLogs(1);
}
function logPerformance(log) {
  const first = Number(log.first_token_ms) || 0, total = Number(log.duration_ms) || Number(log.latency_ms) || 0;
  const duration = first > 0 && total - first >= total / 10 ? total - first : total;
  return {first, total, tps: duration > 0 && log.completion_tokens > 0 ? (log.completion_tokens / (duration / 1000)).toFixed(1) : null};
}
function logTimeLabel(value) {
  const age = Date.now() - new Date(value).getTime();
  if (age >= 0 && age < 60000) return Math.floor(age / 1000) + " 秒前";
  if (age >= 60000 && age < 3600000) return Math.floor(age / 60000) + " 分钟前";
  return fmtTime(value).slice(11);
}
function renderLogs() {
  $("logsTable").querySelector("tbody").innerHTML = logsCache.map((log) => {
    const performance = logPerformance(log), usage = log.billing?.usage, failed = !(log.status >= 200 && log.status < 300);
    const open = 'onclick="openLogDetails(' + log.id + ')"';
    const cache = usage?.cache_read || usage?.cache_write ? '<small class="log-cache" title="缓存创建 / 缓存读取">' + icon("layers") + compactUpstreamNumber(usage.cache_write) + ' / ' + compactUpstreamNumber(usage.cache_read) + '</small>' : '';
    return '<tr class="' + (failed ? 'log-failed-row' : '') + '"><td data-log-column="time"><button class="log-cell-button log-time" ' + open + ' title="' + esc(fmtTime(log.created_at)) + '">' + esc(logTimeLabel(log.created_at)) + '</button><small>' + esc(fmtTime(log.created_at).slice(5, 10)) + '</small></td><td data-log-column="model"><button class="log-cell-button log-model" ' + open + ' title="' + esc(log.model) + '">' + modelIcon(log.model) + '<span>' + esc(log.model || "未识别模型") + '</span></button></td><td data-log-column="upstream"><button class="log-cell-button log-upstream" ' + open + ' title="' + esc(log.upstream_name || "未分配上游") + '">' + esc(log.upstream_name || "未分配上游") + '</button><small class="log-endpoint" title="' + esc(log.endpoint) + '">' + esc(log.endpoint || '—') + '</small></td><td data-log-column="key"><button class="log-cell-button" ' + open + ' title="' + esc(log.key_name) + '">' + esc(log.key_name || '—') + '</button></td><td data-log-column="client"><button class="log-cell-button" ' + open + ' title="' + esc(log.client?.user_agent || logClientLabel(log)) + '">' + esc(logClientLabel(log)) + '</button><small class="log-client-note">' + esc(logClientNote(log)) + '</small></td><td data-log-column="tokens"><button class="log-cell-button log-token-count" ' + open + ' title="输入 / 输出 Tokens">↑' + compactUpstreamNumber(log.prompt_tokens) + ' ↓' + compactUpstreamNumber(log.completion_tokens) + '</button>' + cache + (usage?.reasoning ? '<small class="log-reasoning">推理 ' + compactUpstreamNumber(usage.reasoning) + '</small>' : '') + '</td><td data-log-column="performance"><button class="log-cell-button log-performance" ' + open + ' title="总耗时 / 首 Token 延迟">' + (performance.total ? fmtDuration(performance.total) : '—') + '<span> / ' + (performance.first ? fmtDuration(performance.first) : '—') + '</span></button><small>' + (performance.tps ? performance.tps + ' TPS' : '暂无速度数据') + '</small></td><td data-log-column="cost"><button class="log-cell-button log-cost" ' + open + '>' + fmtUSD(log.cost_usd) + '</button>' + (log.billing?.usage?.estimated_input || log.billing?.usage?.estimated_output ? '<small class="log-estimate">含估算用量</small>' : '') + '</td><td data-log-column="status"><button class="log-status-button" ' + open + ' aria-label="请求 #' + log.id + ' 详情，状态 ' + Number(log.status) + '"><span class="badge ' + (failed ? 'error' : 'on') + '">' + Number(log.status) + '</span></button></td></tr>';
  }).join("") || emptyRow(visibleLogColumns.size, "当前范围没有请求记录", "调整时间范围或筛选条件后重试。", "", "", "activity");
  applyLogColumns();
}
function applyLogColumns() {
  document.querySelectorAll("#logsTable [data-log-column]").forEach((cell) => cell.classList.toggle("hidden", !visibleLogColumns.has(cell.dataset.logColumn)));
  $("logsTable").style.minWidth = logColumnDefinitions.reduce((sum, [id, , width]) => sum + (visibleLogColumns.has(id) ? width : 0), 0) + "px";
  $("logsTable").querySelectorAll(".table-empty").forEach((empty) => { empty.closest("td").colSpan = visibleLogColumns.size; });
}
function toggleLogColumn(id, selected) {
  if (selected) visibleLogColumns.add(id); else visibleLogColumns.delete(id);
  visibleLogColumns.add("time"); visibleLogColumns.add("status");
  localStorage.setItem("apishare_log_columns", JSON.stringify([...visibleLogColumns])); applyLogColumns();
}
function toggleLogColumns(button) {
  const menu = $("logColumnsMenu");
  if (menu.matches(":popover-open")) { menu.hidePopover(); return; }
  menu.innerHTML = '<strong>显示的列</strong>' + logColumnDefinitions.map(([id, label]) => '<label><input type="checkbox" ' + (visibleLogColumns.has(id) ? 'checked ' : '') + (["time", "status"].includes(id) ? 'disabled ' : '') + 'onchange="toggleLogColumn(\'' + id + '\',this.checked)"><span>' + label + '</span></label>').join("");
  menu.showPopover(); button.setAttribute("aria-expanded", "true");
  const rect = button.getBoundingClientRect();
  menu.style.left = Math.max(8, Math.min(innerWidth - menu.offsetWidth - 8, rect.right - menu.offsetWidth)) + "px";
  menu.style.top = Math.max(8, Math.min(innerHeight - menu.offsetHeight - 8, rect.bottom + 6)) + "px";
}
function openLogDetails(id) {
  const log = logsCache.find((item) => item.id === id);
  if (!log) return;
  logDetailRecord = log; logDetailTab = "overview";
  $("logDetailTitle").textContent = "请求 #" + log.id;
  $("logDetailSubtitle").textContent = fmtTime(log.created_at);
  renderLogDetails(); $("logDetailDialog").showModal(); syncModalLock();
}
function setLogDetailTab(tab) { logDetailTab = tab; renderLogDetails(); }
function renderLogDetails() {
  const log = logDetailRecord;
  if (!log) return;
  document.querySelectorAll("[data-log-detail-tab]").forEach((button) => button.setAttribute("aria-pressed", String(button.dataset.logDetailTab === logDetailTab)));
  if (logDetailTab === "raw") { const pre = document.createElement("pre"); pre.textContent = JSON.stringify(log, null, 2); $("logDetailBody").replaceChildren(pre); return; }
  const perf = logPerformance(log);
  if (logDetailTab === "billing") {
    $("logDetailBody").innerHTML = '<section class="log-detail-section"><h3>Token 用量</h3><div class="log-detail-numbers"><div><span>输入</span><strong>' + number(log.prompt_tokens) + '</strong></div><div><span>输出</span><strong>' + number(log.completion_tokens) + '</strong></div></div>' + (logUsageDetails(log) || '<p class="muted">这条记录没有额外的 Token 分类明细。</p>') + '</section><section class="log-detail-section"><h3>调用费用</h3><div class="log-detail-cost">' + fmtUSD(log.cost_usd) + '</div>' + (logCostDetails(log) || '<p class="muted">这条记录没有保存详细的计价信息。</p>') + '</section>';
    $("logDetailBody").querySelectorAll("details").forEach((detail) => { detail.open = true; });
    return;
  }
  const fields = [["模型", log.model || "—"], ["上游服务", log.upstream_name || "未分配"], ["API 密钥", log.key_name || "—"], ["请求端点", log.endpoint || "—"], ["总耗时", perf.total ? fmtDuration(perf.total) : "—"], ["首 Token 延迟", perf.first ? fmtDuration(perf.first) : "—"], ["输出速度", perf.tps ? perf.tps + " tok/s" : "—"], ["费用", fmtUSD(log.cost_usd)]];
  $("logDetailBody").innerHTML = '<div class="log-detail-status">' + statusBadge(log.status) + '<strong>' + (log.status >= 200 && log.status < 300 ? "请求成功" : "请求失败") + '</strong></div><dl class="log-detail-fields">' + fields.map(([label, value]) => '<div><dt>' + label + '</dt><dd>' + (label === "模型" ? modelLabel(log.model) : esc(value)) + '</dd></div>').join("") + '</dl>' + logClientDetails(log) + (log.error ? '<section class="log-error-box"><h3>错误信息</h3><pre>' + esc(log.error) + '</pre></section>' : '');
  renderGatewayAccountAttempts(log);
}
function closeLogDetails() {
  if (!$("logDetailDialog").open) return;
  $("logDetailDialog").close(); logDetailRecord = null; $("logDetailBody").replaceChildren(); syncModalLock();
}
function copyLogRecord() { if (logDetailRecord) copyText(JSON.stringify(logDetailRecord, null, 2), "请求记录已复制"); }
async function exportLogs() {
  let params;
  try { params = logQueryParams(1); } catch (error) { toast(error.message, "error"); return; }
  params.set("export", "csv");
  setBusy($("logExportButton"), true, "导出中…");
  try {
    const response = await fetch("api/logs?" + params, {headers: token ? {Authorization: "Bearer " + token} : {}});
    if (response.status === 401) { showLogin(); throw new Error("unauthorized"); }
    if (!response.ok) { const result = await response.json().catch(() => ({})); throw new Error(result.error || "导出失败，请稍后重试"); }
    const blob = await response.blob(), url = URL.createObjectURL(blob), link = document.createElement("a");
    link.href = url; link.download = "apishare-logs-" + dateKey(new Date()) + ".csv"; document.body.append(link); link.click(); link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000); toast("筛选范围内的记录已导出");
  } catch (error) { reportError(error, "导出失败"); }
  finally { setBusy($("logExportButton"), false); }
}
function initLogs() {
  setLogPeriod("today", false); applyLogColumns();
  $("logColumnsMenu").addEventListener("beforetoggle", (event) => { if (event.newState === "closed") $("logColumnsButton").setAttribute("aria-expanded", "false"); });
  $("logDetailDialog").addEventListener("cancel", (event) => { event.preventDefault(); closeLogDetails(); });
  $("logDetailDialog").addEventListener("click", (event) => {
    const rect = $("logDetailDialog").getBoundingClientRect();
    if (event.target === $("logDetailDialog") && (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom)) closeLogDetails();
  });
}

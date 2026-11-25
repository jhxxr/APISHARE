// Failure diagnostics: a saved console of failed relay attempts.
// The list is intentionally plain so one click reveals the whole request.
let failureLogsCache = [];
let failureLogSettingsCache = { enabled: true, max_mb: 32, entry_kib: 64, full_content: false };
let failureLogSelected = "";
let failureLogRequestVersion = 0;

function initFailures() {
  warnFullContentScope();
}

function warnFullContentScope() {
  const note = $("failureLogSettingsNote");
  if (!note) return;
  note.textContent = $("failureLogFullContent").value === "true"
    ? "完整原文会保存请求体、响应体以及 Authorization 等请求头，仅在你需要精确复现问题期间开启。"
    : "脱敏模式下只保存结构、状态码、错误类型与上游错误摘要，不保存正文与敏感请求头。";
}

function failureLogSettingsFromCache() {
  $("failureLogEnabled").value = failureLogSettingsCache.enabled ? "true" : "false";
  $("failureLogMaxMB").value = String(failureLogSettingsCache.max_mb);
  $("failureLogEntryKiB").value = String(failureLogSettingsCache.entry_kib);
  $("failureLogFullContent").value = String(failureLogSettingsCache.full_content);
  warnFullContentScope();
}

function toggleFailureSettings() {
  const panel = $("failureLogSettingsPanel");
  const open = panel.classList.contains("hidden");
  panel.classList.toggle("hidden", !open);
  $("failureLogSettingsButton").setAttribute("aria-expanded", String(open));
}

async function saveFailureSettings() {
  const error = $("failureLogSettingsError");
  error.classList.add("hidden");
  const body = {
    enabled: $("failureLogEnabled").value === "true",
    max_mb: Number($("failureLogMaxMB").value),
    entry_kib: Number($("failureLogEntryKiB").value),
    full_content: $("failureLogFullContent").value === "true",
  };
  if (!Number.isInteger(body.max_mb) || body.max_mb < 1 || body.max_mb > 16384) {
    error.textContent = "最多保存容量需在 1 到 16384 MB 之间";
    error.classList.remove("hidden");
    return;
  }
  if (!Number.isInteger(body.entry_kib) || body.entry_kib < 1 || body.entry_kib > 1024) {
    error.textContent = "单条记录上限需在 1 到 1024 KiB 之间";
    error.classList.remove("hidden");
    return;
  }
  try {
    const saved = await api("api/failure-logs/settings", { method: "PUT", body: JSON.stringify(body) });
    failureLogSettingsCache = { enabled: saved.enabled, max_mb: saved.max_mb, entry_kib: saved.entry_kib, full_content: saved.full_content };
    failureLogSettingsFromCache();
    await loadFailureLogs(1);
  } catch (err) {
    error.textContent = err.message || "保存失败";
    error.classList.remove("hidden");
  }
}

async function loadFailureLogs(page = failuresPage) {
  const version = ++failureLogRequestVersion;
  const size = Number($("failureLogPageSize").value) || 50;
  const target = Math.max(1, Number(page) || 1);
  try {
    const data = await api("api/failure-logs?limit=" + size + "&offset=" + (target - 1) * size);
    if (version !== failureLogRequestVersion) return;
    failureLogsCache = data.items || [];
    failuresPage = target;
    // The list payload reports a single effective `enabled` flag; settings only
    // carries the retention fields, so merge instead of replacing.
    failureLogSettingsCache = { ...failureLogSettingsCache, ...(data.settings || {}), enabled: Boolean(data.enabled) };
    failureLogSettingsFromCache();
    renderFailureLogList(data);
  } catch (err) {
    if (version !== failureLogRequestVersion) return;
    $("failureLogList").innerHTML = `<div class="failure-empty">${esc(err.message || "无法加载失败记录")}</div>`;
  }
}

function renderFailureLogList(data) {
  const items = data.items || [];
  const total = data.total || 0;
  const size = Number($("failureLogPageSize").value) || 50;
  $("navFailureCount").textContent = total > 999 ? "999+" : String(total);
  $("failureLogSummary").textContent = data.enabled
    ? `共 ${total} 条记录 · 最多保存 ${failureLogSettingsCache.max_mb} MB`
    : "失败诊断当前已关闭，不再记录新的失败请求；已有记录仍可查看。";
  $("failureLogPage").textContent = String(failuresPage);
  $("failureLogPrev").disabled = failuresPage <= 1;
  $("failureLogNext").disabled = failuresPage * size >= total;
  const from = total === 0 ? 0 : (failuresPage - 1) * size + 1;
  const to = Math.min(failuresPage * size, total);
  $("failureLogRange").textContent = total === 0 ? "" : `第 ${from}–${to} 条，共 ${total} 条`;

  if (!items.length) {
    $("failureLogList").innerHTML = `<div class="failure-empty">${data.enabled
      ? "还没有失败记录。请求成功时不会生成条目。"
      : "失败诊断已关闭。"}</div>`;
    $("failureLogDetail").innerHTML = '<div class="failure-detail-empty">从左侧选择一条失败记录查看完整细节</div>';
    failureLogSelected = "";
    return;
  }
  $("failureLogList").innerHTML = items.map((item) => {
    const active = item.name === failureLogSelected;
    return `<button type="button" class="failure-row${active ? " active" : ""}" role="listitem" data-failure="${esc(item.name)}" onclick="openFailureLog(this.dataset.failure)">
      <span class="failure-row-top"><span class="failure-kind">${esc(item.failure_kind || "unknown")}</span><span class="failure-status">${esc(item.gateway_status || 0)}</span><span class="failure-time">${esc(failureLogTime(item.time))}</span></span>
      <span class="failure-row-model">${esc(item.model || "（未识别模型）")}</span>
      <span class="failure-row-summary">${esc(item.summary || item.route || "")}</span>
      <span class="failure-row-meta"><span>${esc(item.channel || "未知上游")}</span><span>${fmtFailureDuration(item.duration_ms)}</span><span>${fmtFailureSize(item.size)}</span></span>
    </button>`;
  }).join("");
  if (failureLogSelected && !items.some((item) => item.name === failureLogSelected)) {
    failureLogSelected = "";
    $("failureLogDetail").innerHTML = '<div class="failure-detail-empty">从左侧选择一条失败记录查看完整细节</div>';
  }
}

function fmtFailureDuration(ms) {
  const value = Number(ms) || 0;
  return value >= 1000 ? (value / 1000).toFixed(2) + " s" : value + " ms";
}

function fmtFailureSize(bytes) {
  const value = Number(bytes) || 0;
  return value >= 1024 ? (value / 1024).toFixed(1) + " KB" : value + " B";
}

function failureLogTime(value) {
  if (!value) return "";
  return new Date(value).toLocaleString("zh-CN", { timeZone: "Asia/Shanghai", hour12: false });
}

async function openFailureLog(name) {
  if (!name) return;
  failureLogSelected = name;
  document.querySelectorAll("[data-failure]").forEach((row) => row.classList.toggle("active", row.dataset.failure === name));
  const detail = $("failureLogDetail");
  detail.innerHTML = '<div class="failure-detail-empty">正在读取…</div>';
  try {
    const entry = await api("api/failure-logs/" + encodeURIComponent(name));
    if (failureLogSelected !== name) return;
    renderFailureLogDetail(entry);
  } catch (err) {
    if (failureLogSelected !== name) return;
    detail.innerHTML = `<div class="failure-detail-empty">${esc(err.message || "无法读取该记录")}</div>`;
  }
}

function renderHeaderTable(headers) {
  const rows = Object.entries(headers || {});
  if (!rows.length) return '<p class="failure-muted">没有可显示的请求头</p>';
  return `<div class="failure-headers">${rows.map(([name, value]) => {
    const sensitive = /^(authorization|proxy-authorization|cookie|x-api-key|api-key)$/i.test(name);
    return `<div class="failure-header-row"><span class="failure-header-name">${esc(name)}</span><span class="failure-header-value${sensitive ? " is-sensitive" : ""}">${esc(value)}</span></div>`;
  }).join("")}</div>`;
}

function renderFailureLogDetail(entry) {
  const request = entry.request || {};
  const upstream = entry.upstream || {};
  const bodyText = request.body && Object.keys(request.body).length
    ? JSON.stringify(request.body, null, 2)
    : (request.raw_body || "");
  const upstreamText = upstream.body || "";
  const parts = [];
  parts.push(`<div class="failure-detail-head">
    <div><span class="failure-kind">${esc(upstream.failure_kind || upstream.error_type || "unknown")}</span><h2>${esc(request.model || "（未识别模型）")}</h2>
    <p class="failure-muted">${esc(request.method || "")} ${esc(request.route || "")} · ${esc(failureLogTime(entry.time))}</p></div>
    <button type="button" class="btn ghost" onclick="deleteFailureLog('${esc(entry.name || failureLogSelected)}')" aria-label="删除这条失败记录"><svg class="icon" aria-hidden="true"><use href="#i-close"/></svg> 删除</button>
  </div>`);
  parts.push(`<div class="failure-facts">
    <div><span>请求编号</span><strong class="failure-mono">${esc(entry.request_id || "—")}</strong></div>
    <div><span>网关状态</span><strong>${esc(entry.gateway_status || 0)}</strong></div>
    <div><span>上游状态</span><strong>${esc(upstream.http_status || "—")}</strong></div>
    <div><span>耗时</span><strong>${fmtFailureDuration(entry.duration_ms)}</strong></div>
    <div><span>上游</span><strong>${esc(upstream.channel || "—")}</strong></div>
    <div><span>上游模型</span><strong>${esc(upstream.model || "—")}</strong></div>
  </div>`);
  if (upstream.summary) parts.push(`<section class="failure-section"><h3>失败摘要</h3><p class="failure-summary-text">${esc(upstream.summary)}</p></section>`);
  parts.push(`<section class="failure-section"><h3>请求体${request.truncated ? ' <span class="failure-badge">已截断</span>' : ""}</h3>
    ${entry.truncated ? '<p class="failure-warning">该记录因超过单条上限而被截断。</p>' : ""}
    ${bodyText ? `<pre class="failure-pre">${esc(bodyText)}</pre>` : '<p class="failure-muted">脱敏模式下未保存请求正文。需要原始内容请在“保存设置”中切换为完整原文。</p>'}</section>`);
  parts.push(`<section class="failure-section"><h3>上游返回${upstream.truncated ? ' <span class="failure-badge">已截断</span>' : ""}</h3>
    ${upstreamText ? `<pre class="failure-pre">${esc(upstreamText)}</pre>` : '<p class="failure-muted">没有上游响应正文</p>'}</section>`);
  parts.push(`<section class="failure-section"><h3>请求头</h3>${renderHeaderTable(request.headers)}</section>`);
  parts.push(`<section class="failure-section"><h3>原始记录</h3><pre class="failure-pre">${esc(JSON.stringify(entry, null, 2))}</pre></section>`);
  $("failureLogDetail").innerHTML = parts.join("");
}

async function deleteFailureLog(name) {
  name = name || failureLogSelected;
  if (!name) return;
  if (!await askAction({ title: "删除这条失败记录？", description: "删除后无法恢复。", confirm: "删除", danger: true })) return;
  try {
    await api("api/failure-logs/" + encodeURIComponent(name), { method: "DELETE" });
    if (failureLogSelected === name) {
      failureLogSelected = "";
      $("failureLogDetail").innerHTML = '<div class="failure-detail-empty">从左侧选择一条失败记录查看完整细节</div>';
    }
    await loadFailureLogs(failuresPage);
  } catch (err) {
    toast(err.message || "删除失败", "error");
  }
}

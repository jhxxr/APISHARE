/* Inbound client identity, retained request details and durable aggregates. */
let clientOptionsCache = null, clientOptionsPending = null, clientStatsCache = null;

async function loadClientOptions() {
  if (!clientOptionsCache) {
    if (!clientOptionsPending) {
      clientOptionsPending = api("api/clients").then((result) => {
        clientOptionsCache = result.items || [];
      }).finally(() => { clientOptionsPending = null; });
    }
    await clientOptionsPending;
  }
  const field = $("logClientFilter"), selected = field.value;
  field.replaceChildren(new Option("全部客户端", ""));
  clientOptionsCache.forEach((client) => field.add(new Option(client.name, client.id)));
  field.value = selected;
}

function logClientLabel(log) { return log.client?.name || "未知客户端"; }
function logClientNote(log) {
  const client = log.client;
  if (!client || !client.rule_version) return "历史记录 · 未采集";
  if (client.version) return client.version;
  return client.sdk_name || "版本未知";
}
function logClientDetails(log) {
  const client = log.client || {};
  const sources = {user_agent: "User-Agent", auxiliary_headers: "应用归属头", originator: "originator", sdk_only: "仅识别出 SDK", unknown: "未识别"};
  const fields = [
    ["客户端", logClientLabel(log)], ["客户端版本", client.version || "—"],
    ["SDK", client.sdk_name || "—"], ["SDK 版本", client.sdk_version || "—"],
    ["识别依据", sources[client.source] || "未识别"],
    ["规则版本", client.rule_version ? String(client.rule_version) : "历史记录 · 未采集"],
  ];
  let note = "";
  if (client.conflict) note = "请求的 UA 与辅助标识存在冲突，已保留 UA 声明和冲突状态。";
  else if (client.ambiguous) note = "此请求声明 Claude Code 身份；部分兼容客户端也会使用相同标识。";
  return '<section class="log-detail-section log-client-details"><h3>请求客户端</h3><dl class="log-detail-fields">'
    + fields.map(([label, value]) => '<div><dt>' + esc(label) + '</dt><dd>' + esc(value) + '</dd></div>').join("")
    + '</dl>' + (note ? '<p class="log-client-note">' + esc(note) + '</p>' : '')
    + '<h3>User-Agent</h3><pre>' + esc(client.user_agent || "未采集或请求未提供") + '</pre></section>';
}

async function loadClientStats() {
  const version = requestVersions.clientStats = (requestVersions.clientStats || 0) + 1;
  const table = $("clientStatsTable");
  table.setAttribute("aria-busy", "true");
  try {
    const result = await api("api/client-stats?period=" + encodeURIComponent($("clientStatsPeriod").value));
    if (version !== requestVersions.clientStats) return;
    clientStatsCache = result;
    table.querySelector("tbody").innerHTML = result.items.map((client) => {
      const success = client.calls ? (client.successes / client.calls * 100).toFixed(1) + "%" : "—";
      return '<tr><td><button type="button" class="text-btn" data-client-id="' + esc(client.id) + '" aria-label="查看 '
        + esc(client.name) + ' 的保留日志">' + esc(client.name) + '</button></td>'
        + '<td>' + number(client.calls) + ' / ' + number(client.successes) + '</td><td>' + success + '</td>'
        + '<td>↑ ' + fmtCompact(client.prompt_tokens) + ' / ↓ ' + fmtCompact(client.completion_tokens) + '</td>'
        + '<td>' + fmtUSD(client.cost_usd) + '</td><td>' + esc(fmtTime(client.last_at) || "—") + '</td></tr>';
    }).join("") || emptyRow(6, "当前范围没有客户端调用", "发送模型请求后会自动记录客户端信息。", "", "", "activity");
    table.querySelectorAll("[data-client-id]").forEach((button) => button.addEventListener("click", () => showClientLogs(button.dataset.clientId).catch((error) => reportError(error, "无法打开客户端日志"))));
    const unknown = result.items.find((client) => client.id === "unknown")?.calls || 0;
    $("clientStatsSummary").textContent = number(result.summary.calls) + " 次请求 · "
      + number(result.summary.prompt_tokens + result.summary.completion_tokens) + " Tokens · "
      + fmtUSD(result.summary.cost_usd) + " · 未知客户端 " + number(unknown) + " 次";
  } catch (error) {
    if (version !== requestVersions.clientStats) return;
    clientStatsCache = null;
    $("clientStatsSummary").textContent = "客户端统计加载失败";
    errorRow("clientStatsTable", 6, error);
    reportError(error, "客户端统计加载失败");
  } finally { if (version === requestVersions.clientStats) table.setAttribute("aria-busy", "false"); }
}

async function showClientLogs(id) {
  await loadClientOptions();
  for (const field of ["logSearch", "logStatusFilter", "logKeyFilter", "logUpstreamFilter", "logModelFilter", "logClientVersionFilter"]) $(field).value = "";
  logFeatures.clear();
  document.querySelectorAll("[data-log-feature]").forEach((button) => button.setAttribute("aria-pressed", "false"));
  $("logClientFilter").value = id;
  $("logStart").value = clientStatsCache?.start ? logInputTime(new Date(clientStatsCache.start)) : "";
  $("logEnd").value = clientStatsCache?.end ? logInputTime(new Date(clientStatsCache.end)) : "";
  logPeriod = "custom";
  updateLogPeriods();
  $("logAdvancedFilters").classList.remove("hidden");
  $("logFilterButton").setAttribute("aria-expanded", "true");
  logsPage = 1;
  await switchTab("logs");
}

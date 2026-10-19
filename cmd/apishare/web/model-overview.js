/* Admin model overview: every model, its configured upstream sources and 24h uptime. */
let modelOverviewCache = null;

const modelStatusText = { operational: "运行正常", incident: "最近调用失败", idle: "暂无近期调用" };
const modelStatusBadge = { operational: "on", incident: "error", idle: "off" };

function fmtModelRate(value) {
  return value === null || value === undefined ? "—" : Number(value).toFixed(2) + "%";
}

function fmtModelTPS(value) {
  return value === null || value === undefined ? "—" : Number(value).toFixed(1) + " tok/s";
}

function modelSourceStatus(source) {
  if (!source.configured) return '<span class="badge off">历史来源</span>';
  if (!source.healthy) return '<span class="badge warn">冷却中</span>';
  if (source.rate_limited) return '<span class="badge warn">限流冷却</span>';
  return '<span class="badge on">可用</span>';
}

function modelSourceTitle(source) {
  const parts = [source.name || ("上游 #" + source.upstream_id), source.type].filter(Boolean).join(" · ");
  const mapping = source.upstream_model && source.upstream_model !== source.name ? " → " + source.upstream_model : "";
  return parts + mapping + (source.explicit ? "" : " · 通配");
}

async function loadModelOverview(quiet = false) {
  const version = requestVersions.modelOverview = (requestVersions.modelOverview || 0) + 1;
  try {
    const result = await api("api/models/overview");
    if (version !== requestVersions.modelOverview) return;
    modelOverviewCache = result || { models: [] };
    renderModelOverview();
    stampRefresh();
  } catch (error) {
    $("modelsOverviewTable").querySelector("tbody").innerHTML = emptyRow(6, "加载失败", error.message, "refreshCurrent()", "重新加载", "info");
    if (!quiet) reportError(error, "模型总览加载失败");
  }
}

function renderModelOverview() {
  const overview = modelOverviewCache || { models: [] };
  const models = overview.models || [];
  const query = ($("modelSearch").value || "").trim().toLowerCase();
  const status = $("modelStatusFilter").value;
  const list = models.filter((item) => {
    if (status && item.status !== status) return false;
    if (!query) return true;
    const haystack = [item.model, item.family, item.status, ...(item.sources || []).map((source) => [source.name, source.type, source.upstream_model].join(" "))].join(" ").toLowerCase();
    return haystack.includes(query);
  });
  $("modelTotal").textContent = models.length;
  $("modelOperational").textContent = models.filter((item) => item.status === "operational").length;
  $("modelIncident").textContent = models.filter((item) => item.status === "incident").length;
  $("navModelCount").textContent = models.length;
  $("navModelCount").setAttribute("aria-label", models.length + " 个模型");
  $("modelListCount").textContent = list.length + (list.length !== models.length ? " / " + models.length : "");
  $("modelOverviewUpdated").textContent = overview.updated_at ? "更新于 " + fmtTime(overview.updated_at) + " · 最近 24 小时" : "";
  if (overview.models_loading) {
    $("modelOverviewUpdated").textContent += (overview.updated_at ? " · " : "") + "部分通配模型正在后台刷新";
  }
  $("modelsOverviewTable").querySelector("tbody").innerHTML = list.map((item) => {
    const sources = item.sources || [];
    const configured = sources.filter((source) => source.configured);
    const sourceCell = sources.length
      ? '<div class="model-source-list">' + sources.map((source) =>
        '<span class="model-source" title="' + esc(modelSourceTitle(source)) + '">' + providerIcon(source.type) + '<span><strong>' + esc(source.name || ("上游 #" + source.upstream_id)) + '</strong><small>' + esc([source.type, source.upstream_model && source.upstream_model !== item.model ? "→ " + source.upstream_model : "", source.explicit ? "显式" : "通配", source.cooldown ? "冷却" : "", source.rate_limited ? "限流" : "", source.configured ? "" : "已移除"].filter(Boolean).join(" · ") || "—") + ' · ' + fmtModelRate(source.uptime_percent) + '</small></span>' + modelSourceStatus(source) + '</span>'
      ).join("") + "</div>"
      : '<span class="muted">暂无来源</span>';
    return '<tr><td><div class="provider-cell">' + providerIcon((configured[0] || sources[0] || {}).type) + '<span class="cell-stack"><strong>' + esc(item.model) + '</strong><small>' + esc(item.family || "") + (item.last_call ? " · 最近 " + esc(fmtTime(item.last_call).slice(5)) : "") + '</small></span></div></td>'
      + '<td class="model-sources-cell">' + sourceCell + '</td>'
      + '<td><code>' + fmtModelRate(item.uptime_percent) + '</code><span class="cell-stack"><small>' + number(item.successes) + " / " + number(item.calls) + '</small></span></td>'
      + '<td><code>' + number(item.calls) + '</code></td>'
      + '<td><code>' + fmtModelTPS(item.avg_tps) + '</code></td>'
      + '<td><span class="badge ' + (modelStatusBadge[item.status] || "off") + '">' + esc(modelStatusText[item.status] || item.status) + '</span>' + (item.last_status ? '<span class="cell-stack"><small>最近 ' + item.last_status + '</small></span>' : "") + '</td></tr>';
  }).join("") || emptyRow(6, query || status ? "没有匹配的模型" : "还没有可用模型", query || status ? "调整搜索条件或状态筛选。" : overview.models_loading ? "通配上游的模型正在后台刷新，稍后点击右上角刷新重试。" : "连接上游并配置模型后，会在这里展示来源与状态。", query || status ? "" : "openCreateUpstream()", "添加上游");
  labelTableCells("modelsOverviewTable");
}

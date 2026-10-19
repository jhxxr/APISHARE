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

// A model can be served by many upstreams, so each source is a compact chip and
// only the first few stay visible; the rest open on demand instead of stretching
// the row down the page.
const modelSourceVisible = 4;

function modelSourceStatus(source) {
  if (!source.healthy) return '<span class="model-source-state warn" title="冷却中"><span class="sr-only">冷却中</span></span><span class="model-source-note">冷却</span>';
  if (source.rate_limited) return '<span class="model-source-state warn" title="限流冷却"><span class="sr-only">限流冷却</span></span><span class="model-source-note">限流</span>';
  return '<span class="model-source-state on" title="可用"><span class="sr-only">可用</span></span>';
}

function modelSourceDetail(source, model) {
  return [
    source.type,
    source.upstream_model && source.upstream_model !== model ? "→ " + source.upstream_model : "",
    source.explicit ? "显式" : "通配",
    source.cooldown ? "冷却" : "",
    source.rate_limited ? "限流" : "",
  ].filter(Boolean).join(" · ");
}

function modelSourceChip(source, model, extra) {
  const name = source.name || ("上游 #" + source.upstream_id);
  const detail = modelSourceDetail(source, model);
  const uptime = fmtModelRate(source.uptime_percent);
  return '<span class="model-source' + (extra ? " model-source-extra" : "") + '" title="' + esc([name, detail, uptime].filter(Boolean).join(" · ")) + '">'
    + providerIcon(source.type)
    + '<span class="model-source-name">' + esc(name) + '</span>'
    + '<span class="model-source-rate">' + (source.uptime_percent === null || source.uptime_percent === undefined ? "—" : Math.round(source.uptime_percent) + "%") + '</span>'
    + modelSourceStatus(source) + '</span>';
}

function toggleModelSources(button) {
  const list = button.closest(".model-source-list");
  if (!list) return;
  const expanded = list.classList.toggle("expanded");
  button.setAttribute("aria-expanded", expanded ? "true" : "false");
  button.textContent = expanded ? "收起" : button.dataset.more;
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
    const hidden = Math.max(0, sources.length - modelSourceVisible);
    const sourceCell = sources.length
      ? '<div class="model-source-list">'
        + sources.map((source, index) => modelSourceChip(source, item.model, index >= modelSourceVisible)).join("")
        + (hidden ? '<button type="button" class="model-source-more" onclick="toggleModelSources(this)" aria-expanded="false" aria-label="展开其余 ' + hidden + ' 个来源" title="展开其余 ' + hidden + ' 个来源" data-more="+' + hidden + '">+' + hidden + "</button>" : "")
        + "</div>"
      : '<span class="muted">暂无来源</span>';
    return '<tr><td><div class="provider-cell">' + providerIcon((sources[0] || {}).type) + '<span class="cell-stack"><strong>' + esc(item.model) + '</strong><small>' + esc(item.family || "") + (item.last_call ? " · 最近 " + esc(fmtTime(item.last_call).slice(5)) : "") + '</small></span></div></td>'
      + '<td class="model-sources-cell">' + sourceCell + '</td>'
      + '<td><code>' + fmtModelRate(item.uptime_percent) + '</code><span class="cell-stack"><small>' + number(item.successes) + " / " + number(item.calls) + '</small></span></td>'
      + '<td><code>' + number(item.calls) + '</code></td>'
      + '<td><code>' + fmtModelTPS(item.avg_tps) + '</code></td>'
      + '<td><span class="badge ' + (modelStatusBadge[item.status] || "off") + '">' + esc(modelStatusText[item.status] || item.status) + '</span>' + (item.last_status ? '<span class="cell-stack"><small>最近 ' + item.last_status + '</small></span>' : "") + '</td></tr>';
  }).join("") || emptyRow(6, query || status ? "没有匹配的模型" : "还没有可用模型", query || status ? "调整搜索条件或状态筛选。" : overview.models_loading ? "通配上游的模型正在后台刷新，稍后点击右上角刷新重试。" : "连接上游并配置模型后，会在这里展示来源与状态。", query || status ? "" : "openCreateUpstream()", "添加上游");
  labelTableCells("modelsOverviewTable");
}

(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const dimensions = [
    ["coding_swe", "工程", "软件工程 / SWE"], ["frontend_ui", "前端", "前端与 UI"],
    ["reasoning_math", "推理", "数学与推理"], ["agentic_tools", "工具", "Agent 与工具"],
    ["writing_knowledge", "写作", "写作与知识"], ["long_context_mm", "长文", "长上下文 / 多模态"],
  ];
  const metricNames = Object.fromEntries([["overall_score", "综合能力"], ...dimensions.map(([key, , name]) => [key, name]), ["bandit_win_rate", "Bandit 后验表现"], ["ewma_ttft_ms", "在线首字延迟"]]);
  const sourceNames = { calibrated: "内置校准", inferred: "模型族推断", override: "站点覆盖", synced: "同步数据" };
  let snapshot = null;
  let loading = false;

  function metricValue(model, metric) {
    const value = metric === "bandit_win_rate" || metric === "ewma_ttft_ms" ? model[metric] : model.capabilities?.[metric];
    if (value == null || !Number.isFinite(Number(value))) return null;
    if (metric === "bandit_win_rate" && !(model.bandit_calls > 0)) return null;
    if (metric === "ewma_ttft_ms" && !(model.bandit_calls > 0 && value > 0)) return null;
    return Number(value);
  }

  function formatScore(value, metric) {
    if (value === null) return "—";
    if (metric === "bandit_win_rate") return (value * 100).toFixed(0) + "%";
    if (metric === "ewma_ttft_ms") return value >= 1000 ? (value / 1000).toFixed(2) + "s" : Math.round(value) + "ms";
    return value.toFixed(1);
  }

  function source(model) {
    return sourceNames[model.capabilities?.benchmark_source] || "其他校准";
  }

  function modelLabel(model) {
    return window.APIShareModelIcons?.labelHTML(model.model, model.family) || '<span>' + esc(model.model) + '</span>';
  }

  function sortedModels(metric) {
    return [...snapshot.models].sort((a, b) => {
      const av = metricValue(a, metric), bv = metricValue(b, metric);
      if (av === null && bv !== null) return 1;
      if (bv === null && av !== null) return -1;
      if (av !== bv) return metric === "ewma_ttft_ms" ? av - bv : bv - av;
      return a.model.localeCompare(b.model, "en");
    }).map((model, index) => ({ model, rank: metricValue(model, metric) === null ? null : index + 1 }));
  }

  function render() {
    if (!snapshot) return;
    const metric = $("rankingSort").value;
    const query = $("rankingSearch").value.trim().toLowerCase();
    const family = $("rankingFamily").value;
    const ordered = sortedModels(metric);
    // Keep ranks consistent with the complete selected-metric leaderboard,
    // rather than renumbering models when a search narrows the visible list.
    const rows = ordered.filter(({ model }) => (!family || model.family === family) && (!query || (model.model + " " + model.family).toLowerCase().includes(query)));
    $("rankingModelCount").textContent = snapshot.models.length.toLocaleString("zh-CN");
    $("rankingMeasuredCount").textContent = snapshot.models.filter((m) => m.bandit_calls > 0).length.toLocaleString("zh-CN");
    $("rankingResultCount").textContent = "显示 " + rows.length + " / " + snapshot.models.length + " 个模型 · 按" + metricNames[metric] + "排序";
    $("rankingScoreHeading").textContent = metricNames[metric];
    $("leadersTitle").textContent = metricNames[metric] + " · 领先模型";
    $("rankingLoadingNote").classList.toggle("hidden", !snapshot.models_loading);
    const time = new Date(snapshot.updated_at);
    $("rankingUpdatedAt").textContent = Number.isNaN(time.getTime()) ? "更新时间未知" : "更新于 " + time.toLocaleString("zh-CN", { timeZone: "Asia/Shanghai", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hourCycle: "h23" }) + " · 北京时间";
    const leaders = rows.filter(({ rank }) => rank !== null).slice(0, 3);
    $("rankingLeaders").innerHTML = leaders.length ? leaders.map(({ model, rank }) => {
      const value = formatScore(metricValue(model, metric), metric);
      const suffix = metric === "overall_score" || dimensions.some(([key]) => key === metric) ? "/ 100" : "";
      return '<article class="ranking-leader"><div class="ranking-leader-top"><span class="ranking-leader-position">NO. ' + String(rank).padStart(2, "0") + '</span><span class="ranking-source">' + esc(source(model)) + '</span></div>'
        + '<h3>' + modelLabel(model) + '</h3><div class="ranking-leader-score"><strong>' + esc(value) + '</strong><span>' + suffix + '</span></div><p>' + esc(model.family) + (model.bandit_calls > 0 ? ' · ' + Number(model.bandit_calls).toLocaleString("zh-CN") + ' 个在线样本' : ' · 暂无在线样本') + '</p></article>';
    }).join("") : '<p class="ranking-placeholder">' + (rows.length ? "当前模型尚无此项在线样本，请切换能力维度查看。" : "暂无匹配模型。") + '</p>';
    $("rankingList").innerHTML = rows.map(({ model, rank }) => {
      const dims = dimensions.map(([key, short, name]) => {
        const value = metricValue(model, key);
        return '<div class="ranking-dimension' + (metric === key ? ' is-selected' : '') + '"><dt title="' + esc(name) + '">' + short + '</dt><dd>' + (value === null ? "—" : value.toFixed(0)) + '<div class="ranking-mini-bar" aria-hidden="true"><i style="width:' + Math.max(0, Math.min(100, value ?? 0)) + '%"></i></div></dd></div>';
      }).join("");
      const online = model.bandit_calls > 0
        ? '<span>后验 <strong>' + formatScore(metricValue(model, "bandit_win_rate"), "bandit_win_rate") + '</strong> · ' + Number(model.bandit_calls).toLocaleString("zh-CN") + ' 样本</span><span>首字 <strong>' + formatScore(metricValue(model, "ewma_ttft_ms"), "ewma_ttft_ms") + '</strong></span>'
        : '<span>暂无在线样本</span><span>能力分仅供参考</span>';
      return '<li class="ranking-row"><div class="ranking-identity"><span class="ranking-position' + (rank !== null && rank <= 3 ? ' is-leading' : '') + '" aria-label="' + (rank === null ? '暂无名次' : '第 ' + rank + ' 名') + '">' + (rank ?? "—") + '</span><div class="ranking-model">'
        + modelLabel(model) + '<div class="ranking-model-meta"><span>' + esc(model.family) + '</span><span>' + esc(source(model)) + '</span></div></div></div>'
        + '<div class="ranking-score"><strong>' + formatScore(metricValue(model, metric), metric) + '</strong><span>' + esc(metricNames[metric]) + '</span></div>'
        + '<dl class="ranking-dimensions" aria-label="六维能力参考分">' + dims + '</dl><div class="ranking-online">' + online + '</div></li>';
    }).join("");
    $("rankingEmpty").classList.toggle("hidden", rows.length > 0);
    $("rankingEmpty").querySelector("h3").textContent = snapshot.models.length ? "没有匹配的模型" : "暂未接入模型";
    $("rankingEmpty").querySelector("p").textContent = snapshot.models.length ? "试试其他关键词，或切换提供方。" : "站点接入模型后，能力排行将自动显示在这里。";
  }

  async function refresh() {
    if (loading) return;
    loading = true;
    const button = $("rankingRefresh");
    button.disabled = true;
    button.querySelector("span").textContent = "读取中…";
    $("rankingList").setAttribute("aria-busy", "true");
    $("rankingLeaders").setAttribute("aria-busy", "true");
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 15000);
    try {
      const response = await fetch("/api/public/rankings", { credentials: "omit", cache: "no-store", signal: controller.signal });
      if (!response.ok) throw new Error("request failed");
      const data = await response.json();
      if (!Array.isArray(data.models) || data.models.some((m) => !m || typeof m.model !== "string" || typeof m.family !== "string")) throw new Error("invalid rankings");
      snapshot = data;
      const selectedFamily = $("rankingFamily").value;
      const families = [...new Set(snapshot.models.map((m) => m.family))].sort((a, b) => a.localeCompare(b, "en"));
      $("rankingFamily").innerHTML = '<option value="">全部提供方</option>' + families.map((f) => '<option value="' + esc(f) + '">' + esc(f) + '</option>').join("");
      $("rankingFamily").value = families.includes(selectedFamily) ? selectedFamily : "";
      $("rankingError").classList.add("hidden");
      render();
    } catch {
      $("rankingError").textContent = snapshot ? "更新失败，当前保留上次读取的排行。请稍后点击“刷新排行”重试。" : "暂时无法读取模型排行，请点击“刷新排行”重试。";
      $("rankingError").classList.remove("hidden");
      if (!snapshot) {
        $("rankingList").innerHTML = "";
        $("rankingLeaders").innerHTML = '<p class="ranking-placeholder">模型排行暂不可用。</p>';
        $("rankingResultCount").textContent = "读取失败 · 可重试";
      }
    } finally {
      clearTimeout(timeout);
      loading = false;
      button.disabled = false;
      button.querySelector("span").textContent = "刷新排行";
      $("rankingList").setAttribute("aria-busy", "false");
      $("rankingLeaders").setAttribute("aria-busy", "false");
    }
  }

  $("rankingFilters").addEventListener("submit", (event) => event.preventDefault());
  $("rankingSearch").addEventListener("input", render);
  $("rankingSort").addEventListener("change", render);
  $("rankingFamily").addEventListener("change", render);
  $("rankingRefresh").addEventListener("click", refresh);
  $("rankingClear").addEventListener("click", () => {
    $("rankingSearch").value = "";
    $("rankingFamily").value = "";
    render();
    $("rankingSearch").focus();
  });
  refresh();
})();

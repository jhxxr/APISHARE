(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&":"&amp;", "<":"&lt;", ">":"&gt;", '"':"&quot;", "'":"&#39;" }[c]));
  const snapshots = new Map();
  const providerNames = { openai:"OpenAI", google:"Google", anthropic:"Anthropic", xai:"xAI", deepseek:"DeepSeek", alibaba:"Alibaba", moonshot:"Moonshot", meta:"Meta", mistral:"Mistral", nvidia:"NVIDIA" };
  const provider = (name) => providerNames[String(name).toLowerCase()] || name || "未标注提供方";
  const boardTabs = [...document.querySelectorAll("[data-board]")];
  let snapshot = null, scope = "", currentPage = 1, requestVersion = 0, activeController = null, detailModel = null;
  const modelLabel = (m) => window.APIShareModelIcons?.labelHTML(m.model, m.family) || '<span>' + esc(m.model) + '</span>';
  const score = (value) => value == null ? "—" : Number(value).toFixed(snapshot?.score_kind === "ips" ? 4 : 2);
  const count = (value) => value == null ? "—" : Number(value).toLocaleString("zh-CN");
  const sampleCount = (value) => value == null ? "—" : value >= 10000 ? (value / 1000).toFixed(1) + "k" : count(value);
  const badge = (m) => m.site_available
    ? '<span class="arena-site-badge" title="' + esc(m.site_match_note || '本站已接入同型号，思考配置可能不同') + '">已接入</span>'
    : m.related_site_models?.length
      ? '<span class="arena-site-badge is-version-mismatch" title="' + esc(m.site_match_note || '本站为不同日期版本，不套用此分数') + '">本站版本不同</span>'
      : '<span class="arena-unmarked">—</span>';
  const normalizeQuery = (value) => String(value).toLowerCase().replace(/[^\p{L}\p{N}]+/gu, "");

  function fitTableHeight() {
    const area = $("rankingTableScroll");
    if (typeof area.getBoundingClientRect !== "function") return;
    const pager = document.querySelector(".arena-pagination");
    const available = window.innerHeight - area.getBoundingClientRect().top - (pager?.offsetHeight || 64) - 16;
    const maximum = window.innerWidth <= 760 ? window.innerHeight * 0.6 : 680;
    area.style.maxHeight = Math.max(240, Math.min(maximum, available)) + "px";
  }

  function filteredRows() {
    if (!snapshot) return [];
    const query = $("rankingSearch").value.trim().toLowerCase(), normalizedQuery = normalizeQuery(query), family = $("rankingFamily").value;
    return snapshot.models.map((model, index) => ({ model, index })).filter(({ model:m }) => {
      const haystack = m.model + " " + m.family + " " + (m.site_models || []).join(" ");
      return (scope !== "site" || m.site_available) && (!family || m.family === family) && (!query || haystack.toLowerCase().includes(query) || (normalizedQuery && normalizeQuery(haystack).includes(normalizedQuery)));
    });
  }

  function render() {
    if (!snapshot) return;
    const rows = filteredRows(), size = [25,50,100].includes(Number($("rankingPageSize").value)) ? Number($("rankingPageSize").value) : 25;
    const pages = Math.max(1, Math.ceil(rows.length / size)); currentPage = Math.max(1, Math.min(currentPage, pages));
    const start = (currentPage - 1) * size, visible = rows.slice(start, start + size);
    $("rankingModelCount").textContent = count(snapshot.models.length);
    $("rankingSiteCount").textContent = count(snapshot.models.filter(m => m.site_available).length);
    $("rankingScopeAll").setAttribute("aria-pressed", String(scope !== "site")); $("rankingScopeSite").setAttribute("aria-pressed", String(scope === "site"));
    boardTabs.forEach(button => button.setAttribute("aria-pressed", String(button.dataset.board === snapshot.board)));
    $("rankingBoardLabel").textContent = snapshot.board_label;
    $("rankingScoreHeading").textContent = snapshot.score_label; $("rankingSampleHeading").textContent = snapshot.sample_label;
    $("rankingPublication").textContent = snapshot.published_at ? "源榜 " + snapshot.published_at : "尚无官方快照";
    const fetched = new Date(snapshot.fetched_at);
    $("rankingUpdatedAt").textContent = Number.isNaN(fetched.getTime()) ? "等待每日同步" : "本地同步 " + fetched.toLocaleString("zh-CN", {timeZone:"Asia/Shanghai",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",hourCycle:"h23"});
    const notices = [];
    if (snapshot.loading) notices.push("后台正在同步官方数据，当前显示本地缓存。");
    if (snapshot.stale) notices.push(snapshot.models.length ? "当前为上次成功同步的快照，请留意源榜日期。" : "官方快照尚未同步成功，等待下次每日同步。");
    if (snapshot.models_loading) notices.push("本站模型目录尚在补全，部分接入标记可能暂未显示。");
    $("rankingLoadingNote").textContent = notices.join(" "); $("rankingLoadingNote").classList.toggle("hidden", !notices.length);
    $("rankingList").innerHTML = visible.map(({model:m,index}) => {
      const interval = m.rating_lower != null && m.rating_upper != null ? score(m.rating_lower) + " – " + score(m.rating_upper) : "—";
      return '<tr class="ranking-row' + (m.site_available ? ' is-connected' : '') + '"><td class="arena-col-rank"><span class="ranking-position' + (m.rank <= 3 ? ' is-leading' : '') + '" aria-label="源榜第 ' + m.rank + ' 名">' + m.rank + '</span></td>'
        + '<td class="arena-col-model"><button type="button" class="arena-model-button" data-model-index="' + index + '" title="查看 ' + esc(m.model) + ' 的原始数据">' + modelLabel(m) + '<svg class="arena-detail-cue" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></button><div class="arena-model-meta"><span>' + esc(provider(m.family)) + '</span><span class="arena-license">' + esc(m.license || "") + '</span>' + (m.site_available || m.related_site_models?.length ? '<span class="arena-mobile-badge">' + badge(m) + '</span>' : '') + '</div></td>'
        + '<td class="arena-col-score arena-score-value">' + score(m.score) + '</td><td class="arena-col-interval arena-interval-value">' + interval + '</td><td class="arena-col-votes" title="' + count(m.votes) + '">' + sampleCount(m.votes) + '</td><td class="arena-col-site">' + badge(m) + '</td></tr>';
    }).join("");
    $("rankingEmpty").classList.toggle("hidden", rows.length > 0);
    $("rankingEmpty").querySelector("h3").textContent = snapshot.models.length ? "没有匹配的模型" : "暂无官方榜单缓存";
    $("rankingEmpty").querySelector("p").textContent = snapshot.models.length ? "尝试其他关键词、提供方，或取消接入范围筛选。" : "等待后台每日同步。没有真实数据时，不生成推断排名。";
    $("rankingResultCount").textContent = rows.length ? "显示 " + (start + 1) + "–" + Math.min(start + size, rows.length) + " / " + rows.length + " 个模型" + (rows.length !== snapshot.models.length ? " · 全榜 " + snapshot.models.length : "") : "0 个匹配模型";
    $("rankingPage").innerHTML = Array.from({length:pages}, (_,i) => '<option value="' + (i + 1) + '">' + (i + 1) + ' / ' + pages + '</option>').join("");
    $("rankingPage").value = String(currentPage); $("rankingPage").disabled = pages === 1;
    $("rankingPrev").disabled = currentPage <= 1; $("rankingNext").disabled = currentPage >= pages;
    const unranked = snapshot.unranked_site_models || [];
    $("rankingUnranked").classList.toggle("hidden", !unranked.length);
    $("rankingUnrankedCount").textContent = String(unranked.length);
    $("rankingUnrankedList").innerHTML = unranked.map(m => '<li><code>' + esc(m.model) + '</code><span>' + esc((m.versions || []).join(" / ")) + '</span><small>' + esc(m.note || '当前分榜未收录对应版本，不生成排名') + '</small></li>').join("");
    fitTableHeight();
  }

  function showBoard(data) {
    snapshot = data;
    const selectedFamily = $("rankingFamily").value;
    const families = [...new Set(data.models.map(m => m.family).filter(Boolean))].sort((a,b) => provider(a).localeCompare(provider(b), "en"));
    $("rankingFamily").innerHTML = '<option value="">全部提供方</option>' + families.map(f => '<option value="' + esc(f) + '">' + esc(provider(f)) + '</option>').join("");
    $("rankingFamily").value = families.includes(selectedFamily) ? selectedFamily : ""; render();
  }
  function resetPage() { currentPage = 1; $("rankingTableScroll").scrollTop = 0; render(); }
  function movePage(page) { currentPage = page; render(); $("rankingTableScroll").scrollTop = 0; }

  function openDetail(index) {
    const m = snapshot?.models[index]; if (!m) return; detailModel = m;
    $("rankingDetailTitle").textContent = m.model;
    $("rankingDetailBoard").textContent = snapshot.board_label + " · 源榜第 " + m.rank + " 名";
    const interval = m.rating_lower != null && m.rating_upper != null ? String(m.rating_lower) + " – " + String(m.rating_upper) : "—";
    const values = [["提供方",provider(m.family)],[snapshot.score_label + " · 原始值",String(m.score)],["源榜评分区间",interval],[snapshot.sample_label,count(m.votes)],["源榜发布日期",snapshot.published_at || "—"],["许可证（源数据）",m.license || "—"]];
    const siteModels = (m.site_models || []).map((name,i) => '<div class="arena-copy-row"><code>' + esc(name) + '</code><button type="button" data-copy-index="' + i + '">复制</button></div>').join("");
    $("rankingDetailContent").innerHTML = '<dl class="arena-detail-metrics">' + values.map(([label,value]) => '<div><dt>' + esc(label) + '</dt><dd>' + esc(value) + '</dd></div>').join("") + '</dl><h3>' + (m.site_available ? '本站已接入的模型标识' : '本站暂无匹配的接入标记') + '</h3>' + siteModels
      + (m.related_site_models?.length ? '<h3>本站另有不同日期版本（不是此行分数）</h3>' + m.related_site_models.map(name => '<div class="arena-copy-row"><code>' + esc(name) + '</code></div>').join("") : '')
      + (m.site_match_note ? '<p class="arena-detail-note">' + esc(m.site_match_note) + '</p>' : '')
      + '<p class="arena-detail-note">接入标记按型号匹配，可忽略已知思考强度；不代表测试配置、实时健康或个人密钥权限。不同日期版本不混同，排名和分数保持原榜口径。自动选模仍严格核对思考配置。</p>';
    $("rankingCopyStatus").textContent = ""; $("rankingDetail").showModal();
  }

  async function refresh() {
    const board = $("rankingBoard").value || "text", version = ++requestVersion;
    activeController?.abort(); const controller = new AbortController(); activeController = controller;
    const button = $("rankingRefresh"); button.disabled = true; button.querySelector("span").textContent = "读取中…";
    snapshot = snapshots.get(board) || null;
    boardTabs.forEach(tab => tab.setAttribute("aria-pressed", String(tab.dataset.board === board)));
    if (snapshot) showBoard(snapshot); else {
      $("rankingUnranked").classList.add("hidden"); $("rankingUnrankedList").innerHTML = ""; $("rankingUnrankedCount").textContent = "0";
      $("rankingList").innerHTML = '<tr><td colspan="6" class="arena-table-placeholder">正在读取所选榜单的本地缓存…</td></tr>';
      $("rankingEmpty").classList.add("hidden"); $("rankingModelCount").textContent = $("rankingSiteCount").textContent = "—";
      $("rankingPublication").textContent = "正在读取源数据日期…"; $("rankingBoardLabel").textContent = "读取榜单中…"; $("rankingResultCount").textContent = "正在读取…";
      $("rankingPrev").disabled = $("rankingNext").disabled = true;
    }
    $("rankingError").classList.add("hidden"); $("rankingList").setAttribute("aria-busy", "true");
    const timeout = setTimeout(() => controller.abort(), 15000);
    try {
      const response = await fetch("/api/public/rankings?board=" + encodeURIComponent(board), {credentials:"omit",cache:"no-store",signal:controller.signal});
      if (!response.ok) throw Error("request failed");
      const data = await response.json();
      if (data.board !== board || !Array.isArray(data.models) || data.models.some(m => !m || typeof m.model !== "string" || typeof m.family !== "string" || !Number.isInteger(m.rank) || m.rank < 1 || !Number.isFinite(m.score))) throw Error("invalid rankings");
      if (version !== requestVersion) return;
      snapshots.set(board,data); showBoard(data);
    } catch {
      if (version !== requestVersion) return;
      $("rankingError").textContent = snapshot ? "页面更新失败，保留已显示的缓存数据；外部数据仍按每日计划同步。" : "暂时无法读取本地榜单缓存，请点击“更新页面”重试。";
      $("rankingError").classList.remove("hidden");
      if (!snapshot) { $("rankingList").innerHTML = ""; $("rankingEmpty").classList.remove("hidden"); $("rankingEmpty").querySelector("h3").textContent = "暂时无法读取榜单"; $("rankingEmpty").querySelector("p").textContent = "请稍后重试，不会显示推断排名。"; $("rankingResultCount").textContent = "读取失败 · 可重试"; }
    } finally {
      clearTimeout(timeout);
      if (version === requestVersion) { activeController = null; button.disabled = false; button.querySelector("span").textContent = "更新页面"; $("rankingList").setAttribute("aria-busy", "false"); }
    }
  }

  $("rankingFilters").addEventListener("submit", event => event.preventDefault());
  $("rankingSearch").addEventListener("input", resetPage); $("rankingFamily").addEventListener("change", resetPage);
  $("rankingScopeAll").addEventListener("click", () => { scope = ""; resetPage(); }); $("rankingScopeSite").addEventListener("click", () => { scope = "site"; resetPage(); });
  $("rankingBoard").addEventListener("change", () => { currentPage = 1; $("rankingTableScroll").scrollTop = 0; return refresh(); });
  boardTabs.forEach(tab => tab.addEventListener("click", () => { $("rankingBoard").value = tab.dataset.board; currentPage = 1; $("rankingTableScroll").scrollTop = 0; return refresh(); }));
  $("rankingRefresh").addEventListener("click", refresh);
  $("rankingClear").addEventListener("click", () => { $("rankingSearch").value = $("rankingFamily").value = ""; scope = ""; resetPage(); });
  $("rankingPageSize").addEventListener("change", resetPage); $("rankingPrev").addEventListener("click", () => movePage(currentPage - 1)); $("rankingNext").addEventListener("click", () => movePage(currentPage + 1)); $("rankingPage").addEventListener("change", () => movePage(Number($("rankingPage").value) || 1));
  $("rankingList").addEventListener("click", event => { const target = event.target.closest("[data-model-index]"); if (target) openDetail(Number(target.dataset.modelIndex)); });
  $("rankingDetailClose").addEventListener("click", () => $("rankingDetail").close());
  $("rankingDetailContent").addEventListener("click", async event => {
    const target = event.target.closest("[data-copy-index]"); if (!target) return;
    const value = detailModel?.site_models?.[Number(target.dataset.copyIndex)]; if (!value) return;
    try { await copyToClipboard(value); $("rankingCopyStatus").textContent = "已复制模型标识"; } catch { $("rankingCopyStatus").textContent = "复制失败，请手动选择模型标识复制。"; }
  });
  window.addEventListener?.("resize", fitTableHeight);
  document.fonts?.ready.then(fitTableHeight);
  refresh();
})();

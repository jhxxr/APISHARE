/* Auto Router (智能路由) management console */
const ROUTER_TIERS = [
  { id: "SIMPLE", label: "SIMPLE · 快档（简单）", subtitle: "闲聊、翻译、改写、简单事实问答", defaultCriteria: "闲聊、翻译、改写、简单事实问答，快模型一次能答好" },
  { id: "MEDIUM", label: "MEDIUM · 快档（中等）", subtitle: "一般写作、总结、短代码片段", defaultCriteria: "一般写作、总结、短代码片段，快模型基本能答好" },
  { id: "COMPLEX", label: "COMPLEX · 强档（复杂）", subtitle: "多文件代码、调试、架构设计、长文档分析", defaultCriteria: "多文件代码、调试、架构设计、长文档分析，需要强模型" },
  { id: "REASONING", label: "REASONING · 推理档（深度）", subtitle: "数学证明、多步推理、复杂规划", defaultCriteria: "数学证明、多步推理、复杂规划，需要最强推理模型" },
];

const ROUTER_DRYRUN_SAMPLES = {
  simple: "把下面这句话翻译成英文：今天下午三点我们开会讨论智能路由上线方案。",
  medium: "用 Go 写一个带超时控制的 HTTP GET 请求工具函数，支持自定义 Header 并解析 JSON 响应。",
  complex: "请帮我重构一个跨多文件的 Go 微服务网关模块：支持上游连接池、会话粘性、指数退避冷却以及多协议（OpenAI Chat/Responses、Anthropic、Gemini）无损转换，给出完整的接口设计与并发安全注意事项。",
  reasoning: "证明：对于任意正整数 n，若 2^n - 1 是素数，则 n 必为素数；并进一步推导当 n=11 时 2^11 - 1 的质因数分解步骤。",
  tool: "__TOOL_TURN__",
};

let routerConfigCache = null;
let routerDraft = null;
let routerStatsCache = null;
let routerDirty = false;
let routerStatsPeriod = "24h";

function markRouterDirty(dirty = true) {
  routerDirty = dirty;
  const indicator = $("routerDirtyHint");
  if (indicator) {
    indicator.textContent = dirty ? "有未保存的修改" : "所有修改已保存";
    indicator.classList.toggle("is-dirty", dirty);
  }
}

function updateNavRouterBadge(cfg) {
  const badge = $("navRouterBadge");
  if (!badge || !cfg) return;
  const mode = (cfg.mode || (cfg.enabled ? "live" : "off")).toLowerCase();
  if (mode === "live") {
    badge.textContent = "LIVE";
    badge.className = "nav-count badge on";
  } else if (mode === "shadow") {
    badge.textContent = "SHADOW";
    badge.className = "nav-count badge warn";
  } else {
    badge.textContent = "OFF";
    badge.className = "nav-count";
  }
}

async function loadAutoRouterBadgeQuietly() {
  try {
    const cfg = await api("api/auto-router/config");
    routerConfigCache = cfg;
    updateNavRouterBadge(cfg);
  } catch { /* ignore background badge load errors */ }
}

function cloneRouterDraftFromConfig(cfg) {
  const tiers = {};
  ROUTER_TIERS.forEach(({ id }) => {
    tiers[id] = Array.isArray(cfg.tiers?.[id]) ? [...cfg.tiers[id]] : [];
  });
  const criteria = {};
  ROUTER_TIERS.forEach(({ id, defaultCriteria }) => {
    criteria[id] = cfg.tier_criteria?.[id] || defaultCriteria;
  });
  return {
    enabled: Boolean(cfg.enabled),
    mode: cfg.mode || (cfg.enabled ? "live" : "off"),
    virtual_model: cfg.virtual_model || "auto",
    classifier: cfg.classifier || "cloudflare",
    classifier_model: cfg.classifier_model || "@cf/cloudflare/clef-flash",
    cloudflare_account_id: cfg.cloudflare_account_id || "",
    endpoint_url: cfg.endpoint_url || "",
    api_key: "",
    clear_api_key: false,
    proxy_id: Number(cfg.proxy_id || 0),
    classifier_timeout_ms: Number(cfg.classifier_timeout_ms || 1500),
    classifier_fallback: cfg.classifier_fallback || "heuristic",
    classifier_instructions: cfg.classifier_instructions || "",
    min_confidence: Number(cfg.min_confidence || 0.55),
    context_user_turns: Number(cfg.context_user_turns ?? 2),
    skip_short_chars: Number(cfg.skip_short_chars ?? 60),
    route_housekeeping: cfg.route_housekeeping !== false,
    session_mode: cfg.session_mode || "user_turn",
    cascade_tiers: cfg.cascade_tiers !== false,
    allow_unlisted_target_models: cfg.allow_unlisted_target_models !== false,
    default_model: cfg.default_model || "",
    tiers,
    tier_criteria: criteria,
    response_headers: cfg.response_headers !== false,
    escalation_keyword: cfg.escalation_keyword || "",
    allow_user_auto_router: cfg.allow_user_auto_router !== false,
    share_site_classifier_with_users: Boolean(cfg.share_site_classifier_with_users),
    shared_classifier_models: Array.isArray(cfg.shared_classifier_models) ? [...cfg.shared_classifier_models] : [],
  };
}

async function loadAutoRouter() {
  const version = requestVersions.router = (requestVersions.router || 0) + 1;
  try {
    const [cfg, stats, proxiesResp] = await Promise.all([
      api("api/auto-router/config"),
      api("api/auto-router/stats?period=" + encodeURIComponent(routerStatsPeriod)),
      api("api/proxies").catch(() => ({ items: [] })),
    ]);
    if (version !== requestVersions.router) return;
    routerConfigCache = cfg;
    routerStatsCache = stats;
    routerDraft = cloneRouterDraftFromConfig(cfg);
    updateNavRouterBadge(cfg);
    populateRouterProxyOptions(proxiesResp.items || []);
    populateRouterKeyOptions();
    renderAutoRouterConfig();
    renderAutoRouterStats();
    markRouterDirty(false);
    stampRefresh();
  } catch (error) {
    if (error.name === "AbortError" || version !== requestVersions.router) return;
    reportError(error, "加载智能路由配置失败");
  }
}

async function setRouterStatsPeriod(period) {
  routerStatsPeriod = period || "24h";
  document.querySelectorAll("[data-router-period]").forEach((btn) => {
    btn.classList.toggle("active", btn.dataset.routerPeriod === routerStatsPeriod);
    btn.setAttribute("aria-pressed", String(btn.dataset.routerPeriod === routerStatsPeriod));
  });
  try {
    const stats = await api("api/auto-router/stats?period=" + encodeURIComponent(routerStatsPeriod));
    routerStatsCache = stats;
    renderAutoRouterStats();
  } catch (error) {
    reportError(error, "刷新路由统计失败");
  }
}

function populateRouterProxyOptions(proxies) {
  const select = $("routerProxySelect");
  if (!select) return;
  const current = String(routerDraft?.proxy_id || 0);
  select.replaceChildren(new Option("直连（跟随系统默认网络）", "0"));
  (proxies || []).forEach((p) => {
    if (p.enabled || String(p.id) === current) {
      select.add(new Option(p.name + " (" + p.url + ")", String(p.id)));
    }
  });
  select.value = current;
}

function populateRouterKeyOptions() {
  const select = $("routerDryRunKey");
  if (!select) return;
  const current = select.value;
  select.replaceChildren(new Option("不绑定特定密钥（仅检查上游健康度）", "0"));
  (keysCache || []).forEach((k) => {
    select.add(new Option((k.name || "#" + k.id) + (k.models ? " · [" + k.models + "]" : " · [全部模型]"), String(k.id)));
  });
  if (current) select.value = current;
}

function renderAutoRouterStats() {
  const s = routerStatsCache;
  if (!s) return;
  const total = Number(s.total_calls ?? s.total_routed ?? 0);
  const shadowCalls = Number(s.shadow_calls || 0);
  const liveCalls = Math.max(0, total - shadowCalls);
  $("routerKpiTotal").textContent = number(total);
  $("routerKpiTotalSub").textContent = "生效 " + number(liveCalls) + " · 影子 " + number(shadowCalls) + " · 成功 " + number(s.success_calls || 0);

  const fastCalls = Number(s.fast_calls || 0);
  const fastPct = (total > 0 ? (fastCalls / total) * 100 : 0).toFixed(1);
  $("routerKpiFastRatio").textContent = fastPct + "%";
  const saved = Number(s.saved_usd ?? s.estimated_saved_usd ?? 0);
  $("routerKpiSavedSub").textContent = saved > 0
    ? "相比全走 " + (routerDraft?.default_model || "默认模型") + " 预计省 " + fmtUSD(saved)
    : "累计消费 " + fmtUSD(s.actual_cost_usd || 0);

  const p50 = Number(s.classifier_p50_ms || 0);
  const p95 = Number(s.classifier_p95_ms || 0);
  $("routerKpiLatency").textContent = p50 > 0 ? p50 + " ms" : "0 ms";
  const fbCalls = Number(s.fallback_calls || 0);
  const fbPct = (total > 0 ? (fbCalls / total) * 100 : 0).toFixed(1);
  const brk = routerConfigCache?.breaker?.open ? " · 熔断中" : " · 分类器正常";
  $("routerKpiLatencySub").textContent = "P95 " + p95 + " ms · 回退率 " + fbPct + "%" + brk;

  const skipped = Number(s.skipped_models ?? s.quota_skipped_calls ?? 0);
  $("routerKpiFailover").textContent = number(skipped);

  const counts = {};
  if (Array.isArray(s.tiers)) {
    s.tiers.forEach((item) => { if (item?.tier) counts[item.tier] = Number(item.calls || 0); });
  } else if (s.tier_counts) {
    Object.assign(counts, s.tier_counts);
  }
  const bar = $("routerTierBar");
  const legend = $("routerTierLegend");
  const tiersOrder = ["SIMPLE", "MEDIUM", "COMPLEX", "REASONING", "DEFAULT"];
  const colors = { SIMPLE: "#10b981", MEDIUM: "#0ea5e9", COMPLEX: "#f59e0b", REASONING: "#8b5cf6", DEFAULT: "#64748b" };
  if (total <= 0) {
    bar.innerHTML = '<span style="width:100%;background:#e5e3df"></span>';
  } else {
    bar.innerHTML = tiersOrder.map((t) => {
      const c = Number(counts[t] || 0);
      if (c <= 0) return "";
      const pct = Math.max(2, (c / total) * 100).toFixed(1);
      return '<span class="router-tier-seg-' + t + '" style="width:' + pct + '%" title="' + t + ': ' + c + ' 次"></span>';
    }).join("");
  }
  legend.innerHTML = tiersOrder.map((t) => {
    const c = Number(counts[t] || 0);
    return '<span><i style="background:' + colors[t] + '"></i>' + t + ' <strong>' + number(c) + '</strong></span>';
  }).join("");
}

function setRouterMode(mode) {
  if (!routerDraft) return;
  routerDraft.mode = mode;
  routerDraft.enabled = mode !== "off";
  document.querySelectorAll(".router-mode-btn").forEach((btn) => {
    const active = btn.dataset.mode === mode;
    btn.classList.toggle("active", active);
    btn.setAttribute("aria-pressed", String(active));
  });
  markRouterDirty(true);
}

function healthBadgeForRouterModel(model) {
  const healthMap = routerConfigCache?.model_health || {};
  const info = healthMap[model];
  if (!info || info.status === "missing") {
    return '<span class="router-health-pill missing" title="当前没有已启用的上游提供此模型">无可用上游</span>';
  }
  const totalCount = Number(info.total_count ?? info.total_upstreams ?? 0);
  const healthyCount = Number(info.healthy_count ?? info.healthy_upstreams ?? 0);
  if (info.status === "cooling") {
    return '<span class="router-health-pill cooling" title="关联上游当前处于错误冷却中">冷却中 (0/' + totalCount + ')</span>';
  }
  return '<span class="router-health-pill healthy" title="可用上游：' + esc((info.upstream_names || []).join(", ")) + '">健康 · ' + healthyCount + ' 上游</span>';
}

function priceHintForRouterModel(model) {
  const healthMap = routerConfigCache?.model_health || {};
  const info = healthMap[model];
  if (!info || (!info.price_in && !info.price_out)) return "";
  return '<span class="router-price-tag" title="输入 / 输出单价 (USD / 1M tokens)">$' + Number(info.price_in || 0).toFixed(2) + '/$' + Number(info.price_out || 0).toFixed(2) + '</span>';
}

function upstreamModelGroupsForRouter() {
  const vm = (routerDraft?.virtual_model || routerConfigCache?.virtual_model || "auto").toLowerCase();
  const rawGroups = Array.isArray(routerConfigCache?.upstream_models) ? routerConfigCache.upstream_models : [];
  if (rawGroups.length) {
    return rawGroups
      .map((g) => ({
        id: g.id,
        name: g.name || ("#" + g.id),
        type: g.type || "openai",
        enabled: g.enabled !== false,
        healthy: Boolean(g.healthy),
        models: (Array.isArray(g.models) ? g.models : []).filter((m) => m && m.toLowerCase() !== vm),
      }))
      .filter((g) => g.models.length > 0);
  }
  if (Array.isArray(upsCache) && upsCache.length) {
    return upsCache
      .map((u) => {
        const models = String(u.models || "")
          .split(/[,，\n\r]+/)
          .map((s) => s.trim())
          .filter((m) => m && m.toLowerCase() !== vm);
        return {
          id: u.id,
          name: u.name || ("#" + u.id),
          type: u.type || "openai",
          enabled: u.enabled !== false && Number(u.weight ?? 1) > 0,
          healthy: u.healthy !== false,
          models,
        };
      })
      .filter((g) => g.models.length > 0);
  }
  return [];
}

function availableGatewayModelOptions() {
  const vm = (routerDraft?.virtual_model || routerConfigCache?.virtual_model || "auto").toLowerCase();
  const set = new Set();
  (routerConfigCache?.available_models || []).forEach((m) => {
    if (m && m.toLowerCase() !== vm) set.add(m);
  });
  const groups = upstreamModelGroupsForRouter();
  groups.forEach((g) => {
    if (g.enabled) {
      g.models.forEach((m) => { if (m && m.toLowerCase() !== vm) set.add(m); });
    }
  });
  if (set.size === 0) {
    groups.forEach((g) => {
      g.models.forEach((m) => { if (m && m.toLowerCase() !== vm) set.add(m); });
    });
  }
  const healthMap = routerConfigCache?.model_health || {};
  Object.keys(healthMap).forEach((m) => {
    const info = healthMap[m];
    if (m && m.toLowerCase() !== vm && info && info.status !== "missing" && Number(info.total_count || 0) > 0) {
      set.add(m);
    }
  });
  return [...set].sort();
}

function buildUpstreamModelSelectOptionsHTML(excludeList = [], placeholderText = "+ 从上游已绑定模型选择…", selectedValue = "") {
  const excludeSet = new Set(excludeList || []);
  const groups = upstreamModelGroupsForRouter();
  const allModels = availableGatewayModelOptions();
  if (!groups.length && !allModels.length) {
    let html = '<option value="">（当前无已绑定模型的上游，请先在「上游服务」绑定模型）</option>';
    if (selectedValue) {
      html += '<option value="' + esc(selectedValue) + '" selected>' + esc(selectedValue) + ' (未绑定上游)</option>';
    }
    return html;
  }

  let optGroupsHtml = "";
  const coveredInGroups = new Set();
  let totalAvailable = 0;

  groups.forEach((g) => {
    const models = g.models.filter((m) => !excludeSet.has(m));
    g.models.forEach((m) => coveredInGroups.add(m));
    if (!models.length) return;
    totalAvailable += models.length;
    const stateLabel = !g.enabled ? " · 已停用" : (g.healthy ? " · 健康" : " · 冷却中");
    optGroupsHtml += '<optgroup label="' + esc("上游：" + g.name + " (" + g.type + stateLabel + ")") + '">'
      + models.map((m) => '<option value="' + esc(m) + '"' + (selectedValue === m ? " selected" : "") + '>' + esc(m) + ' · [' + esc(g.name) + ']</option>').join("")
      + '</optgroup>';
  });

  const uncovered = allModels.filter((m) => !coveredInGroups.has(m) && !excludeSet.has(m));
  if (uncovered.length) {
    totalAvailable += uncovered.length;
    optGroupsHtml += '<optgroup label="上游已发现模型">'
      + uncovered.map((m) => '<option value="' + esc(m) + '"' + (selectedValue === m ? " selected" : "") + '>' + esc(m) + '</option>').join("")
      + '</optgroup>';
  }

  if (selectedValue && !allModels.includes(selectedValue) && !coveredInGroups.has(selectedValue)) {
    optGroupsHtml = '<optgroup label="当前自定义模型"><option value="' + esc(selectedValue) + '" selected>' + esc(selectedValue) + ' (自定义)</option></optgroup>' + optGroupsHtml;
  }

  if (!totalAvailable && !selectedValue) {
    return '<option value="">（所有上游已绑定模型均已添加）</option>';
  }
  return '<option value="">' + esc(placeholderText) + '</option>' + optGroupsHtml;
}

function populateRouterDefaultModelSelect() {
  const sel = $("routerDefaultModelSelect");
  if (!sel || !routerDraft) return;
  const current = (routerDraft.default_model || "").trim();
  sel.innerHTML = buildUpstreamModelSelectOptionsHTML([], "从上游已绑定模型选择兜底模型…", current);
  sel.value = current;
}

function syncRouterDefaultModelSelect() {
  const sel = $("routerDefaultModelSelect");
  if (!sel || !routerDraft) return;
  const current = (routerDraft.default_model || "").trim();
  const hasOption = Array.from(sel.options).some((opt) => opt.value === current);
  if (hasOption) {
    sel.value = current;
  } else {
    populateRouterDefaultModelSelect();
  }
}

function onSelectRouterDefaultModel(val) {
  if (!routerDraft) return;
  routerDraft.default_model = (val || "").trim();
  if ($("routerDefaultModel")) $("routerDefaultModel").value = routerDraft.default_model;
  renderRouterTiersEditor();
  markRouterDirty(true);
}

function onSelectRouterGatewayClassifierModel(val) {
  if (!routerDraft || !val) return;
  routerDraft.classifier_model = val.trim();
  if ($("routerClassifierModel")) $("routerClassifierModel").value = routerDraft.classifier_model;
  markRouterDirty(true);
}

function syncRouterGatewayClassifierSelect() {
  const sel = $("routerClassifierUpstreamModelSelect");
  if (!sel || !routerDraft) return;
  const current = (routerDraft.classifier_model || "").trim();
  const hasOption = Array.from(sel.options).some((opt) => opt.value === current);
  if (hasOption) {
    sel.value = current;
  } else {
    sel.innerHTML = buildUpstreamModelSelectOptionsHTML([], "从上游已绑定模型中选择决策模型…", current);
    sel.value = current;
  }
}

function renderRouterSharedClassifierControls() {
  if (!routerDraft) return;
  const list = Array.isArray(routerDraft.shared_classifier_models) ? routerDraft.shared_classifier_models : [];
  const sel = $("routerSharedClassifierModelSelect");
  if (sel) {
    sel.innerHTML = buildUpstreamModelSelectOptionsHTML(list, "+ 从上游已绑定模型选择添加到共享白名单…");
  }
  const chips = $("routerSharedClassifierChips");
  if (chips) {
    chips.innerHTML = list.map((m) =>
      '<span class="router-preset-chip" style="display:inline-flex;align-items:center;gap:6px">'
      + esc(m)
      + '<button type="button" style="border:0;background:transparent;cursor:pointer;padding:0 2px;font-weight:700;color:inherit" title="移除 ' + esc(m) + '" onclick="removeRouterSharedClassifierModel(\'' + esc(m) + '\')">×</button>'
      + '</span>'
    ).join("");
  }
}

function addRouterSharedClassifierModel(model) {
  if (!routerDraft || !model) return;
  const m = model.trim();
  if (!m) return;
  if (!Array.isArray(routerDraft.shared_classifier_models)) routerDraft.shared_classifier_models = [];
  if (!routerDraft.shared_classifier_models.includes(m)) {
    routerDraft.shared_classifier_models.push(m);
    if ($("routerSharedClassifierModels")) {
      $("routerSharedClassifierModels").value = routerDraft.shared_classifier_models.join(", ");
    }
    renderRouterSharedClassifierControls();
    markRouterDirty(true);
  }
}

function removeRouterSharedClassifierModel(model) {
  if (!routerDraft || !Array.isArray(routerDraft.shared_classifier_models)) return;
  routerDraft.shared_classifier_models = routerDraft.shared_classifier_models.filter((x) => x !== model);
  if ($("routerSharedClassifierModels")) {
    $("routerSharedClassifierModels").value = routerDraft.shared_classifier_models.join(", ");
  }
  renderRouterSharedClassifierControls();
  markRouterDirty(true);
}

function syncRouterSharedClassifierModelsFromInput() {
  if (!routerDraft || !$("routerSharedClassifierModels")) return;
  routerDraft.shared_classifier_models = $("routerSharedClassifierModels").value
    .split(/[,，\n\r]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  renderRouterSharedClassifierControls();
}

function autoFillRouterTiersFromUpstreams() {
  if (!routerDraft) return;
  const pool = availableGatewayModelOptions();
  if (!pool.length) {
    toast("当前还没有在上游服务中绑定任何模型，请先在「上游服务」中配置模型", "error");
    return;
  }
  const fast = pool.filter((m) => /flash|mini|haiku|lite|turbo|instant|nano/i.test(m));
  const reason = pool.filter((m) => /reasoner|r1|o1|o3|o4|thinking|grok-4|opus|qwq/i.test(m));
  const strong = pool.filter((m) => !fast.includes(m));

  routerDraft.tiers.SIMPLE = (fast.length ? fast : pool).slice(0, 3);
  routerDraft.tiers.MEDIUM = (fast.length ? [...fast, ...strong] : pool).slice(0, 3);
  routerDraft.tiers.COMPLEX = (strong.length ? strong : pool).slice(0, 3);
  routerDraft.tiers.REASONING = (reason.length ? reason : (strong.length ? strong : pool)).slice(0, 2);
  if (!routerDraft.default_model || !pool.includes(routerDraft.default_model)) {
    routerDraft.default_model = routerDraft.tiers.COMPLEX[0] || pool[0] || "";
    if ($("routerDefaultModel")) $("routerDefaultModel").value = routerDraft.default_model;
  }
  renderRouterTiersEditor();
  markRouterDirty(true);
  toast("已根据上游绑定的模型特征智能填充四档候选与默认模型，请点击「保存设置」生效");
}

function renderRouterTiersEditor() {
  const container = $("routerTiersContainer");
  if (!container || !routerDraft) return;
  const allModels = availableGatewayModelOptions();
  const datalist = $("routerAvailableModelsList");
  if (datalist) {
    datalist.innerHTML = allModels.map((m) => '<option value="' + esc(m) + '"></option>').join("");
  }
  populateRouterDefaultModelSelect();
  renderRouterSharedClassifierControls();

  const noUpstreamBanner = !allModels.length
    ? '<div class="router-candidate-empty" style="margin-bottom:8px;border-style:dashed">'
      + '当前尚未检测到「上游服务」绑定的模型。请先前往左侧 <strong>上游服务</strong> 为渠道绑定可用模型，即可在下方下拉框中直接选择。'
      + '</div>'
    : '';

  container.innerHTML = noUpstreamBanner + ROUTER_TIERS.map(({ id, label, subtitle }) => {
    const list = routerDraft.tiers[id] || [];
    const criteria = routerDraft.tier_criteria[id] || "";
    const candidatesHtml = list.length
      ? list.map((model, idx) => {
          return '<div class="router-candidate-item">'
            + '<div class="router-candidate-main">'
            + '<span class="router-priority-rank" title="优先级 #' + (idx + 1) + '">#' + (idx + 1) + '</span>'
            + modelIcon(model)
            + '<span class="router-candidate-name" title="' + esc(model) + '">' + esc(model) + '</span>'
            + '</div>'
            + '<div class="router-candidate-meta">'
            + priceHintForRouterModel(model)
            + healthBadgeForRouterModel(model)
            + '<div class="router-candidate-actions">'
            + '<button type="button" class="router-order-btn" title="提高优先级（上移）" aria-label="上移 ' + esc(model) + '" ' + (idx === 0 ? "disabled" : "") + ' onclick="moveRouterCandidate(\'' + id + '\',' + idx + ',-1)">↑</button>'
            + '<button type="button" class="router-order-btn" title="降低优先级（下移）" aria-label="下移 ' + esc(model) + '" ' + (idx === list.length - 1 ? "disabled" : "") + ' onclick="moveRouterCandidate(\'' + id + '\',' + idx + ',1)">↓</button>'
            + '<button type="button" class="router-order-btn remove" title="从本档移除" aria-label="移除 ' + esc(model) + '" onclick="removeRouterCandidate(\'' + id + '\',' + idx + ')">×</button>'
            + '</div></div></div>';
        }).join("")
      : '<div class="router-candidate-empty">当前档位暂无候选模型，可从下方下拉框选择上游已绑定的模型添加（为空时自动顺延到更高档位或兜底默认模型）</div>';

    const quickSelectHtml = '<select class="router-tier-select" aria-label="从上游已绑定模型添加到 ' + id + '" onchange="if(this.value){addRouterCandidate(\'' + id + '\',this.value);this.value=\'\';}">'
      + buildUpstreamModelSelectOptionsHTML(list, "+ 从上游已绑定模型选择加入 " + id + "…")
      + '</select>';

    return '<div class="router-tier-card" data-tier="' + id + '">'
      + '<div class="router-tier-header">'
      + '<div class="router-tier-title"><span class="router-tier-badge tier-' + id + '">' + esc(label) + '</span><small class="muted">' + esc(subtitle) + '</small></div>'
      + '<span class="count-pill">' + list.length + ' 个候选</span>'
      + '</div>'
      + '<div class="router-candidate-list">' + candidatesHtml + '</div>'
      + '<div class="router-tier-add-bar">'
      + quickSelectHtml
      + '<input id="routerAddInput_' + id + '" class="router-tier-input" type="text" list="routerAvailableModelsList" placeholder="或手动输入模型 ID 回车添加…" onkeydown="if(event.key===\'Enter\'){event.preventDefault();addRouterCandidateFromInput(\'' + id + '\');}">'
      + '<button type="button" class="btn mini ghost router-tier-add-btn" onclick="addRouterCandidateFromInput(\'' + id + '\')">添加候选</button>'
      + '</div>'
      + '<details class="router-tier-criteria">'
      + '<summary>自定义本档给分类器的判定标准说明</summary>'
      + '<textarea rows="2" maxlength="500" oninput="updateRouterTierCriteria(\'' + id + '\', this.value)">' + esc(criteria) + '</textarea>'
      + '<div style="margin-top:4px;text-align:right"><button type="button" class="text-btn" onclick="resetRouterTierCriteria(\'' + id + '\')">恢复本档默认说明</button></div>'
      + '</details>'
      + '</div>';
  }).join("");

  const defHealth = $("routerDefaultModelHealth");
  if (defHealth) {
    defHealth.innerHTML = routerDraft.default_model
      ? healthBadgeForRouterModel(routerDraft.default_model)
      : '<span class="router-health-pill missing" title="请从上游已绑定模型中选择兜底默认模型">未选择</span>';
  }
}

function moveRouterCandidate(tier, idx, delta) {
  if (!routerDraft || !Array.isArray(routerDraft.tiers[tier])) return;
  const list = routerDraft.tiers[tier];
  const target = idx + delta;
  if (target < 0 || target >= list.length) return;
  const tmp = list[idx];
  list[idx] = list[target];
  list[target] = tmp;
  renderRouterTiersEditor();
  markRouterDirty(true);
}

function removeRouterCandidate(tier, idx) {
  if (!routerDraft || !Array.isArray(routerDraft.tiers[tier])) return;
  routerDraft.tiers[tier].splice(idx, 1);
  renderRouterTiersEditor();
  markRouterDirty(true);
}

function addRouterCandidate(tier, rawModel) {
  if (!routerDraft) return;
  const parts = String(rawModel || "").split(/[,，\n\r]+/).map((s) => s.trim()).filter(Boolean);
  if (!parts.length) return;
  if (!Array.isArray(routerDraft.tiers[tier])) routerDraft.tiers[tier] = [];
  const list = routerDraft.tiers[tier];
  let added = 0;
  parts.forEach((m) => {
    if (m === routerDraft.virtual_model) {
      toast("候选模型不能是虚拟模型自身 (" + m + ")", "error");
      return;
    }
    if (!list.includes(m)) {
      list.push(m);
      added++;
    }
  });
  if (added > 0) {
    renderRouterTiersEditor();
    markRouterDirty(true);
  }
}

function addRouterCandidateFromInput(tier) {
  const input = $("routerAddInput_" + tier);
  if (!input) return;
  const val = input.value.trim();
  if (!val) return;
  addRouterCandidate(tier, val);
  input.value = "";
}

function updateRouterTierCriteria(tier, value) {
  if (!routerDraft) return;
  routerDraft.tier_criteria[tier] = value;
  markRouterDirty(true);
}

function resetRouterTierCriteria(tier) {
  if (!routerDraft) return;
  const meta = ROUTER_TIERS.find((t) => t.id === tier);
  if (!meta) return;
  routerDraft.tier_criteria[tier] = meta.defaultCriteria;
  renderRouterTiersEditor();
  markRouterDirty(true);
}

function selectRouterClassifier(provider) {
  if (!routerDraft) return;
  routerDraft.classifier = provider;
  if (provider === "cloudflare" && (!routerDraft.classifier_model || !routerDraft.classifier_model.startsWith("@cf/"))) {
    routerDraft.classifier_model = "@cf/cloudflare/clef-flash";
  } else if (provider === "jev") {
    if (!routerDraft.classifier_model || routerDraft.classifier_model.startsWith("@cf/")) routerDraft.classifier_model = "jev";
    if (!routerDraft.endpoint_url) routerDraft.endpoint_url = "https://api.typesafe.ai/v1/systemone";
  } else if (provider === "systemone") {
    if (!routerDraft.classifier_model || routerDraft.classifier_model.startsWith("@cf/")) routerDraft.classifier_model = "Julia-1";
    if (!routerDraft.endpoint_url) routerDraft.endpoint_url = "http://127.0.0.1:8000/v1/systemone";
  } else if (provider === "openai_chat") {
    if (!routerDraft.classifier_model || routerDraft.classifier_model.startsWith("@cf/")) routerDraft.classifier_model = "decision-model-preview";
    if (!routerDraft.endpoint_url) routerDraft.endpoint_url = "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions";
  } else if (provider === "gateway_upstream") {
    const gwModels = availableGatewayModelOptions();
    if (!routerDraft.classifier_model || routerDraft.classifier_model.startsWith("@cf/") || (gwModels.length > 0 && !gwModels.includes(routerDraft.classifier_model))) {
      routerDraft.classifier_model = gwModels[0] || "";
    }
  }
  renderRouterClassifierForm();
  markRouterDirty(true);
}

function applyRouterClassifierPreset(provider, model, endpoint = "") {
  if (!routerDraft) return;
  routerDraft.classifier = provider;
  routerDraft.classifier_model = model;
  if (endpoint) routerDraft.endpoint_url = endpoint;
  renderRouterClassifierForm();
  markRouterDirty(true);
}

function renderRouterClassifierForm() {
  if (!routerDraft) return;
  const p = routerDraft.classifier || "cloudflare";
  document.querySelectorAll(".router-provider-pill").forEach((btn) => {
    const active = btn.dataset.provider === p;
    btn.classList.toggle("active", active);
    btn.setAttribute("aria-pressed", String(active));
  });

  const isHeuristic = p === "heuristic";
  const isCloudflare = p === "cloudflare";
  const isGatewayUpstream = p === "gateway_upstream";
  $("routerClassifierRemoteFields").classList.toggle("hidden", isHeuristic);
  $("routerCloudflareAccountField").classList.toggle("hidden", !isCloudflare);
  $("routerEndpointField").classList.toggle("hidden", isHeuristic || isGatewayUpstream);
  if ($("routerExternalCredFields")) {
    $("routerExternalCredFields").classList.toggle("hidden", isHeuristic || isGatewayUpstream);
  }
  const gwClassifierSel = $("routerClassifierUpstreamModelSelect");
  if (gwClassifierSel) {
    gwClassifierSel.classList.toggle("hidden", !isGatewayUpstream);
    if (isGatewayUpstream) {
      const currentClf = (routerDraft.classifier_model || "").trim();
      gwClassifierSel.innerHTML = buildUpstreamModelSelectOptionsHTML([], "从上游已绑定模型中选择决策模型…", currentClf);
      gwClassifierSel.value = currentClf;
    }
  }

  const clfInput = $("routerClassifierModel");
  if (clfInput) {
    clfInput.value = routerDraft.classifier_model || "";
    const placeholders = {
      cloudflare: "@cf/cloudflare/clef-flash",
      jev: "jev",
      systemone: "Julia-1",
      openai_chat: "decision-model-preview",
      gateway_upstream: "从左侧下拉框选择上游已绑定的模型，或输入模型 ID",
    };
    clfInput.placeholder = placeholders[p] || "@cf/cloudflare/clef-flash";
  }
  $("routerCloudflareAccount").value = routerDraft.cloudflare_account_id || "";
  $("routerEndpointURL").value = routerDraft.endpoint_url || "";
  $("routerProxySelect").value = String(routerDraft.proxy_id || 0);

  const envCfAcct = routerConfigCache?.env_cloudflare_account_id;
  const envCfTok = routerConfigCache?.env_cloudflare_token_present;
  const envJev = routerConfigCache?.env_jev_key_present;
  const keyPreview = routerConfigCache?.api_key_saved ? (routerConfigCache?.api_key_masked || "已保存") : "";
  const keyHint = $("routerApiKeyHint");
  if (keyHint) {
    if (keyPreview) {
      keyHint.textContent = "已保存后台密钥 (" + keyPreview + ")；留空则保留原密钥。";
    } else if (isCloudflare && envCfTok) {
      keyHint.textContent = "已检测到环境变量 CLOUDFLARE_API_TOKEN，留空将自动使用环境变量。";
    } else if (p === "jev" && envJev) {
      keyHint.textContent = "已检测到环境变量 TYPESAFE_API_KEY / JEV_API_KEY，留空将自动使用环境变量。";
    } else {
      keyHint.textContent = "密钥仅加密/掩码保存在服务端，不在接口明文返回。";
    }
  }
  const cfAcctHint = $("routerCfAccountHint");
  if (cfAcctHint) {
    cfAcctHint.textContent = envCfAcct
      ? "留空将自动使用环境变量 CLOUDFLARE_ACCOUNT_ID (" + envCfAcct + ")"
      : "填写 Cloudflare Account ID，或设置环境变量 CLOUDFLARE_ACCOUNT_ID";
  }

  const presetsBox = $("routerModelPresets");
  if (presetsBox) {
    if (isCloudflare) {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'cloudflare\',\'@cf/cloudflare/clef-flash\')">@cf/cloudflare/clef-flash (推荐·快)</button>'
        + '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'cloudflare\',\'@cf/cloudflare/clef\')">@cf/cloudflare/clef (高精度)</button>';
    } else if (p === "jev") {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'jev\',\'jev\',\'https://api.typesafe.ai/v1/systemone\')">TypeSafe Jev 官方 (/v1/systemone)</button>';
    } else if (p === "systemone") {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'systemone\',\'Julia-1\',\'http://127.0.0.1:8000/v1/systemone\')">本地 Julia-1 / vLLM (/v1/systemone)</button>';
    } else if (p === "openai_chat") {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'openai_chat\',\'decision-model-preview\',\'https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions\')">阿里云百炼 decision-model-preview</button>';
    } else if (p === "gateway_upstream") {
      const gwModels = availableGatewayModelOptions().slice(0, 8);
      const healthMap = routerConfigCache?.model_health || {};
      presetsBox.innerHTML = gwModels.length
        ? gwModels.map((m) => {
            const upNames = (healthMap[m]?.upstream_names || []).join(", ");
            const suffix = upNames ? " (" + upNames + ")" : " (网关上游)";
            return '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'gateway_upstream\',\'' + esc(m) + '\')">' + esc(m + suffix) + '</button>';
          }).join("")
        : '<small class="muted">当前暂无已绑定的上游模型，请先在「上游服务」中为渠道绑定模型。</small>';
    } else {
      presetsBox.innerHTML = "";
    }
  }
}

function renderAutoRouterConfig() {
  if (!routerDraft) return;
  setRouterMode(routerDraft.mode);
  $("routerVirtualModel").value = routerDraft.virtual_model || "auto";
  $("routerDefaultModel").value = routerDraft.default_model || "";
  $("routerCascadeTiers").checked = routerDraft.cascade_tiers !== false;
  $("routerAllowUnlisted").checked = routerDraft.allow_unlisted_target_models !== false;
  $("routerResponseHeaders").checked = routerDraft.response_headers !== false;

  $("routerTimeoutMS").value = routerDraft.classifier_timeout_ms || 1500;
  $("routerFallback").value = routerDraft.classifier_fallback || "heuristic";
  $("routerMinConfidence").value = routerDraft.min_confidence || 0.55;
  $("routerContextTurns").value = routerDraft.context_user_turns ?? 2;
  $("routerSkipShortChars").value = routerDraft.skip_short_chars ?? 60;
  $("routerSessionMode").value = routerDraft.session_mode || "user_turn";
  $("routerHousekeeping").checked = routerDraft.route_housekeeping !== false;
  $("routerEscalationKeyword").value = routerDraft.escalation_keyword || "";
  if ($("routerAllowUserRouter")) $("routerAllowUserRouter").checked = routerDraft.allow_user_auto_router !== false;
  if ($("routerShareSiteClassifier")) $("routerShareSiteClassifier").checked = Boolean(routerDraft.share_site_classifier_with_users);
  if ($("routerSharedClassifierModels")) $("routerSharedClassifierModels").value = (routerDraft.shared_classifier_models || []).join(", ");
  $("routerAPIKey").value = "";
  $("routerClearAPIKey").checked = false;

  renderRouterTiersEditor();
  renderRouterClassifierForm();
  markRouterDirty(false);
}

function collectRouterDraftFromInputs() {
  if (!routerDraft) return null;
  routerDraft.virtual_model = $("routerVirtualModel").value.trim() || "auto";
  routerDraft.default_model = $("routerDefaultModel").value.trim();
  routerDraft.cascade_tiers = $("routerCascadeTiers").checked;
  routerDraft.allow_unlisted_target_models = $("routerAllowUnlisted").checked;
  routerDraft.response_headers = $("routerResponseHeaders").checked;

  routerDraft.classifier_model = $("routerClassifierModel").value.trim();
  routerDraft.cloudflare_account_id = $("routerCloudflareAccount").value.trim();
  routerDraft.endpoint_url = $("routerEndpointURL").value.trim();
  routerDraft.proxy_id = Number($("routerProxySelect").value || 0);
  routerDraft.api_key = $("routerAPIKey").value.trim();
  routerDraft.clear_api_key = $("routerClearAPIKey").checked;

  routerDraft.classifier_timeout_ms = Number($("routerTimeoutMS").value || 1500);
  routerDraft.classifier_fallback = $("routerFallback").value || "heuristic";
  routerDraft.min_confidence = Number($("routerMinConfidence").value || 0.55);
  routerDraft.context_user_turns = Number($("routerContextTurns").value || 2);
  routerDraft.skip_short_chars = Number($("routerSkipShortChars").value || 0);
  routerDraft.session_mode = $("routerSessionMode").value || "user_turn";
  routerDraft.route_housekeeping = $("routerHousekeeping").checked;
  routerDraft.escalation_keyword = $("routerEscalationKeyword").value.trim();
  if ($("routerAllowUserRouter")) routerDraft.allow_user_auto_router = $("routerAllowUserRouter").checked;
  if ($("routerShareSiteClassifier")) routerDraft.share_site_classifier_with_users = $("routerShareSiteClassifier").checked;
  if ($("routerSharedClassifierModels")) {
    routerDraft.shared_classifier_models = $("routerSharedClassifierModels").value
      .split(/[,，\n\r]+/)
      .map((s) => s.trim())
      .filter(Boolean);
  }
  return routerDraft;
}

async function saveAutoRouter(button) {
  const payload = collectRouterDraftFromInputs();
  if (!payload) return;
  const saveBtn = button || $("routerSaveBtn");
  setBusy(saveBtn, true, "正在保存…");
  try {
    const saved = await api("api/auto-router/config", {
      method: "PUT",
      body: JSON.stringify(payload),
    });
    routerConfigCache = saved;
    routerDraft = cloneRouterDraftFromConfig(saved);
    updateNavRouterBadge(saved);
    renderAutoRouterConfig();
    toast("智能路由配置已保存并生效");
  } catch (error) {
    reportError(error, "保存智能路由失败");
  } finally {
    setBusy(saveBtn, false);
  }
}

async function testAutoRouterClassifier(button) {
  const draft = collectRouterDraftFromInputs();
  if (!draft) return;
  const out = $("routerClassifierTestResult");
  out.classList.remove("hidden");
  out.textContent = "正在连接分类器并发送探测请求…";
  setBusy(button, true, "测试中…");
  try {
    const samplePrompt = $("routerDryRunPrompt")?.value.trim() || "请帮我用 Go 实现一个带并发安全缓存和指数退避重试的智能路由模块。";
    const res = await api("api/auto-router/test-classifier", {
      method: "POST",
      body: JSON.stringify({ ...draft, prompt: samplePrompt }),
    });
    const probs = res.probabilities || {};
    const probText = ["SIMPLE", "MEDIUM", "COMPLEX", "REASONING"]
      .filter((k) => probs[k] !== undefined)
      .map((k) => k + ": " + (Number(probs[k]) * 100).toFixed(1) + "%")
      .join(" · ");
    out.innerHTML = '<strong>连接成功</strong> · 分类器 <code>' + esc(res.provider) + ' / ' + esc(res.model) + '</code> · 延迟 <strong>' + Number(res.latency_ms || 0) + ' ms</strong> · 判定档位 <span class="router-tier-badge tier-' + esc(res.tier) + '">' + esc(res.tier) + '</span>'
      + (probText ? '<br><small class="muted">各档概率：' + esc(probText) + '</small>' : '');
  } catch (error) {
    out.innerHTML = '<span style="color:#dc2626"><strong>连接失败：</strong>' + esc(error.message) + '</span>';
  } finally {
    setBusy(button, false);
  }
}

function fillRouterDryRunSample(kind) {
  const box = $("routerDryRunPrompt");
  if (!box) return;
  if (kind === "tool") {
    box.value = "[模拟工具回传 Tool Result Turn] stdout: PASS ok apishare/cmd/apishare 0.42s";
    box.dataset.simulateTool = "true";
  } else {
    box.value = ROUTER_DRYRUN_SAMPLES[kind] || "";
    box.dataset.simulateTool = "false";
  }
  runAutoRouterDryRun();
}

async function runAutoRouterDryRun(button) {
  const draft = collectRouterDraftFromInputs();
  if (!draft) return;
  const box = $("routerDryRunPrompt");
  const promptText = box?.value.trim() || "";
  if (!promptText) {
    toast("请输入要试算的测试 Prompt", "error");
    return;
  }
  const isToolSim = box?.dataset.simulateTool === "true";
  const messages = isToolSim
    ? [
        { role: "user", content: "运行单元测试并检查结果" },
        { role: "assistant", content: "我将调用测试工具" },
        { role: "tool", content: promptText },
      ]
    : [{ role: "user", content: promptText }];
  const keyID = Number($("routerDryRunKey")?.value || 0);
  const btn = button || $("routerDryRunBtn");
  setBusy(btn, true, "正在试算…");
  try {
    const res = await api("api/auto-router/dry-run", {
      method: "POST",
      body: JSON.stringify({
        key_id: keyID,
        prompt: promptText,
        messages,
        config: draft,
      }),
    });
    renderRouterDryRunResult(res);
  } catch (error) {
    reportError(error, "试算分档失败");
  } finally {
    setBusy(btn, false);
  }
}

function renderRouterDryRunResult(res) {
  const container = $("routerDryRunOutput");
  if (!container) return;
  container.classList.remove("hidden");
  const dec = res.decision || {};
  const probs = dec.probabilities || {};
  const steps = dec.steps || [];

  const probHtml = ["SIMPLE", "MEDIUM", "COMPLEX", "REASONING"].map((t) => {
    const val = Number(probs[t] || 0);
    const pct = Math.min(100, Math.max(0, val * 100)).toFixed(1);
    return '<div class="router-prob-row">'
      + '<span class="router-tier-badge tier-' + t + '">' + t + '</span>'
      + '<div class="router-prob-track"><div class="router-prob-fill" style="width:' + pct + '%"></div></div>'
      + '<strong style="text-align:right;font-family:var(--mono,monospace)">' + pct + '%</strong>'
      + '</div>';
  }).join("");

  const stepsHtml = steps.map((st) => {
    const statusLabel = st.status === "selected"
      ? '<span class="badge on">已选中 #' + st.priority + '</span>'
      : st.status === "standby"
      ? '<span class="badge">顺位待命 #' + st.priority + '</span>'
      : '<span class="badge warn">已跳过 · ' + esc(st.reason || "不可用") + '</span>';
    return '<div class="router-step-row ' + esc(st.status) + '">'
      + '<div style="display:flex;align-items:center;gap:6px">'
      + '<span class="router-tier-badge tier-' + esc(st.source_tier) + '">' + esc(st.source_tier) + '</span>'
      + modelIcon(st.model)
      + '<code>' + esc(st.model) + '</code>'
      + (st.upstream_name ? '<small class="muted">→ ' + esc(st.upstream_name) + '</small>' : '')
      + '</div>'
      + statusLabel
      + '</div>';
  }).join("");

  container.innerHTML = '<div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:8px;padding-bottom:8px;border-bottom:1px solid var(--border,#e5e3df)">'
    + '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">'
    + '<span>判定档位：</span><span class="router-tier-badge tier-' + esc(dec.tier || "DEFAULT") + '">' + esc(dec.tier || "DEFAULT") + '</span>'
    + '<span>→ 选中模型：</span>' + modelLabel(dec.selected_model || "")
    + (dec.shadow && dec.shadow_model ? '<small class="muted">(Shadow 拟选: ' + esc(dec.shadow_model) + ')</small>' : '')
    + '</div>'
    + '<small class="muted">原因: <code>' + esc(dec.cause || "—") + '</code> · 分类耗时 ' + Number(dec.classifier_ms || 0) + ' ms'
    + (dec.heuristic_score !== undefined && dec.heuristic_score !== null ? ' · 启发式分 ' + Number(dec.heuristic_score).toFixed(2) : '')
    + '</small>'
    + '</div>'
    + (Object.keys(probs).length ? '<div class="router-prob-grid">' + probHtml + '</div>' : '')
    + '<div class="router-steps-list"><strong>候选优先级评估链（Failover 顺序）：</strong>' + stepsHtml + '</div>';
}

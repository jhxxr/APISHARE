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
  const defaultThinking = { SIMPLE: "none", MEDIUM: "low", COMPLEX: "medium", REASONING: "high" };
  const tierThinking = {};
  ROUTER_TIERS.forEach(({ id }) => {
    tierThinking[id] = cfg.tier_thinking?.[id] || defaultThinking[id] || "auto";
  });
  const criteria = {};
  ROUTER_TIERS.forEach(({ id, defaultCriteria }) => {
    criteria[id] = cfg.tier_criteria?.[id] || defaultCriteria;
  });
  return {
    enabled: Boolean(cfg.enabled),
    mode: cfg.mode || (cfg.enabled ? "live" : "off"),
    virtual_model: cfg.virtual_model || "auto",
    strategy_profile: cfg.strategy_profile || "balanced",
    arena_mode: cfg.arena_mode || "live",
    bandit_enabled: cfg.bandit_enabled !== false,
    speculative_cascade: cfg.speculative_cascade !== false,
    model_pool: [...new Set([...(cfg.model_pool || []), ...Object.values(tiers).flat()])].sort(),
    excluded_models: Array.isArray(cfg.excluded_models) ? [...cfg.excluded_models] : [],
    model_capabilities: cfg.model_capabilities ? { ...cfg.model_capabilities } : {},
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
    long_context_threshold: Number(cfg.long_context_threshold ?? 32000),
    route_housekeeping: cfg.route_housekeeping !== false,
    session_mode: cfg.session_mode || "user_turn",
    cascade_tiers: false,
    allow_unlisted_target_models: cfg.allow_unlisted_target_models !== false,
    default_model: cfg.default_model || "",
    tiers: {},
    tier_thinking: tierThinking,
    tier_criteria: criteria,
    model_context_limits: cfg.model_context_limits ? { ...cfg.model_context_limits } : {},
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

function parseRouterCandidateSpecJS(raw) {
  const s = String(raw || "").trim();
  const openIdx = s.lastIndexOf("(");
  if (openIdx > 0 && s.endsWith(")")) {
    const base = s.slice(0, openIdx).trim();
    const inside = s.slice(openIdx + 1, -1).trim();
    const parts = inside.split(",").map((p) => p.trim()).filter(Boolean);
    let thinking = "";
    const otherParts = [];
    parts.forEach((p) => {
      const low = p.toLowerCase();
      if (["none", "off", "disabled", "minimal", "low", "medium", "high", "xhigh", "max", "adaptive", "auto", "keep"].includes(low)) {
        thinking = low === "off" || low === "disabled" ? "none" : low;
      } else {
        otherParts.push(p);
      }
    });
    return { base, thinking, otherParts, raw: s };
  }
  return { base: s, thinking: "", otherParts: [], raw: s };
}

function formatContextWindowTokens(tokens) {
  const n = Number(tokens || 0);
  if (n <= 0) return "";
  if (n >= 1000000) return (n / 1000000).toFixed(n % 1000000 === 0 ? 0 : 1) + "M";
  if (n >= 1000) return Math.round(n / 1000) + "K";
  return String(n);
}

function healthBadgeForRouterModel(model) {
  const healthMap = routerConfigCache?.model_health || {};
  const spec = parseRouterCandidateSpecJS(model);
  const info = healthMap[model] || healthMap[spec.base];
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
  const spec = parseRouterCandidateSpecJS(model);
  const info = healthMap[model] || healthMap[spec.base];
  let html = "";
  const ctxLimit = Number(info?.context_limit || 0);
  if (ctxLimit > 0) {
    const srcLabels = {
      spec: "候选内联指定",
      config: "管理员手动覆盖",
      learned: "上游错误响应自动学习",
      probed: "上游 /v1/models 自动探测",
      catalog: "LiteLLM 官方目录同步",
      builtin: "内置模型族识别",
    };
    const srcText = srcLabels[info?.context_source] || "自动识别";
    const maxOut = Number(info?.max_output || 0);
    const outHint = maxOut > 0 ? " · 最大输出 " + formatContextWindowTokens(maxOut) : "";
    html += '<span class="router-price-tag" title="最大上下文窗口：' + ctxLimit.toLocaleString() + ' tokens（来源：' + srcText + outHint + '）">' + formatContextWindowTokens(ctxLimit) + ' ctx</span>';
  }
  if (info && (info.price_in || info.price_out)) {
    html += '<span class="router-price-tag" title="输入 / 输出单价 (USD / 1M tokens)">$' + Number(info.price_in || 0).toFixed(2) + '/$' + Number(info.price_out || 0).toFixed(2) + '</span>';
  }
  return html;
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
  const add = (m) => {
    const trimmed = String(m || "").trim();
    if (trimmed && trimmed.toLowerCase() !== vm) set.add(trimmed);
  };
  (routerConfigCache?.available_models || []).forEach(add);
  (modelOverviewCache?.models || []).forEach((item) => add(item?.model));
  if (typeof keyModelPickers !== "undefined" && Array.isArray(keyModelPickers?.key?.models)) {
    keyModelPickers.key.models.forEach(add);
  }
  const groups = upstreamModelGroupsForRouter();
  groups.forEach((g) => {
    if (g.enabled) {
      g.models.forEach(add);
    }
  });
  const healthMap = routerConfigCache?.model_health || {};
  Object.keys(healthMap).forEach((m) => {
    const info = healthMap[m];
    if (info && info.status !== "missing" && Number(info.total_count || 0) > 0) {
      add(m);
    }
  });
  return [...set].sort();
}

function buildUpstreamModelSelectOptionsHTML(excludeList = [], placeholderText = "+ 选择可用模型…", selectedValue = "") {
  const excludeSet = new Set(excludeList || []);
  const allModels = availableGatewayModelOptions();
  if (!allModels.length) {
    let html = '<option value="">（当前无可用模型，请先在「上游服务」配置模型）</option>';
    if (selectedValue) {
      html += '<option value="' + esc(selectedValue) + '" selected>' + esc(selectedValue) + '</option>';
    }
    return html;
  }

  const selectable = allModels.filter((m) => !excludeSet.has(m));
  let optionsHtml = selectable
    .map((m) => '<option value="' + esc(m) + '"' + (selectedValue === m ? " selected" : "") + '>' + esc(m) + '</option>')
    .join("");

  if (selectedValue && !selectable.includes(selectedValue)) {
    optionsHtml = '<option value="' + esc(selectedValue) + '" selected>' + esc(selectedValue) + '</option>' + optionsHtml;
  }

  if (!selectable.length && !selectedValue) {
    return '<option value="">（所有可用模型均已添加）</option>';
  }
  return '<option value="">' + esc(placeholderText) + '</option>' + optionsHtml;
}

function populateRouterDefaultModelSelect() {
  const sel = $("routerDefaultModelSelect");
  if (!sel || !routerDraft) return;
  const current = (routerDraft.default_model || "").trim();
  sel.innerHTML = buildUpstreamModelSelectOptionsHTML([], "选择可用模型作为兜底模型…", current);
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
  refreshRouterModelControls();
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
    sel.innerHTML = buildUpstreamModelSelectOptionsHTML([], "选择可用模型作为决策模型…", current);
    sel.value = current;
  }
}

function renderRouterSharedClassifierControls() {
  if (!routerDraft) return;
  const list = Array.isArray(routerDraft.shared_classifier_models) ? routerDraft.shared_classifier_models : [];
  const sel = $("routerSharedClassifierModelSelect");
  if (sel) {
    sel.innerHTML = buildUpstreamModelSelectOptionsHTML(list, "+ 选择可用模型添加到共享白名单…");
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

function refreshRouterModelControls() {
  if (!routerDraft) return;
  const allModels = availableGatewayModelOptions();
  const datalist = $("routerAvailableModelsList");
  if (datalist) datalist.innerHTML = allModels.map((m) => '<option value="' + esc(m) + '"></option>').join("");
  populateRouterDefaultModelSelect();
  renderRouterSharedClassifierControls();
  const defHealth = $("routerDefaultModelHealth");
  if (defHealth) defHealth.innerHTML = routerDraft.default_model
    ? healthBadgeForRouterModel(routerDraft.default_model) : '<span class="router-health-pill healthy">自动寻优</span>';
  renderRouterAutonomousPoolEditor();
}

// Commas in explicit variants such as model(high,128k) are not delimiters.
function splitRouterPoolInput(raw) {
  const models = []; let current = "", depth = 0;
  for (const ch of String(raw || "")) {
    if (ch === "(") depth++;
    else if (ch === ")" && depth > 0) depth--;
    if (depth === 0 && /[,，\r\n]/.test(ch)) {
      if (current.trim()) models.push(current.trim());
      current = "";
    } else current += ch;
  }
  if (current.trim()) models.push(current.trim());
  return [...new Set(models)].sort();
}

function updateRouterModelPool(value) {
  if (!routerDraft) return;
  routerDraft.model_pool = splitRouterPoolInput(value);
  renderRouterAutonomousPoolEditor();
  markRouterDirty(true);
}

function selectRouterClassifier(provider) {
  if (!routerDraft) return;
  routerDraft.classifier = provider;
  if (provider === "cloudflare" && (!routerDraft.classifier_model || !routerDraft.classifier_model.startsWith("@cf/"))) {
    routerDraft.classifier_model = "@cf/cloudflare/clef-flash";
  } else if (provider === "openrouter") {
    if (!routerDraft.classifier_model || !routerDraft.classifier_model.startsWith("parallel/")) {
      routerDraft.classifier_model = "parallel/decisions-fast:free";
    }
    if (!routerDraft.endpoint_url || !routerDraft.endpoint_url.includes("openrouter.ai")) {
      routerDraft.endpoint_url = "https://openrouter.ai/api/alpha/decisions";
    }
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
      gwClassifierSel.innerHTML = buildUpstreamModelSelectOptionsHTML([], "选择可用模型作为决策模型…", currentClf);
      gwClassifierSel.value = currentClf;
    }
  }

  const clfInput = $("routerClassifierModel");
  if (clfInput) {
    clfInput.value = routerDraft.classifier_model || "";
    const placeholders = {
      cloudflare: "@cf/cloudflare/clef-flash",
      openrouter: "parallel/decisions-fast:free",
      jev: "jev",
      systemone: "Julia-1",
      openai_chat: "decision-model-preview",
      gateway_upstream: "从左侧可用模型列表中选择，或输入模型 ID",
    };
    clfInput.placeholder = placeholders[p] || "@cf/cloudflare/clef-flash";
  }
  $("routerCloudflareAccount").value = routerDraft.cloudflare_account_id || "";
  $("routerEndpointURL").value = routerDraft.endpoint_url || "";
  $("routerProxySelect").value = String(routerDraft.proxy_id || 0);

  const envCfAcct = routerConfigCache?.env_cloudflare_account_id;
  const envCfTok = routerConfigCache?.env_cloudflare_token_present;
  const envJev = routerConfigCache?.env_jev_key_present;
  const envOpenRouter = routerConfigCache?.env_openrouter_key_present;
  const keyPreview = routerConfigCache?.api_key_saved ? (routerConfigCache?.api_key_masked || "已保存") : "";
  const keyHint = $("routerApiKeyHint");
  if (keyHint) {
    if (keyPreview) {
      keyHint.textContent = "已保存后台密钥 (" + keyPreview + ")；留空则保留原密钥。";
    } else if (isCloudflare && envCfTok) {
      keyHint.textContent = "已检测到环境变量 CLOUDFLARE_API_TOKEN，留空将自动使用环境变量。";
    } else if (p === "openrouter" && envOpenRouter) {
      keyHint.textContent = "已检测到环境变量 OPENROUTER_API_KEY，留空将自动使用环境变量。";
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
    } else if (p === "openrouter") {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'openrouter\',\'parallel/decisions-fast:free\',\'https://openrouter.ai/api/alpha/decisions\')">parallel/decisions-fast:free (OpenRouter 免费推荐)</button>'
        + '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'openrouter\',\'parallel/decisions-fast\',\'https://openrouter.ai/api/alpha/decisions\')">parallel/decisions-fast</button>';
    } else if (p === "jev") {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'jev\',\'jev\',\'https://api.typesafe.ai/v1/systemone\')">TypeSafe Jev 官方 (/v1/systemone)</button>';
    } else if (p === "systemone") {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'systemone\',\'Julia-1\',\'http://127.0.0.1:8000/v1/systemone\')">本地 Julia-1 / vLLM (/v1/systemone)</button>'
        + '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'systemone\',\'parallel/decisions-fast:free\',\'https://openrouter.ai/api/alpha/decisions\')">OpenRouter Decisions (/api/alpha/decisions)</button>';
    } else if (p === "openai_chat") {
      presetsBox.innerHTML = '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'openai_chat\',\'decision-model-preview\',\'https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions\')">阿里云百炼 decision-model-preview</button>';
    } else if (p === "gateway_upstream") {
      const gwModels = availableGatewayModelOptions().slice(0, 8);
      presetsBox.innerHTML = gwModels.length
        ? gwModels.map((m) =>
            '<button type="button" class="router-preset-chip" onclick="applyRouterClassifierPreset(\'gateway_upstream\',\'' + esc(m) + '\')">' + esc(m) + '</button>'
          ).join("")
        : '<small class="muted">当前暂无可用模型，请先在「上游服务」中配置模型。</small>';
    } else {
      presetsBox.innerHTML = "";
    }
  }
}

const ROUTER_DOMAIN_LABELS = {
  coding_swe: "SWE工程/代码",
  frontend_ui: "前端/UI视觉",
  reasoning_math: "数学/形式推理",
  agentic_tools: "Agent/工具编排",
  writing_knowledge: "通用知识/写作",
  long_context_mm: "超长上下文/多模态",
};

function toggleExcludeAutonomousModel(model) {
  if (!routerDraft || !model) return;
  if (!Array.isArray(routerDraft.excluded_models)) routerDraft.excluded_models = [];
  const raw = model.toLowerCase(), base = parseRouterCandidateSpecJS(model).base.toLowerCase();
  const matches = (m) => m.toLowerCase() === raw || m.toLowerCase() === base;
  if (routerDraft.excluded_models.some(matches)) {
    routerDraft.excluded_models = routerDraft.excluded_models.filter((m) => !matches(m));
  } else {
    routerDraft.excluded_models.push(model);
  }
  renderRouterAutonomousPoolEditor();
  markRouterDirty(true);
}

async function resetAutoRouterBanditState(button) {
  setBusy(button, true, "重置中…");
  try {
    await api("api/auto-router/bandit/reset", { method: "POST", body: "{}" });
    await loadAutoRouter();
    toast("已清空在线 Bandit 历史学习数据，清空历史表现记录（不修改真实榜单）");
  } catch (error) {
    reportError(error, "重置 Bandit 失败");
  } finally {
    setBusy(button, false);
  }
}

function capabilityTagsForRouterModel(model) {
  const healthMap = routerConfigCache?.model_health || {};
  const spec = parseRouterCandidateSpecJS(model);
  const info = healthMap[model] || healthMap[spec.base];
  const cap = info?.capabilities;
  if (!cap) return "";
  const dims = [
    { k: "SWE", v: Number(cap.coding_swe || 0), title: "旧启发式工程参考分，不是 SWE-bench / Aider 实测" },
    { k: "UI", v: Number(cap.frontend_ui || 0), title: "旧启发式 UI 参考分，不是 WebDev 原始评分" },
    { k: "Math", v: Number(cap.reasoning_math || 0), title: "旧启发式数学参考分，不是 AIME / GPQA 实测" },
    { k: "Agent", v: Number(cap.agentic_tools || 0), title: "旧启发式工具参考分，不是 BFCL 实测" },
  ];
  dims.sort((a, b) => b.v - a.v);
  const top2 = dims.slice(0, 2);
  let html = top2.map((d) =>
    '<span class="router-price-tag" title="' + esc(d.title) + ': ' + d.v.toFixed(0) + '/100">' + d.k + ' ' + d.v.toFixed(0) + '</span>'
  ).join("");
  if (info?.bandit_calls > 0 && info?.bandit_win_rate > 0) {
    const wr = (Number(info.bandit_win_rate) * 100).toFixed(0);
    const ttft = info.ewma_ttft_ms > 0 ? " · TTFT " + Math.round(info.ewma_ttft_ms) + "ms" : "";
    html += '<span class="router-price-tag" title="旧 Bandit 后验，不是回答正确率 (' + info.bandit_calls + ' 次调用' + ttft + ')">旧后验 ' + wr + '%</span>';
  }
  return html;
}

function renderRouterAutonomousPoolEditor() {
  const box = $("routerAutonomousPoolContainer");
  const lbBox = $("routerBanditLeaderboardBox");
  if (!box || !routerDraft) return;
  const allModels = routerDraft.model_pool?.length ? [...routerDraft.model_pool] : availableGatewayModelOptions();
  const excludedSet = new Set((routerDraft.excluded_models || []).map((m) => m.toLowerCase()));
  const isExcluded = (m) => excludedSet.has(m.toLowerCase()) || excludedSet.has(parseRouterCandidateSpecJS(m).base.toLowerCase());
  const modePill = '<span class="badge on">统一候选池 · 无手动优先级</span>';

  if (!allModels.length) {
    box.innerHTML = '<div class="router-candidate-empty" style="border-style:dashed">'
      + '当前尚未检测到可用上游模型。请先前往左侧 <strong>上游服务</strong> 绑定模型，系统会在可用模型之间按真实榜单与策略择优。'
      + '</div>';
  } else {
    const activeCount = allModels.filter((m) => !isExcluded(m)).length;
    const rows = allModels.map((m) => {
      const excluded = isExcluded(m);
      const spec = parseRouterCandidateSpecJS(m);
      return '<div class="router-candidate-item" style="' + (excluded ? 'opacity:0.52;' : '') + '">'
        + '<div class="router-candidate-main">'
        + modelIcon(spec.base)
        + '<span class="router-candidate-name" title="' + esc(m) + '">' + esc(m) + '</span>'
        + (excluded ? '<span class="badge warn" style="font-size:10px">已从自动池排除</span>' : '')
        + '</div>'
        + '<div class="router-candidate-meta">'
        + (routerDraft.arena_mode === "live" ? "" : capabilityTagsForRouterModel(m))
        + priceHintForRouterModel(m)
        + healthBadgeForRouterModel(m)
        + '<button type="button" class="btn mini ghost" style="padding:2px 8px;font-size:11px" data-router-exclude="' + esc(m) + '">'
        + (excluded ? '恢复参与寻优' : '排除')
        + '</button>'
        + '</div></div>';
    }).join("");

    box.innerHTML = '<div class="router-tier-card">'
      + '<div class="router-tier-header">'
      + '<div class="router-tier-title"><strong>自治候选模型池 (' + activeCount + ' / ' + allModels.length + ' 可用)</strong><small class="muted">只控制模型是否参与；列表顺序不是优先级。榜单证据见试运行明细</small></div>'
      + modePill
      + '</div>'
      + '<div class="router-candidate-list" style="max-height:290px;overflow-y:auto">' + rows + '</div>'
      + '</div>';
    box.querySelectorAll("[data-router-exclude]").forEach((button) => {
      button.addEventListener("click", () => toggleExcludeAutonomousModel(button.dataset.routerExclude));
    });
  }

  if (lbBox) {
    const lb = Array.isArray(routerConfigCache?.bandit_leaderboard) ? routerConfigCache.bandit_leaderboard : [];
    const activeArms = (routerDraft.arena_mode === "live" ? [] : lb).filter((a) => a.upstream_id === 0 && (a.calls > 0 || a.implicit_negative > 0 || a.speculative_escalations > 0)).slice(0, 8);
    if (!activeArms.length) {
      lbBox.innerHTML = "";
    } else {
      const armChips = activeArms.map((a) => {
        const tot = (Number(a.alpha || 2.2) + Number(a.beta || 1.0));
        const wr = tot > 0 ? ((Number(a.alpha || 2.2) / tot) * 100).toFixed(0) : "69";
        const domName = ROUTER_DOMAIN_LABELS[a.domain] || a.domain;
        const negHint = a.implicit_negative > 0 ? ' · 纠错-' + a.implicit_negative : '';
        const escHint = a.speculative_escalations > 0 ? ' · 探针升档-' + a.speculative_escalations : '';
        return '<span class="router-preset-chip" title="Bandit 实测后验 Beta(' + Number(a.alpha).toFixed(1) + ', ' + Number(a.beta).toFixed(1) + ')">'
          + '<code>' + esc(a.model) + '</code> · ' + esc(domName) + ' · 旧后验 <strong>' + wr + '%</strong> (' + a.calls + '次' + negHint + escHint + ')'
          + '</span>';
      }).join("");
      lbBox.innerHTML = '<div style="padding:8px 10px;border-radius:8px;background:var(--surface-2,#f8f7f4);border:1px solid var(--border,#e5e3df)">'
        + '<div style="font-size:12px;font-weight:600;margin-bottom:6px">旧 Bandit 后验记录（非回答正确率；Arena 模式不将其当质量分）：</div>'
        + '<div class="router-preset-chips">' + armChips + '</div>'
        + '</div>';
    }
  }
}

function renderAutoRouterConfig() {
  if (!routerDraft) return;
  setRouterMode(routerDraft.mode);
  $("routerVirtualModel").value = routerDraft.virtual_model || "auto";
  if ($("routerStrategyProfile")) $("routerStrategyProfile").value = routerDraft.strategy_profile || "balanced";
  if ($("routerArenaMode")) $("routerArenaMode").value = routerDraft.arena_mode || "live";
  if ($("routerBanditEnabled")) $("routerBanditEnabled").checked = routerDraft.bandit_enabled !== false;
  if ($("routerSpeculativeCascade")) $("routerSpeculativeCascade").checked = routerDraft.speculative_cascade !== false;
  $("routerDefaultModel").value = routerDraft.default_model || "";
  if ($("routerModelPool")) $("routerModelPool").value = (routerDraft.model_pool || []).join("\n");
  $("routerAllowUnlisted").checked = routerDraft.allow_unlisted_target_models !== false;
  $("routerResponseHeaders").checked = routerDraft.response_headers !== false;

  $("routerTimeoutMS").value = routerDraft.classifier_timeout_ms || 1500;
  $("routerFallback").value = routerDraft.classifier_fallback || "heuristic";
  $("routerMinConfidence").value = routerDraft.min_confidence || 0.55;
  $("routerContextTurns").value = routerDraft.context_user_turns ?? 2;
  $("routerSkipShortChars").value = routerDraft.skip_short_chars ?? 60;
  if ($("routerLongContextThreshold")) $("routerLongContextThreshold").value = routerDraft.long_context_threshold ?? 32000;
  $("routerSessionMode").value = routerDraft.session_mode || "user_turn";
  $("routerHousekeeping").checked = routerDraft.route_housekeeping !== false;
  $("routerEscalationKeyword").value = routerDraft.escalation_keyword || "";
  if ($("routerAllowUserRouter")) $("routerAllowUserRouter").checked = routerDraft.allow_user_auto_router !== false;
  if ($("routerShareSiteClassifier")) $("routerShareSiteClassifier").checked = Boolean(routerDraft.share_site_classifier_with_users);
  if ($("routerSharedClassifierModels")) $("routerSharedClassifierModels").value = (routerDraft.shared_classifier_models || []).join(", ");
  $("routerAPIKey").value = "";
  $("routerClearAPIKey").checked = false;

  renderRouterAutonomousPoolEditor();
  refreshRouterModelControls();
  renderRouterClassifierForm();
  markRouterDirty(false);
}

function collectRouterDraftFromInputs() {
  if (!routerDraft) return null;
  routerDraft.virtual_model = $("routerVirtualModel").value.trim() || "auto";
  if ($("routerStrategyProfile")) routerDraft.strategy_profile = $("routerStrategyProfile").value || "balanced";
  if ($("routerArenaMode")) routerDraft.arena_mode = $("routerArenaMode").value || "live";
  if ($("routerBanditEnabled")) routerDraft.bandit_enabled = $("routerBanditEnabled").checked;
  if ($("routerSpeculativeCascade")) routerDraft.speculative_cascade = $("routerSpeculativeCascade").checked;
  routerDraft.default_model = $("routerDefaultModel").value.trim();
  routerDraft.model_pool = splitRouterPoolInput($("routerModelPool")?.value || "");
  routerDraft.tiers = {};
  routerDraft.cascade_tiers = false;
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
  if ($("routerLongContextThreshold")) routerDraft.long_context_threshold = Number($("routerLongContextThreshold").value || 0);
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
    out.innerHTML = '<strong>连接成功</strong> · 分类器 <code>' + esc(res.provider) + ' / ' + esc(res.model) + '</code> · 延迟 <strong>' + Number(res.latency_ms || 0) + ' ms</strong> · 任务难度 <span class="router-tier-badge tier-' + esc(res.tier) + '">' + esc(res.tier) + '</span>'
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
    reportError(error, "选模试算失败");
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
    const ctxTag = st.context_limit ? '<span class="router-price-tag">' + formatContextWindowTokens(st.context_limit) + ' ctx</span>' : '';
    const thinkTag = st.thinking ? '<span class="router-price-tag" title="实际适配思考档位">思考: ' + esc(st.thinking) + '</span>' : '';
    const utilTag = st.utility_score ? '<span class="router-price-tag" title="旧启发式策略参考，不是真实榜单评分">旧策略 U=' + Number(st.utility_score).toFixed(1) + '</span>' : '';
    const evidence = st.arena;
    const arenaTag = evidence ? '<span class="router-price-tag" title="' + esc(evidence.identity || evidence.status) + '">Arena: ' + (evidence.status === "matched" && evidence.score != null ? '#' + evidence.rank + ' · ' + Number(evidence.score).toFixed(dec.arena?.board === "agent" ? 4 : 2) : esc(evidence.status)) + '</span>' : '';
    const runtimeTag = evidence ? '<span class="router-price-tag" title="费用只作相同输出预算下的比较参考，不是实付账单">'
      + (evidence.price_known && evidence.comparison_cost_usd != null ? '费用参考 $' + Number(evidence.comparison_cost_usd).toPrecision(4) + ' · 输出 ' + Number(evidence.cost_reference_output_tokens || 0) + ' token' : evidence.price_source === 'byok_cost_unknown' ? '自带渠道费用未知' : '费率未知')
      + (evidence.channel_response_ms != null ? ' · ' + (evidence.channel_response_kind === 'nonstream_total' ? '非流式响应 ' : '流式 TTFT ') + Math.round(evidence.channel_response_ms) + ' ms' : ' · 速度未知')
      + (evidence.channel_unreliable ? ' · 主要渠道近期不稳定' : '') + '</span>' : '';
    return '<div class="router-step-row ' + esc(st.status) + '">'
      + '<div style="display:flex;align-items:center;gap:6px;flex-wrap:wrap">'
      + '<span class="router-tier-badge tier-' + esc(st.source_tier) + '">' + esc(st.source_tier) + '</span>'
      + modelIcon(st.model)
      + '<code>' + esc(st.model) + '</code>'
      + utilTag
      + arenaTag
      + runtimeTag
      + thinkTag
      + ctxTag
      + (st.upstream_name ? '<small class="muted">→ ' + esc(st.upstream_name) + '</small>' : '')
      + '</div>'
      + statusLabel
      + '</div>';
  }).join("");

  const domLabel = dec.primary_domain ? (ROUTER_DOMAIN_LABELS[dec.primary_domain] || dec.primary_domain) : "";
  const domBadge = domLabel ? '<span class="router-price-tag" style="background:rgba(139,92,246,0.14);color:#6d28d9" title="6维基准识别主领域">领域: ' + esc(domLabel) + '</span>' : '';
  const diffBadge = dec.required_difficulty ? '<span class="router-price-tag" title="启发式任务难度，仅决定候选政策，不是 Arena 分数">难度参考 ' + Number(dec.required_difficulty).toFixed(0) + '</span>' : '';
  const poolBadge = dec.autonomous_pool ? '<span class="router-price-tag" style="background:rgba(16,185,129,0.14);color:#047857">全自动池寻优</span>' : '';
  const arena = dec.arena;
  const arenaNote = arena ? '<p class="muted" style="margin-top:8px">Arena ' + (arena.mode === "live" ? '正式启用' : '观察对照，不改变实际转发') + ' · ' + esc(arena.board || '无分榜') + (arena.published_at ? ' · 源榜 ' + esc(arena.published_at) : '')
    + ' · 原候选 ' + esc(arena.baseline_model || '—') + ' → 建议 <strong>' + esc(arena.proposed_model || '—') + '</strong> · 实际 ' + esc(arena.actual_model || dec.selected_model || '—')
    + (arena.quality_floor != null ? ' · 候选门槛 ' + Number(arena.quality_floor).toFixed(arena.board === 'agent' ? 4 : 2) + '（源榜分位政策，非准确率）' : '')
    + (arena.general_reference_only ? ' · 仅通用弱参考，不代表专项能力' : '') + ' · ' + esc(arena.reason || '') + '</p>' : '';

  container.innerHTML = '<div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:8px;padding-bottom:8px;border-bottom:1px solid var(--border,#e5e3df)">'
    + '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">'
    + '<span>判定难度：</span><span class="router-tier-badge tier-' + esc(dec.tier || "DEFAULT") + '">' + esc(dec.tier || "DEFAULT") + '</span>'
    + domBadge
    + diffBadge
    + poolBadge
    + '<span>→ 选中模型：</span>' + modelLabel(dec.selected_model || "")
    + (dec.selected_thinking ? '<span class="router-price-tag" title="自动适配思考模式">思考: ' + esc(dec.selected_thinking) + '</span>' : '')
    + (dec.estimated_input_tokens ? '<span class="router-price-tag" title="估算输入上下文 Tokens">~' + Number(dec.estimated_input_tokens).toLocaleString() + ' tok</span>' : '')
    + (dec.shadow && dec.shadow_model ? '<small class="muted">(Shadow 拟选: ' + esc(dec.shadow_model) + ')</small>' : '')
    + '</div>'
    + '<small class="muted">原因: <code>' + esc(dec.cause || "—") + '</code> · 分类耗时 ' + Number(dec.classifier_ms || 0) + ' ms'
    + (dec.heuristic_score !== undefined && dec.heuristic_score !== null ? ' · 启发式分 ' + Number(dec.heuristic_score).toFixed(2) : '')
    + '</small>'
    + '</div>'
    + arenaNote
    + (Object.keys(probs).length ? '<div class="router-prob-grid">' + probHtml + '</div>' : '')
    + '<div class="router-steps-list"><strong>候选与 Failover 顺序（Arena 观察建议单独记录，不改变旧策略）：</strong>' + stepsHtml + '</div>';
}

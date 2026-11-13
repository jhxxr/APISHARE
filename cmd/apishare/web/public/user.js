(() => {
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  const state = {
    dash: null,
    keyVisible: false,
    snippetKind: "openai",
    trendDays: 7,
    trendMetric: "cost",
    logPage: 1,
    logSize: 20,
    logTotal: 0,
    logQuery: "",
    logStatus: "",
    toastTimer: 0,
    routerView: null,
    routerPool: [],
    routerExcluded: [],
  };

  const numFmt = new Intl.NumberFormat("zh-CN");
  const compactFmt = new Intl.NumberFormat("zh-CN", { notation: "compact", maximumFractionDigits: 1 });

  const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (ch) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[ch]));

  const fmtInt = (n) => numFmt.format(Math.round(Number(n) || 0));
  const fmtCompact = (n) => {
    const v = Number(n) || 0;
    return v >= 10000 ? compactFmt.format(v) : fmtInt(v);
  };
  const fmtUSD = (n) => {
    const v = Number(n) || 0;
    if (v === 0) return "$0.00";
    if (v < 0.01) return "$" + v.toFixed(4);
    return "$" + v.toFixed(2);
  };
  const fmtQuotaNumber = (n) => {
    const v = Number(n) || 0;
    return v.toFixed(4).replace(/\.?0+$/, "");
  };
  const fmtBeijingTime = (iso) => {
    if (!iso) return "—";
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return String(iso).replace("T", " ").slice(0, 19);
    return d.toLocaleString("zh-CN", {
      timeZone: "Asia/Shanghai",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    });
  };
  const fmtBeijingDate = (iso) => {
    if (!iso) return "—";
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return String(iso).slice(0, 10);
    return d.toLocaleDateString("zh-CN", {
      timeZone: "Asia/Shanghai",
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    });
  };

  function toast(msg) {
    const el = $("#userToast");
    if (!el) return;
    el.textContent = msg;
    el.classList.remove("hidden");
    window.clearTimeout(state.toastTimer);
    state.toastTimer = window.setTimeout(() => el.classList.add("hidden"), 2600);
  }

  async function copyText(text, successMsg) {
    try {
      if (typeof window.copyToClipboard === "function") {
        await window.copyToClipboard(text);
      } else if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text);
      }
      toast(successMsg || "已复制到剪贴板");
    } catch {
      toast("复制失败，请手动选择复制");
    }
  }

  function modelLabelHTML(modelName, familyName = "") {
    if (window.APIShareModelIcons?.labelHTML) {
      return window.APIShareModelIcons.labelHTML(modelName, familyName, { compact: true });
    }
    return `<code>${esc(modelName || "-")}</code>`;
  }

  async function loadBranding() {
    try {
      const res = await fetch("/api/public/branding", { cache: "no-store" });
      if (!res.ok) return;
      const b = await res.json();
      const title = String(b.site_title || "").trim() || "APIShare";
      document.title = `${title} · 个人控制台`;
      $$("[data-site-brand-name]").forEach((el) => { el.textContent = title; });
      const iconHref = b.has_custom_icon && b.site_icon_url ? b.site_icon_url : "";
      if (iconHref) {
        $$("[data-site-brand-logo]").forEach((img) => { img.src = iconHref; });
        $$("[data-site-brand-icon]").forEach((link) => {
          link.href = iconHref;
          if (b.site_icon_mime) link.type = b.site_icon_mime;
        });
      }
    } catch {
      // ignore branding errors
    }
  }

  function renderProfile(user) {
    const chip = $("#userProfileChip");
    const logoutBtn = $("#logoutBtn");
    if (!user) {
      chip?.classList.add("hidden");
      logoutBtn?.classList.add("hidden");
      return;
    }
    const name = String(user.name || user.email || user.sub || "Auth 用户").trim();
    const email = String(user.email || "").trim();
    const initial = (name.replace(/^@/, "").trim()[0] || "U").toUpperCase();
    if ($("#userAvatar")) $("#userAvatar").textContent = initial;
    if ($("#userName")) $("#userName").textContent = name;
    if ($("#userEmail")) {
      $("#userEmail").textContent = email && email !== name ? email : `ID: ${String(user.sub || "").slice(0, 14)}`;
    }
    chip?.classList.remove("hidden");
    logoutBtn?.classList.remove("hidden");
  }

  function renderKeyCard() {
    const dash = state.dash;
    if (!dash?.key) return;
    const k = dash.key;
    if ($("#keyTitle")) $("#keyTitle").textContent = k.name || "我的专属密钥";
    const badge = $("#keyStatusBadge");
    if (badge) {
      const hasNoAccess = (!Array.isArray(k.models) || k.models.length === 0) && Number(k.quota_usd) === 0;
      if (!k.enabled) {
        badge.textContent = "已停用";
        badge.className = "status-pill off";
      } else if (hasNoAccess) {
        badge.textContent = "额度已清零";
        badge.className = "status-pill off";
      } else {
        badge.textContent = "启用中";
        badge.className = "status-pill on";
      }
    }
    const keyDisplay = $("#keyDisplay");
    if (keyDisplay) {
      keyDisplay.textContent = state.keyVisible ? k.key : (k.key_masked || "sk-••••••••••••••••");
    }
    const toggleBtn = $("#toggleKeyBtn");
    const toggleLabel = $("#toggleKeyLabel");
    if (toggleBtn) toggleBtn.setAttribute("aria-pressed", state.keyVisible ? "true" : "false");
    if (toggleLabel) toggleLabel.textContent = state.keyVisible ? "隐藏" : "显示";

    if ($("#keyQps")) $("#keyQps").textContent = k.qps > 0 ? `${k.qps} QPS` : "不限";
    if ($("#keyConc")) $("#keyConc").textContent = k.concurrency > 0 ? `${k.concurrency} 并发` : "不限";
    if ($("#keyCreatedAt")) $("#keyCreatedAt").textContent = fmtBeijingDate(k.created_at);

    renderSnippet();
  }

  function renderSnippet() {
    const dash = state.dash;
    const codeEl = $("#snippetCode");
    if (!codeEl || !dash?.key) return;
    const origin = window.location.origin;
    const keyText = state.keyVisible ? dash.key.key : (dash.key.key_masked || "sk-YOUR_API_KEY");
    const firstModel = (Array.isArray(dash.key.models) && dash.key.models[0]) || "gpt-4o-mini";

    if (state.snippetKind === "claude") {
      codeEl.textContent = [
        `export ANTHROPIC_BASE_URL="${origin}"`,
        `export ANTHROPIC_AUTH_TOKEN="${keyText}"`,
        `claude --model ${firstModel}`,
      ].join("\n");
      return;
    }
    if (state.snippetKind === "codex") {
      codeEl.textContent = [
        `export OPENAI_BASE_URL="${origin}/v1"`,
        `export OPENAI_API_KEY="${keyText}"`,
        `codex --model ${firstModel}`,
      ].join("\n");
      return;
    }
    codeEl.textContent = [
      `curl ${origin}/v1/chat/completions \\`,
      `  -H "Authorization: Bearer ${keyText}" \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -d '{"model":"${firstModel}","messages":[{"role":"user","content":"Hello!"}]}'`,
    ].join("\n");
  }

  function renderQuotaAndTelemetry() {
    const dash = state.dash;
    if (!dash) return;
    const k = dash.key || {};
    const ov = dash.overview || {};
    const boundModels = Array.isArray(k.models) ? k.models : [];
    const remEl = $("#quotaRemaining");
    const subEl = $("#quotaSubtext");
    const barWrap = $("#quotaProgressWrap");
    const barFill = $("#quotaProgressFill");
    const usedLabel = $("#quotaUsedLabel");
    const totalLabel = $("#quotaTotalLabel");

    const independentModels = boundModels.filter((m) => ov.models?.[m]?.mode === "independent");
    const sharedModels = boundModels.filter((m) => ov.models?.[m]?.mode !== "independent");

    if (boundModels.length === 0) {
      if (remEl) remEl.textContent = "$0.00";
      if (subEl) subEl.textContent = Number(k.quota_usd) === 0
        ? "当前密钥暂无可用额度与绑定模型，如需开通请联系管理员。"
        : `共享额度池 $${fmtQuotaNumber(k.quota_usd)}（暂未绑定可用模型）`;
      barWrap?.classList.add("hidden");
    } else if (sharedModels.length === 0 && independentModels.length > 0) {
      // All bound models use independent quotas
      let totalQuota = 0;
      let totalUsed = 0;
      let anyUnlimited = false;
      independentModels.forEach((m) => {
        const pool = ov.models?.[m]?.pool;
        if (!pool || pool.unlimited) {
          anyUnlimited = true;
        } else {
          totalQuota += Number(pool.quota_usd) || 0;
          totalUsed += Number(pool.used_usd) || 0;
        }
      });
      if (anyUnlimited && totalQuota === 0) {
        if (remEl) remEl.textContent = "不限额度";
        if (subEl) subEl.textContent = `已绑定 ${boundModels.length} 个独立配额模型 · 累计已用 ${fmtUSD(k.used_usd)}`;
        barWrap?.classList.add("hidden");
      } else {
        const rem = Math.max(0, totalQuota - totalUsed);
        const pct = totalQuota > 0 ? Math.min(100, (totalUsed / totalQuota) * 100) : 0;
        if (remEl) remEl.textContent = fmtUSD(rem);
        if (subEl) subEl.textContent = `${independentModels.length} 个独立额度模型合计剩余${anyUnlimited ? "（部分模型不限额）" : ""}`;
        barWrap?.classList.remove("hidden");
        if (barFill) {
          barFill.style.width = `${pct.toFixed(1)}%`;
          barFill.className = pct >= 95 ? "danger" : pct >= 80 ? "warn" : "";
        }
        if (usedLabel) usedLabel.textContent = `已用 ${fmtUSD(totalUsed)}`;
        if (totalLabel) totalLabel.textContent = `独立总额 $${fmtQuotaNumber(totalQuota)}`;
      }
    } else if (ov.shared && !ov.shared.unlimited) {
      const rem = Number(ov.shared.remaining_usd) || 0;
      const quota = Number(ov.shared.quota_usd) || 0;
      const used = Number(ov.shared.used_usd) || 0;
      const pct = quota > 0 ? Math.min(100, (used / quota) * 100) : 0;
      if (remEl) remEl.textContent = fmtUSD(rem);
      if (subEl) {
        subEl.textContent = independentModels.length > 0
          ? `共享额度池剩余（另有 ${independentModels.length} 个模型享有独立额度）`
          : `共享额度池剩余 · 共绑定 ${boundModels.length} 个可用模型`;
      }
      barWrap?.classList.remove("hidden");
      if (barFill) {
        barFill.style.width = `${pct.toFixed(1)}%`;
        barFill.className = pct >= 95 ? "danger" : pct >= 80 ? "warn" : "";
      }
      if (usedLabel) usedLabel.textContent = `已用 ${fmtUSD(used)}`;
      if (totalLabel) totalLabel.textContent = `总额 $${fmtQuotaNumber(quota)}`;
    } else {
      if (remEl) remEl.textContent = "不限额度";
      if (subEl) subEl.textContent = `已绑定 ${boundModels.length} 个可用模型 · 累计总花费 ${fmtUSD(k.used_usd)}`;
      barWrap?.classList.add("hidden");
    }

    const s = dash.summary || {};
    const calls = Number(s.calls) || 0;
    const succ = Number(s.successes) || 0;
    const promptTok = Number(s.prompt_tokens) || 0;
    const compTok = Number(s.completion_tokens) || 0;

    if ($("#statTotalCalls")) $("#statTotalCalls").textContent = fmtInt(calls);
    if ($("#statSuccessRate")) {
      $("#statSuccessRate").textContent = calls > 0
        ? `成功率 ${((succ / calls) * 100).toFixed(1)}%`
        : "尚无调用记录";
    }
    if ($("#statTotalCost")) $("#statTotalCost").textContent = fmtUSD(s.cost_usd);
    if ($("#statTodayCost")) $("#statTodayCost").textContent = `今日 ${fmtUSD(s.today_cost_usd)}`;
    if ($("#statTotalTokens")) $("#statTotalTokens").textContent = fmtCompact(promptTok + compTok);
    if ($("#statTokenSplit")) $("#statTokenSplit").textContent = `输入 ${fmtCompact(promptTok)} / 输出 ${fmtCompact(compTok)}`;
    if ($("#statTodayCalls")) $("#statTodayCalls").textContent = fmtInt(s.today_calls);
    if ($("#statUpdatedAt")) $("#statUpdatedAt").textContent = `更新于 ${fmtBeijingTime(dash.updated_at)}`;
  }

  function metricValue(point, metric) {
    if (!point) return 0;
    if (metric === "calls") return Number(point.calls) || 0;
    if (metric === "tokens") return (Number(point.prompt_tokens) || 0) + (Number(point.completion_tokens) || 0);
    return Number(point.cost_usd) || 0;
  }

  function formatMetric(val, metric) {
    if (metric === "calls") return `${fmtInt(val)} 次`;
    if (metric === "tokens") return `${fmtCompact(val)} Tokens`;
    return fmtUSD(val);
  }

  function renderTrendChart() {
    const dash = state.dash;
    const host = $("#userTrendChart");
    if (!host || !dash) return;
    const allDays = Array.isArray(dash.daily) ? dash.daily : [];
    const slice = allDays.slice(-state.trendDays);
    const values = slice.map((d) => metricValue(d, state.trendMetric));
    const total = values.reduce((a, b) => a + b, 0);
    const maxVal = Math.max(...values, 0);

    if ($("#trendWindowSum")) $("#trendWindowSum").textContent = formatMetric(total, state.trendMetric);
    if ($("#trendWindowLabel")) $("#trendWindowLabel").textContent = `近 ${slice.length} 天累计`;
    const hoverNote = $("#trendHoverNote");
    if (hoverNote) hoverNote.textContent = "悬浮柱条查看单日明细";

    host.innerHTML = slice.map((d, idx) => {
      const val = values[idx];
      const pct = maxVal > 0 ? Math.max(val > 0 ? 7 : 2, Math.round((val / maxVal) * 100)) : 2;
      const shortDate = String(d.date || "").slice(5);
      const showLabel = slice.length <= 7 || idx === 0 || idx === slice.length - 1 || idx % 5 === 0;
      const summary = `${d.date} · 费用 ${fmtUSD(d.cost_usd)} · 调用 ${fmtInt(d.calls)} 次 · Tokens ${fmtCompact((d.prompt_tokens || 0) + (d.completion_tokens || 0))}`;
      return `<div class="trend-bar-col" data-summary="${esc(summary)}" title="${esc(summary)}">
        <div class="trend-bar-track">
          <div class="trend-bar-fill ${val === 0 ? "zero" : ""}" style="height:${pct}%"></div>
        </div>
        <span class="trend-bar-date">${showLabel ? esc(shortDate) : ""}</span>
      </div>`;
    }).join("");

    $$(".trend-bar-col", host).forEach((col) => {
      col.addEventListener("mouseenter", () => {
        if (hoverNote) hoverNote.textContent = col.dataset.summary || "";
      });
    });
    host.addEventListener("mouseleave", () => {
      if (hoverNote) hoverNote.textContent = "悬浮柱条查看单日明细";
    });
  }

  function renderModelsList() {
    const dash = state.dash;
    const host = $("#userModelsList");
    if (!host || !dash) return;
    const items = Array.isArray(dash.model_usage) ? dash.model_usage : [];
    const boundCount = Array.isArray(dash.key?.models) ? dash.key.models.length : 0;
    if ($("#boundModelCount")) $("#boundModelCount").textContent = String(boundCount);

    if (!items.length) {
      host.innerHTML = `<div class="empty-cell">当前密钥尚未绑定可用模型，请联系管理员分配模型。</div>`;
      return;
    }

    host.innerHTML = items.map((item) => {
      const pool = item.pool;
      let badgeHTML = `<span class="model-quota-tag">历史调用模型（当前未绑定）</span>`;
      let progressHTML = "";

      if (item.bound) {
        if (item.quota_mode === "independent") {
          if (pool && !pool.unlimited) {
            const rem = Number(pool.remaining_usd) || 0;
            const quota = Number(pool.quota_usd) || 0;
            const used = Number(pool.used_usd) || 0;
            const pct = quota > 0 ? Math.min(100, (used / quota) * 100) : 0;
            badgeHTML = `<span class="model-quota-tag independent">独立额度 · 剩余 ${fmtUSD(rem)} / $${fmtQuotaNumber(quota)}</span>`;
            progressHTML = `<div class="quota-bar"><span style="width:${pct.toFixed(1)}%" class="${pct >= 95 ? "danger" : pct >= 80 ? "warn" : ""}"></span></div>`;
          } else {
            badgeHTML = `<span class="model-quota-tag independent">独立额度 · 不限</span>`;
          }
        } else if (pool && !pool.unlimited) {
          badgeHTML = `<span class="model-quota-tag">共享额度池 · 剩余 ${fmtUSD(pool.remaining_usd)}</span>`;
        } else {
          badgeHTML = `<span class="model-quota-tag">共享额度池 · 不限额</span>`;
        }
      }

      const tokens = (Number(item.prompt_tokens) || 0) + (Number(item.completion_tokens) || 0);
      return `<div class="user-model-item">
        <div class="user-model-top">
          <div class="user-model-name">${modelLabelHTML(item.model, item.family)}</div>
          ${badgeHTML}
        </div>
        ${progressHTML}
        <div class="user-model-meta">
          <span>调用 <strong>${fmtInt(item.calls)}</strong> 次</span>
          <span>Tokens <strong>${fmtCompact(tokens)}</strong></span>
          <span>累计花费 <strong>${fmtUSD(item.cost_usd)}</strong></span>
        </div>
      </div>`;
    }).join("");
  }

  async function loadDashboard() {
    try {
      const res = await fetch("/api/user/dashboard", { cache: "no-store" });
      if (res.status === 401) {
        state.dash = null;
        renderProfile(null);
        $("#dashboardContent")?.classList.add("hidden");
        $("#unauthCard")?.classList.remove("hidden");
        return false;
      }
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        toast(data.error || "加载个人控制台失败");
        return false;
      }
      state.dash = data;
      $("#unauthCard")?.classList.add("hidden");
      $("#dashboardContent")?.classList.remove("hidden");
      renderProfile(data.user);
      renderKeyCard();
      renderQuotaAndTelemetry();
      renderTrendChart();
      renderModelsList();
      return true;
    } catch {
      toast("网络异常，无法加载个人控制台");
      return false;
    }
  }

  function renderUserLogRouterMeta(row) {
    const r = row?.billing?.router;
    if (!r || !r.requested_model) return "";
    const failedList = Array.isArray(r.failed_models) && r.failed_models.length
      ? r.failed_models
      : Array.isArray(r.fallbacks_tried) ? r.fallbacks_tried : [];
    const degradedDefault = Boolean(r.degraded_to_default) || r.selected_source_tier === "DEFAULT";
    const tierName = r.tier || "AUTO";
    const routePill = `<span class="user-log-route-pill" title="请求模型 ${esc(r.requested_model)} → 任务难度 ${esc(tierName)} (${esc(r.cause || "")})">${esc(r.requested_model)} → ${esc(tierName)}</span>`;

    if (degradedDefault) {
      const failedText = failedList.length ? `候选模型异常 (${failedList.join(", ")})，` : `候选池无可用模型，`;
      const tip = `${failedText}已自动降级路由至最终后备模型 ${row.model || ""}`;
      return `<div class="user-log-route-wrap">${routePill}<span class="user-log-fallback-badge danger" title="${esc(tip)}">⚠️ 候选模型异常 · 已回退至兜底模型</span></div>`
        + (failedList.length ? `<div class="user-log-failed-note" title="${esc(tip)}">异常模型: <code>${esc(failedList.join(", "))}</code></div>` : "");
    }
    if (failedList.length > 0) {
      const tip = `首选模型异常 (${failedList.join(", ")})，已自动故障切换至备选模型 ${row.model || ""}`;
      return `<div class="user-log-route-wrap">${routePill}<span class="user-log-fallback-badge warn" title="${esc(tip)}">⚠️ 候选故障切换</span></div>`
        + `<div class="user-log-failed-note" title="${esc(tip)}">已跳过异常模型: <code>${esc(failedList.join(", "))}</code></div>`;
    }
    return `<div class="user-log-route-wrap">${routePill}</div>`;
  }

  async function loadUserLogs() {
    const tbody = $("#userLogsBody");
    if (!tbody) return;
    const params = new URLSearchParams({
      page: String(state.logPage),
      size: String(state.logSize),
    });
    if (state.logQuery) params.set("q", state.logQuery);
    if (state.logStatus) params.set("status", state.logStatus);

    try {
      const res = await fetch(`/api/user/logs?${params.toString()}`, { cache: "no-store" });
      if (!res.ok) {
        tbody.innerHTML = `<tr><td colspan="7" class="empty-cell">无法加载调用记录</td></tr>`;
        return;
      }
      const data = await res.json();
      const items = Array.isArray(data.items) ? data.items : [];
      state.logTotal = Number(data.total) || 0;
      const totalPages = Math.max(1, Math.ceil(state.logTotal / state.logSize));
      if (state.logPage > totalPages) state.logPage = totalPages;

      if ($("#userLogTotal")) $("#userLogTotal").textContent = fmtInt(state.logTotal);
      if ($("#userLogPageInfo")) $("#userLogPageInfo").textContent = `${state.logPage} / ${totalPages}`;
      if ($("#userLogPrev")) $("#userLogPrev").disabled = state.logPage <= 1;
      if ($("#userLogNext")) $("#userLogNext").disabled = state.logPage >= totalPages;
      if ($("#userLogSummaryText")) {
        const s = data.summary || {};
        $("#userLogSummaryText").textContent = state.logTotal > 0
          ? `共 ${fmtInt(state.logTotal)} 条匹配记录 · 合计花费 ${fmtUSD(s.cost_usd)}`
          : "暂无匹配的调用记录";
      }

      if (!items.length) {
        tbody.innerHTML = `<tr><td colspan="7" class="empty-cell">暂无调用记录</td></tr>`;
        return;
      }

      tbody.innerHTML = items.map((row) => {
        const ok = row.status >= 200 && row.status < 300;
        const statusCls = ok ? "ok" : "err";
        const latency = row.latency_ms ? `${ (row.latency_ms / 1000).toFixed(2) }s` : "—";
        const ttft = row.first_token_ms ? `首字 ${(row.first_token_ms / 1000).toFixed(2)}s` : "";
        const errNote = !ok && row.error ? `<div class="muted" style="font-size:11px;max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${esc(row.error)}">${esc(row.error)}</div>` : "";
        const routerMeta = renderUserLogRouterMeta(row);
        return `<tr>
          <td><code>${esc(fmtBeijingTime(row.created_at))}</code></td>
          <td>${modelLabelHTML(row.model)}${routerMeta}</td>
          <td><code>${esc(row.endpoint || "/v1/chat/completions")}</code></td>
          <td><code>${fmtInt(row.prompt_tokens)}</code> / <code>${fmtInt(row.completion_tokens)}</code></td>
          <td><code>${esc(latency)}</code>${ttft ? `<div class="muted" style="font-size:11px">${esc(ttft)}</div>` : ""}</td>
          <td><code>${fmtUSD(row.cost_usd)}</code></td>
          <td><span class="http-badge ${statusCls}">${esc(row.status || 0)}</span>${errNote}</td>
        </tr>`;
      }).join("");
    } catch {
      tbody.innerHTML = `<tr><td colspan="7" class="empty-cell">加载调用记录失败</td></tr>`;
    }
  }

  async function rotateUserKey() {
    const btn = $("#confirmRotateBtn");
    if (btn) btn.disabled = true;
    try {
      const res = await fetch("/api/user/key/regenerate", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-APIShare-Request": "user-console",
        },
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        toast(data.error || "轮换密钥失败");
        return;
      }
      $("#rotateDialog")?.close();
      state.keyVisible = true;
      await loadDashboard();
      toast("已生成新的专属密钥，请及时更新客户端配置");
    } catch {
      toast("网络异常，轮换密钥失败");
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  async function logoutUser() {
    try {
      await fetch("/api/auth/logout", {
        method: "POST",
        headers: { "X-APIShare-Request": "user-console" },
      });
    } finally {
      window.location.href = "/";
    }
  }

  // ---------- BYOK & Personal Auto Router ----------

  async function loadUserRouterAndByok() {
    try {
      const res = await fetch("/api/user/auto-router", { cache: "no-store" });
      if (!res.ok) return;
      const data = await res.json();
      applyUserRouterView(data);
    } catch {
      // ignore background error
    }
  }

  function applyUserRouterView(data) {
    state.routerView = data;
    state.routerPool = [...new Set([...(data.model_pool || []), ...Object.values(data.tiers || {}).filter(Array.isArray).flat()])]
      .filter((m) => typeof m === "string" && m.trim()).sort();
    state.routerExcluded = [...(data.excluded_models || [])];
    renderUserByokList();
    renderUserRouterPanel();
  }

  function renderUserByokList() {
    const host = $("#userByokList");
    const countEl = $("#userByokCount");
    const items = Array.isArray(state.routerView?.upstreams) ? state.routerView.upstreams : [];
    if (countEl) countEl.textContent = String(items.length);
    if (!host) return;

    if (!items.length) {
      host.innerHTML = `<div class="empty-cell">尚未添加个人渠道，点击右上角「+ 添加渠道」接入自带 API Key（调用自带渠道不消耗站点额度）。</div>`;
      return;
    }

    host.innerHTML = items.map((u) => {
      const statusBadge = !u.enabled
        ? `<span class="status-pill off">已停用</span>`
        : u.healthy
        ? `<span class="status-pill on">健康</span>`
        : `<span class="status-pill off">冷却中</span>`;
      let proxyLabel = "系统默认";
      if (u.proxy_mode === "custom") {
        proxyLabel = `自定义代理 (${u.proxy_url || "已配置"})`;
      }
      const modelsHTML = (Array.isArray(u.models) ? u.models : []).slice(0, 8).map((m) => `<span class="byok-model-chip">${esc(m)}</span>`).join("");
      const moreCount = Math.max(0, (u.models?.length || 0) - 8);
      return `<div class="byok-item" data-byok-id="${u.id}">
        <div class="byok-item-top">
          <div class="byok-item-title">
            <span class="byok-type-badge">${esc(u.type || "openai")}</span>
            <span>${esc(u.name)}</span>
            ${statusBadge}
          </div>
          <div class="byok-item-actions">
            <button type="button" class="btn ghost mini" data-action="toggle-byok" data-id="${u.id}">${u.enabled ? "停用" : "启用"}</button>
            <button type="button" class="btn ghost mini" data-action="edit-byok" data-id="${u.id}">编辑</button>
            <button type="button" class="btn ghost mini danger-text" data-action="delete-byok" data-id="${u.id}">删除</button>
          </div>
        </div>
        <div class="byok-item-meta">
          <span>地址: <code>${esc(u.base_url)}</code></span>
          <span>密钥: <code>${esc(u.key_masked || "sk-***")}</code></span>
          <span>代理: <code>${esc(proxyLabel)}</code></span>
          <span>优先级: <strong>${esc(u.priority)}</strong> · 权重: <strong>${esc(u.weight)}</strong></span>
        </div>
        <div class="byok-models-wrap">
          ${modelsHTML}
          ${moreCount > 0 ? `<span class="byok-model-chip">+${moreCount} 个模型</span>` : ""}
        </div>
      </div>`;
    }).join("");

    $$("[data-action='toggle-byok']", host).forEach((btn) => {
      btn.addEventListener("click", () => toggleByokUpstream(Number(btn.dataset.id)));
    });
    $$("[data-action='edit-byok']", host).forEach((btn) => {
      btn.addEventListener("click", () => openByokModal(Number(btn.dataset.id)));
    });
    $$("[data-action='delete-byok']", host).forEach((btn) => {
      btn.addEventListener("click", () => deleteByokUpstream(Number(btn.dataset.id)));
    });
  }

  function openByokModal(editId = 0) {
    const dlg = $("#userUpstreamDialog");
    if (!dlg) return;
    const items = Array.isArray(state.routerView?.upstreams) ? state.routerView.upstreams : [];
    const target = editId > 0 ? items.find((x) => x.id === editId) : null;

    if ($("#userUpstreamDialogTitle")) {
      $("#userUpstreamDialogTitle").textContent = target ? `编辑个人渠道 · ${target.name}` : "添加个人自带渠道 (BYOK)";
    }
    if ($("#byokEditId")) $("#byokEditId").value = target ? String(target.id) : "0";
    if ($("#byokName")) $("#byokName").value = target ? target.name : "";
    if ($("#byokType")) $("#byokType").value = target ? target.type : "openai";
    if ($("#byokBaseURL")) $("#byokBaseURL").value = target ? target.base_url : "";
    if ($("#byokApiKey")) $("#byokApiKey").value = "";
    if ($("#byokKeyHint")) {
      $("#byokKeyHint").textContent = target
        ? `已保存密钥 (${target.key_masked || "sk-***"})；留空则保留原密钥`
        : "填写该渠道的 API 密钥（仅加密/掩码存储）";
    }
    if ($("#byokModels")) $("#byokModels").value = target ? (target.models || []).join(", ") : "";
    if ($("#byokPriority")) $("#byokPriority").value = target ? String(target.priority) : "10";
    if ($("#byokWeight")) $("#byokWeight").value = target ? String(target.weight) : "1";
    if ($("#byokModelMap")) {
      $("#byokModelMap").value = target && target.model_map && target.model_map !== "{}" ? target.model_map : "";
    }
    if ($("#byokProxyMode")) $("#byokProxyMode").value = target?.proxy_mode === "custom" ? "custom" : "default";
    if ($("#byokProxyURL")) $("#byokProxyURL").value = target?.proxy_url || "";
    if ($("#byokProxyUsername")) $("#byokProxyUsername").value = target?.proxy_username || "";
    if ($("#byokProxyPassword")) $("#byokProxyPassword").value = "";
    if ($("#byokClearProxyPassword")) $("#byokClearProxyPassword").checked = false;
    if ($("#byokClearProxyPassRow")) {
      $("#byokClearProxyPassRow").classList.toggle("hidden", !target?.proxy_password_saved);
    }
    if ($("#byokProxyPassHint")) {
      $("#byokProxyPassHint").textContent = target?.proxy_password_saved
        ? "已保存代理密码；留空则保留原密码"
        : "";
    }
    if ($("#byokProxyDefaultHint")) {
      $("#byokProxyDefaultHint").textContent = "「系统默认」自动跟随管理员设置的默认出口（直连或站点绑定代理）；若选择「自定义代理」，则由服务器直接连接你提供的代理，不会再经过管理员配置的代理。";
    }
    syncByokTypeHints(false);
    syncByokProxyModeVisibility();
    if ($("#byokEnabled")) $("#byokEnabled").checked = target ? Boolean(target.enabled) : true;
    $("#byokProbeResult")?.classList.add("hidden");
    dlg.showModal();
  }

  function syncByokTypeHints(autoFillModels = true) {
    const type = $("#byokType")?.value || "openai";
    const baseInput = $("#byokBaseURL");
    const modelsInput = $("#byokModels");
    if (baseInput) {
      if (type === "openrouter") {
        baseInput.placeholder = "例如：https://openrouter.ai/api/alpha/decisions";
        if (autoFillModels && !baseInput.value.trim()) {
          baseInput.value = "https://openrouter.ai/api/alpha/decisions";
        }
      } else if (type === "systemone") {
        baseInput.placeholder = "例如：https://api.typesafe.ai/v1/systemone";
      } else if (type === "cloudflare") {
        baseInput.placeholder = "例如：https://api.cloudflare.com/client/v4/accounts/<ACCOUNT_ID>/ai/run";
      } else if (type === "anthropic") {
        baseInput.placeholder = "例如：https://api.anthropic.com";
      } else if (type === "gemini") {
        baseInput.placeholder = "例如：https://generativelanguage.googleapis.com";
      } else {
        baseInput.placeholder = "例如：https://api.deepseek.com 或 https://api.openai.com";
      }
    }
    if (modelsInput) {
      if (type === "openrouter") {
        modelsInput.placeholder = "默认自动绑定：parallel/decisions-fast:free, parallel/decisions-fast（也可留空）";
        if (autoFillModels && !modelsInput.value.trim()) {
          modelsInput.value = "parallel/decisions-fast:free, parallel/decisions-fast";
        }
      } else if (type === "systemone") {
        modelsInput.placeholder = "默认自动绑定：jev, Julia-1, parallel/decisions-fast:free（也可留空）";
        if (autoFillModels && !modelsInput.value.trim()) {
          modelsInput.value = "jev, Julia-1";
        }
      } else if (type === "cloudflare") {
        modelsInput.placeholder = "默认自动绑定：@cf/cloudflare/clef-flash, @cf/cloudflare/clef（也可留空）";
        if (autoFillModels && !modelsInput.value.trim()) {
          modelsInput.value = "@cf/cloudflare/clef-flash, @cf/cloudflare/clef";
        }
      } else {
        modelsInput.placeholder = "例如：deepseek-chat, deepseek-reasoner, gpt-4o-mini";
      }
    }
  }

  function syncByokProxyModeVisibility() {
    const mode = $("#byokProxyMode")?.value || "default";
    $("#byokCustomProxyWrap")?.classList.toggle("hidden", mode !== "custom");
  }

  async function probeByokModels() {
    const btn = $("#probeByokBtn");
    const note = $("#byokProbeResult");
    const editId = Number($("#byokEditId")?.value || 0);
    const type = $("#byokType")?.value || "openai";
    const base_url = ($("#byokBaseURL")?.value || "").trim();
    const api_key = ($("#byokApiKey")?.value || "").trim();
    const proxy_mode = $("#byokProxyMode")?.value || "default";
    const proxy_url = ($("#byokProxyURL")?.value || "").trim();
    const proxy_username = ($("#byokProxyUsername")?.value || "").trim();
    const proxy_password = $("#byokProxyPassword")?.value || "";

    if (!base_url) {
      toast("请先填写渠道 Base URL");
      return;
    }
    if (btn) btn.disabled = true;
    if (note) {
      note.textContent = "正在连接渠道探测模型列表…";
      note.classList.remove("hidden");
    }
    try {
      const res = await fetch("/api/user/upstreams/probe", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-APIShare-Request": "user-console",
        },
        body: JSON.stringify({ id: editId, type, base_url, api_key, proxy_mode, proxy_url, proxy_username, proxy_password }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.ok) {
        if (note) note.textContent = `探测失败: ${data.error || "连接异常"}`;
        return;
      }
      const models = Array.isArray(data.models) ? data.models : [];
      if (note) note.textContent = `连接成功 (${data.latency_ms || 0} ms) · 共发现 ${models.length} 个模型，已填入下方列表`;
      if (models.length && $("#byokModels")) {
        $("#byokModels").value = models.join(", ");
      }
    } catch {
      if (note) note.textContent = "网络异常，探测失败";
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  async function saveByokUpstream(e) {
    if (e) e.preventDefault();
    const editId = Number($("#byokEditId")?.value || 0);
    const payload = {
      name: ($("#byokName")?.value || "").trim(),
      type: $("#byokType")?.value || "openai",
      base_url: ($("#byokBaseURL")?.value || "").trim(),
      api_key: ($("#byokApiKey")?.value || "").trim(),
      models: ($("#byokModels")?.value || "").trim(),
      priority: Number($("#byokPriority")?.value || 10),
      weight: Number($("#byokWeight")?.value || 1),
      model_map: ($("#byokModelMap")?.value || "").trim(),
      proxy_mode: $("#byokProxyMode")?.value || "default",
      proxy_url: ($("#byokProxyURL")?.value || "").trim(),
      proxy_username: ($("#byokProxyUsername")?.value || "").trim(),
      proxy_password: $("#byokProxyPassword")?.value || "",
      clear_proxy_password: Boolean($("#byokClearProxyPassword")?.checked),
      enabled: Boolean($("#byokEnabled")?.checked),
    };
    const btn = $("#saveByokBtn");
    if (btn) btn.disabled = true;
    try {
      const url = editId > 0 ? `/api/user/upstreams/${editId}` : "/api/user/upstreams";
      const method = editId > 0 ? "PUT" : "POST";
      const res = await fetch(url, {
        method,
        headers: {
          "Content-Type": "application/json",
          "X-APIShare-Request": "user-console",
        },
        body: JSON.stringify(payload),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        toast(data.error || "保存个人渠道失败");
        return;
      }
      $("#userUpstreamDialog")?.close();
      await loadUserRouterAndByok();
      toast(editId > 0 ? "个人渠道已更新" : "个人渠道已添加");
    } catch {
      toast("网络异常，保存渠道失败");
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  async function toggleByokUpstream(id) {
    const items = Array.isArray(state.routerView?.upstreams) ? state.routerView.upstreams : [];
    const target = items.find((x) => x.id === id);
    if (!target) return;
    try {
      const res = await fetch(`/api/user/upstreams/${id}`, {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          "X-APIShare-Request": "user-console",
        },
        body: JSON.stringify({ enabled: !target.enabled }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        toast(data.error || "更新状态失败");
        return;
      }
      await loadUserRouterAndByok();
      toast(!target.enabled ? "已启用渠道" : "已停用渠道");
    } catch {
      toast("网络异常，操作失败");
    }
  }

  async function deleteByokUpstream(id) {
    const items = Array.isArray(state.routerView?.upstreams) ? state.routerView.upstreams : [];
    const target = items.find((x) => x.id === id);
    if (!target) return;
    if (!window.confirm(`确认删除个人渠道「${target.name}」吗？`)) return;
    try {
      const res = await fetch(`/api/user/upstreams/${id}`, {
        method: "DELETE",
        headers: { "X-APIShare-Request": "user-console" },
      });
      if (!res.ok) {
        toast("删除个人渠道失败");
        return;
      }
      await loadUserRouterAndByok();
      toast("个人渠道已删除");
    } catch {
      toast("网络异常，删除失败");
    }
  }

  function renderUserRouterPanel() {
    const v = state.routerView;
    if (!v) return;

    const badge = $("#userRouterStatusBadge");
    if (badge) {
      if (!v.allow_user_auto_router) {
        badge.textContent = "站点已禁用";
        badge.className = "status-pill off";
      } else if (v.enabled) {
        badge.textContent = `运行中 · ${v.virtual_model || "my-auto"}`;
        badge.className = "status-pill on";
      } else {
        badge.textContent = "未启用";
        badge.className = "status-pill off";
      }
    }

    if ($("#userRouterEnabled")) $("#userRouterEnabled").checked = Boolean(v.enabled);
    if ($("#userRouterSpeculativeCascade")) $("#userRouterSpeculativeCascade").checked = v.speculative_cascade !== false;
    if ($("#userRouterVirtualModel")) $("#userRouterVirtualModel").value = v.virtual_model || "my-auto";
    if ($("#siteVirtualModelCode")) $("#siteVirtualModelCode").textContent = v.site_virtual_model || "auto";
    if ($("#userRouterStrategyProfile")) $("#userRouterStrategyProfile").value = v.strategy_profile || "balanced";
    if ($("#userRouterArenaMode")) $("#userRouterArenaMode").value = v.arena_mode || "live";
    if ($("#userRouterChannelPref")) $("#userRouterChannelPref").value = v.channel_preference || "personal_first";
    if ($("#userRouterDefaultModel")) $("#userRouterDefaultModel").value = v.default_model || "";

    // Populate shared classifier models dropdown
    const sharedSelect = $("#userSharedClassifierModel");
    const sharedList = Array.isArray(v.shared_classifier_models) ? v.shared_classifier_models : [];
    if (sharedSelect) {
      if (!sharedList.length) {
        sharedSelect.innerHTML = `<option value="">（站点暂未开放共享决策模型，请选择本地打分或自带渠道）</option>`;
      } else {
        sharedSelect.innerHTML = sharedList.map((item) => `<option value="${esc(item.id)}">${esc(item.label)}</option>`).join("");
        if (v.shared_classifier_model) sharedSelect.value = v.shared_classifier_model;
      }
    }

    // Reset upstream classifier channel to automatic selection (0)
    const upSelect = $("#userClassifierUpstreamSelect");
    if (upSelect) {
      upSelect.value = "0";
    }

    if ($("#userClassifierSource")) $("#userClassifierSource").value = v.classifier_source || "heuristic";
    if ($("#userUpstreamClassifierModel")) $("#userUpstreamClassifierModel").value = v.user_classifier_model || "";
    if ($("#userExternalClassifierProvider")) $("#userExternalClassifierProvider").value = v.user_classifier_provider || "cloudflare";
    if ($("#userExternalClassifierModel")) $("#userExternalClassifierModel").value = v.user_classifier_model || "";
    if ($("#userExternalCfAccount")) $("#userExternalCfAccount").value = v.user_cloudflare_account_id || "";
    if ($("#userExternalEndpoint")) $("#userExternalEndpoint").value = v.user_classifier_endpoint || "";
    if ($("#userExternalApiKey")) $("#userExternalApiKey").value = "";
    if ($("#userExternalKeyHint")) {
      $("#userExternalKeyHint").textContent = v.user_classifier_api_key_saved
        ? `已保存外部决策密钥 (${v.user_classifier_api_key_masked || "***"})；留空保留原密钥`
        : "仅用于你的私人路由任务分析请求";
    }

    // Populate datalists & bound model selects
    const personalModels = Array.isArray(v.personal_models) ? v.personal_models : [];
    const siteModels = Array.isArray(v.site_models) ? v.site_models : [];
    const allModels = Array.from(new Set([...personalModels, ...siteModels])).sort();
    if ($("#userPersonalModelsList")) {
      $("#userPersonalModelsList").innerHTML = allModels.map((m) => `<option value="${esc(m)}"></option>`).join("");
    }
    if ($("#userAllCandidateModelsList")) {
      $("#userAllCandidateModelsList").innerHTML = allModels.map((m) => `<option value="${esc(m)}"></option>`).join("");
    }
    populateUserDefaultModelSelect();
    populateUserUpstreamClassifierModelSelect();

    syncUserClassifierVisibility();
    if ($("#userRouterModelPool")) $("#userRouterModelPool").value = state.routerPool.join("\n");
    if ($("#userRouterExcludedModels")) $("#userRouterExcludedModels").value = state.routerExcluded.join("\n");
    if ($("#userRouterModelOrder")) {
      $("#userRouterModelOrder").value = (Array.isArray(state.routerView?.model_order) ? state.routerView.model_order : []).join("\n");
    }
    renderUserPoolEditor();
  }

  function collectUserAvailableModelList() {
    const v = state.routerView || {};
    const personalModels = Array.isArray(v.personal_models) ? v.personal_models : [];
    const siteModels = Array.isArray(v.site_models) ? v.site_models : [];
    return Array.from(new Set([...personalModels, ...siteModels])).filter(Boolean).sort();
  }

  function populateUserDefaultModelSelect() {
    const sel = $("#userRouterDefaultModelSelect");
    if (!sel) return;
    const allModels = collectUserAvailableModelList();
    const current = ($("#userRouterDefaultModel")?.value || "").trim();
    if (!allModels.length) {
      sel.innerHTML = `<option value="">（当前无可用模型）</option>`
        + (current ? `<option value="${esc(current)}" selected>${esc(current)}</option>` : "");
      sel.value = current;
      return;
    }
    let html = `<option value="">选择可用模型作为兜底模型…</option>`
      + allModels.map((m) => `<option value="${esc(m)}"${current === m ? " selected" : ""}>${esc(m)}</option>`).join("");
    if (current && !allModels.includes(current)) {
      html += `<option value="${esc(current)}" selected>${esc(current)}</option>`;
    }
    sel.innerHTML = html;
    sel.value = current;
  }

  function populateUserUpstreamClassifierModelSelect() {
    const sel = $("#userUpstreamClassifierModelSelect");
    if (!sel) return;
    const allModels = collectUserAvailableModelList();
    const current = ($("#userUpstreamClassifierModel")?.value || "").trim();
    if (!allModels.length) {
      sel.innerHTML = `<option value="">（当前无可用模型）</option>`
        + (current ? `<option value="${esc(current)}" selected>${esc(current)}</option>` : "");
      sel.value = current;
      return;
    }
    let html = `<option value="">选择可用模型作为决策模型…</option>`
      + allModels.map((m) => `<option value="${esc(m)}"${current === m ? " selected" : ""}>${esc(m)}</option>`).join("");
    if (current && !allModels.includes(current)) {
      html += `<option value="${esc(current)}" selected>${esc(current)}</option>`;
    }
    sel.innerHTML = html;
    sel.value = current;
  }

  function syncUserClassifierVisibility() {
    const src = $("#userClassifierSource")?.value || "heuristic";
    $("#userSharedClassifierWrap")?.classList.toggle("hidden", src !== "gateway_shared");
    $("#userUpstreamClassifierWrap")?.classList.toggle("hidden", src !== "user_upstream");
    $("#userExternalClassifierWrap")?.classList.toggle("hidden", src !== "user_external");
    const extProvider = $("#userExternalClassifierProvider")?.value || "cloudflare";
    $("#userExternalCfAccountField")?.classList.toggle("hidden", extProvider !== "cloudflare");
    const modelInput = $("#userExternalClassifierModel");
    const endpointInput = $("#userExternalEndpoint");
    if (extProvider === "openrouter") {
      if (modelInput) {
        modelInput.placeholder = "parallel/decisions-fast:free";
        if (!modelInput.value.trim() || modelInput.value.startsWith("@cf/")) {
          modelInput.value = "parallel/decisions-fast:free";
        }
      }
      if (endpointInput) {
        endpointInput.placeholder = "https://openrouter.ai/api/alpha/decisions (可留空自动使用)";
      }
    } else if (extProvider === "cloudflare") {
      if (modelInput) modelInput.placeholder = "@cf/cloudflare/clef-flash";
      if (endpointInput) endpointInput.placeholder = "留空自动拼接 Cloudflare Workers AI 地址";
    } else if (extProvider === "jev" || extProvider === "systemone") {
      if (modelInput) modelInput.placeholder = "jev 或 Julia-1";
      if (endpointInput) endpointInput.placeholder = "https://api.typesafe.ai/v1/systemone";
    } else {
      if (modelInput) modelInput.placeholder = "例如：decision-model-preview";
      if (endpointInput) endpointInput.placeholder = "https://.../v1/chat/completions";
    }
  }

  function parseUserCandidateSpec(raw) {
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

  function formatUserCtxLimit(tokens) {
    const n = Number(tokens || 0);
    if (n <= 0) return "";
    if (n >= 1000000) return (n / 1000000).toFixed(n % 1000000 === 0 ? 0 : 1) + "M";
    if (n >= 1000) return Math.round(n / 1000) + "K";
    return String(n);
  }

  function sourceBadgeForCandidate(model) {
    const v = state.routerView;
    const spec = parseUserCandidateSpec(model);
    const st = v?.candidate_status?.[model] || v?.candidate_status?.[spec.base];
    const personalSet = new Set(v?.personal_models || []);
    const siteSet = new Set(v?.site_models || []);
    const inByok = st ? st.in_byok : (personalSet.has(model) || personalSet.has(spec.base));
    const inSite = st ? st.in_site : (siteSet.has(model) || siteSet.has(spec.base));
    const srcLabels = {
      spec: "候选内联指定",
      config: "站点管理员配置",
      learned: "上游错误响应自动学习",
      probed: "上游 /v1/models 自动探测",
      catalog: "LiteLLM 官方目录同步",
      builtin: "内置模型族识别",
    };
    const ctxSrc = srcLabels[st?.context_source] || "自动识别";
    const ctxTag = st?.context_limit ? `<span class="user-source-pill site" title="模型上下文窗口：${Number(st.context_limit).toLocaleString()} tokens（来源：${ctxSrc}）">${formatUserCtxLimit(st.context_limit)} ctx</span>` : "";

    if (inByok && inSite) {
      return `${ctxTag}<span class="user-source-pill both" title="同时存在于你的自带渠道与站点授权模型">自带+站点双备</span>`;
    }
    if (inByok) {
      return `${ctxTag}<span class="user-source-pill byok" title="来自你的自带渠道，调用不扣站点额度">我的渠道 · 免额度</span>`;
    }
    if (inSite) {
      if (st?.site_exhausted) {
        return `${ctxTag}<span class="user-source-pill warn" title="站点该模型额度已耗尽(402)，路由时将自动跳过">站点 · 402耗尽</span>`;
      }
      return `${ctxTag}<span class="user-source-pill site" title="来自站点公共渠道，按密钥额度计费">站点模型</span>`;
    }
    return `${ctxTag}<span class="user-source-pill warn" title="当前未在自带渠道或密钥授权模型中找到">未绑定来源</span>`;
  }

  function splitUserPoolInput(raw) {
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

  // Same parsing, but keeps the operator's order (used by the manual priority list).
  function splitUserOrderInput(raw) {
    const models = []; let current = "", depth = 0;
    for (const ch of String(raw || "")) {
      if (ch === "(") depth++;
      else if (ch === ")" && depth > 0) depth--;
      if (depth === 0 && /[,\uFF0C\r\n]/.test(ch)) {
        if (current.trim()) models.push(current.trim());
        current = "";
      } else current += ch;
    }
    if (current.trim()) models.push(current.trim());
    const seen = new Set();
    return models.filter((m) => {
      const key = m.toLowerCase();
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  }

  function renderUserPoolEditor() {
    const host = $("#userModelPoolPreview");
    if (!host) return;
    const all = state.routerPool.length ? state.routerPool : collectUserAvailableModelList();
    const excluded = new Set(state.routerExcluded.map((m) => m.toLowerCase()));
    const models = all.filter((m) => !excluded.has(m.toLowerCase()) && !excluded.has(parseUserCandidateSpec(m).base.toLowerCase()));
    host.innerHTML = `<p class="muted">统一候选池 · ${models.length} 个模型参与 · 不设手动档位或模型优先级</p>`
      + models.map((m) => `<div class="user-cand-row"><span class="user-cand-name">${esc(m)}</span>${sourceBadgeForCandidate(m)}</div>`).join("");
  }

  function collectUserRouterPayload() {
    const src = $("#userClassifierSource")?.value || "heuristic";
    const classifierModel = src === "user_upstream"
      ? ($("#userUpstreamClassifierModel")?.value || "").trim()
      : ($("#userExternalClassifierModel")?.value || "").trim();

    return {
      enabled: Boolean($("#userRouterEnabled")?.checked),
      virtual_model: ($("#userRouterVirtualModel")?.value || "my-auto").trim(),
      strategy_profile: $("#userRouterStrategyProfile")?.value || "balanced",
      arena_mode: $("#userRouterArenaMode")?.value || state.routerView?.arena_mode || "live",
      speculative_cascade: $("#userRouterSpeculativeCascade") ? Boolean($("#userRouterSpeculativeCascade").checked) : true,
      channel_preference: $("#userRouterChannelPref")?.value || "personal_first",
      default_model: ($("#userRouterDefaultModel")?.value || "").trim(),
      classifier_source: src,
      shared_classifier_model: $("#userSharedClassifierModel")?.value || "",
      user_classifier_upstream: Number($("#userClassifierUpstreamSelect")?.value || 0),
      user_classifier_provider: $("#userExternalClassifierProvider")?.value || "cloudflare",
      user_classifier_model: classifierModel,
      user_cloudflare_account_id: ($("#userExternalCfAccount")?.value || "").trim(),
      user_classifier_endpoint: ($("#userExternalEndpoint")?.value || "").trim(),
      user_classifier_api_key: ($("#userExternalApiKey")?.value || "").trim(),
      model_pool: splitUserPoolInput($("#userRouterModelPool")?.value || ""),
      model_order: splitUserOrderInput($("#userRouterModelOrder")?.value || ""),
      excluded_models: splitUserPoolInput($("#userRouterExcludedModels")?.value || ""),
      tiers: {},
      cascade_tiers: false,
    };
  }

  async function saveUserRouter() {
    const btn = $("#saveUserRouterBtn");
    const payload = collectUserRouterPayload();
    const siteVM = state.routerView?.site_virtual_model || "auto";
    if (payload.virtual_model.toLowerCase() === siteVM.toLowerCase() || payload.virtual_model.toLowerCase() === "auto") {
      toast(`私人路由模型名不能与站点公共模型 (${siteVM}) 同名，请修改为其它名称`);
      return;
    }
    if (btn) btn.disabled = true;
    try {
      const res = await fetch("/api/user/auto-router", {
        method: "PUT",
        headers: {
          "Content-Type": "application/json",
          "X-APIShare-Request": "user-console",
        },
        body: JSON.stringify(payload),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        toast(data.error || "保存私人路由策略失败");
        return;
      }
      applyUserRouterView(data);
      toast(`私人智能路由 (${data.virtual_model}) 已保存生效`);
    } catch {
      toast("网络异常，保存私人路由策略失败");
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  async function runUserDryRun() {
    const prompt = ($("#userDryRunPrompt")?.value || "").trim();
    const out = $("#userDryRunResult");
    const btn = $("#userDryRunBtn");
    if (!prompt) {
      toast("请输入要测试的提问内容");
      return;
    }
    if (btn) btn.disabled = true;
    if (out) {
      out.textContent = "正在试算任务分析与选模链路…";
      out.classList.remove("hidden");
    }
    try {
      const override = collectUserRouterPayload();
      const res = await fetch("/api/user/auto-router/dry-run", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-APIShare-Request": "user-console",
        },
        body: JSON.stringify({ prompt, override }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        if (out) out.textContent = `试算失败: ${data.error || "参数错误"}`;
        return;
      }
      const dec = data.decision || {};
      const steps = Array.isArray(dec.steps) ? dec.steps : [];
      const stepsHTML = steps.length
        ? steps.map((s) => {
            const stText = s.status === "selected"
              ? `<strong style="color:var(--green)">✓ 选中 #${s.priority} (${esc(s.upstream_name || "")})</strong>`
              : s.status === "standby"
              ? `<span class="muted">顺位备选 #${s.priority} (${esc(s.upstream_name || "")})</span>`
              : `<span style="color:var(--red)">跳过 #${s.priority} (${esc(s.reason || "不可用")})</span>`;
            const utilText = s.utility_score ? ` <small class="muted">[旧策略 U=${Number(s.utility_score).toFixed(1)}]</small>` : "";
            const arenaText = s.arena ? ` <small class="muted" title="${esc(s.arena.version_reference_note || "")}">[Arena ${
              s.arena.score != null && ["matched", "version_reference"].includes(s.arena.status)
                ? (s.arena.version_reference ? "旧版参考／新版推断 · " + esc(s.arena.source_model || "") + " · " : "")
                  + "#" + s.arena.rank + " · " + Number(s.arena.score).toFixed(dec.arena?.board === "agent" ? 4 : 2)
                : esc(s.arena.status)}]</small>` : "";
            return `<div style="display:flex;justify-content:space-between;gap:8px;padding:3px 0;border-bottom:1px dashed var(--border)">
              <span><code>[${esc(s.source_tier)}]</code> <strong>${esc(s.model)}</strong>${utilText}${arenaText}</span>
              ${stText}
            </div>`;
          }).join("")
        : `<div class="muted">候选池中未找到可用模型${data.quota_error ? ` (${esc(data.quota_error)})` : ""}</div>`;

      if (out) {
        const domHint = dec.primary_domain ? ` · 领域: <code>${esc(dec.primary_domain)}</code>` : "";
        const diffHint = dec.required_difficulty ? ` · D(x)=<code>${Number(dec.required_difficulty).toFixed(0)}</code>` : "";
        out.innerHTML = `<div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:8px">
          <div>
            <span>判定难度: <span class="user-tier-badge ${esc(dec.tier || "DEFAULT")}">${esc(dec.tier || "DEFAULT")}</span>${domHint}${diffHint}</span>
            <span style="margin-left:8px">最终命中模型: <strong><code>${esc(dec.selected_model || "无可用模型")}</code></strong></span>
            ${dec.selected_thinking ? `<span style="margin-left:6px" class="muted">(思考: <code>${esc(dec.selected_thinking)}</code>)</span>` : ""}
          </div>
          <span class="muted">分类器: <code>${esc(dec.cause || "heuristic")}</code></span>
        </div>
        ${dec.arena ? `<p class="muted">Arena ${dec.arena.mode === "live" ? "正式启用" : "观察（不改变实际转发）"} · ${esc(dec.arena.board || "无分榜")} · 源榜 ${esc(dec.arena.published_at || "未知")} · 建议 ${esc(dec.arena.proposed_model || "—")} · 实际 ${esc(dec.arena.actual_model || dec.selected_model || "—")}${dec.arena.general_reference_only ? " · 通用弱参考，不代表专项能力" : ""} · ${esc(dec.arena.reason || "")}</p>` : ""}
        <div>${stepsHTML}</div>`;
      }
    } catch {
      if (out) out.textContent = "网络异常，试算失败";
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function copyUserRouterCommand() {
    const vm = ($("#userRouterVirtualModel")?.value || state.routerView?.virtual_model || "my-auto").trim();
    const keyText = state.dash?.key?.key || "sk-YOUR_API_KEY";
    const origin = window.location.origin;
    const cmd = [
      `curl ${origin}/v1/chat/completions \\`,
      `  -H "Authorization: Bearer ${keyText}" \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -d '{"model":"${vm}","messages":[{"role":"user","content":"Hello!"}]}'`,
    ].join("\n");
    copyText(cmd, `已复制私人路由模型 (${vm}) 调用示例`);
  }

  function bindEvents() {
    $("#toggleKeyBtn")?.addEventListener("click", () => {
      state.keyVisible = !state.keyVisible;
      renderKeyCard();
    });
    $("#copyKeyBtn")?.addEventListener("click", () => {
      if (state.dash?.key?.key) {
        copyText(state.dash.key.key, "专属 API 密钥已复制");
      }
    });
    $("#copySnippetBtn")?.addEventListener("click", () => {
      const code = $("#snippetCode")?.textContent || "";
      if (code) copyText(code, "接入配置代码已复制");
    });

    $$("[data-snippet]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.snippetKind = btn.dataset.snippet || "openai";
        $$("[data-snippet]").forEach((b) => b.setAttribute("aria-pressed", b === btn ? "true" : "false"));
        renderSnippet();
      });
    });

    $$("[data-days]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.trendDays = Number(btn.dataset.days) || 7;
        $$("[data-days]").forEach((b) => b.setAttribute("aria-pressed", b === btn ? "true" : "false"));
        renderTrendChart();
      });
    });

    $$("[data-metric]").forEach((btn) => {
      btn.addEventListener("click", () => {
        state.trendMetric = btn.dataset.metric || "cost";
        $$("[data-metric]").forEach((b) => b.setAttribute("aria-pressed", b === btn ? "true" : "false"));
        renderTrendChart();
      });
    });

    const rotateDialog = $("#rotateDialog");
    $("#rotateKeyBtn")?.addEventListener("click", () => rotateDialog?.showModal());
    $("#closeRotateDialog")?.addEventListener("click", () => rotateDialog?.close());
    $("#cancelRotateBtn")?.addEventListener("click", () => rotateDialog?.close());
    $("#confirmRotateBtn")?.addEventListener("click", rotateUserKey);

    $("#refreshBtn")?.addEventListener("click", async () => {
      const ok = await loadDashboard();
      if (ok) {
        await Promise.all([loadUserLogs(), loadUserRouterAndByok()]);
      }
      toast("控制台数据已刷新");
    });

    $("#logoutBtn")?.addEventListener("click", logoutUser);

    // BYOK & Personal Router events
    $("#addByokBtn")?.addEventListener("click", () => openByokModal(0));
    $("#closeUserUpstreamDialog")?.addEventListener("click", () => $("#userUpstreamDialog")?.close());
    $("#cancelByokBtn")?.addEventListener("click", () => $("#userUpstreamDialog")?.close());
    $("#byokType")?.addEventListener("change", () => syncByokTypeHints(true));
    $("#byokProxyMode")?.addEventListener("change", syncByokProxyModeVisibility);
    $("#probeByokBtn")?.addEventListener("click", probeByokModels);
    $("#userUpstreamForm")?.addEventListener("submit", saveByokUpstream);
    $("#userClassifierSource")?.addEventListener("change", syncUserClassifierVisibility);
    $("#userExternalClassifierProvider")?.addEventListener("change", syncUserClassifierVisibility);
    $("#userRouterDefaultModelSelect")?.addEventListener("change", (e) => {
      if (e.target.value && $("#userRouterDefaultModel")) {
        $("#userRouterDefaultModel").value = e.target.value;
      }
    });
    $("#userRouterDefaultModel")?.addEventListener("input", () => populateUserDefaultModelSelect());
    $("#userClassifierUpstreamSelect")?.addEventListener("change", () => {
      populateUserUpstreamClassifierModelSelect();
      const sel = $("#userUpstreamClassifierModelSelect");
      const inp = $("#userUpstreamClassifierModel");
      const selectedUpId = Number($("#userClassifierUpstreamSelect")?.value || 0);
      if (selectedUpId > 0 && inp) {
        const ups = Array.isArray(state.routerView?.upstreams) ? state.routerView.upstreams : [];
        const target = ups.find((u) => Number(u.id) === selectedUpId);
        const models = Array.isArray(target?.models) ? target.models : [];
        if (models.length && (!inp.value.trim() || !models.includes(inp.value.trim()))) {
          inp.value = models[0];
          if (sel) sel.value = models[0];
        }
      }
    });
    $("#userUpstreamClassifierModelSelect")?.addEventListener("change", (e) => {
      if (e.target.value && $("#userUpstreamClassifierModel")) {
        $("#userUpstreamClassifierModel").value = e.target.value;
      }
    });
    $("#userUpstreamClassifierModel")?.addEventListener("input", () => populateUserUpstreamClassifierModelSelect());
    $("#userRouterModelPool")?.addEventListener("input", (e) => {
      state.routerPool = splitUserPoolInput(e.target.value);
      renderUserPoolEditor();
    });
    $("#userRouterExcludedModels")?.addEventListener("input", (e) => {
      state.routerExcluded = splitUserPoolInput(e.target.value);
      renderUserPoolEditor();
    });
    $("#saveUserRouterBtn")?.addEventListener("click", saveUserRouter);
    $("#copyUserRouterCmdBtn")?.addEventListener("click", copyUserRouterCommand);
    $("#userDryRunBtn")?.addEventListener("click", runUserDryRun);
    $$("[data-user-sample]").forEach((btn) => {
      btn.addEventListener("click", () => {
        if ($("#userDryRunPrompt")) {
          $("#userDryRunPrompt").value = btn.dataset.userSample || "";
          runUserDryRun();
        }
      });
    });

    let searchTimer = 0;
    $("#userLogSearch")?.addEventListener("input", (e) => {
      window.clearTimeout(searchTimer);
      searchTimer = window.setTimeout(() => {
        state.logQuery = String(e.target.value || "").trim();
        state.logPage = 1;
        loadUserLogs();
      }, 260);
    });
    $("#userLogStatus")?.addEventListener("change", (e) => {
      state.logStatus = String(e.target.value || "");
      state.logPage = 1;
      loadUserLogs();
    });
    $("#userLogRefresh")?.addEventListener("click", () => loadUserLogs());
    $("#userLogPrev")?.addEventListener("click", () => {
      if (state.logPage > 1) {
        state.logPage -= 1;
        loadUserLogs();
      }
    });
    $("#userLogNext")?.addEventListener("click", () => {
      state.logPage += 1;
      loadUserLogs();
    });
  }

  async function init() {
    bindEvents();
    loadBranding();
    const ok = await loadDashboard();
    if (ok) {
      await Promise.all([loadUserLogs(), loadUserRouterAndByok()]);
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();

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
        return `<tr>
          <td><code>${esc(fmtBeijingTime(row.created_at))}</code></td>
          <td>${modelLabelHTML(row.model)}</td>
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
      if (ok) await loadUserLogs();
      toast("控制台数据已刷新");
    });

    $("#logoutBtn")?.addEventListener("click", logoutUser);

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
      await loadUserLogs();
    }
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init, { once: true });
  } else {
    init();
  }
})();

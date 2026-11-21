/* Dedicated model pricing management, USD / one million tokens. */
let pricingCatalog = null;
let pricingPage = 1;
const pricingPageSize = 50;
const pricingSourceNames = { override: "手动覆盖", official: "官方定价", builtin: "内置兜底", default: "默认兜底", none: "未匹配", upstream: "按上游计价" };
const pricingFields = [
  ["in", "输入", true], ["out", "输出", true],
  ["cache_read", "缓存读取"], ["cache_write", "缓存创建"], ["cache_write_1h", "1 小时缓存创建"],
  ["reasoning", "推理输出"], ["in_audio", "音频输入"], ["out_audio", "音频输出"], ["cache_read_audio", "音频缓存读取"],
];

function pricingModelName(model) {
  return /deepseek/i.test(model) && /flash/i.test(model) ? "deepseek-flash" : model;
}
function pricingRate(value) {
  return "$" + Number(value || 0).toLocaleString("en-US", { maximumSignificantDigits: 12 });
}
function renderPricingStatus(info) {
  $("pricingStatus").textContent = !info?.last_update ? "尚未同步官方定价" : "已同步 " + number(info.model_count) + " 个模型 · " + fmtTime(info.last_update);
  $("pricingSyncState").textContent = info?.enabled ? "自动同步已开启" : "自动同步已关闭";
}
async function loadPricing(preserveDrafts = false) {
  const version = requestVersions.pricing = (requestVersions.pricing || 0) + 1;
  const keepJSON = preserveDrafts && pricingCatalog && $("setPricing").value !== JSON.stringify(pricingCatalog.pricing || {}, null, 2);
  const keepSync = preserveDrafts && pricingCatalog && ($("setAutoPricing").checked !== !!pricingCatalog.auto_pricing.enabled || $("setPricingURL").value !== (pricingCatalog.auto_pricing.url || ""));
  try {
    const result = await api("api/settings/pricing");
    if (version !== requestVersions.pricing) return;
    pricingCatalog = result;
    if (!keepSync) {
      $("setAutoPricing").checked = !!result.auto_pricing.enabled;
      $("setPricingURL").value = result.auto_pricing.url || "";
    }
    if (!keepJSON) {
      $("setPricing").value = JSON.stringify(result.pricing || {}, null, 2);
      setPricingError("");
    }
    renderPricingStatus(result.auto_pricing);
    renderPricingTable();
    document.querySelectorAll("[data-pricing-ready]").forEach((button) => { button.disabled = false; });
    stampRefresh();
    return true;
  } catch (error) {
    if (!pricingCatalog) $("pricingTableBody").innerHTML = '<tr><td colspan="7" class="table-message">定价加载失败，请点击右上角刷新重试。</td></tr>';
    reportError(error, "定价加载失败");
    return false;
  }
}
function filterPricing() { pricingPage = 1; renderPricingTable(); }
function renderPricingTable() {
  if (!pricingCatalog) return;
  const query = $("pricingSearch").value.trim().toLowerCase();
  const source = $("pricingSourceFilter").value, scope = $("pricingScopeFilter").value;
  const all = pricingCatalog.models || [];
  const filtered = all.filter((row) => (!query || (row.model + " " + row.pricing_model).toLowerCase().includes(query)) && (!source || row.source === source) && (scope !== "connected" || row.connected));
  const pages = Math.max(1, Math.ceil(filtered.length / pricingPageSize));
  pricingPage = Math.min(pricingPage, pages);
  const start = (pricingPage - 1) * pricingPageSize;
  $("pricingTotal").textContent = number(filtered.length);
  $("pricingPageInfo").textContent = filtered.length ? (start + 1) + "–" + Math.min(start + pricingPageSize, filtered.length) + " / " + number(filtered.length) : "0 个模型";
  $("pricingPrevious").disabled = pricingPage <= 1;
  $("pricingNext").disabled = pricingPage >= pages;
  $("pricingConnectedCount").textContent = number(all.filter((row) => row.connected).length);
  $("pricingOverrideCount").textContent = number(Object.keys(pricingCatalog.pricing || {}).filter((model) => model !== "default").length);
  $("pricingOfficialCount").textContent = number(pricingCatalog.auto_pricing.model_count);
  $("pricingLoadingNote").classList.toggle("hidden", !pricingCatalog.models_loading);
  $("pricingTableBody").innerHTML = filtered.slice(start, start + pricingPageSize).map((row) => {
    const price = row.price, model = row.model;
    const name = model === "default" ? '<strong>默认兜底价格</strong><small>未匹配单价的模型使用此价格</small>' : modelLabel(model) + (row.connected ? '<small>本站已接入</small>' : "") + (row.pricing_model && row.pricing_model !== model && row.pricing_model !== "default" ? '<small>按 ' + esc(row.pricing_model) + ' 计价</small>' : "");
    const reset = row.override && model !== "default" ? '<button type="button" class="text-btn" data-pricing-reset="' + esc(model) + '">恢复自动</button>' : "";
    const rates = row.variants?.length ? ["依上游", "依上游", "依上游", "依上游"] : [price.in, price.out, price.cache_read ?? price.in, price.cache_write ?? price.in].map(pricingRate);
    return '<tr><td class="pricing-model-cell">' + name + '</td>' + rates.map((rate) => '<td class="pricing-rate">' + rate + '</td>').join("") + '<td><span class="pricing-source pricing-source-' + esc(row.source) + '">' + esc(pricingSourceNames[row.source] || row.source) + '</span></td><td><div class="pricing-row-actions"><button type="button" class="btn mini ghost" data-pricing-edit="' + esc(model) + '">' + icon("edit") + ' 编辑</button>' + reset + '</div></td></tr>';
  }).join("") || '<tr><td colspan="7" class="table-message">没有匹配的模型。可以调整筛选，或点击「添加定价」设置模型单价。</td></tr>';
}
function changePricingPage(delta) { pricingPage += delta; renderPricingTable(); }

async function lookupPrice() {
  const model = $("lookupModel").value.trim();
  if (!model) { $("lookupModel").focus(); return; }
  $("lookupResult").textContent = "正在查询…";
  try {
    const result = await api("api/settings/pricing/lookup?model=" + encodeURIComponent(model));
    const price = result.price;
    $("lookupResult").textContent = result.model + " · 输入 " + pricingRate(price.in) + " · 输出 " + pricingRate(price.out) + " · 缓存读取 " + pricingRate(price.cache_read ?? price.in) + " / 1M tokens · " + (pricingSourceNames[result.source] || result.source) + (result.pricing_model && result.pricing_model !== result.model ? " · 计价模型：" + result.pricing_model : "");
  } catch (error) { $("lookupResult").textContent = "查询失败：" + error.message; }
}

async function savePricingSync(event) {
  event.preventDefault();
  if (!pricingCatalog) return false;
  const button = event.currentTarget.querySelector('[type="submit"]');
  setBusy(button, true, "正在保存…");
  try {
    await api("api/settings", { method: "PUT", body: JSON.stringify({ auto_pricing_enabled: $("setAutoPricing").checked, auto_pricing_url: $("setPricingURL").value.trim() }) });
    await loadPricing(true);
    toast("定价同步设置已保存");
  } catch (error) { reportError(error, "保存同步设置失败"); }
  finally { setBusy(button, false); }
  return false;
}
async function refreshPricing(button) {
  setBusy(button, true, "正在同步…");
  $("pricingSyncError").textContent = "";
  try {
    const result = await api("api/settings/pricing/refresh", { method: "POST" });
    if (!result.ok) throw new Error(result.error || "同步失败");
    await loadPricing(true);
    toast("已同步 " + number(result.model_count) + " 个模型的官方定价");
  } catch (error) {
    $("pricingSyncError").textContent = "同步失败：" + error.message + "。已保留上次成功同步的价格，可稍后重试。";
    reportError(error, "定价同步失败");
  } finally { setBusy(button, false); }
}

function openPricingEditor(model = "") {
  if (!pricingCatalog) return;
  const canonical = pricingModelName(model);
  const row = pricingCatalog.models.find((item) => item.model === model);
  const price = pricingCatalog.pricing[canonical] || row?.variants?.[0]?.price || row?.price || { in: 0, out: 0 };
  $("pricingEditForm").reset();
  $("pricingEditModel").value = canonical;
  $("pricingEditModel").readOnly = !!model;
  $("pricingEditTitle").textContent = model === "default" ? "编辑默认兜底价格" : model ? "编辑模型价格" : "添加模型定价";
  $("pricingEditDescription").textContent = model === "default" ? "没有匹配到模型单价时使用此价格。" : "保存后立即生效，手动价格优先于自动同步和内置价格。";
  $("pricingVariantSummary").textContent = row?.variants?.length ? "此别名按实际路由到的上游模型计费：" + row.variants.map((variant) => variant.pricing_model + "（输入 " + pricingRate(variant.price.in) + "，输出 " + pricingRate(variant.price.out) + "）").join("；") + "。下方预填第一份价格，保存后将统一使用手动价格。" : "";
  for (const [field] of pricingFields) $("price_" + field).value = price[field] ?? "";
  $("pricingAdvancedRates").open = pricingFields.slice(2).some(([field]) => price[field] != null);
  $("pricingEditError").textContent = "";
  $("pricingFlashHint").classList.toggle("hidden", canonical === model && canonical !== "deepseek-flash");
  $("pricingEditDialog").showModal(); syncModalLock();
  (model ? $("price_in") : $("pricingEditModel")).focus();
}
function closePricingEditor() { $("pricingEditDialog").close(); syncModalLock(); }
async function savePricingModel(event) {
  event.preventDefault();
  const model = pricingModelName($("pricingEditModel").value.trim());
  if (!model) { $("pricingEditModel").focus(); return false; }
  const price = {};
  for (const [field, label, required] of pricingFields) {
    const input = $("price_" + field), value = input.value.trim();
    if (!value && !required) continue;
    const rate = Number(value);
    if (!value || !Number.isFinite(rate) || rate < 0) {
      $("pricingAdvancedRates").open = true;
      $("pricingEditError").textContent = label + "单价必须为有限的非负数。";
      input.focus(); return false;
    }
    price[field] = rate;
  }
  const button = event.currentTarget.querySelector('[type="submit"]');
  setBusy(button, true, "正在保存…");
  $("pricingEditError").textContent = "";
  try {
    await api("api/settings/pricing/model", { method: "PUT", body: JSON.stringify({ model, price }) });
    closePricingEditor();
    await loadPricing(true);
    toast(model + " 的价格已保存");
  } catch (error) { $("pricingEditError").textContent = "保存失败：" + error.message; }
  finally { setBusy(button, false); }
  return false;
}
async function resetPricingModel(model, button) {
  if (!await askAction({ title: "恢复自动定价", description: "删除 " + pricingModelName(model) + " 的手动覆盖后，将按当前自动同步、内置或默认兜底价格计费。", confirm: "恢复自动定价" })) return;
  setBusy(button, true, "正在恢复…");
  try {
    await api("api/settings/pricing/model?model=" + encodeURIComponent(model), { method: "DELETE" });
    await loadPricing(true);
    toast("已恢复自动定价");
  } catch (error) { reportError(error, "恢复定价失败"); }
  finally { setBusy(button, false); }
}

function validatePricingJSON(text) {
  const pricing = JSON.parse(text);
  const validRate = (value) => typeof value === "number" && Number.isFinite(value) && value >= 0;
  if (!pricing || Array.isArray(pricing) || typeof pricing !== "object" || !Object.keys(pricing).length) throw new Error("请填写非空的模型价格 JSON 对象。");
  const allowed = pricingFields.map(([field]) => field);
  for (const [model, price] of Object.entries(pricing)) {
    if (!model.trim() || model !== model.trim() || model.startsWith("bare:")) throw new Error("请使用有效的模型名称，名称前后不能有空格。");
    if (pricingModelName(model) !== model) throw new Error(model + "：DeepSeek Flash 系列请统一设置 deepseek-flash 的价格。");
    if (!price || Array.isArray(price) || typeof price !== "object" || !validRate(price.in) || !validRate(price.out)) throw new Error(model + "：in 和 out 必须是有限的非负单价。");
    for (const field of Object.keys(price)) {
      if (!allowed.includes(field)) throw new Error(model + "：不支持价格字段 " + field + "。");
      if (price[field] != null && !validRate(price[field])) throw new Error(model + "：" + field + " 必须是有限的非负单价。");
    }
  }
  return pricing;
}
function setPricingError(message) {
  $("pricingError").textContent = message;
  $("pricingError").classList.toggle("hidden", !message);
  $("setPricing").setCustomValidity(message);
  $("setPricing").setAttribute("aria-invalid", String(!!message));
}
async function savePricingJSON(event) {
  event.preventDefault();
  if (!pricingCatalog) return false;
  let pricing;
  try { pricing = validatePricingJSON($("setPricing").value); }
  catch (error) { setPricingError(error.message); $("setPricing").focus(); return false; }
  setPricingError("");
  const button = event.currentTarget.querySelector('[type="submit"]');
  setBusy(button, true, "正在保存…");
  try {
    await api("api/settings", { method: "PUT", body: JSON.stringify({ pricing }) });
    await loadPricing(true);
    $("setPricing").value = JSON.stringify(pricingCatalog.pricing, null, 2);
    toast("模型价格已批量保存");
  } catch (error) { setPricingError("保存失败：" + error.message); }
  finally { setBusy(button, false); }
  return false;
}

function initPricing() {
  $("pricingTableBody").addEventListener("click", (event) => {
    const edit = event.target.closest("[data-pricing-edit]"), reset = event.target.closest("[data-pricing-reset]");
    if (edit) openPricingEditor(edit.dataset.pricingEdit);
    if (reset) resetPricingModel(reset.dataset.pricingReset, reset);
  });
  $("pricingEditDialog").addEventListener("cancel", (event) => { event.preventDefault(); closePricingEditor(); });
  $("pricingEditModel").addEventListener("input", () => {
    const model = $("pricingEditModel").value.trim();
    $("pricingFlashHint").classList.toggle("hidden", pricingModelName(model) === model && model !== "deepseek-flash");
  });
  $("setPricing").addEventListener("input", () => setPricingError(""));
}

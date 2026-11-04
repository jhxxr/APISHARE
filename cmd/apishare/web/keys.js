/* Per-key model permissions, visual model chips, and batch model provisioning. */
const keyModelPickers = {};
const selectedKeyIDs = new Set();
let editingKeyModelsID = null, savingKeyModels = false, savingBatchKeyModels = false;

function initKeyModels() {
  for (const prefix of ["key", "keyEdit", "xauthDefault", "keyBatch"]) {
    const host = $(prefix + "ModelControl");
    if (!host) continue;
    keyModelPickers[prefix] = { models: [], quotas: new Map(), generation: 0, controller: null };
    const isBatch = prefix === "keyBatch";
    const isXAuthDefault = prefix === "xauthDefault";
    const labelTitle = isBatch
      ? '要追加的模型 <span class="required">*</span>'
      : isXAuthDefault
        ? '新用户默认可用模型 <span class="optional">可选</span>'
        : '模型白名单 <span class="optional">可选</span>';
    const quotaTitle = isBatch
      ? "新追加模型的额度方式"
      : isXAuthDefault
        ? "默认赠送额度配置"
        : "模型额度";
    const quotaDesc = isBatch
      ? "若目标密钥已绑定某模型，将自动跳过并保留该密钥原有的额度设置；此处仅对本次新加上去的模型生效。"
      : "共享额度由多个模型共同使用。独立额度仅供对应模型使用，用完后不会扣减共享额度。金额单位为 USD，0 表示不限。";
    const sharedLabel = isXAuthDefault ? "默认赠送共享额度" : "共享额度";
    host.innerHTML =
      '<div class="field"><div class="field-label-row"><label for="' + prefix + 'ModelInput">' + labelTitle + '</label><small id="' + prefix + 'ModelSummary" aria-live="polite">未绑定模型，无法调用</small></div>' +
      '<input id="' + prefix + 'Models" type="hidden">' +
      '<div id="' + prefix + 'ModelTagInput" class="model-tag-input"><div id="' + prefix + 'ModelChips" class="model-chips" aria-label="模型白名单"></div><input id="' + prefix + 'ModelInput" type="text" placeholder="输入模型名，按回车添加" spellcheck="false" autocomplete="off" list="' + prefix + 'ModelOptions" aria-describedby="' + prefix + 'ModelHint"></div>' +
      '<datalist id="' + prefix + 'ModelOptions"></datalist>' +
      '<small id="' + prefix + 'ModelHint">' + (isBatch ? "回车或逗号添加，支持粘贴多行模型；已有该模型的密钥会自动跳过，未有的自动加上。" : "回车或逗号添加，支持粘贴多行模型；点击 × 移除。模型别名需单独填写；留空表示不允许调用任何模型。") + '</small></div>' +
      '<div class="key-model-actions"><button type="button" class="btn ghost" id="' + prefix + 'LoadModels">获取可用模型</button><button type="button" class="text-btn" id="' + prefix + 'ClearModels">清空模型选择</button></div>' +
      '<p id="' + prefix + 'ModelStatus" class="probe-info" role="status"></p>' +
      '<div id="' + prefix + 'ModelPicker" class="model-picker hidden"><div class="model-picker-toolbar"><label class="search-field"><span class="sr-only">搜索可用模型</span><input id="' + prefix + 'ModelSearch" placeholder="搜索可用模型…"></label><button type="button" class="text-btn" id="' + prefix + 'SelectAllModels">全选当前筛选</button></div><div id="' + prefix + 'ModelList" class="probe-box"></div></div>' +
      '<section class="key-quota-control" aria-labelledby="' + prefix + 'QuotaHeading"><h3 id="' + prefix + 'QuotaHeading">' + quotaTitle + '</h3><p class="quota-hint">' + quotaDesc + '</p>' +
      '<label class="field key-shared-quota ' + (isBatch ? "hidden" : "") + '"><span>' + sharedLabel + ' <span class="field-unit">USD</span></span><input id="' + prefix + 'Quota" type="number" min="0" step="0.01" value="' + (isXAuthDefault ? "5" : "0") + '" ' + (isBatch ? "disabled" : "required") + ' aria-describedby="' + prefix + 'SharedUsage"><small id="' + prefix + 'SharedUsage">' + (isXAuthDefault ? "0 表示不限额度" : "已使用 $0.00") + '</small></label>' +
      '<div class="key-model-actions"><button type="button" class="text-btn" id="' + prefix + 'AllShared">全部使用共享额度</button><button type="button" class="text-btn" id="' + prefix + 'AllIndependent">全部独立设置</button></div>' +
      '<div id="' + prefix + 'QuotaRows" class="model-quota-rows"></div></section>';

    initKeyModelTagInput(prefix);
    $(prefix + "ModelSearch").addEventListener("input", () => renderKeyModelPicker(prefix));
    $(prefix + "LoadModels").addEventListener("click", () => loadKeyModelOptions(prefix));
    $(prefix + "SelectAllModels").addEventListener("click", () => {
      const state = keyModelPickers[prefix];
      const query = $(prefix + "ModelSearch").value.trim().toLowerCase();
      const current = new Set(parseAllowlist($(prefix + "Models").value));
      state.models.filter((m) => m.toLowerCase().includes(query)).forEach((m) => current.add(m));
      $(prefix + "Models").value = [...current].join(",");
      clearKeyModelError(prefix);
      renderKeyModelPicker(prefix);
    });
    $(prefix + "ClearModels").addEventListener("click", () => {
      $(prefix + "Models").value = "";
      $(prefix + "ModelInput").value = "";
      clearKeyModelError(prefix);
      renderKeyModelPicker(prefix);
      $(prefix + "ModelInput").focus();
    });
    $(prefix + "AllShared").addEventListener("click", () => setAllKeyQuotaModes(prefix, false));
    $(prefix + "AllIndependent").addEventListener("click", () => setAllKeyQuotaModes(prefix, true));
    renderKeyModelPicker(prefix);
  }
  $("keyModelsForm").addEventListener("submit", saveKeyModels);
  $("keyModelsDialog").addEventListener("cancel", (event) => { event.preventDefault(); closeKeyModels(); });
  if ($("keyBatchModelsForm")) {
    $("keyBatchModelsForm").addEventListener("submit", saveBatchAddKeyModels);
    $("keyBatchModelsDialog").addEventListener("cancel", (event) => { event.preventDefault(); closeBatchAddKeyModels(); });
  }
}

function resetKeyModelPicker(prefix, key = null) {
  const state = keyModelPickers[prefix];
  if (!state) return;
  state.generation++;
  state.controller?.abort();
  state.controller = null;
  state.models = [];
  state.quotas = new Map(Object.entries(key?.model_quotas || {}).map(([model, budget]) => [model, { independent: true, amount: String(budget.quota_usd ?? 0), used: budget.used_usd || 0 }]));
  if (prefix !== "keyBatch") {
    $(prefix + "Quota").value = key?.quota_usd ?? (prefix === "xauthDefault" ? 5 : 0);
    $(prefix + "SharedUsage").textContent = prefix === "xauthDefault"
      ? "新用户首次登录默认赠送的共享额度（0 表示不限）"
      : "已使用 " + fmtUSD(key?.shared_used_usd || 0) + " · 已用金额会保留";
  }
  setBusy($(prefix + "LoadModels"), false);
  $(prefix + "ModelPicker").classList.add("hidden");
  $(prefix + "ModelSearch").value = "";
  $(prefix + "ModelInput").value = "";
  $(prefix + "ModelOptions").replaceChildren();
  $(prefix + "ModelStatus").textContent = "";
  clearKeyModelError(prefix);
  renderKeyModelPicker(prefix);
}

function clearKeyModelError(prefix) {
  for (const id of [prefix + "Models", prefix + "ModelInput"]) {
    const field = $(id);
    if (!field) continue;
    field.setCustomValidity("");
    field.removeAttribute("aria-invalid");
  }
  $(prefix + "ModelsError")?.classList.add("hidden");
}

function keyModelSummary(prefix, selected) {
  if (prefix === "keyBatch") {
    return selected.size ? "待追加 " + selected.size + " 个模型（已有跳过）" : "请选择要追加的模型";
  }
  return selected.size ? "已添加 " + selected.size + " 个模型" : "未绑定模型，无法调用";
}

function commitKeyModelInput(prefix) {
  const field = $(prefix + "ModelInput");
  if (!field || !field.value.trim()) { if (field) field.value = ""; return; }
  const current = new Set(parseAllowlist($(prefix + "Models").value));
  parseAllowlist(field.value).forEach((model) => current.add(model));
  field.value = "";
  $(prefix + "Models").value = [...current].join(",");
  clearKeyModelError(prefix);
  renderKeyModelPicker(prefix);
}

function initKeyModelTagInput(prefix) {
  const field = $(prefix + "ModelInput");
  field.addEventListener("keydown", (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (["Enter", ",", "，"].includes(event.key) && !event.ctrlKey && !event.metaKey) {
      event.preventDefault();
      commitKeyModelInput(prefix);
    } else if (event.key === "Backspace" && !field.value) {
      const chips = $(prefix + "ModelChips");
      if (chips.lastElementChild) {
        event.preventDefault();
        chips.lastElementChild.querySelector("button").focus();
      }
    }
  });
  field.addEventListener("input", (event) => {
    if (!event.isComposing && /[,，\r\n]/.test(field.value)) commitKeyModelInput(prefix);
  });
  field.addEventListener("compositionend", () => { if (/[,，\r\n]/.test(field.value)) commitKeyModelInput(prefix); });
  field.addEventListener("paste", (event) => {
    const value = event.clipboardData?.getData("text");
    if (!value || !/[,，\r\n]/.test(value)) return;
    event.preventDefault();
    field.setRangeText(value.replace(/\r?\n/g, ","), field.selectionStart, field.selectionEnd, "end");
    commitKeyModelInput(prefix);
  });
  field.addEventListener("blur", (event) => {
    if (!event.relatedTarget?.closest("#" + prefix + "ModelChips")) commitKeyModelInput(prefix);
  });
  $(prefix + "ModelTagInput").addEventListener("click", (event) => {
    if (event.target === $(prefix + "ModelTagInput") || event.target === $(prefix + "ModelChips")) field.focus();
  });
}

function renderKeyModelChips(prefix) {
  const selected = parseAllowlist($(prefix + "Models").value);
  const box = $(prefix + "ModelChips");
  box.replaceChildren();
  selected.forEach((model) => {
    const chip = document.createElement("span");
    chip.className = "model-chip";
    const text = document.createElement("span");
    text.textContent = model;
    const button = document.createElement("button");
    button.type = "button";
    button.setAttribute("aria-label", "移除模型 " + model);
    button.innerHTML = icon("close");
    button.addEventListener("click", () => {
      commitKeyModelInput(prefix);
      const current = parseAllowlist($(prefix + "Models").value).filter((item) => item !== model);
      $(prefix + "Models").value = current.join(",");
      clearKeyModelError(prefix);
      renderKeyModelPicker(prefix);
      $(prefix + "ModelInput").focus();
    });
    const brand = document.createElement("span"); brand.innerHTML = modelIcon(model);
    chip.append(brand.firstElementChild, text, button);
    box.append(chip);
  });
  $(prefix + "ModelInput").placeholder = selected.length ? "继续添加模型…" : "输入模型名，按回车添加";
  $(prefix + "ModelSummary").textContent = keyModelSummary(prefix, new Set(selected));
  const allowed = new Set(selected);
  $(prefix + "ModelList").querySelectorAll("input[data-model]").forEach((checkbox) => { checkbox.checked = allowed.has(checkbox.dataset.model); });
}

function renderKeyModelPicker(prefix) {
  const state = keyModelPickers[prefix];
  if (!state) return;
  const selected = new Set(parseAllowlist($(prefix + "Models").value));
  renderKeyModelChips(prefix);
  const query = $(prefix + "ModelSearch").value.trim().toLowerCase();
  const models = state.models.filter((model) => model.toLowerCase().includes(query));
  $(prefix + "ModelList").innerHTML = models.map((model) => '<label><input type="checkbox" data-model="' + esc(model) + '" ' + (selected.has(model) ? "checked" : "") + '>' + modelLabel(model) + '</label>').join("") || '<span class="muted">没有匹配的模型</span>';
  $(prefix + "ModelList").querySelectorAll("input").forEach((checkbox) => checkbox.addEventListener("change", () => {
    const current = new Set(parseAllowlist($(prefix + "Models").value));
    if (checkbox.checked) current.add(checkbox.dataset.model); else current.delete(checkbox.dataset.model);
    $(prefix + "Models").value = [...current].join(",");
    clearKeyModelError(prefix);
    renderKeyModelChips(prefix);
    renderKeyQuotaRows(prefix);
  }));
  renderKeyQuotaRows(prefix);
}

function keyQuotaDraft(prefix, model) {
  const quotas = keyModelPickers[prefix].quotas;
  if (!quotas.has(model)) quotas.set(model, { independent: false, amount: "0", used: 0 });
  return quotas.get(model);
}

function renderKeyQuotaRows(prefix) {
  const models = parseAllowlist($(prefix + "Models").value);
  const sharedCount = models.filter((model) => !keyQuotaDraft(prefix, model).independent).length;
  const isBatch = prefix === "keyBatch";
  if (!isBatch) {
    $(prefix + "SharedUsage").closest("label").classList.toggle("hidden", models.length > 0 && sharedCount === 0);
    $(prefix + "Quota").disabled = models.length > 0 && sharedCount === 0;
  }
  $(prefix + "AllShared").disabled = $(prefix + "AllIndependent").disabled = !models.length;
  $(prefix + "QuotaRows").innerHTML = models.map((model, index) => {
    const budget = keyQuotaDraft(prefix, model), rowID = prefix + "Budget" + index;
    return '<div class="model-quota-row" data-model="' + esc(model) + '"><strong>' + modelLabel(model) + '</strong><label class="field"><span class="sr-only">' + esc(model) + ' 的额度方式</span><select class="model-quota-mode" aria-controls="' + rowID + '"><option value="shared" ' + (!budget.independent ? "selected" : "") + '>使用共享额度</option><option value="independent" ' + (budget.independent ? "selected" : "") + '>独立额度</option></select></label><label id="' + rowID + '" class="field model-quota-amount ' + (budget.independent ? "" : "hidden") + '"><span class="sr-only">' + esc(model) + ' 的独立额度（USD）</span><input id="' + rowID + 'Amount" type="number" min="0" step="0.01" value="' + esc(budget.amount) + '" required ' + (budget.independent ? "" : "disabled") + '><small>' + (isBatch || prefix === "xauthDefault" ? "0 表示不限额度" : "已使用 " + fmtUSD(budget.used) + " · 0 表示不限") + '</small></label></div>';
  }).join("") || '<p class="quota-hint">绑定模型后，可以逐个选择额度方式。</p>';
  $(prefix + "QuotaRows").querySelectorAll(".model-quota-row").forEach((row) => {
    const model = row.dataset.model, budget = keyQuotaDraft(prefix, model);
    row.querySelector("select").addEventListener("change", (event) => {
      budget.independent = event.target.value === "independent";
      renderKeyQuotaRows(prefix);
      $(prefix + "QuotaRows").querySelectorAll("select")[models.indexOf(model)].focus();
    });
    row.querySelector("input").addEventListener("input", (event) => { budget.amount = event.target.value; });
  });
}

function setAllKeyQuotaModes(prefix, independent) {
  parseAllowlist($(prefix + "Models").value).forEach((model) => { keyQuotaDraft(prefix, model).independent = independent; });
  renderKeyQuotaRows(prefix);
}

function keyQuotaPayload(prefix) {
  const modelQuotas = Object.fromEntries(parseAllowlist($(prefix + "Models").value)
    .filter((model) => keyQuotaDraft(prefix, model).independent)
    .map((model) => [model, { quota_usd: Number(keyQuotaDraft(prefix, model).amount) }]));
  if (prefix === "keyBatch") {
    return { model_quotas: modelQuotas };
  }
  return {
    quota_usd: Number($(prefix + "Quota").value),
    model_quotas: modelQuotas,
  };
}

function renderKeyModelChipItem(model, key) {
  const hasIndependent = Object.hasOwn(key.model_quotas || {}, model);
  const budget = hasIndependent ? key.model_quotas[model] : null;
  const badge = hasIndependent
    ? '<em class="key-chip-quota" title="独立额度：已用 ' + fmtUSD(budget.used_usd || 0) + ' / ' + (budget.quota_usd > 0 ? fmtUSD(budget.quota_usd) : "不限") + '">' + (budget.quota_usd > 0 ? fmtUSD(budget.quota_usd) : "独立不限") + '</em>'
    : "";
  return '<span class="key-table-chip" title="' + esc(model) + (hasIndependent ? "（独立额度）" : "（共享额度）") + '">' + modelIcon(model) + '<span>' + esc(model) + '</span>' + badge + '</span>';
}

function renderKeyModelsCell(key) {
  const models = parseAllowlist(key.models);
  if (!models.length) {
    return '<div class="key-models-cell"><span class="key-models-empty">未绑定模型 · 无法调用</span></div>';
  }
  const inlineLimit = 6;
  const visible = models.slice(0, inlineLimit);
  const rest = models.slice(inlineLimit);
  let html = '<div class="key-models-cell"><div class="key-table-chips">' + visible.map((m) => renderKeyModelChipItem(m, key)).join("") + '</div>';
  if (rest.length > 0) {
    html += '<details class="key-models-more"><summary>展开其余 ' + rest.length + ' 个模型（共 ' + models.length + ' 个）</summary><div class="key-table-chips key-table-chips-more">' + rest.map((m) => renderKeyModelChipItem(m, key)).join("") + '</div></details>';
  }
  html += '</div>';
  return html;
}

function renderKeyQuotaSummary(key) {
  const models = parseAllowlist(key.models);
  const budgets = models.filter((model) => Object.hasOwn(key.model_quotas || {}, model));
  const hasShared = !models.length || budgets.length < models.length;
  const sharedUsed = key.shared_used_usd || 0;
  const isZeroBlocked = !models.length && (key.quota_usd || 0) === 0;
  const percentage = key.quota_usd > 0 ? Math.min(100, sharedUsed / key.quota_usd * 100) : 0;
  const sharedLabel = isZeroBlocked ? "$0.00（已置零）" : (key.quota_usd > 0 ? fmtUSD(key.quota_usd) : "不限");
  const shared = hasShared ? '<span>共享：' + fmtUSD(sharedUsed) + ' <span class="muted">/ ' + sharedLabel + '</span></span>' + (key.quota_usd > 0 ? '<div class="quota-track" role="meter" aria-label="共享额度使用" aria-valuenow="' + percentage + '" aria-valuemin="0" aria-valuemax="100"><span style="width:' + percentage + '%"></span></div>' : "") : "";
  const independent = budgets.length ? '<details class="key-quota-details"><summary>独立额度 · ' + budgets.length + ' 个模型</summary>' + budgets.map((model) => { const budget = key.model_quotas[model]; return '<div><strong>' + modelLabel(model) + '</strong><span>' + fmtUSD(budget.used_usd) + ' / ' + (budget.quota_usd > 0 ? fmtUSD(budget.quota_usd) : "不限") + '</span></div>'; }).join("") + '</details>' : "";
  return shared + independent + '<small class="key-total-usage">累计使用 ' + fmtUSD(key.used_usd) + '</small>';
}

async function loadKeyModelOptions(prefix) {
  const state = keyModelPickers[prefix], generation = ++state.generation;
  state.controller?.abort();
  state.controller = new AbortController();
  setBusy($(prefix + "LoadModels"), true, "获取中…");
  const status = $(prefix + "ModelStatus");
  status.className = "probe-info";
  status.textContent = "正在获取启用上游的可用模型…";
  try {
    const result = await api("api/keys/models", { signal: state.controller.signal });
    if (generation !== state.generation) return;
    state.models = result.models || [];
    $(prefix + "ModelOptions").innerHTML = state.models.map((model) => '<option value="' + esc(model) + '"></option>').join("");
    $(prefix + "ModelPicker").classList.toggle("hidden", !state.models.length);
    status.textContent = state.models.length ? "已获取 " + state.models.length + " 个模型，勾选后加入列表。" : "上游尚未提供可用模型，可手动填写模型 ID。";
    renderKeyModelPicker(prefix);
  } catch (error) {
    if (generation !== state.generation || error.name === "AbortError") return;
    status.className = "probe-info error";
    status.textContent = error.message || "模型获取失败，请重试或手动填写。";
  } finally {
    if (generation === state.generation) {
      state.controller = null;
      setBusy($(prefix + "LoadModels"), false);
    }
  }
}

function validateKeyModels(prefix) {
  commitKeyModelInput(prefix);
  const field = $(prefix + "ModelInput");
  field.setCustomValidity("");
  if (prefix === "keyBatch" && parseAllowlist($(prefix + "Models").value).length === 0) {
    field.setCustomValidity("请至少填写或勾选一个要追加的模型");
    field.reportValidity();
    return false;
  }
  if (!field.checkValidity()) { field.reportValidity(); return false; }
  const inputs = [...$(prefix + "QuotaRows").querySelectorAll("input")];
  if (prefix !== "keyBatch") inputs.unshift($(prefix + "Quota"));
  for (const input of inputs) {
    if (!input.disabled && !input.checkValidity()) { input.reportValidity(); return false; }
  }
  return true;
}

function openKeyModels(id) {
  const key = keysCache.find((item) => item.id === id);
  if (!key) return;
  editingKeyModelsID = id;
  const userHint = key.xauth_user ? "（XAuth 用户：" + (key.xauth_user.email || key.xauth_user.name || key.xauth_user.sub) + "）" : "";
  $("keyModelsDescription").textContent = "单独调整“" + key.name + "”" + userHint + "的速率限制、可用模型和额度，保存后立即生效。";
  $("keyEditName").value = key.name || "";
  $("keyEditQps").value = key.qps || 3;
  $("keyEditConc").value = key.concurrency || 5;
  $("keyEditNote").value = key.note || "";
  $("keyEditModels").value = key.models || "";
  resetKeyModelPicker("keyEdit", key);
  $("keyModelsError").textContent = "";
  $("keyModelsError").classList.add("hidden");
  $("keyModelsDialog").showModal();
  syncModalLock();
  $("keyEditModelInput").focus();
}

function closeKeyModels() {
  if (savingKeyModels) return;
  resetKeyModelPicker("keyEdit");
  $("keyModelsDialog").close();
  editingKeyModelsID = null;
  syncModalLock();
}

async function saveKeyModels(event) {
  event.preventDefault();
  if (savingKeyModels || !validateKeyModels("keyEdit")) return;
  const name = $("keyEditName").value.trim();
  if (!name) { $("keyEditName").focus(); return; }
  const qps = Number($("keyEditQps").value);
  const concurrency = Number($("keyEditConc").value);
  if (!(qps > 0) || !(concurrency >= 1)) return;
  savingKeyModels = true;
  $("keyModelsFields").disabled = true;
  setBusy($("keyModelsSave"), true, "保存中…");
  $("keyModelsError").classList.add("hidden");
  try {
    await api("api/keys/" + editingKeyModelsID, {
      method: "PATCH",
      body: JSON.stringify({
        name,
        qps,
        concurrency,
        note: $("keyEditNote").value.trim(),
        models: parseAllowlist($("keyEditModels").value).join(","),
        ...keyQuotaPayload("keyEdit"),
      }),
    });
    savingKeyModels = false;
    closeKeyModels();
    await loadKeys();
    toast("密钥限制、模型与额度已更新");
  } catch (error) {
    $("keyModelsError").textContent = error.message || "保存失败，请重试。";
    $("keyModelsError").classList.remove("hidden");
  } finally {
    savingKeyModels = false;
    $("keyModelsFields").disabled = false;
    setBusy($("keyModelsSave"), false);
  }
}

/* Batch selection & batch add models (skip existing, add missing) */
function filteredKeys() {
  const query = ($("keySearch")?.value || "").trim().toLowerCase();
  const source = $("keySourceFilter")?.value || "";
  return keysCache.filter((key) => {
    if (source === "xauth" && !key.xauth_user) return false;
    if (source === "manual" && key.xauth_user) return false;
    if (!query) return true;
    const hay = [
      key.name,
      key.note,
      key.models,
      key.xauth_user?.name,
      key.xauth_user?.email,
      key.xauth_user?.sub,
    ].filter(Boolean).join(" ").toLowerCase();
    return hay.includes(query);
  });
}

function toggleKeySelection(id, checked) {
  if (checked) selectedKeyIDs.add(id);
  else selectedKeyIDs.delete(id);
  syncKeySelectionUI();
}

function selectAllKeys(checked) {
  filteredKeys().forEach((k) => {
    if (checked) selectedKeyIDs.add(k.id);
    else selectedKeyIDs.delete(k.id);
  });
  renderKeys();
}

function clearKeySelection() {
  selectedKeyIDs.clear();
  renderKeys();
}

function syncKeySelectionUI() {
  const validIDs = new Set(keysCache.map((k) => k.id));
  for (const id of [...selectedKeyIDs]) {
    if (!validIDs.has(id)) selectedKeyIDs.delete(id);
  }
  const visible = filteredKeys();
  const selectAll = $("keySelectAll");
  if (selectAll) {
    const selectedVisible = visible.filter((k) => selectedKeyIDs.has(k.id)).length;
    selectAll.checked = visible.length > 0 && selectedVisible === visible.length;
    selectAll.indeterminate = selectedVisible > 0 && selectedVisible < visible.length;
  }
  const bar = $("keyBulkBar");
  if (bar) {
    bar.classList.toggle("hidden", selectedKeyIDs.size === 0);
    $("keySelectedCount").textContent = "已选择 " + selectedKeyIDs.size + " 个密钥";
  }
}

function openBatchAddKeyModels(defaultScope = "", prefillFromXAuthDefault = false) {
  const scopeSelect = $("keyBatchScope");
  const selectedOpt = scopeSelect.querySelector('option[value="selected"]');
  selectedOpt.disabled = selectedKeyIDs.size === 0;
  selectedOpt.textContent = selectedKeyIDs.size > 0 ? "当前勾选的密钥（" + selectedKeyIDs.size + " 个）" : "当前勾选的密钥（未勾选）";
  if (defaultScope) {
    scopeSelect.value = defaultScope === "selected" && selectedKeyIDs.size === 0 ? "all" : defaultScope;
  } else {
    scopeSelect.value = selectedKeyIDs.size > 0 ? "selected" : "all";
  }
  if (prefillFromXAuthDefault && $("xauthDefaultModels")) {
    commitKeyModelInput("xauthDefault");
    $("keyBatchModels").value = $("xauthDefaultModels").value;
    const defaultPayload = keyQuotaPayload("xauthDefault");
    resetKeyModelPicker("keyBatch", { model_quotas: defaultPayload.model_quotas });
  } else {
    $("keyBatchModels").value = "";
    resetKeyModelPicker("keyBatch");
  }
  $("keyBatchModelsError").textContent = "";
  $("keyBatchModelsError").classList.add("hidden");
  $("keyBatchModelsDialog").showModal();
  syncModalLock();
  $("keyBatchModelInput").focus();
}

function closeBatchAddKeyModels() {
  if (savingBatchKeyModels) return;
  resetKeyModelPicker("keyBatch");
  $("keyBatchModelsDialog").close();
  syncModalLock();
}

async function saveBatchAddKeyModels(event) {
  event.preventDefault();
  if (savingBatchKeyModels || !validateKeyModels("keyBatch")) return;
  const scope = $("keyBatchScope").value;
  if (scope === "selected" && selectedKeyIDs.size === 0) {
    $("keyBatchModelsError").textContent = "请先在列表中勾选密钥，或切换作用范围。";
    $("keyBatchModelsError").classList.remove("hidden");
    return;
  }
  savingBatchKeyModels = true;
  $("keyBatchModelsFields").disabled = true;
  setBusy($("keyBatchModelsSave"), true, "正在追加…");
  $("keyBatchModelsError").classList.add("hidden");
  try {
    const result = await api("api/keys/batch-models", {
      method: "POST",
      body: JSON.stringify({
        scope,
        key_ids: [...selectedKeyIDs],
        models: parseAllowlist($("keyBatchModels").value).join(","),
        ...keyQuotaPayload("keyBatch"),
      }),
    });
    savingBatchKeyModels = false;
    closeBatchAddKeyModels();
    await loadKeys();
    toast("已更新 " + result.updated_keys + " 个密钥（新增 " + result.added_models + " 项模型绑定，已有跳过 " + result.skipped_keys + " 个密钥）");
  } catch (error) {
    $("keyBatchModelsError").textContent = error.message || "批量追加失败，请重试。";
    $("keyBatchModelsError").classList.remove("hidden");
  } finally {
    savingBatchKeyModels = false;
    $("keyBatchModelsFields").disabled = false;
    setBusy($("keyBatchModelsSave"), false);
  }
}

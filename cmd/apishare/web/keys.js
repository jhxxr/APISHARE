/* Per-key model permissions, visual model chips, multi-key selection, and batch add/remove models. */
const keyModelPickers = {};
const selectedKeyIDs = new Set();
let editingKeyModelsID = null, savingKeyModels = false, savingBatchKeyModels = false;
let batchKeyModelAction = "add"; // "add" | "remove"

function collectKnownKeyModels(extraModels = []) {
  const seen = new Set();
  const out = [];
  const push = (m) => {
    const trimmed = String(m || "").trim();
    if (trimmed && !seen.has(trimmed)) {
      seen.add(trimmed);
      out.push(trimmed);
    }
  };
  extraModels.forEach(push);
  if (Array.isArray(keysCache)) {
    keysCache.forEach((k) => parseAllowlist(k.models).forEach(push));
  }
  if (Array.isArray(upsCache)) {
    upsCache.filter((u) => u && u.enabled).forEach((u) => parseAllowlist(u.models).forEach(push));
  }
  return out;
}

function initKeyModels() {
  for (const prefix of ["key", "keyEdit", "xauthDefault", "keyBatch"]) {
    const host = $(prefix + "ModelControl");
    if (!host) continue;
    keyModelPickers[prefix] = {
      models: [],
      filter: "all",
      quotas: new Map(),
      generation: 0,
      controller: null,
      loadedOnce: false,
    };
    const isBatch = prefix === "keyBatch";
    const isXAuthDefault = prefix === "xauthDefault";
    const labelTitle = isBatch
      ? '2. 多选要添加的模型 <span class="required">*</span>'
      : isXAuthDefault
        ? '新用户默认可用模型 <span class="optional">可选</span>'
        : '模型白名单（支持多选添加 / 多选减少） <span class="optional">可选</span>';
    const quotaTitle = isBatch
      ? "3. 新追加模型的额度方式"
      : isXAuthDefault
        ? "默认赠送额度配置"
        : "模型额度";
    const quotaDesc = isBatch
      ? "若目标密钥已绑定某模型，将自动跳过并保留该密钥原有的额度设置；此处仅对本次新加上去的模型生效。"
      : "共享额度由多个模型共同使用。独立额度仅供对应模型使用，用完后不会扣减共享额度。金额单位为 USD，0 表示不限。";
    const sharedLabel = isXAuthDefault ? "默认赠送共享额度" : "共享额度";
    host.innerHTML =
      '<div class="field"><div class="field-label-row"><label id="' + prefix + 'ModelLabel" for="' + prefix + 'ModelInput">' + labelTitle + '</label><small id="' + prefix + 'ModelSummary" aria-live="polite">未绑定模型，无法调用</small></div>' +
      '<input id="' + prefix + 'Models" type="hidden">' +
      '<div id="' + prefix + 'ModelTagInput" class="model-tag-input"><div id="' + prefix + 'ModelChips" class="model-chips" aria-label="模型白名单"></div><input id="' + prefix + 'ModelInput" type="text" placeholder="输入模型名，按回车添加" spellcheck="false" autocomplete="off" list="' + prefix + 'ModelOptions" aria-describedby="' + prefix + 'ModelHint"></div>' +
      '<datalist id="' + prefix + 'ModelOptions"></datalist>' +
      '<small id="' + prefix + 'ModelHint">' + (isBatch ? "可在下方列表直接勾选多个模型，或输入/粘贴模型名；已有该模型的密钥自动跳过。" : "可在下方列表多选勾选添加或取消勾选减少，也支持回车/逗号/多行粘贴；留空表示不允许调用任何模型。") + '</small></div>' +
      '<div class="key-model-actions"><button type="button" class="btn ghost" id="' + prefix + 'LoadModels">获取可用模型</button><button type="button" class="text-btn" id="' + prefix + 'ClearModels">清空已选模型</button></div>' +
      '<p id="' + prefix + 'ModelStatus" class="probe-info" role="status"></p>' +
      '<div id="' + prefix + 'ModelPicker" class="model-picker hidden"><div class="model-picker-toolbar"><div id="' + prefix + 'ModelFilterTabs" class="key-model-filter-tabs" role="group" aria-label="模型筛选"></div><label class="search-field"><span class="sr-only">搜索可用模型</span><input id="' + prefix + 'ModelSearch" placeholder="搜索模型名称…"></label><button type="button" class="text-btn" id="' + prefix + 'SelectAllModels">多选添加当前</button><button type="button" class="text-btn" id="' + prefix + 'DeselectModels">多选减少当前</button></div><div id="' + prefix + 'ModelList" class="probe-box"></div></div>' +
      '<section id="' + prefix + 'QuotaSection" class="key-quota-control" aria-labelledby="' + prefix + 'QuotaHeading"><h3 id="' + prefix + 'QuotaHeading">' + quotaTitle + '</h3><p id="' + prefix + 'QuotaDesc" class="quota-hint">' + quotaDesc + '</p>' +
      '<label class="field key-shared-quota ' + (isBatch ? "hidden" : "") + '"><span>' + sharedLabel + ' <span class="field-unit">USD</span></span><input id="' + prefix + 'Quota" type="number" min="0" step="0.01" value="' + (isXAuthDefault ? "5" : "0") + '" ' + (isBatch ? "disabled" : "required") + ' aria-describedby="' + prefix + 'SharedUsage"><small id="' + prefix + 'SharedUsage">' + (isXAuthDefault ? "0 表示不限额度" : "已使用 $0.00") + '</small></label>' +
      '<div class="key-model-actions"><button type="button" class="text-btn" id="' + prefix + 'AllShared">全部使用共享额度</button><button type="button" class="text-btn" id="' + prefix + 'AllIndependent">全部独立设置</button></div>' +
      '<div id="' + prefix + 'QuotaRows" class="model-quota-rows"></div></section>' +
      (isBatch ? '<div id="keyBatchRemoveNote" class="inline-note key-batch-remove-note hidden">' + icon("info") + '<span>批量减少模式：仅从目标密钥中移除上方所选模型及其独立额度配置；未绑定该模型的密钥会自动跳过，不影响共享额度与其他已绑定模型。</span></div>' : "");

    initKeyModelTagInput(prefix);
    $(prefix + "ModelSearch").addEventListener("input", () => renderKeyModelPicker(prefix));
    $(prefix + "LoadModels").addEventListener("click", () => loadKeyModelOptions(prefix, false));
    $(prefix + "SelectAllModels").addEventListener("click", () => {
      const visible = visiblePickerModels(prefix);
      const current = new Set(parseAllowlist($(prefix + "Models").value));
      visible.forEach((m) => current.add(m));
      $(prefix + "Models").value = [...current].join(",");
      clearKeyModelError(prefix);
      renderKeyModelPicker(prefix);
    });
    $(prefix + "DeselectModels").addEventListener("click", () => {
      const visible = new Set(visiblePickerModels(prefix));
      const remaining = parseAllowlist($(prefix + "Models").value).filter((m) => !visible.has(m));
      $(prefix + "Models").value = remaining.join(",");
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

function candidateModelsForPicker(prefix) {
  const state = keyModelPickers[prefix];
  if (!state) return [];
  const selected = parseAllowlist($(prefix + "Models").value);
  if (prefix === "keyBatch") {
    const targets = batchTargetKeys();
    const boundCounts = batchTargetModelCounts(targets);
    const boundModels = [...boundCounts.keys()].sort((a, b) => (boundCounts.get(b) - boundCounts.get(a)) || a.localeCompare(b));
    if (batchKeyModelAction === "remove") {
      return collectKnownKeyModels([...boundModels, ...selected, ...state.models]);
    }
    return collectKnownKeyModels([...state.models, ...boundModels, ...selected]);
  }
  return collectKnownKeyModels([...state.models, ...selected]);
}

function visiblePickerModels(prefix) {
  const state = keyModelPickers[prefix];
  if (!state) return [];
  const allModels = candidateModelsForPicker(prefix);
  const selected = new Set(parseAllowlist($(prefix + "Models").value));
  const query = $(prefix + "ModelSearch").value.trim().toLowerCase();
  const filter = state.filter || "all";

  let boundCounts = null;
  let targetCount = 0;
  if (prefix === "keyBatch") {
    const targets = batchTargetKeys();
    targetCount = targets.length;
    boundCounts = batchTargetModelCounts(targets);
  }

  return allModels.filter((model) => {
    if (query && !model.toLowerCase().includes(query)) return false;
    if (prefix === "keyBatch") {
      const bCount = boundCounts.get(model) || 0;
      if (batchKeyModelAction === "remove") {
        if (filter === "bound") return bCount > 0 || selected.has(model);
        if (filter === "selected") return selected.has(model);
        return true;
      } else {
        if (filter === "missing") return targetCount === 0 || bCount < targetCount;
        if (filter === "selected") return selected.has(model);
        return true;
      }
    }
    if (filter === "unselected") return !selected.has(model);
    if (filter === "selected") return selected.has(model);
    return true;
  });
}

function resetKeyModelPicker(prefix, key = null) {
  const state = keyModelPickers[prefix];
  if (!state) return;
  state.generation++;
  state.controller?.abort();
  state.controller = null;
  state.filter = prefix === "keyBatch" && batchKeyModelAction === "remove" ? "bound" : "all";
  state.quotas = new Map(Object.entries(key?.model_quotas || {}).map(([model, budget]) => [model, { independent: true, amount: String(budget.quota_usd ?? 0), used: budget.used_usd || 0 }]));
  if (prefix !== "keyBatch") {
    $(prefix + "Quota").value = key?.quota_usd ?? (prefix === "xauthDefault" ? 5 : 0);
    $(prefix + "SharedUsage").textContent = prefix === "xauthDefault"
      ? "新用户首次登录默认赠送的共享额度（0 表示不限）"
      : "已使用 " + fmtUSD(key?.shared_used_usd || 0) + " · 已用金额会保留";
  }
  setBusy($(prefix + "LoadModels"), false);
  $(prefix + "ModelSearch").value = "";
  $(prefix + "ModelInput").value = "";
  $(prefix + "ModelStatus").textContent = "";
  clearKeyModelError(prefix);
  const candidates = candidateModelsForPicker(prefix);
  $(prefix + "ModelOptions").innerHTML = candidates.map((model) => '<option value="' + esc(model) + '"></option>').join("");
  $(prefix + "ModelPicker").classList.toggle("hidden", candidates.length === 0 && prefix !== "keyBatch");
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
    if (batchKeyModelAction === "remove") {
      return selected.size ? "待移除 " + selected.size + " 个模型（未绑跳过）" : "请勾选或输入要移除的模型";
    }
    return selected.size ? "待添加 " + selected.size + " 个模型（已有跳过）" : "请勾选或输入要添加的模型";
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
  const isBatchRemove = prefix === "keyBatch" && batchKeyModelAction === "remove";
  selected.forEach((model) => {
    const chip = document.createElement("span");
    chip.className = "model-chip" + (isBatchRemove ? " is-remove-chip" : "");
    const text = document.createElement("span");
    text.textContent = model;
    const button = document.createElement("button");
    button.type = "button";
    button.setAttribute("aria-label", (isBatchRemove ? "取消选择移除模型 " : "移除模型 ") + model);
    button.innerHTML = icon("close");
    button.addEventListener("click", () => {
      commitKeyModelInput(prefix);
      const current = parseAllowlist($(prefix + "Models").value).filter((item) => item !== model);
      $(prefix + "Models").value = current.join(",");
      clearKeyModelError(prefix);
      renderKeyModelPicker(prefix);
      $(prefix + "ModelInput").focus();
    });
    const brand = document.createElement("span");
    brand.innerHTML = modelIcon(model);
    chip.append(brand.firstElementChild, text, button);
    box.append(chip);
  });
  if (isBatchRemove) {
    $(prefix + "ModelInput").placeholder = selected.length ? "继续输入要移除的模型…" : "勾选下方已绑模型，或输入要移除的模型名";
  } else {
    $(prefix + "ModelInput").placeholder = selected.length ? "继续添加模型…" : "输入模型名，按回车添加（或在下方多选勾选）";
  }
  $(prefix + "ModelSummary").textContent = keyModelSummary(prefix, new Set(selected));
  const allowed = new Set(selected);
  $(prefix + "ModelList").querySelectorAll("input[data-model]").forEach((checkbox) => { checkbox.checked = allowed.has(checkbox.dataset.model); });
}

function renderKeyModelFilterTabs(prefix, allModels, selected) {
  const tabsHost = $(prefix + "ModelFilterTabs");
  if (!tabsHost) return;
  const state = keyModelPickers[prefix];
  let tabs = [];
  if (prefix === "keyBatch") {
    const targets = batchTargetKeys();
    const boundCounts = batchTargetModelCounts(targets);
    if (batchKeyModelAction === "remove") {
      const boundCount = allModels.filter((m) => (boundCounts.get(m) || 0) > 0).length;
      tabs = [
        { id: "bound", label: "目标已绑 (" + boundCount + ")" },
        { id: "selected", label: "待移除 (" + selected.size + ")" },
        { id: "all", label: "全部 (" + allModels.length + ")" },
      ];
    } else {
      const missingCount = allModels.filter((m) => targets.length === 0 || (boundCounts.get(m) || 0) < targets.length).length;
      tabs = [
        { id: "all", label: "全部可选 (" + allModels.length + ")" },
        { id: "missing", label: "目标缺失 (" + missingCount + ")" },
        { id: "selected", label: "待添加 (" + selected.size + ")" },
      ];
    }
  } else {
    const unselectedCount = allModels.filter((m) => !selected.has(m)).length;
    tabs = [
      { id: "all", label: "全部 (" + allModels.length + ")" },
      { id: "unselected", label: "未添加 (" + unselectedCount + ")" },
      { id: "selected", label: "已添加 (" + selected.size + ")" },
    ];
  }
  if (!tabs.some((t) => t.id === state.filter)) {
    state.filter = tabs[0].id;
  }
  tabsHost.innerHTML = tabs.map((t) =>
    '<button type="button" class="key-model-tab-btn ' + (state.filter === t.id ? "active" : "") + '" data-filter="' + t.id + '" aria-pressed="' + (state.filter === t.id) + '">' + esc(t.label) + '</button>'
  ).join("");
  tabsHost.querySelectorAll("button").forEach((btn) => {
    btn.addEventListener("click", () => {
      state.filter = btn.dataset.filter;
      renderKeyModelPicker(prefix);
    });
  });
}

function renderKeyModelPicker(prefix) {
  const state = keyModelPickers[prefix];
  if (!state) return;
  const selected = new Set(parseAllowlist($(prefix + "Models").value));
  renderKeyModelChips(prefix);
  const allModels = candidateModelsForPicker(prefix);
  renderKeyModelFilterTabs(prefix, allModels, selected);
  const models = visiblePickerModels(prefix);

  const isBatch = prefix === "keyBatch";
  const isBatchRemove = isBatch && batchKeyModelAction === "remove";
  $(prefix + "SelectAllModels").textContent = isBatchRemove
    ? "全选当前待移除"
    : isBatch
      ? "全选当前待添加"
      : "多选添加当前";
  $(prefix + "DeselectModels").textContent = isBatch
    ? "取消当前筛选"
    : "多选减少当前";

  let boundCounts = null;
  let targetCount = 0;
  if (isBatch) {
    const targets = batchTargetKeys();
    targetCount = targets.length;
    boundCounts = batchTargetModelCounts(targets);
  }

  $(prefix + "ModelList").innerHTML = models.map((model) => {
    const checked = selected.has(model);
    let badgeHTML = "";
    if (isBatch) {
      const bCount = boundCounts.get(model) || 0;
      if (isBatchRemove) {
        badgeHTML = '<span class="key-model-bind-stat ' + (bCount > 0 ? "is-bound" : "is-muted") + '">' + (bCount > 0 ? "已绑 " + bCount + "/" + targetCount : "目标未绑") + '</span>';
      } else if (targetCount > 0) {
        if (bCount === 0) {
          badgeHTML = '<span class="key-model-bind-stat is-new">可新增 ' + targetCount + ' 个</span>';
        } else if (bCount < targetCount) {
          badgeHTML = '<span class="key-model-bind-stat">可新增 ' + (targetCount - bCount) + ' · 已绑 ' + bCount + '</span>';
        } else {
          badgeHTML = '<span class="key-model-bind-stat is-muted">全部已绑</span>';
        }
      }
    } else {
      badgeHTML = '<span class="key-model-bind-stat ' + (checked ? "is-bound" : "") + '">' + (checked ? "已添加" : "可添加") + '</span>';
    }
    return '<label class="key-model-option ' + (checked ? "is-checked" : "") + '"><input type="checkbox" data-model="' + esc(model) + '" ' + (checked ? "checked" : "") + '>' + modelLabel(model) + badgeHTML + '</label>';
  }).join("") || '<span class="muted">' + (isBatchRemove && state.filter === "bound" ? "当前目标密钥暂未绑定匹配的模型，可切换至「全部」或选择其他密钥。" : "没有匹配的模型，可点击「获取可用模型」或在上方直接输入。") + '</span>';

  $(prefix + "ModelList").querySelectorAll("input").forEach((checkbox) => checkbox.addEventListener("change", () => {
    const current = new Set(parseAllowlist($(prefix + "Models").value));
    if (checkbox.checked) current.add(checkbox.dataset.model);
    else current.delete(checkbox.dataset.model);
    $(prefix + "Models").value = [...current].join(",");
    clearKeyModelError(prefix);
    renderKeyModelPicker(prefix);
  }));
  renderKeyQuotaRows(prefix);
  if (isBatch) renderBatchImpactPreview();
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
  if (isBatch) {
    const isRemove = batchKeyModelAction === "remove";
    $(prefix + "QuotaSection")?.classList.toggle("hidden", isRemove);
    $("keyBatchRemoveNote")?.classList.toggle("hidden", !isRemove);
    if (isRemove) return;
  } else {
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

function ensureKeyModelOptionsLoaded(prefix) {
  const state = keyModelPickers[prefix];
  if (!state) return;
  const candidates = candidateModelsForPicker(prefix);
  if (candidates.length > 0 || prefix === "keyBatch") {
    $(prefix + "ModelPicker").classList.remove("hidden");
    renderKeyModelPicker(prefix);
  }
  if (!state.loadedOnce && !state.controller) {
    loadKeyModelOptions(prefix, true);
  }
}

async function loadKeyModelOptions(prefix, silent = false) {
  const state = keyModelPickers[prefix], generation = ++state.generation;
  state.controller?.abort();
  state.controller = new AbortController();
  if (!silent) setBusy($(prefix + "LoadModels"), true, "获取中…");
  const status = $(prefix + "ModelStatus");
  if (!silent) {
    status.className = "probe-info";
    status.textContent = "正在获取启用上游的可用模型…";
  }
  try {
    const result = await api("api/keys/models", { signal: state.controller.signal });
    if (generation !== state.generation) return;
    state.loadedOnce = true;
    state.models = result.models || [];
    for (const otherPrefix of Object.keys(keyModelPickers)) {
      if (keyModelPickers[otherPrefix]) {
        keyModelPickers[otherPrefix].models = state.models.slice();
        keyModelPickers[otherPrefix].loadedOnce = true;
      }
    }
    const candidates = candidateModelsForPicker(prefix);
    $(prefix + "ModelOptions").innerHTML = candidates.map((model) => '<option value="' + esc(model) + '"></option>').join("");
    $(prefix + "ModelPicker").classList.toggle("hidden", !candidates.length && prefix !== "keyBatch");
    if (!silent) {
      status.textContent = state.models.length ? "已获取 " + state.models.length + " 个上游可用模型，可直接多选勾选。" : "上游尚未提供可用模型，可手动填写模型 ID。";
    }
    renderKeyModelPicker(prefix);
  } catch (error) {
    if (generation !== state.generation || error.name === "AbortError") return;
    if (!silent) {
      status.className = "probe-info error";
      status.textContent = error.message || "模型获取失败，请重试或手动填写。";
    }
  } finally {
    if (generation === state.generation) {
      state.controller = null;
      if (!silent) setBusy($(prefix + "LoadModels"), false);
    }
  }
}

function validateKeyModels(prefix) {
  commitKeyModelInput(prefix);
  const field = $(prefix + "ModelInput");
  field.setCustomValidity("");
  if (prefix === "keyBatch" && parseAllowlist($(prefix + "Models").value).length === 0) {
    field.setCustomValidity(batchKeyModelAction === "remove" ? "请至少勾选或输入一个要移除的模型" : "请至少勾选或输入一个要追加的模型");
    field.reportValidity();
    return false;
  }
  if (!field.checkValidity()) { field.reportValidity(); return false; }
  if (prefix === "keyBatch" && batchKeyModelAction === "remove") {
    return true;
  }
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
  $("keyModelsDescription").textContent = "单独调整“" + key.name + "”" + userHint + "的速率限制、可用模型和额度，支持在模型列表中多选添加或多选减少，保存后立即生效。";
  $("keyEditName").value = key.name || "";
  $("keyEditQps").value = key.qps || 3;
  $("keyEditConc").value = key.concurrency || 5;
  $("keyEditNote").value = key.note || "";
  $("keyEditModels").value = key.models || "";
  resetKeyModelPicker("keyEdit", key);
  ensureKeyModelOptionsLoaded("keyEdit");
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

/* Multi-key selection & batch add/remove models */
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

function invertKeySelection() {
  filteredKeys().forEach((k) => {
    if (selectedKeyIDs.has(k.id)) selectedKeyIDs.delete(k.id);
    else selectedKeyIDs.add(k.id);
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
  if ($("keyBatchModelsDialog")?.open) {
    renderBatchKeyPicker();
    renderKeyModelPicker("keyBatch");
  }
}

function batchTargetKeys(scopeOverride = "") {
  const scope = scopeOverride || $("keyBatchScope")?.value || "all";
  if (scope === "selected") return keysCache.filter((k) => selectedKeyIDs.has(k.id));
  if (scope === "xauth") return keysCache.filter((k) => !!k.xauth_user);
  if (scope === "manual") return keysCache.filter((k) => !k.xauth_user);
  return keysCache.slice();
}

function batchTargetModelCounts(targets) {
  const counts = new Map();
  targets.forEach((k) => {
    parseAllowlist(k.models).forEach((m) => {
      counts.set(m, (counts.get(m) || 0) + 1);
    });
  });
  return counts;
}

function batchFilteredDialogKeys() {
  const query = ($("keyBatchKeySearch")?.value || "").trim().toLowerCase();
  const source = $("keyBatchKeySource")?.value || "";
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

function renderBatchKeyPicker() {
  const scopeSelect = $("keyBatchScope");
  if (!scopeSelect) return;
  const scope = scopeSelect.value;
  const xauthCount = keysCache.filter((k) => !!k.xauth_user).length;
  const manualCount = keysCache.filter((k) => !k.xauth_user).length;
  const selectedOpt = scopeSelect.querySelector('option[value="selected"]');
  const xauthOpt = scopeSelect.querySelector('option[value="xauth"]');
  const manualOpt = scopeSelect.querySelector('option[value="manual"]');
  const allOpt = scopeSelect.querySelector('option[value="all"]');
  if (selectedOpt) selectedOpt.textContent = "指定多个密钥（已勾选 " + selectedKeyIDs.size + " / 共 " + keysCache.length + " 个）";
  if (xauthOpt) xauthOpt.textContent = "所有 XAuth 用户密钥（" + xauthCount + " 个）";
  if (manualOpt) manualOpt.textContent = "所有手动创建的密钥（" + manualCount + " 个）";
  if (allOpt) allOpt.textContent = "全部 API 密钥（" + keysCache.length + " 个）";

  const targets = batchTargetKeys(scope);
  if ($("keyBatchTargetSummary")) {
    $("keyBatchTargetSummary").textContent = scope === "selected"
      ? "已勾选 " + selectedKeyIDs.size + " 个目标密钥"
      : "当前范围匹配 " + targets.length + " 个目标密钥";
  }

  const picker = $("keyBatchKeyPicker");
  const hintBar = $("keyBatchScopeHintBar");
  if (picker) picker.classList.toggle("hidden", scope !== "selected");
  if (hintBar) {
    hintBar.classList.toggle("hidden", scope === "selected");
    const boundKeys = targets.filter((k) => parseAllowlist(k.models).length > 0).length;
    $("keyBatchScopeHintText").textContent = "将作用于该范围内的 " + targets.length + " 个密钥（其中 " + boundKeys + " 个已绑定模型）。如需排除个别密钥，可转为多选微调。";
  }

  if (scope !== "selected" || !$("keyBatchKeyList")) return;
  const list = batchFilteredDialogKeys();
  $("keyBatchKeyList").innerHTML = list.map((key) => {
    const checked = selectedKeyIDs.has(key.id);
    const models = parseAllowlist(key.models);
    const xauthText = key.xauth_user ? (key.xauth_user.email || key.xauth_user.name || key.xauth_user.sub) : "";
    const modelPreview = models.length
      ? "已绑 " + models.length + " 个：" + models.slice(0, 2).join(", ") + (models.length > 2 ? "…" : "")
      : "未绑定模型";
    return '<label class="key-batch-key-item ' + (checked ? "is-checked" : "") + '">' +
      '<input type="checkbox" data-batch-key="' + key.id + '" ' + (checked ? "checked" : "") + '>' +
      '<span class="key-batch-key-main"><strong>' + esc(key.name) + '</strong>' +
      (xauthText ? '<span class="key-xauth-badge">XAuth · ' + esc(xauthText) + '</span>' : '') +
      '</span>' +
      '<span class="key-batch-key-models ' + (models.length ? "" : "is-empty") + '" title="' + esc(models.join(", ")) + '">' + esc(modelPreview) + '</span>' +
      '</label>';
  }).join("") || '<span class="muted">没有匹配的密钥</span>';

  $("keyBatchKeyList").querySelectorAll("input[data-batch-key]").forEach((cb) => {
    cb.addEventListener("change", () => {
      const id = Number(cb.dataset.batchKey);
      if (cb.checked) selectedKeyIDs.add(id);
      else selectedKeyIDs.delete(id);
      $("keysTable")?.querySelectorAll('input[data-key-select="' + id + '"]').forEach((tableCb) => { tableCb.checked = cb.checked; });
      syncKeySelectionUI();
    });
  });
}

function onBatchScopeChange() {
  $("keyBatchModelsError")?.classList.add("hidden");
  renderBatchKeyPicker();
  renderKeyModelPicker("keyBatch");
}

function batchSelectFilteredKeys(checked) {
  batchFilteredDialogKeys().forEach((k) => {
    if (checked) selectedKeyIDs.add(k.id);
    else selectedKeyIDs.delete(k.id);
  });
  renderKeys();
}

function batchInvertFilteredKeys() {
  batchFilteredDialogKeys().forEach((k) => {
    if (selectedKeyIDs.has(k.id)) selectedKeyIDs.delete(k.id);
    else selectedKeyIDs.add(k.id);
  });
  renderKeys();
}

function convertScopeToSelectedKeys() {
  const currentTargets = batchTargetKeys();
  selectedKeyIDs.clear();
  currentTargets.forEach((k) => selectedKeyIDs.add(k.id));
  $("keyBatchScope").value = "selected";
  renderKeys();
}

function setBatchKeyModelAction(action) {
  batchKeyModelAction = action === "remove" ? "remove" : "add";
  const isRemove = batchKeyModelAction === "remove";
  $("keyBatchModeAdd")?.classList.toggle("active", !isRemove);
  $("keyBatchModeAdd")?.setAttribute("aria-selected", String(!isRemove));
  $("keyBatchModeRemove")?.classList.toggle("active", isRemove);
  $("keyBatchModeRemove")?.setAttribute("aria-selected", String(isRemove));

  $("keyBatchModelsHeading").textContent = isRemove ? "批量减少模型" : "批量添加模型";
  $("keyBatchModelsDescription").textContent = isRemove
    ? "从目标密钥中批量移除所选模型：支持勾选多个密钥与多个模型，未绑定该模型的密钥将自动跳过。"
    : "为目标密钥批量追加可用模型：支持勾选多个密钥与多个模型，已绑定该模型的密钥将自动跳过并保留原额度设置。";
  if ($("keyBatchModelLabel")) {
    $("keyBatchModelLabel").innerHTML = isRemove
      ? '2. 多选要减少（移除）的模型 <span class="required">*</span>'
      : '2. 多选要添加的模型 <span class="required">*</span>';
  }
  if ($("keyBatchModelHint")) {
    $("keyBatchModelHint").textContent = isRemove
      ? "下方列表已自动汇总目标密钥已绑定的模型，勾选即可多选移除；也可手动输入或粘贴模型名。"
      : "可在下方列表中勾选多个模型批量添加，或直接输入/粘贴模型名；已有该模型的密钥自动跳过。";
  }
  if ($("keyBatchModelsSave")) {
    $("keyBatchModelsSave").textContent = isRemove ? "确认批量移除" : "确认批量添加";
    $("keyBatchModelsSave").className = isRemove ? "btn danger" : "btn";
  }
  const state = keyModelPickers.keyBatch;
  if (state) {
    state.filter = isRemove ? "bound" : "all";
  }
  clearKeyModelError("keyBatch");
  $("keyBatchModelsError")?.classList.add("hidden");
  renderKeyModelPicker("keyBatch");
}

function renderBatchImpactPreview() {
  const box = $("keyBatchImpactPreview");
  if (!box) return;
  const targets = batchTargetKeys();
  const models = parseAllowlist($("keyBatchModels")?.value || "");
  const isRemove = batchKeyModelAction === "remove";
  if (targets.length === 0) {
    box.className = "key-batch-impact is-empty";
    box.textContent = "请先在上方至少勾选 1 个目标密钥，或切换目标密钥范围。";
    return;
  }
  if (models.length === 0) {
    box.className = "key-batch-impact is-empty";
    box.textContent = "已选定 " + targets.length + " 个目标密钥 · 请在上方勾选或输入要" + (isRemove ? "移除" : "添加") + "的模型。";
    return;
  }
  let updatedKeys = 0, skippedKeys = 0, deltaBindings = 0;
  targets.forEach((k) => {
    const existing = new Set(parseAllowlist(k.models));
    let count = 0;
    models.forEach((m) => {
      if (isRemove && existing.has(m)) count++;
      if (!isRemove && !existing.has(m)) count++;
    });
    if (count > 0) {
      updatedKeys++;
      deltaBindings += count;
    } else {
      skippedKeys++;
    }
  });
  box.className = "key-batch-impact" + (isRemove ? " is-remove" : " is-add");
  if (updatedKeys === 0) {
    box.textContent = isRemove
      ? "所选的 " + targets.length + " 个目标密钥均未绑定这些模型，无需移除。"
      : "所选的 " + targets.length + " 个目标密钥均已绑定这些模型，将全部跳过。";
    return;
  }
  box.textContent = "执行预览：共匹配 " + targets.length + " 个密钥，将" +
    (isRemove ? "从 " + updatedKeys + " 个密钥中移除 " + deltaBindings + " 项模型绑定" : "为 " + updatedKeys + " 个密钥新增 " + deltaBindings + " 项模型绑定") +
    (skippedKeys > 0 ? "（其余 " + skippedKeys + " 个密钥自动跳过）" : "");
}

function openBatchKeyModels(action = "add", defaultScope = "", prefillFromXAuthDefault = false) {
  batchKeyModelAction = action === "remove" ? "remove" : "add";
  const scopeSelect = $("keyBatchScope");
  if (defaultScope) {
    scopeSelect.value = defaultScope;
  } else {
    scopeSelect.value = selectedKeyIDs.size > 0 ? "selected" : "all";
  }
  if ($("keyBatchKeySearch")) $("keyBatchKeySearch").value = "";
  if ($("keyBatchKeySource")) $("keyBatchKeySource").value = "";
  if (prefillFromXAuthDefault && $("xauthDefaultModels") && batchKeyModelAction === "add") {
    commitKeyModelInput("xauthDefault");
    $("keyBatchModels").value = $("xauthDefaultModels").value;
    const defaultPayload = keyQuotaPayload("xauthDefault");
    resetKeyModelPicker("keyBatch", { model_quotas: defaultPayload.model_quotas });
  } else {
    $("keyBatchModels").value = "";
    resetKeyModelPicker("keyBatch");
  }
  renderBatchKeyPicker();
  setBatchKeyModelAction(batchKeyModelAction);
  ensureKeyModelOptionsLoaded("keyBatch");
  $("keyBatchModelsError").textContent = "";
  $("keyBatchModelsError").classList.add("hidden");
  $("keyBatchModelsDialog").showModal();
  syncModalLock();
  $("keyBatchModelInput").focus();
}

function openBatchAddKeyModels(defaultScope = "", prefillFromXAuthDefault = false) {
  openBatchKeyModels("add", defaultScope, prefillFromXAuthDefault);
}

function openBatchRemoveKeyModels(defaultScope = "") {
  openBatchKeyModels("remove", defaultScope, false);
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
    $("keyBatchModelsError").textContent = "请先在上方密钥列表中至少勾选 1 个密钥，或切换目标范围。";
    $("keyBatchModelsError").classList.remove("hidden");
    return;
  }
  const isRemove = batchKeyModelAction === "remove";
  savingBatchKeyModels = true;
  $("keyBatchModelsFields").disabled = true;
  setBusy($("keyBatchModelsSave"), true, isRemove ? "正在移除…" : "正在追加…");
  $("keyBatchModelsError").classList.add("hidden");
  try {
    const payload = {
      action: batchKeyModelAction,
      scope,
      key_ids: [...selectedKeyIDs],
      models: parseAllowlist($("keyBatchModels").value).join(","),
      ...(isRemove ? {} : keyQuotaPayload("keyBatch")),
    };
    const result = await api("api/keys/batch-models", {
      method: "POST",
      body: JSON.stringify(payload),
    });
    savingBatchKeyModels = false;
    closeBatchAddKeyModels();
    await loadKeys();
    if (isRemove) {
      toast("已更新 " + result.updated_keys + " 个密钥（移除 " + result.removed_models + " 项模型绑定，未绑定跳过 " + result.skipped_keys + " 个密钥）");
    } else {
      toast("已更新 " + result.updated_keys + " 个密钥（新增 " + result.added_models + " 项模型绑定，已有跳过 " + result.skipped_keys + " 个密钥）");
    }
  } catch (error) {
    $("keyBatchModelsError").textContent = error.message || (isRemove ? "批量移除失败，请重试。" : "批量追加失败，请重试。");
    $("keyBatchModelsError").classList.remove("hidden");
  } finally {
    savingBatchKeyModels = false;
    $("keyBatchModelsFields").disabled = false;
    setBusy($("keyBatchModelsSave"), false);
  }
}

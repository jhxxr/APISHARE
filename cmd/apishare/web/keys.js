/* Per-key model permissions. The hidden input remains the source of truth. */
const keyModelPickers = {};
let editingKeyModelsID = null, savingKeyModels = false;

function initKeyModels() {
  for (const prefix of ["key", "keyEdit"]) {
    keyModelPickers[prefix] = { models: [], quotas: new Map(), generation: 0, controller: null };
    $(prefix + "ModelControl").innerHTML = '<div class="field"><div class="field-label-row"><label for="' + prefix + 'ModelInput">模型白名单 <span class="optional">可选</span></label><small id="' + prefix + 'ModelSummary" aria-live="polite">未绑定模型，无法调用</small></div><input id="' + prefix + 'Models" type="hidden"><div id="' + prefix + 'ModelTagInput" class="model-tag-input"><div id="' + prefix + 'ModelChips" class="model-chips" aria-label="模型白名单"></div><input id="' + prefix + 'ModelInput" type="text" placeholder="输入模型名，按回车添加" spellcheck="false" autocomplete="off" list="' + prefix + 'ModelOptions" aria-describedby="' + prefix + 'ModelHint"></div><datalist id="' + prefix + 'ModelOptions"></datalist><small id="' + prefix + 'ModelHint">回车或逗号添加，支持粘贴多行模型；点击 × 移除。模型别名需单独填写；留空表示不允许调用任何模型。</small></div><div class="key-model-actions"><button type="button" class="btn ghost" id="' + prefix + 'LoadModels">获取可用模型</button><button type="button" class="text-btn" id="' + prefix + 'ClearModels">清空模型绑定</button></div><p id="' + prefix + 'ModelStatus" class="probe-info" role="status"></p><div id="' + prefix + 'ModelPicker" class="model-picker hidden"><div class="model-picker-toolbar"><label class="search-field"><span class="sr-only">搜索可用模型</span><input id="' + prefix + 'ModelSearch" placeholder="搜索可用模型…"></label></div><div id="' + prefix + 'ModelList" class="probe-box"></div></div><section class="key-quota-control" aria-labelledby="' + prefix + 'QuotaHeading"><h3 id="' + prefix + 'QuotaHeading">模型额度</h3><p class="quota-hint">共享额度由多个模型共同使用。独立额度仅供对应模型使用，用完后不会扣减共享额度。金额单位为 USD，0 表示不限。</p><label class="field key-shared-quota"><span>共享额度 <span class="field-unit">USD</span></span><input id="' + prefix + 'Quota" type="number" min="0" step="0.01" value="0" required aria-describedby="' + prefix + 'SharedUsage"><small id="' + prefix + 'SharedUsage">已使用 $0.00</small></label><div class="key-model-actions"><button type="button" class="text-btn" id="' + prefix + 'AllShared">全部使用共享额度</button><button type="button" class="text-btn" id="' + prefix + 'AllIndependent">全部独立设置</button></div><div id="' + prefix + 'QuotaRows" class="model-quota-rows"></div></section>';
    initKeyModelTagInput(prefix);
    $(prefix + "ModelSearch").addEventListener("input", () => renderKeyModelPicker(prefix));
    $(prefix + "LoadModels").addEventListener("click", () => loadKeyModelOptions(prefix));
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
}

function resetKeyModelPicker(prefix, key = null) {
  const state = keyModelPickers[prefix];
  state.generation++;
  state.controller?.abort();
  state.controller = null;
  state.models = [];
  state.quotas = new Map(Object.entries(key?.model_quotas || {}).map(([model, budget]) => [model, { independent: true, amount: String(budget.quota_usd), used: budget.used_usd }]));
  $(prefix + "Quota").value = key?.quota_usd || 0;
  $(prefix + "SharedUsage").textContent = "已使用 " + fmtUSD(key?.shared_used_usd || 0) + " · 已用金额会保留";
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
    field.setCustomValidity("");
    field.removeAttribute("aria-invalid");
  }
  $(prefix + "ModelsError")?.classList.add("hidden");
}

function keyModelSummary(prefix, selected) {
  return selected.size ? "已添加 " + selected.size + " 个模型" : "未绑定模型，无法调用";
}

function commitKeyModelInput(prefix) {
  const field = $(prefix + "ModelInput");
  if (!field.value.trim()) { field.value = ""; return; }
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
  $(prefix + "SharedUsage").closest("label").classList.toggle("hidden", models.length > 0 && sharedCount === 0);
  $(prefix + "Quota").disabled = models.length > 0 && sharedCount === 0;
  $(prefix + "AllShared").disabled = $(prefix + "AllIndependent").disabled = !models.length;
  $(prefix + "QuotaRows").innerHTML = models.map((model, index) => {
    const budget = keyQuotaDraft(prefix, model), rowID = prefix + "Budget" + index;
    return '<div class="model-quota-row" data-model="' + esc(model) + '"><strong>' + modelLabel(model) + '</strong><label class="field"><span class="sr-only">' + esc(model) + ' 的额度方式</span><select class="model-quota-mode" aria-controls="' + rowID + '"><option value="shared" ' + (!budget.independent ? "selected" : "") + '>使用共享额度</option><option value="independent" ' + (budget.independent ? "selected" : "") + '>独立额度</option></select></label><label id="' + rowID + '" class="field model-quota-amount ' + (budget.independent ? "" : "hidden") + '"><span class="sr-only">' + esc(model) + ' 的独立额度（USD）</span><input id="' + rowID + 'Amount" type="number" min="0" step="0.01" value="' + esc(budget.amount) + '" required ' + (budget.independent ? "" : "disabled") + '><small>已使用 ' + fmtUSD(budget.used) + ' · 0 表示不限</small></label></div>';
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
  return {
    quota_usd: Number($(prefix + "Quota").value),
    model_quotas: Object.fromEntries(parseAllowlist($(prefix + "Models").value)
      .filter((model) => keyQuotaDraft(prefix, model).independent)
      .map((model) => [model, { quota_usd: Number(keyQuotaDraft(prefix, model).amount) }]))
  };
}

function renderKeyQuotaSummary(key) {
  const models = parseAllowlist(key.models);
  const budgets = models.filter((model) => Object.hasOwn(key.model_quotas || {}, model));
  const hasShared = !models.length || budgets.length < models.length;
  const sharedUsed = key.shared_used_usd || 0;
  const percentage = key.quota_usd > 0 ? Math.min(100, sharedUsed / key.quota_usd * 100) : 0;
  const shared = hasShared ? '<span>共享：' + fmtUSD(sharedUsed) + ' <span class="muted">/ ' + (key.quota_usd > 0 ? fmtUSD(key.quota_usd) : "不限") + '</span></span>' + (key.quota_usd > 0 ? '<div class="quota-track" role="meter" aria-label="共享额度使用" aria-valuenow="' + percentage + '" aria-valuemin="0" aria-valuemax="100"><span style="width:' + percentage + '%"></span></div>' : "") : "";
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
    status.textContent = state.models.length ? "已获取 " + state.models.length + " 个模型，勾选后加入白名单。" : "上游尚未提供可用模型，可手动填写模型 ID。";
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
  if (!field.checkValidity()) { field.reportValidity(); return false; }
  for (const input of [$(prefix + "Quota"), ...$(prefix + "QuotaRows").querySelectorAll("input")]) {
    if (!input.checkValidity()) { input.reportValidity(); return false; }
  }
  return true;
}

function openKeyModels(id) {
  const key = keysCache.find((item) => item.id === id);
  if (!key) return;
  editingKeyModelsID = id;
  $("keyModelsDescription").textContent = "设置“" + key.name + "”的可用模型和额度，保存后对新请求立即生效。";
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
  savingKeyModels = true;
  $("keyModelsFields").disabled = true;
  setBusy($("keyModelsSave"), true, "保存中…");
  $("keyModelsError").classList.add("hidden");
  try {
    await api("api/keys/" + editingKeyModelsID, { method: "PATCH", body: JSON.stringify({ models: parseAllowlist($("keyEditModels").value).join(","), ...keyQuotaPayload("keyEdit") }) });
    savingKeyModels = false;
    closeKeyModels();
    await loadKeys();
    toast("密钥的模型与额度已更新");
  } catch (error) {
    $("keyModelsError").textContent = error.message || "保存失败，请重试。";
    $("keyModelsError").classList.remove("hidden");
  } finally {
    savingKeyModels = false;
    $("keyModelsFields").disabled = false;
    setBusy($("keyModelsSave"), false);
  }
}

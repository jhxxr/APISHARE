/* A single-turn test sent directly to a saved upstream. */
const MODEL_TEST_CACHE_MS = 5 * 60 * 1000;
const modelTest = {
  id: null, controller: null, probeController: null,
  mapping: {}, configured: [], fetched: [], selected: "",
  open: false, active: -1, options: [], pointerInside: false, cache: new Map(),
};

function uniqueModelNames(values) {
  return [...new Set(values.map((value) => String(value || "").trim()).filter(Boolean))];
}

// Probed catalogs belong to the exact connection they were read from, so the
// cache key covers every field that can change the upstream's model list.
function modelTestCacheSignature(upstream) {
  return [upstream.type, upstream.base_url, upstream.key_masked, upstream.proxy_id, upstream.openai_protocol, upstream.opencode_tier, upstream.header_overrides].join("\n");
}
function modelTestCachedCatalog(upstream) {
  const cached = modelTest.cache.get(upstream.id);
  if (!cached || cached.signature !== modelTestCacheSignature(upstream)) return { models: [], fresh: false };
  // An entry past its TTL is still shown, but must be refreshed in the background.
  return { models: cached.models, fresh: Date.now() - cached.at < MODEL_TEST_CACHE_MS };
}

function openModelTest(id) {
  const upstream = upsCache.find((item) => item.id === id);
  if (!upstream) return;
  // A dialog left open for another upstream must not deliver its catalog here.
  modelTest.probeController?.abort();
  modelTest.probeController = null;
  modelTest.id = id;
  let mapping = {};
  try { mapping = JSON.parse(upstream.model_map || "{}"); } catch { /* Allow testing legacy configurations. */ }
  if (!mapping || Array.isArray(mapping) || typeof mapping !== "object") mapping = {};
  modelTest.mapping = mapping;
  modelTest.configured = uniqueModelNames([...parseAllowlist(upstream.models), ...Object.keys(mapping)]);
  const cached = modelTestCachedCatalog(upstream);
  modelTest.fetched = cached.models;
  modelTest.selected = modelTest.configured[0] || modelTest.fetched[0] || "";
  modelTest.active = -1;
  modelTest.options = [];
  modelTest.open = false;
  setBusy($("modelTestFetch"), false);
  $("modelTestDescription").textContent = "向「" + (upstream.name || "未命名上游") + "」发送真实模型请求，使用已保存的配置" + (upstream.type === "opencode-free" ? "，仅接受 OpenCode 免费模型及其别名，可能消耗免费额度。" : "，可能产生上游费用。");
  $("modelTestSearch").value = "";
  $("modelTestMessage").value = "hi";
  $("modelTestMaxTokens").value = "4096";
  $("modelTestStream").value = "true";
  $("modelTestResult").classList.add("hidden");
  $("modelTestError").classList.add("hidden");
  clearModelTestModelError();
  renderModelTestSelection();
  renderModelTestList();
  setModelTestHint();
  $("modelTestDialog").showModal(); syncModalLock();
  setModelTestPanel(true, false);
  const search = $("modelTestSearch");
  if (matchMedia("(max-width: 600px)").matches) $("modelTestPickerButton").focus();
  else search.focus();
  if (cached.fresh) return;
  fetchModelTestModels();
}

function setModelTestHint(extra = "") {
  if (extra) { $("modelTestHint").textContent = extra; return; }
  const configured = modelTest.configured.length;
  const fetched = modelTest.fetched.length;
  const parts = [];
  if (configured) parts.push("已配置 " + configured + " 个模型");
  if (fetched) parts.push("上游返回 " + fetched + " 个可用模型");
  $("modelTestHint").textContent = parts.length ? parts.join(" · ") + "；也可直接输入其他模型名。" : "可直接输入模型名称，模型映射会自动生效。";
}

function updateModelTestMeta() {
  const parts = [];
  if (modelTest.configured.length) parts.push("已配置 " + modelTest.configured.length);
  if (modelTest.fetched.length) parts.push("上游 " + modelTest.fetched.length);
  $("modelTestModelMeta").textContent = parts.join(" · ");
}

function renderModelTestSelection() {
  const value = modelTest.selected;
  const label = $("modelTestPickerValue");
  label.textContent = value || "选择测试模型";
  label.classList.toggle("is-placeholder", !value);
  const mapped = value && Object.hasOwn(modelTest.mapping, value) ? modelTest.mapping[value] : "";
  $("modelTestMappingBadge").textContent = mapped ? "→ " + mapped : "";
  $("modelTestMappingBadge").classList.toggle("hidden", !mapped);
  updateModelTestMeta();
}

// The dialog scrolls, so an absolutely positioned panel is clipped by the
// dialog frame. Fit it to whichever side has room instead of a fixed height.
function positionModelTestPanel() {
  const panel = $("modelTestPickerPanel");
  panel.classList.remove("is-above");
  panel.style.maxHeight = "";
  if (!modelTest.open) return;
  const picker = $("modelTestPicker").getBoundingClientRect();
  const frame = $("modelTestDialog").getBoundingClientRect();
  const gap = 6, margin = 10;
  const below = frame.bottom - picker.bottom - gap - margin;
  const above = picker.top - frame.top - gap - margin;
  const space = Math.max(below, above);
  panel.style.maxHeight = Math.min(340, Math.max(120, space)) + "px";
  if (above > below) panel.classList.add("is-above");
}

function setModelTestPanel(open, focus = true) {
  modelTest.open = open;
  $("modelTestPickerPanel").classList.toggle("hidden", !open);
  $("modelTestPicker").classList.toggle("is-open", open);
  $("modelTestPickerButton").setAttribute("aria-expanded", String(open));
  $("modelTestSearch").setAttribute("aria-expanded", String(open));
  if (open) {
    renderModelTestList();
    positionModelTestPanel();
    // Keep the current choice visible even when the list is long.
    const list = $("modelTestList"), selected = list.querySelector(".is-selected");
    if (selected) {
      const box = list.getBoundingClientRect(), row = selected.getBoundingClientRect();
      list.scrollTop += row.top - box.top - (box.height - row.height) / 2;
    }
    if (focus) $("modelTestSearch").focus();
    return;
  }
  modelTest.active = -1;
  $("modelTestSearch").removeAttribute("aria-activedescendant");
}

function appendModelTestOption(container, value, options = {}) {
  const button = document.createElement("button");
  button.type = "button"; button.tabIndex = -1;
  button.className = "model-test-option" + (options.custom ? " is-custom" : "");
  button.id = "modelTestOption" + modelTest.options.length;
  button.setAttribute("role", "option");
  const selected = value === modelTest.selected;
  button.classList.toggle("is-selected", selected);
  button.setAttribute("aria-selected", String(selected));
  const main = document.createElement("span"); main.className = "model-test-option-main";
  const name = document.createElement("span"); name.className = "model-test-option-name"; name.textContent = value;
  main.append(name);
  if (options.mapped) {
    const mapped = document.createElement("small"); mapped.textContent = "→ " + options.mapped;
    main.append(mapped);
  } else if (options.custom) {
    const note = document.createElement("small"); note.textContent = "使用自定义模型名";
    main.append(note);
  }
  const mark = document.createElement("span"); mark.className = "model-test-check";
  mark.innerHTML = icon(options.custom ? "plus" : "check");
  button.append(main, mark);
  button.addEventListener("click", () => chooseModelTestModel(value));
  container.append(button);
  modelTest.options.push({ value, button });
}

// Grouping stays a real listbox group so the headings are announced with the
// options they label instead of becoming stray text nodes inside the listbox.
function appendModelTestGroup(list, title, models, optionOptions) {
  const group = document.createElement("div");
  group.className = "model-test-group-block";
  group.setAttribute("role", "group");
  group.setAttribute("aria-label", title + "，" + models.length + " 个");
  const heading = document.createElement("p"); heading.className = "model-test-group"; heading.setAttribute("aria-hidden", "true");
  const label = document.createElement("strong"); label.textContent = title;
  const total = document.createElement("span"); total.textContent = models.length + " 个";
  heading.append(label, total); group.append(heading);
  models.forEach((model) => appendModelTestOption(group, model, optionOptions(model)));
  list.append(group);
}

function renderModelTestList() {
  const list = $("modelTestList");
  const query = $("modelTestSearch").value.trim();
  const needle = query.toLowerCase();
  const configuredSet = new Set(modelTest.configured);
  const matches = (model) => !needle || model.toLowerCase().includes(needle);
  const configured = modelTest.configured.filter(matches);
  const upstream = modelTest.fetched.filter((model) => !configuredSet.has(model) && matches);
  modelTest.options = [];
  modelTest.active = -1;
  list.replaceChildren();
  if (configured.length) appendModelTestGroup(list, "已配置模型", configured, (model) => ({ mapped: modelTest.mapping[model] || "" }));
  if (upstream.length) appendModelTestGroup(list, "上游可用模型", upstream, () => ({}));
  const allModels = [...modelTest.configured, ...modelTest.fetched];
  const custom = query && !allModels.includes(query) && !allModels.some((model) => model.toLowerCase() === needle);
  // The custom row is a sibling option, so it stays a direct listbox child.
  if (custom) appendModelTestOption(list, query, { custom: true });
  if (!list.children.length) {
    const empty = document.createElement("p"); empty.className = "model-test-empty"; empty.setAttribute("role", "presentation");
    empty.textContent = query ? "没有匹配的模型。按 Enter 使用当前输入。" : "暂无可用模型，可直接在上方输入模型名称。";
    list.append(empty);
  }
  $("modelTestSearch").removeAttribute("aria-activedescendant");
}

function chooseModelTestModel(model, close = true) {
  const value = String(model || "").trim();
  if (!value) return;
  modelTest.selected = value;
  $("modelTestSearch").value = "";
  clearModelTestModelError();
  renderModelTestSelection();
  renderModelTestList();
  if (!close) return;
  setModelTestPanel(false);
  $("modelTestPickerButton").focus();
}

function moveModelTestActive(delta) {
  const total = modelTest.options.length;
  if (!total) return;
  if (modelTest.active < 0) modelTest.active = delta > 0 ? 0 : total - 1;
  else modelTest.active = (modelTest.active + delta + total) % total;
  modelTest.options.forEach((option, index) => option.button.classList.toggle("is-active", index === modelTest.active));
  const button = modelTest.options[modelTest.active].button;
  // Scroll the list only; scrollIntoView would also drag the scrolling dialog.
  const list = $("modelTestList");
  if (button.offsetTop < list.scrollTop) list.scrollTop = button.offsetTop;
  else if (button.offsetTop + button.offsetHeight > list.scrollTop + list.clientHeight) {
    list.scrollTop = button.offsetTop + button.offsetHeight - list.clientHeight;
  }
  $("modelTestSearch").setAttribute("aria-activedescendant", button.id);
}

function chooseActiveModelTestModel() {
  const query = $("modelTestSearch").value.trim();
  // An active option only exists after arrow-key navigation, so it always wins.
  if (modelTest.active >= 0 && modelTest.options[modelTest.active]) { chooseModelTestModel(modelTest.options[modelTest.active].value); return true; }
  const exact = query && [...modelTest.configured, ...modelTest.fetched].find((model) => model.toLowerCase() === query.toLowerCase());
  if (exact) { chooseModelTestModel(exact); return true; }
  if (query) { chooseModelTestModel(query); return true; }
  // No query and no active option: keep the current selection rather than
  // silently switching to the first model in the list.
  return false;
}

function showModelTestModelError(message) {
  $("modelTestModelError").textContent = message;
  $("modelTestModelError").classList.remove("hidden");
  $("modelTestPicker").classList.add("has-error");
}

function clearModelTestModelError() {
  $("modelTestModelError").textContent = "";
  $("modelTestModelError").classList.add("hidden");
  $("modelTestPicker").classList.remove("has-error");
}

function closeModelTest() {
  $("modelTestDialog").close();
  modelTest.controller?.abort(); modelTest.probeController?.abort();
  modelTest.controller = null; modelTest.probeController = null;
  modelTest.open = false;
  modelTest.active = -1;
  modelTest.pointerInside = false;
  $("modelTestPickerPanel").classList.add("hidden");
  $("modelTestPickerPanel").classList.remove("is-above");
  $("modelTestPickerPanel").style.maxHeight = "";
  $("modelTestPicker").classList.remove("is-open");
  $("modelTestPickerButton").setAttribute("aria-expanded", "false");
  $("modelTestSearch").setAttribute("aria-expanded", "false");
  $("modelTestSearch").removeAttribute("aria-activedescendant");
  $("modelTestFields").disabled = false;
  $("modelTestForm").setAttribute("aria-busy", "false");
  $("modelTestProgress").textContent = "";
  setBusy($("modelTestFetch"), false); setBusy($("modelTestSend"), false);
  syncModalLock();
}

async function fetchModelTestModels() {
  if (modelTest.probeController) return;
  const controller = new AbortController(); modelTest.probeController = controller;
  const id = modelTest.id;
  // The gateway probes the saved upstream, so cache against the configuration
  // that was current when the request went out, not a later edit.
  const signature = modelTestCacheSignature(upsCache.find((item) => item.id === id) || {});
  const configured = modelTest.configured.length ? "已配置 " + modelTest.configured.length + " 个模型 · " : "";
  setBusy($("modelTestFetch"), true, "");
  $("modelTestHint").textContent = configured + "正在读取上游模型…";
  try {
    const result = await api("api/upstreams/" + id + "/probe", { method: "POST", body: "{}", signal: controller.signal });
    if (controller.signal.aborted || id !== modelTest.id) return;
    if (!result.ok) throw new Error(result.error || "获取模型失败");
    const models = uniqueModelNames(result.models || []);
    modelTest.cache.set(id, { models, signature, at: Date.now() });
    modelTest.fetched = models;
    if (!modelTest.selected && models.length) modelTest.selected = models[0];
    renderModelTestSelection();
    renderModelTestList();
    setModelTestHint(models.length ? "" : "上游未返回模型；可直接输入模型名称。");
  } catch (error) {
    if (!controller.signal.aborted && error.name !== "AbortError" && id === modelTest.id) {
      $("modelTestHint").textContent = "上游模型读取失败：" + error.message + "。仍可直接输入模型名称。";
    }
  } finally {
    if (modelTest.probeController === controller) {
      modelTest.probeController = null; setBusy($("modelTestFetch"), false);
    }
  }
}

async function submitModelTest(event) {
  event.preventDefault();
  if (modelTest.controller) return;
  setModelTestPanel(false);
  const model = modelTest.selected.trim();
  if (!model) {
    showModelTestModelError("请选择或输入测试模型。");
    setModelTestPanel(true);
    return;
  }
  if (new TextEncoder().encode(model).length > 512) {
    showModelTestModelError("模型名称最多 512 字节。");
    return;
  }
  clearModelTestModelError();
  const controller = new AbortController(); modelTest.controller = controller;
  $("modelTestFields").disabled = true;
  $("modelTestResult").classList.add("hidden"); $("modelTestError").classList.add("hidden");
  $("modelTestForm").setAttribute("aria-busy", "true");
  $("modelTestProgress").textContent = "正在等待模型回复，最长等待 90 秒…";
  setBusy($("modelTestSend"), true, "测试中…");
  try {
    const result = await api("api/upstreams/" + modelTest.id + "/model-test", {
      method: "POST", signal: controller.signal,
      body: JSON.stringify({ model, message: $("modelTestMessage").value, max_tokens: Number($("modelTestMaxTokens").value), stream: $("modelTestStream").value === "true" })
    });
    if (controller.signal.aborted) return;
    if (!result.ok) throw new Error((result.error || "模型测试失败") + "（耗时 " + result.latency_ms + " ms）");
    $("modelTestSummary").textContent = "测试成功 · " + (result.stream ? "流式" : "非流式") + " · " + result.latency_ms + " ms · " + result.upstream_model + (result.input_tokens != null ? " · 输入 " + result.input_tokens + " Token" : "") + (result.output_tokens != null ? " · 输出 " + result.output_tokens + " Token" : "");
    $("modelTestReply").textContent = result.reply || "模型未返回文本。结束原因：" + (result.finish_reason || "未知") + "。可增加最大输出 Token 后重试。";
    $("modelTestThinking").textContent = result.thinking || "";
    $("modelTestThinkingDetails").classList.toggle("hidden", !result.thinking);
    $("modelTestThinkingDetails").open = false;
    $("modelTestResult").classList.remove("hidden");
  } catch (error) {
    if (!controller.signal.aborted && error.name !== "AbortError") {
      $("modelTestError").textContent = error.message;
      $("modelTestError").classList.remove("hidden");
    }
  } finally {
    if (modelTest.controller === controller) {
      modelTest.controller = null; $("modelTestFields").disabled = false;
      $("modelTestForm").setAttribute("aria-busy", "false");
      $("modelTestProgress").textContent = ""; setBusy($("modelTestSend"), false);
    }
  }
}

function initModelTest() {
  $("modelTestForm").addEventListener("submit", submitModelTest);
  $("modelTestFetch").addEventListener("click", () => fetchModelTestModels());
  $("modelTestPickerButton").addEventListener("click", () => {
    if (modelTest.open) { setModelTestPanel(false); return; }
    $("modelTestSearch").value = "";
    setModelTestPanel(true);
  });
  $("modelTestPickerButton").addEventListener("keydown", (event) => {
    if (!["ArrowDown", "ArrowUp"].includes(event.key)) return;
    event.preventDefault();
    $("modelTestSearch").value = "";
    setModelTestPanel(true);
  });
  $("modelTestSearch").addEventListener("input", () => { renderModelTestList(); clearModelTestModelError(); });
  $("modelTestSearch").addEventListener("keydown", (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault(); moveModelTestActive(event.key === "ArrowDown" ? 1 : -1);
    } else if (event.key === "Enter") {
      event.preventDefault(); chooseActiveModelTestModel();
    } else if (event.key === "Escape") {
      event.preventDefault(); event.stopPropagation();
      setModelTestPanel(false); $("modelTestPickerButton").focus();
    } else if (event.key === "Tab") {
      setModelTestPanel(false);
    }
  });
  $("modelTestDialog").addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || !modelTest.open) return;
    event.preventDefault(); event.stopPropagation();
    setModelTestPanel(false); $("modelTestPickerButton").focus();
  }, true);
  document.addEventListener("pointerdown", (event) => {
    if (!modelTest.open) return;
    if (event.target.closest("#modelTestPicker")) { modelTest.pointerInside = true; return; }
    modelTest.pointerInside = false;
    $("modelTestSearch").value = "";
    renderModelTestList();
    setModelTestPanel(false);
  }, true);
  document.addEventListener("pointerup", () => { modelTest.pointerInside = false; }, true);
  $("modelTestPicker").addEventListener("focusout", (event) => {
    if (!modelTest.open || event.relatedTarget?.closest("#modelTestPicker")) return;
    // Clicking a non-focusable part of the panel (hint, group title) blurs the
    // search field; keep the panel open until the click completes.
    if (!event.relatedTarget && modelTest.pointerInside) return;
    setModelTestPanel(false);
  });
  $("modelTestDialog").addEventListener("cancel", (event) => { event.preventDefault(); closeModelTest(); });
  $("modelTestDialog").addEventListener("close", syncModalLock);
  // Scrolling or resizing the dialog moves the picker, so refit the panel.
  $("modelTestDialog").addEventListener("scroll", positionModelTestPanel, { passive: true });
  window.addEventListener("resize", () => { if (modelTest.open) positionModelTestPanel(); });
}

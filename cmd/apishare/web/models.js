/* Upstream model tags and one-hop model mapping editor. */
const modelEditor = { mode: "visual" };

function modelNames(value) { return [...new Set(value.split(/[,，\r\n]+/).map((name) => name.trim()).filter(Boolean))]; }

function renderModelChips() {
  const box = $("selectedModelChips");
  box.replaceChildren();
  modelNames($("editModels").value).forEach((model) => {
    const chip = document.createElement("span"); chip.className = "model-chip";
    const text = document.createElement("span"); text.textContent = model;
    const button = document.createElement("button"); button.type = "button";
    button.setAttribute("aria-label", "移除模型 " + model); button.innerHTML = icon("close");
    button.addEventListener("click", () => {
      commitModelInput();
      editProbe.selected.delete(model);
      syncEditModels(); renderEditProbeList(); $("editModelInput").focus();
    });
    chip.append(text, button); box.append(chip);
  });
  $("editModelInput").placeholder = editProbe.selected.size ? "继续添加模型…" : "输入模型名，按回车添加";
  $("selectedModelCount").textContent = editProbe.selected.size ? "已添加 " + editProbe.selected.size + " 个模型" : "留空允许全部模型";
  refreshModelSuggestions();
}

function commitModelInput() {
  const field = $("editModelInput");
  if (!field.value.trim()) { field.value = ""; return; }
  modelNames(field.value).forEach((model) => editProbe.selected.add(model));
  field.value = "";
  syncEditModels(); renderEditProbeList();
}

function syncEditModels() {
  $("editModels").value = [...editProbe.selected].join(",");
  renderModelChips(); updateEditorDirty();
}

function initModelEditor() {
  const field = $("editModelInput");
  field.addEventListener("keydown", (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (["Enter", ",", "，"].includes(event.key) && !event.ctrlKey && !event.metaKey) {
      event.preventDefault(); commitModelInput();
    } else if (event.key === "Backspace" && !field.value && editProbe.selected.size) {
      event.preventDefault();
      $("selectedModelChips").lastElementChild?.querySelector("button").focus();
    }
  });
  field.addEventListener("input", (event) => {
    if (!event.isComposing && /[,，\r\n]/.test(field.value)) commitModelInput();
  });
  field.addEventListener("compositionend", () => { if (/[,，\r\n]/.test(field.value)) commitModelInput(); });
  field.addEventListener("paste", (event) => {
    const value = event.clipboardData?.getData("text");
    if (!value || !/[,，\r\n]/.test(value)) return;
    event.preventDefault();
    field.setRangeText(value.replace(/\r?\n/g, ","), field.selectionStart, field.selectionEnd, "end");
    commitModelInput();
  });
  field.addEventListener("blur", (event) => {
    if (!event.relatedTarget?.closest("#selectedModelChips")) commitModelInput();
  });
  $("modelTagInput").addEventListener("click", (event) => {
    if (event.target === $("modelTagInput") || event.target === $("selectedModelChips")) field.focus();
  });
}

function resetModelMappingEditor() {
  $("modelMapRows").replaceChildren();
  $("editModelInput").value = "";
  $("modelMapBatch").value = "";
  $("modelMapBatchArea").classList.add("hidden");
  $("modelMapBatchToggle").setAttribute("aria-expanded", "false");
  $("modelMapBatchError").classList.add("hidden");
  modelEditor.mode = "json";
  // Invalid legacy values stay visible in JSON mode for repair.
  if (!setModelMapMode("visual")) setModelMapMode("json");
}

function readModelMappingJSON() {
  const text = $("editModelMap").value.trim();
  if (!text) return Object.create(null);
  const mapping = JSON.parse(text);
  if (!mapping || Array.isArray(mapping) || typeof mapping !== "object") throw new Error("请填写 JSON 对象。");
  const result = Object.create(null);
  if (Object.entries(mapping).some(([source, value]) => !source.trim() || typeof value !== "string" || !value.trim())) throw new Error("请求模型和上游模型必须是非空字符串。");
  // The object is flat and contains only strings. Read its string tokens too,
  // since JSON.parse alone silently discards duplicate source keys.
  const tokens = text.match(/"(?:[^"\\]|\\.)*"/g) || [];
  for (let index = 0; index < tokens.length; index += 2) {
    const source = JSON.parse(tokens[index]), value = JSON.parse(tokens[index + 1]);
    if (Object.hasOwn(result, source.trim())) throw new Error("请求模型「" + source.trim() + "」存在重复映射。");
    result[source.trim()] = value.trim();
  }
  return result;
}

function setModelMapError(message) {
  $("modelMapError").textContent = message;
  $("modelMapError").classList.toggle("hidden", !message);
  $("editModelMap").setCustomValidity(modelEditor.mode === "json" ? message : "");
  $("editModelMap").setAttribute("aria-invalid", String(!!message));
}

function modelMappingRows() { return [...$("modelMapRows").children]; }
function modelMappingDraft() { return modelMappingRows().map((row) => [...row.querySelectorAll("input")].map((field) => field.value)); }
function modelMappingDirtyValue() {
  let entries;
  if (modelEditor.mode === "visual") entries = modelMappingDraft().map((row) => row.map((value) => value.trim())).filter((row) => row.some(Boolean));
  else {
    try { entries = Object.entries(readModelMappingJSON()); }
    catch { return $("editModelMap").value; }
  }
  return entries.sort(([a, av], [b, bv]) => a.localeCompare(b) || av.localeCompare(bv));
}

function appendModelMapRow(source = "", target = "", focus = true) {
  const row = document.createElement("div"); row.className = "model-map-row";
  for (const [side, value, list, label] of [["source", source, "requestModelOptions", "请求模型名称"], ["target", target, "upstreamModelOptions", "上游模型名称"]]) {
    const field = document.createElement("input");
    field.className = "model-map-" + side; field.value = value; field.spellcheck = false;
    field.setAttribute("list", list); field.setAttribute("aria-label", label);
    field.setAttribute("aria-describedby", "modelMapHint modelMapError");
    field.placeholder = side === "source" ? "选择或输入请求模型" : "选择或输入上游模型";
    field.addEventListener("input", () => { validateModelMap(); updateEditorDirty(); });
    const wrapper = document.createElement("label"); wrapper.className = "model-map-field model-map-" + side + "-field";
    const caption = document.createElement("span"); caption.className = "model-map-mobile-label"; caption.textContent = label;
    wrapper.append(caption, field); row.append(wrapper);
    if (side === "source") { const arrow = document.createElement("span"); arrow.className = "model-map-arrow"; arrow.textContent = "→"; arrow.setAttribute("aria-hidden", "true"); row.append(arrow); }
  }
  const remove = document.createElement("button"); remove.type = "button"; remove.className = "icon-btn danger";
  remove.setAttribute("aria-label", "删除此模型映射"); remove.innerHTML = icon("trash");
  remove.addEventListener("click", () => {
    const next = row.nextElementSibling || row.previousElementSibling;
    row.remove(); validateModelMap(); updateEditorDirty();
    (next?.querySelector("input") || $("addModelMapButton")).focus();
  });
  row.append(remove); $("modelMapRows").append(row);
  if (focus) row.querySelector("input").focus();
}

function addModelMapRow() { appendModelMapRow(); updateEditorDirty(); }

function validateModelMap() {
  if (modelEditor.mode === "json") {
    try { const mapping = readModelMappingJSON(); $("modelMapCount").textContent = Object.keys(mapping).length + " 条规则"; setModelMapError(""); return true; }
    catch (error) { setModelMapError(error instanceof SyntaxError ? 'JSON 格式不正确，例如：{"gpt-4o":"deepseek-chat"}' : error.message); return false; }
  }
  const mapping = Object.create(null);
  let error = "";
  modelMappingRows().forEach((row) => {
    const source = row.querySelector(".model-map-source"), target = row.querySelector(".model-map-target");
    source.setCustomValidity(""); target.setCustomValidity("");
    source.removeAttribute("aria-invalid"); target.removeAttribute("aria-invalid");
    const key = source.value.trim(), value = target.value.trim();
    if (!key && !value) return;
    let message = "", invalid = source;
    if (!key) message = "请填写请求模型名称。";
    else if (!value) { message = "请填写上游模型名称。"; invalid = target; }
    else if (Object.hasOwn(mapping, key)) message = "请求模型「" + key + "」存在重复映射。";
    if (message) { invalid.setCustomValidity(message); invalid.setAttribute("aria-invalid", "true"); error ||= message; }
    else mapping[key] = value;
  });
  if (!error) $("editModelMap").value = Object.keys(mapping).length ? JSON.stringify(mapping, null, 2) : "";
  $("modelMapCount").textContent = Object.keys(mapping).length + " 条规则";
  setModelMapError(error);
  return !error;
}

function setModelMapMode(mode) {
  let mapping;
  if (mode !== modelEditor.mode && !validateModelMap()) return false;
  if (mode === "visual" && modelEditor.mode !== "visual") {
    try { mapping = readModelMappingJSON(); } catch { return false; }
    $("modelMapRows").replaceChildren();
    Object.entries(mapping).forEach(([source, target]) => appendModelMapRow(source, target, false));
    if (!Object.keys(mapping).length) appendModelMapRow("", "", false);
  }
  modelEditor.mode = mode;
  $("modelMapVisual").classList.toggle("hidden", mode !== "visual");
  $("modelMapJSON").classList.toggle("hidden", mode !== "json");
  for (const current of ["visual", "json"]) {
    const button = $(current === "visual" ? "modelMapVisualTab" : "modelMapJSONTab");
    button.classList.toggle("active", mode === current); button.setAttribute("aria-pressed", String(mode === current));
  }
  validateModelMap(); refreshModelSuggestions();
  return true;
}

function refreshModelSuggestions() {
  let mapping = {};
  try { mapping = readModelMappingJSON(); } catch { /* Keep existing selections while repairing JSON. */ }
  const selected = modelNames($("editModels").value);
  const fill = (id, values) => {
    $(id).replaceChildren(...[...new Set(values)].sort().map((value) => { const option = document.createElement("option"); option.value = value; return option; }));
  };
  fill("requestModelOptions", [...selected, ...Object.keys(mapping)]);
  fill("upstreamModelOptions", [...editProbe.models, ...selected, ...Object.values(mapping)]);
}

function toggleModelMapBatch() {
  const opening = $("modelMapBatchArea").classList.contains("hidden");
  $("modelMapBatchArea").classList.toggle("hidden", !opening);
  $("modelMapBatchToggle").setAttribute("aria-expanded", String(opening));
  if (opening) $("modelMapBatch").focus();
}

function applyModelMapBatch() {
  if (!validateModelMap()) return false;
  try {
    const mapping = readModelMappingJSON();
    for (const line of $("modelMapBatch").value.split(/\r?\n/).filter((line) => line.trim())) {
      const pair = line.split(/\s*(?:->|=>|→|=)\s*/);
      if (pair.length !== 2 || !pair[0].trim() || !pair[1].trim()) throw new Error("每行填写：请求模型 = 上游模型。");
      const [source, target] = pair.map((value) => value.trim());
      if (Object.hasOwn(mapping, source) && mapping[source] !== target) throw new Error("请求模型「" + source + "」已有不同的映射，请先修改或删除原规则。");
      mapping[source] = target;
    }
    $("editModelMap").value = Object.keys(mapping).length ? JSON.stringify(mapping, null, 2) : "";
    // Rebuild visual rows from the merged mapping without overwriting it.
    modelEditor.mode = "json"; setModelMapMode("visual");
    $("modelMapBatch").value = "";
    $("modelMapBatchError").classList.add("hidden");
    $("modelMapBatchArea").classList.add("hidden");
    $("modelMapBatchToggle").setAttribute("aria-expanded", "false");
    updateEditorDirty(); return true;
  } catch (error) {
    $("modelMapBatchError").textContent = error.message; $("modelMapBatchError").classList.remove("hidden");
    $("modelMapBatchArea").classList.remove("hidden");
    $("modelMapBatchToggle").setAttribute("aria-expanded", "true");
    $("modelMapBatch").focus(); return false;
  }
}

function fillModelMapTemplate() {
  $("modelMapBatchArea").classList.remove("hidden");
  $("modelMapBatchToggle").setAttribute("aria-expanded", "true");
  if (!$("modelMapBatch").value.trim()) $("modelMapBatch").value = "gpt-4o = gpt-4o-2024-08-06\ngpt-4o-mini = gpt-4o-mini-2024-07-18";
  $("modelMapBatch").focus(); updateEditorDirty();
}

/* Supplier credentials: masked edits, sticky routing and per-key proxy choices. */
let editorAPIKeys = [];

function newEditorAPIKeyID() {
  return Array.from(crypto.getRandomValues(new Uint8Array(12)), byte => byte.toString(16).padStart(2, "0")).join("");
}
function editorUsesAPIKeys() {
  return $("editKeyMode").value === "multiple" && editorUpstreamType() !== "cliproxy";
}
function readEditorAPIKeys() {
  $("editAPIKeys").querySelectorAll("[data-upstream-key]").forEach((row, index) => {
    const key = editorAPIKeys[index];
    key.name = row.querySelector("[data-key-name]").value.trim();
    key.api_key = row.querySelector("[data-key-secret]").value.trim();
    const proxy = Number(row.querySelector("[data-key-proxy]").value);
    key.proxy_id = proxy < 0 ? null : proxy;
    key.disabled = !row.querySelector("[data-key-enabled]").checked;
  });
  return editorAPIKeys.map(({ id, name, api_key, proxy_id, disabled }) => ({ id, name, api_key, proxy_id, disabled }));
}
function editorAPIKeysPayload() {
  return editorUsesAPIKeys() ? readEditorAPIKeys() : [];
}
function prepareAPIKeysEditor(upstream) {
  editorAPIKeys = (upstream?.api_keys || []).map(key => ({ ...key, api_key: "" }));
  $("editKeyMode").value = editorAPIKeys.length ? "multiple" : "single";
  $("editKeyProxyMode").value = upstream?.key_proxy_mode || "shared";
  $("editKeyBatch").value = "";
  $("editKeyBatchDetails").open = false;
  renderEditorAPIKeys();
}
function keyProxyOptions(selected) {
  let html = '<option value="-1">跟随供应商默认代理</option><option value="0">默认连接（跟随系统环境）</option>';
  proxiesCache.forEach(proxy => {
    html += '<option value="' + proxy.id + '"' + (proxy.enabled ? "" : " disabled") + '>' + esc(proxyName(proxy) + (proxy.enabled ? "" : "（已停用）")) + '</option>';
  });
  if (selected > 0 && !proxiesCache.some(proxy => proxy.id === selected)) html += '<option value="' + selected + '" disabled>代理 #' + selected + '（不可用）</option>';
  return html;
}
function renderEditorAPIKeys() {
  const individual = $("editKeyProxyMode").value === "individual";
  $("editAPIKeys").innerHTML = editorAPIKeys.map((key, index) => {
    const label = key.name || "密钥 " + (index + 1);
    const cooling = key.cooldown_until && new Date(key.cooldown_until) > new Date();
    const state = key.disabled ? "已停用" : cooling ? "冷却至 " + fmtTime(key.cooldown_until) : key.healthy === false ? "连接不可用" : key.key_masked ? "可用" : "待保存";
    return '<div class="upstream-key-editor" data-upstream-key="' + index + '"><div class="upstream-key-editor-heading"><strong>密钥 ' + (index + 1) + '</strong><small' + (cooling ? ' class="upstream-key-cooling"' : "") + '>' + esc(state) + '</small><label class="upstream-key-enabled"><input type="checkbox" data-key-enabled aria-label="启用' + esc(label) + '"' + (key.disabled ? "" : " checked") + '>启用</label><button type="button" class="icon-btn danger" data-key-remove="' + index + '" aria-label="移除' + esc(label) + '" title="移除密钥">' + icon("trash") + '</button></div><div class="form-grid"><label class="field"><span>备注名称 <span class="optional">可选</span></span><input data-key-name maxlength="120" value="' + esc(key.name || "") + '" placeholder="例如：主账号"></label><label class="field"><span>API 密钥' + (key.key_masked ? " · " + esc(key.key_masked) : ' <span class="required">*</span>') + '</span><span class="secret-field"><input data-key-secret type="password" autocomplete="off" spellcheck="false" value="' + esc(key.api_key || "") + '" placeholder="' + (key.key_masked ? "留空保留已保存密钥" : "输入上游 API 密钥") + '"' + (key.key_masked ? "" : " required") + '><button type="button" class="icon-btn" data-key-reveal aria-label="显示' + esc(label) + '" aria-pressed="false">' + icon("eye") + '</button></span></label></div><label class="field upstream-key-proxy' + (individual ? "" : " hidden") + '"><span>此密钥的请求代理</span><select data-key-proxy aria-label="' + esc(label) + '的请求代理">' + keyProxyOptions(key.proxy_id) + '</select></label></div>';
  }).join("");
  $("editAPIKeys").querySelectorAll("[data-key-proxy]").forEach((select, index) => { select.value = String(editorAPIKeys[index].proxy_id ?? -1); });
  refreshProbeKeyOptions();
  updateAPIKeyControls();
}
function refreshProbeKeyOptions() {
  const select = $("editProbeKey"), previous = select.value;
  select.replaceChildren();
  editorAPIKeys.forEach((key, index) => {
    const option = new Option((key.name || "密钥 " + (index + 1)) + (key.disabled ? "（已停用）" : ""), key.id);
    option.disabled = !!key.disabled;
    select.add(option);
  });
  if ([...select.options].some(option => option.value === previous && !option.disabled)) select.value = previous;
  else select.value = editorAPIKeys.find(key => !key.disabled)?.id || "";
}
function updateAPIKeyControls() {
  const supported = editorUpstreamType() !== "cliproxy", multiple = editorUsesAPIKeys();
  $("editKeyModeField").classList.toggle("hidden", !supported);
  $("editSingleKeyField").classList.toggle("hidden", multiple);
  $("editAPIKeysPanel").classList.toggle("hidden", !multiple || !supported);
  $("editKey").disabled = multiple || !supported;
  if (multiple) $("editKey").required = false;
  $("editAPIKeys").querySelectorAll("input,select,button").forEach(field => { field.disabled = !multiple; });
  $("editProbeKeyField").classList.toggle("hidden", !multiple);
  $("editProbeKey").disabled = !multiple;
  $("editProxyLabel").textContent = multiple ? ($("editKeyProxyMode").value === "individual" ? "供应商默认代理" : "全部密钥统一代理") : "请求代理";
  if (multiple) $("probeEditorButton").innerHTML = icon("refresh") + " 获取所选密钥模型";
  if (multiple) $("clineLoginCard").classList.add("hidden");
}
function changeAPIKeyMode() {
  const multiple = editorUsesAPIKeys();
  if (multiple) {
    if (!editorAPIKeys.length) {
      const saved = editProbe.id !== null;
      const upstream = upsCache.find(item => item.id === editProbe.id);
      editorAPIKeys.push({ id: saved ? "legacy" : newEditorAPIKeyID(), name: "", api_key: $("editKey").value.trim(), proxy_id: null, disabled: false, key_masked: saved ? upstream?.key_masked || "***" : "" });
    } else if ($("editKey").value.trim()) editorAPIKeys[0].api_key = $("editKey").value.trim();
    $("editKey").value = "";
    renderEditorAPIKeys();
  } else {
    readEditorAPIKeys();
    if (editorAPIKeys[0]?.api_key) $("editKey").value = editorAPIKeys[0].api_key;
    updateEditorProvider(false);
  }
  updateAPIKeyControls(); clearProbeResults(); updateEditorDirty();
}
function changeAPIKeyProxyMode() {
  readEditorAPIKeys(); renderEditorAPIKeys(); clearProbeResults(); updateEditorDirty();
}
function addEditorAPIKey() {
  readEditorAPIKeys();
  editorAPIKeys.push({ id: newEditorAPIKeyID(), name: "", api_key: "", proxy_id: null, disabled: false });
  renderEditorAPIKeys(); clearProbeResults(); updateEditorDirty();
  $("editAPIKeys").lastElementChild.querySelector("[data-key-secret]").focus();
}
function addBatchAPIKeys() {
  readEditorAPIKeys();
  const keys = [...new Set($("editKeyBatch").value.split(/\r?\n/).map(value => value.trim()).filter(Boolean))];
  if (keys.length) editorAPIKeys = editorAPIKeys.filter(key => key.api_key || key.key_masked || key.name);
  const existing = new Set(editorAPIKeys.map(key => key.api_key).filter(Boolean));
  keys.filter(key => !existing.has(key)).forEach(api_key => {
    editorAPIKeys.push({ id: newEditorAPIKeyID(), name: "", api_key, proxy_id: null, disabled: false });
  });
  $("editKeyBatch").value = "";
  renderEditorAPIKeys(); clearProbeResults(); updateEditorDirty();
}
function validateEditorAPIKeys() {
  if (!editorUsesAPIKeys()) return true;
  readEditorAPIKeys();
  let message = "";
  if (!editorAPIKeys.length) message = "请至少添加一把密钥。";
  else if (editorAPIKeys.length > 256) message = "每个供应商最多配置 256 把密钥。";
  else if (new Set(editorAPIKeys.map(key => key.api_key).filter(Boolean)).size !== editorAPIKeys.filter(key => key.api_key).length) message = "请移除重复的密钥。";
  $("editAPIKeysError").textContent = message;
  $("editAPIKeysError").classList.toggle("hidden", !message);
  if (message) { jumpEditor("editorConnection"); return false; }
  const invalid = $("editAPIKeys").querySelector(":invalid");
  if (invalid) { jumpEditor("editorConnection"); invalid.reportValidity(); return false; }
  return true;
}
function addAPIKeysToRequest(body, probe = false) {
  body.api_keys = editorAPIKeysPayload();
  body.key_proxy_mode = $("editKeyProxyMode").value;
  if (editorUsesAPIKeys()) {
    delete body.api_key;
    if (probe) body.probe_key_id = $("editProbeKey").value;
  }
}
$("editAPIKeys").addEventListener("click", event => {
  const remove = event.target.closest("[data-key-remove]");
  if (remove) {
    readEditorAPIKeys(); editorAPIKeys.splice(Number(remove.dataset.keyRemove), 1);
    renderEditorAPIKeys(); clearProbeResults(); updateEditorDirty();
    const rows = $("editAPIKeys").querySelectorAll("[data-key-remove]");
    (rows[Math.min(Number(remove.dataset.keyRemove), rows.length - 1)] || $("addAPIKeyButton")).focus();
  }
  const reveal = event.target.closest("[data-key-reveal]");
  if (reveal) {
    const field = reveal.previousElementSibling, visible = field.type === "password";
    field.type = visible ? "text" : "password";
    reveal.setAttribute("aria-pressed", String(visible));
    reveal.setAttribute("aria-label", (visible ? "隐藏" : "显示") + "密钥");
  }
});
$("editAPIKeys").addEventListener("input", () => { readEditorAPIKeys(); clearProbeResults(); });
$("editAPIKeys").addEventListener("change", () => { readEditorAPIKeys(); refreshProbeKeyOptions(); clearProbeResults(); });
$("editProbeKey").addEventListener("change", clearProbeResults);

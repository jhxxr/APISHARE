const requestRedactionLabels = {
  api_key: "API Key 与令牌",
  email: "邮箱",
  phone: "手机号",
  id_card: "身份证",
  bank_card: "银行卡",
  ipv4: "IPv4 地址",
};
const requestRedactionSample = JSON.stringify({
  model: "example-model",
  messages: [{ role: "user", content: "联系 jane@example.com，电话 13812345678" }],
}, null, 2);
let requestRedactionState = null;

function renderRequestRedactionPolicy(data) {
  const policy = data.policy;
  requestRedactionState = { policy, keys: data.ordinary_keys || [] };
  $("requestRedactionMode").value = policy.mode || "off";
  $("requestRedactionCoverage").value = policy.coverage || "text";
  $("requestRedactionRevision").textContent = String(policy.revision ?? 0);
  const selectedKeys = new Set(policy.scope?.key_ids || []);
  $("requestRedactionKeys").innerHTML = requestRedactionState.keys.length
    ? requestRedactionState.keys.map((key) => '<label><input type="checkbox" data-redaction-key="' + Number(key.id) + '"' + (selectedKeys.has(Number(key.id)) ? " checked" : "") + '><span>' + esc(key.name || "Key #" + key.id) + '</span></label>').join("")
    : '<span class="muted">当前没有普通 API Key。</span>';
  $("requestRedactionModels").value = (policy.scope?.models || []).join("\n");
  $("requestRedactionCategories").innerHTML = Object.entries(requestRedactionLabels).map(([id, label]) => '<label class="request-redaction-category"><input type="checkbox" data-redaction-category="' + id + '"' + ((policy.categories || []).includes(id) ? " checked" : "") + '><span>' + label + '</span></label>').join("");
  renderRequestRedactionReplacements(policy.categories || [], policy.replacements || {});
  $("requestRedactionRules").value = JSON.stringify(policy.rules || [], null, 2);
  $("requestRedactionOverlays").value = JSON.stringify(policy.channel_overlays || [], null, 2);
  $("requestRedactionSample").value = requestRedactionSample;
  $("requestRedactionStatus").textContent = "已加载全局策略；仅作用于普通 API Key。个人设置独立。";
  $("saveRequestRedaction").disabled = false;
}

function renderRequestRedactionReplacements(categories, replacements) {
  $("requestRedactionReplacements").innerHTML = categories.map((category) => '<label class="field"><span>' + esc(requestRedactionLabels[category] || category) + ' 替换文本</span><input data-redaction-replacement="' + esc(category) + '" maxlength="64" value="' + esc(replacements[category] || "[" + category.toUpperCase() + "]") + '"></label>').join("");
}

async function loadRequestRedactionPolicy() {
  const status = $("requestRedactionStatus");
  const saveButton = $("saveRequestRedaction");
  if (!status || !saveButton) return;
  saveButton.disabled = true;
  status.textContent = "正在加载脱敏策略…";
  try {
    renderRequestRedactionPolicy(await api("api/request-processing/redaction"));
  } catch (error) {
    requestRedactionState = null;
    status.textContent = "脱敏策略加载失败：" + error.message;
  }
}

function readRequestRedactionDraft() {
  if (!requestRedactionState) throw new Error("请先加载脱敏策略");
  let rules, channelOverlays;
  try { rules = JSON.parse($("requestRedactionRules").value || "[]"); }
  catch { throw new Error("自定义规则 JSON 格式无效"); }
  try { channelOverlays = JSON.parse($("requestRedactionOverlays").value || "[]"); }
  catch { throw new Error("渠道追加规则 JSON 格式无效"); }
  if (!Array.isArray(rules) || !Array.isArray(channelOverlays)) throw new Error("规则必须是 JSON 数组");
  const policy = structuredClone(requestRedactionState.policy);
  policy.mode = $("requestRedactionMode").value;
  policy.coverage = $("requestRedactionCoverage").value;
  policy.scope = policy.scope || {};
  const visibleKeys = new Set(requestRedactionState.keys.map((key) => Number(key.id)));
  policy.scope.key_ids = [...new Set([
    ...(requestRedactionState.policy.scope?.key_ids || []).filter((id) => !visibleKeys.has(Number(id))),
    ...[...document.querySelectorAll("[data-redaction-key]:checked")].map((item) => Number(item.dataset.redactionKey)),
  ])];
  policy.scope.models = $("requestRedactionModels").value.split(/[\r\n,]+/).map((value) => value.trim()).filter(Boolean);
  policy.categories = [...document.querySelectorAll("[data-redaction-category]:checked")].map((item) => item.dataset.redactionCategory);
  policy.replacements = Object.fromEntries([...document.querySelectorAll("[data-redaction-replacement]")].map((item) => [item.dataset.redactionReplacement, item.value]));
  policy.rules = rules;
  policy.channel_overlays = channelOverlays;
  policy.headers = { strip_forwarding_identity: true, scan_custom_text: true, ...(policy.headers || {}) };
  return policy;
}

async function saveRequestRedactionPolicy() {
  const button = $("saveRequestRedaction");
  try {
    const policy = readRequestRedactionDraft();
    setBusy(button, true, "正在保存…");
    const saved = await api("api/request-processing/redaction", { method: "PUT", body: JSON.stringify(policy) });
    await loadRequestRedactionPolicy();
    toast("请求脱敏策略已保存 · 修订 " + saved.revision);
  } catch (error) { reportError(error, "保存脱敏策略失败"); }
  finally { if (button) setBusy(button, false); }
}

async function dryRunRequestRedaction() {
  const output = $("requestRedactionResult");
  const button = $("requestRedactionDryRun");
  try {
    const policy = readRequestRedactionDraft();
    const sample = JSON.parse($("requestRedactionSample").value);
    setBusy(button, true, "正在试运行…");
    const result = await api("api/request-processing/redaction/dry-run", {
      method: "POST", body: JSON.stringify({ format: $("requestRedactionFormat").value, body: sample, policy }),
    });
    output.textContent = JSON.stringify(result, null, 2);
  } catch (error) {
    output.textContent = error.message;
  } finally { setBusy(button, false); }
}

function bindRequestRedactionEvents() {
$("requestRedactionCategories")?.addEventListener("change", () => {
  const policy = requestRedactionState?.policy;
  if (!policy) return;
  const selected = [...document.querySelectorAll("[data-redaction-category]:checked")].map((item) => item.dataset.redactionCategory);
  const replacements = { ...(policy.replacements || {}) };
  for (const item of document.querySelectorAll("[data-redaction-replacement]")) replacements[item.dataset.redactionReplacement] = item.value;
  renderRequestRedactionReplacements(selected, replacements);
});
document.querySelector(".request-redaction-admin")?.addEventListener("input", () => {
  if (requestRedactionState) $("requestRedactionStatus").textContent = "有未保存修改；试运行使用草稿，保存成功后才生效。";
});
$("saveRequestRedaction")?.addEventListener("click", saveRequestRedactionPolicy);
$("requestRedactionDryRun")?.addEventListener("click", dryRunRequestRedaction);
}

// app.js defines shared helpers after this script has loaded.
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", bindRequestRedactionEvents, { once: true });
} else {
  bindRequestRedactionEvents();
}

/* Per-upstream request header configuration. */
const transportHeaderNames = new Set(["host", "content-length", "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade"]);
const headerNamePattern = /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/;
const clientVersionState = { clients: [], promise: null, error: "" };
function validateHeaderOverrides() {
  const field = $("editHeaderOverrides"), raw = field.value.trim();
  let error = "";
  if (raw) {
    try {
      const headers = JSON.parse(raw), names = new Set();
      if (!headers || Array.isArray(headers) || typeof headers !== "object") throw new Error("请填写 JSON 对象，值支持字符串、true 或 false。");
      if (new TextEncoder().encode(raw).length > 65536) throw new Error("请求头配置不能超过 64 KiB。");
      for (const [name, value] of Object.entries(headers)) {
        const lower = name.toLowerCase(), pattern = name.startsWith("re:");
        if (names.has(lower)) throw new Error("请求头名称不能重复（不区分大小写）。");
        names.add(lower);
        if (name === "*" || pattern) {
          if (pattern && name.length === 3) throw new Error("请求头正则表达式不能为空。");
          if (typeof value !== "boolean") throw new Error("通配符和正则规则的值必须是 true 或 false。");
        } else {
          if (!headerNamePattern.test(name)) throw new Error("请求头名称 “" + name + "” 无效。");
          if (transportHeaderNames.has(lower)) throw new Error("请求头 “" + name + "” 由 HTTP 连接自动管理，不能覆盖。");
          if (typeof value !== "string" && typeof value !== "boolean") throw new Error("请求头的值必须是字符串、true 或 false。");
          if (typeof value === "string") {
            if (/[\x00-\x08\x0a-\x1f\x7f]/.test(value)) throw new Error("请求头值不能包含换行或控制字符。");
            for (const match of value.matchAll(/\{client_header:([^{}]+)\}/g)) {
              if (!headerNamePattern.test(match[1])) throw new Error("变量中的客户端请求头名称无效。");
            }
          }
        }
      }
    } catch (cause) { error = cause instanceof SyntaxError ? "JSON 格式不正确，请检查引号、逗号与括号。" : cause.message; }
  }
  field.setCustomValidity(error);
  field.setAttribute("aria-invalid", String(!!error));
  $("headerOverridesError").textContent = error;
  $("headerOverridesError").classList.toggle("hidden", !error);
  $("headerJSONStatus").textContent = error ? "配置有误" : raw ? "JSON 有效" : "未配置";
  $("headerJSONStatus").classList.toggle("error", !!error);
  syncClientHeaderTemplate();
  return !error;
}

function syncClientHeaderTemplate() {
  let value = "";
  try {
    const headers = JSON.parse($("editHeaderOverrides").value.trim() || "{}");
    const key = Object.keys(headers || {}).find((name) => name.toLowerCase() === "user-agent");
    value = key ? headers[key] : "";
  } catch { /* Keep JSON validation beside the editor. */ }
  const id = value === "{codex_user_agent}" ? "codex" : value === "{claude_code_user_agent}" ? "claude_code" : "";
  $("clientHeaderTemplate").value = id;
  const client = clientVersionState.clients.find((item) => item.id === id);
  $("clientHeaderPreview").classList.toggle("hidden", !id);
  $("clientHeaderUserAgent").textContent = client?.user_agent || "等待版本检测…";
}

function renderClientHeaderVersions() {
  for (const id of ["codex", "claude_code"]) {
    const option = $("clientHeaderTemplate").querySelector('option[value="' + id + '"]');
    const client = clientVersionState.clients.find((item) => item.id === id);
    const name = id === "codex" ? "Codex" : "Claude Code";
    option.disabled = !client?.version;
    option.textContent = name + " 模板" + (client?.version ? " · " + client.version + (client.error ? "（缓存）" : "") : " · 尚未获取版本");
  }
  const errors = clientVersionState.clients.filter((item) => item.error);
  const checked = clientVersionState.clients.filter((item) => item.version).map((item) => new Date(item.checked_at).getTime()).filter(Number.isFinite);
  $("clientVersionStatus").textContent = clientVersionState.error || (errors.length ? errors.map((item) => item.name + "：" + (item.version ? "检测失败，沿用 " + item.version : item.error)).join("；") : checked.length ? "最近检测：" + fmtTime(new Date(Math.min(...checked)).toISOString()) + " · 每小时自动检查" : "打开编辑器时自动检测最新稳定版。");
  $("clientVersionStatus").classList.toggle("error", !!clientVersionState.error || errors.length > 0);
  syncClientHeaderTemplate();
}

async function loadClientHeaderVersions(force = false) {
  if (clientVersionState.promise) return clientVersionState.promise;
  clientVersionState.error = "";
  renderClientHeaderVersions();
  $("clientVersionStatus").textContent = "正在检测 Codex 和 Claude Code 最新稳定版…";
  setBusy($("refreshClientVersionsButton"), true, "检测中…");
  clientVersionState.promise = (async () => {
    try {
      const result = await api(force ? "api/client-versions/refresh" : "api/client-versions", force ? { method: "POST" } : {});
      clientVersionState.clients = result.clients || [];
    } catch (error) {
      clientVersionState.error = error.message === "unauthorized" ? "请登录后检测客户端版本。" : "版本检测失败：" + error.message + "。可继续使用已有请求头。";
    } finally {
      clientVersionState.promise = null;
      setBusy($("refreshClientVersionsButton"), false);
      renderClientHeaderVersions();
    }
  })();
  return clientVersionState.promise;
}

function applyClientHeaderTemplate() {
  const id = $("clientHeaderTemplate").value;
  if (!id) return;
  if (!validateHeaderOverrides()) { $("editHeaderOverrides").reportValidity(); return; }
  const client = clientVersionState.clients.find((item) => item.id === id);
  if (!client?.version) { toast("请先成功检测该客户端的版本", "error"); return; }
  const field = $("editHeaderOverrides"), headers = JSON.parse(field.value.trim() || "{}");
  for (const name of Object.keys(headers)) {
    if (name.toLowerCase() === "user-agent") delete headers[name];
  }
  headers["User-Agent"] = "{" + id + "_user_agent}";
  field.value = JSON.stringify(headers, null, 2);
  validateHeaderOverrides(); clearProbeResults(); updateEditorDirty();
  field.focus();
}
function setHeaderTemplate(kind) {
  let headers = "";
  if (kind === "passthrough") headers = { "*": true };
  if (kind === "example") {
    headers = { "*": true, "re:^X-Trace-.*$": true, "X-Foo": "{client_header:X-Foo}" };
    const type = $("editType").value;
    headers[type === "anthropic" ? "x-api-key" : type === "gemini" ? "x-goog-api-key" : "Authorization"] = type === "openai" ? "Bearer {api_key}" : "{api_key}";
  }
  $("editHeaderOverrides").value = headers ? JSON.stringify(headers, null, 2) : "";
  validateHeaderOverrides(); clearProbeResults(); updateEditorDirty();
  $("editHeaderOverrides").focus();
}
function formatHeaderOverrides() {
  if (!validateHeaderOverrides()) { $("editHeaderOverrides").reportValidity(); return; }
  const field = $("editHeaderOverrides");
  if (field.value.trim()) field.value = JSON.stringify(JSON.parse(field.value), null, 2);
  updateEditorDirty();
}
function copyHeaderOverrides() { return copyText($("editHeaderOverrides").value || "{}", "请求头配置已复制"); }

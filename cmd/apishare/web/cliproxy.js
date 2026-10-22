/* CLIProxyAPI is managed by the gateway; management secrets never enter the browser. */
const coreUI = { status: null, credentials: [], busy: false, login: null, timer: null, generation: 0 };

document.addEventListener("click", (event) => {
  const settings = document.querySelector(".core-settings");
  if (settings.open && !settings.contains(event.target)) settings.open = false;
});
document.addEventListener("keydown", (event) => {
  const settings = document.querySelector(".core-settings");
  if (event.key === "Escape" && settings.open) {
    settings.open = false;
    if (settings.contains(event.target)) settings.querySelector("summary").focus();
  }
});

function coreMessage(message, error = false) {
  $("coreMessage").textContent = message;
  $("coreMessage").classList.toggle("hidden", !message);
  $("coreMessage").classList.toggle("form-error", error);
}
function renderCoreStatus() {
  const s = coreUI.status;
  $("coreVersion").textContent = s?.version || "内置";
  $("coreRuntime").textContent = s?.running ? "运行中" : "已停止";
  $("coreRuntime").className = "badge " + (s?.running ? "on" : "off");
  $("coreAutoStart").textContent = s?.auto_start ? "随网关启动" : "手动启动";
  $("coreStart").disabled = coreUI.busy || !s || s.running;
  $("coreStop").disabled = coreUI.busy || !s?.running;
  $("coreConnect").disabled = coreUI.busy || !s?.running;
  $("coreAccountFields").disabled = !s?.running;
}
async function loadCLIProxy() {
  try {
    const [status] = await Promise.all([api("api/cliproxy/status", { cache: "no-store" }), loadProxies(true)]);
    coreUI.status = status;
    renderCoreStatus();
    renderCoreProxy();
    if (coreUI.status.error) coreMessage(coreUI.status.error, true);
    if (coreUI.status.running) {
      const result = await api("api/cliproxy/credentials", { cache: "no-store" });
      coreUI.credentials = result.files || [];
    } else coreUI.credentials = [];
    renderCoreAccounts();
    stampRefresh();
  } catch (error) {
    coreUI.credentials = [];
    renderCoreAccounts();
    coreMessage(error.message, true);
  }
}
function renderCoreProxy() {
  const selected = Number(coreUI.status?.proxy_id || 0);
  const field = $("coreProxy");
  field.replaceChildren(new Option("默认连接（跟随系统环境）", "0"));
  proxiesCache.forEach((proxy) => {
    const option = new Option(proxyName(proxy) + " · " + proxy.url.split(":")[0].toUpperCase() + (proxy.enabled ? "" : "（已停用）"), String(proxy.id));
    option.disabled = !proxy.enabled;
    field.add(option);
  });
  if (selected && !proxiesCache.some((item) => item.id === selected)) field.add(new Option("代理 #" + selected + "（不可用）", String(selected)));
  field.value = String(selected);
}
async function saveCoreProxy(select) {
  const previous = coreUI.status?.proxy_id || 0;
  select.disabled = true;
  try {
    await api("api/cliproxy/proxy", { method: "PUT", body: JSON.stringify({ proxy_id: Number(select.value) }) });
    coreUI.status = { ...coreUI.status, proxy_id: Number(select.value) };
    toast(select.value === "0" ? "核心已恢复默认出网" : "核心出站代理已更新");
  } catch (error) {
    select.value = String(previous);
    reportError(error, "出站代理保存失败");
  } finally { select.disabled = false; }
}
function renderCoreAccounts() {
  const tbody = $("coreAccountsTable").querySelector("tbody");
  if (!coreUI.credentials.length) {
    tbody.innerHTML = emptyRow(4, coreUI.status?.running ? "还没有订阅账号" : "启动核心后管理账号", "登录或导入凭据后，再将账号池接入上游服务。");
    return;
  }
  tbody.innerHTML = coreUI.credentials.map((item, index) => '<tr><td><strong>' + esc(item.name || item.id) + '</strong></td><td>' + esc(item.provider || "—") + '</td><td><span class="badge ' + (item.disabled ? 'off' : item.unavailable ? 'warn' : 'on') + '">' + esc(item.disabled ? "已停用" : item.status || "就绪") + '</span></td><td class="core-row-actions"><button class="btn ghost" data-core-action="toggle" data-index="' + index + '">' + (item.disabled ? "启用" : "停用") + '</button>' + (!item.runtime_only ? '<button class="btn ghost" data-core-action="refresh" data-index="' + index + '">刷新凭据</button><button class="btn ghost" data-core-action="delete" data-index="' + index + '">删除</button>' : '') + '</td></tr>').join("");
  tbody.querySelectorAll("[data-core-action]").forEach(button => button.addEventListener("click", () => coreAccountAction(button.dataset.coreAction, Number(button.dataset.index), button)));
  labelTableCells("coreAccountsTable");
}
async function runCoreAction(action, button) {
  if (coreUI.busy) return;
  if (action === "stop" && !await askAction({ title: "停止账号池核心？", description: "账号池正在处理的请求可能中断。普通 API 上游可继续使用，请选择空闲时操作。", confirm: "继续操作" })) return;
  coreUI.busy = true;
  setBusy(button, true, "处理中…");
  renderCoreStatus();
  coreMessage("正在处理…");
  try {
    const body = {};
    await api("api/cliproxy/" + action, { method: "POST", body: JSON.stringify(body) });
    coreMessage("操作完成。");
    toast("核心操作完成");
  } catch (error) { coreMessage(error.message, true); }
  finally { coreUI.busy = false; setBusy(button, false); await loadCLIProxy(); }
}
async function connectCorePool(button) {
  setBusy(button, true, "接入中…");
  try {
    const result = await api("api/cliproxy/upstream", { method: "POST", body: "{}" });
    toast(result.existing ? "账号池已接入，可在上游服务中配置" : "账号池已接入，请在 API 密钥中授权可用模型");
    await loadUpstreams(true);
  } catch (error) { reportError(error, "接入失败"); }
  finally { setBusy(button, false); }
}
async function importCoreCredential(input) {
  const file = input.files?.[0];
  if (!file) return;
  const button = $("coreImportButton");
  setBusy(button, true, "导入中…");
  try {
    if (file.size > 1024 * 1024) throw new Error("凭据文件不能超过 1 MB");
    const credential = JSON.parse(await file.text());
    await api("api/cliproxy/credentials/import", { method: "POST", body: JSON.stringify({ name: file.name, credential }) });
    toast("凭据已导入"); await loadCLIProxy();
  } catch (error) { reportError(error, "导入失败"); }
  finally { input.value = ""; setBusy(button, false); }
}
async function coreAccountAction(action, index, button) {
  const item = coreUI.credentials[index];
  if (!item) return;
  if (action === "delete" && !await askAction({ title: "删除账号凭据？", description: item.name + " 将从账号池移除，需要重新登录或导入才能恢复。", confirm: "删除账号", danger: true })) return;
  setBusy(button, true);
  try {
    const name = item.name || item.id;
    if (action === "delete") await api("api/cliproxy/credentials?name=" + encodeURIComponent(name), { method: "DELETE" });
    else await api("api/cliproxy/credentials/" + (action === "toggle" ? "status" : "refresh"), { method: action === "toggle" ? "PATCH" : "POST", body: JSON.stringify({ name, ...(action === "toggle" ? { disabled: !item.disabled } : {}) }) });
    toast("账号已更新"); await loadCLIProxy();
  } catch (error) { reportError(error); }
  finally { setBusy(button, false); }
}
function cancelCoreLogin(notify = true) {
  const state = coreUI.login?.state;
  coreUI.generation++;
  clearTimeout(coreUI.timer);
  coreUI.login = null;
  $("coreLoginChallenge").classList.add("hidden");
  $("coreLoginLink").removeAttribute("href");
  $("coreCallback").value = "";
  if (notify && state) api("api/cliproxy/oauth/session?state=" + encodeURIComponent(state), { method: "DELETE" }).catch(() => {});
}
async function startCoreLogin(button) {
  cancelCoreLogin();
  const generation = coreUI.generation;
  setBusy(button, true, "获取授权链接…");
  try {
    const result = await api("api/cliproxy/oauth/start", { method: "POST", body: JSON.stringify({ provider: $("coreProvider").value }) });
    if (generation !== coreUI.generation) { api("api/cliproxy/oauth/session?state=" + encodeURIComponent(result.state), { method: "DELETE" }).catch(() => {}); return; }
    coreUI.login = { ...result, deadline: Date.now() + 15 * 60 * 1000 };
    $("coreLoginLink").href = result.url;
    $("coreLoginCode").textContent = result.user_code ? "验证码：" + result.user_code : "";
    $("coreLoginStatus").textContent = "在授权页完成登录。远程部署时，如浏览器停在本地回调地址，请复制完整地址到下方回填。";
    $("coreLoginChallenge").classList.remove("hidden");
    coreUI.timer = setTimeout(() => pollCoreLogin(generation), 2000);
  } catch (error) { reportError(error, "登录失败"); }
  finally { setBusy(button, false); }
}
async function pollCoreLogin(generation) {
  if (generation !== coreUI.generation || !coreUI.login) return;
  if (Date.now() > coreUI.login.deadline) { cancelCoreLogin(); toast("登录已过期，请重试", "error"); return; }
  try {
    const result = await api("api/cliproxy/oauth/status?state=" + encodeURIComponent(coreUI.login.state));
    if (generation !== coreUI.generation) return;
    if (result.status === "ok") { cancelCoreLogin(false); toast("账号登录成功"); await loadCLIProxy(); return; }
    if (result.status === "error") { cancelCoreLogin(false); toast(result.error || "授权失败，请重试", "error"); return; }
  } catch (error) {
    if (generation !== coreUI.generation) return;
    if ([401, 404].includes(error.status) || error.message === "unauthorized") { cancelCoreLogin(false); return; }
    $("coreLoginStatus").textContent = "暂时无法读取状态，正在重试：" + error.message;
  }
  coreUI.timer = setTimeout(() => pollCoreLogin(generation), 2000);
}
async function submitCoreCallback(event) {
  event.preventDefault();
  if (!coreUI.login) return false;
  const button = $("coreCallbackSubmit");
  setBusy(button, true, "提交中…");
  try {
    await api("api/cliproxy/oauth/callback", { method: "POST", body: JSON.stringify({ state: coreUI.login.state, redirect_url: $("coreCallback").value.trim() }) });
    $("coreCallback").value = "";
    $("coreLoginStatus").textContent = "回调已提交，正在等待账号就绪…";
  } catch (error) { $("coreLoginStatus").textContent = error.message; }
  finally { setBusy(button, false); }
  return false;
}

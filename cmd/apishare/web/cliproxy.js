/* Account pools live inside the upstream editor. Secrets stay on the server. */
const coreUI = { poolID: null, status: null, credentials: [], pending: [], busy: false, loginBusy: false, login: null, timer: null, refreshTimer: null, uptimeAccount: null, uptimeError: "", generation: 0, epoch: 0, defaultProxy: 0 };
const poolProviders = [
  { id: "codex", name: "Codex", hint: "浏览器授权", brand: "openai.svg" },
  { id: "claude", name: "Claude", hint: "浏览器授权", brand: "claude-color.svg" },
  { id: "antigravity", name: "Antigravity", hint: "浏览器授权", brand: "antigravity-color.svg" },
  { id: "kimi", name: "Kimi 国内站", hint: "设备码登录 · kimi.com", brand: "kimi-color.svg" },
  { id: "kimi-ai", name: "Kimi 国际站", hint: "设备码登录 · kimi.ai", brand: "kimi-color.svg" },
  { id: "xai", name: "xAI / Grok", hint: "浏览器授权", brand: "xai.svg" },
  { id: "meta", name: "Muse · Meta", hint: "浏览器授权", brand: "meta-color.svg" },
  { id: "devin", name: "Devin", hint: "浏览器授权", brand: "devin.svg" },
];
function poolAPI(path, id = coreUI.poolID) { return "api/upstreams/" + id + "/cliproxy/" + path; }
function coreMessage(message = "", error = false) {
  $("coreMessage").textContent = message;
  $("coreMessage").classList.toggle("hidden", !message);
  $("coreMessage").classList.toggle("form-error", error);
}
function poolBrand(provider) {
  return '<span class="pool-provider-brand">' + (provider.brand ? '<img src="/public/static/brand/model-icons-1.95.1/' + provider.brand + '" alt="" width="26" height="26">' : '<span class="pool-monogram" aria-hidden="true">' + provider.letter + '</span>') + '</span>';
}
function renderPoolProviders() {
  $("poolProviderCards").innerHTML = poolProviders.map((provider) => '<button type="button" class="pool-provider-card" data-pool-provider="' + provider.id + '">' + poolBrand(provider) + '<span class="pool-provider-copy"><strong>' + provider.name + '</strong><small>' + provider.hint + '</small></span><span class="pool-card-add" aria-hidden="true">+</span></button>').join("");
  $("poolProviderCards").querySelectorAll("[data-pool-provider]").forEach((button) => {
    button.setAttribute("aria-label", "添加 " + (button.dataset.poolProvider === "kimi" ? "Kimi 国内站" : button.dataset.poolProvider === "kimi-ai" ? "Kimi 国际站" : poolProviders.find(p => p.id === button.dataset.poolProvider)?.name) + " 账号");
    button.addEventListener("click", () => startCoreLogin(button, button.dataset.poolProvider));
  });
}
function preparePoolEditor(upstream) {
  clearTimeout(coreUI.refreshTimer);
  coreUI.uptimeAccount = null; coreUI.uptimeError = "";
  coreUI.epoch++;
  cancelCoreLogin();
  coreUI.poolID = upstream?.type === "cliproxy" ? upstream.id : null;
  coreUI.status = null; coreUI.credentials = []; coreUI.pending = []; coreUI.busy = false;
  coreUI.defaultProxy = Number(upstream?.pool_proxy_id || 0);
  $("poolAccountSearch").value = "";
  $("poolSIWCPicker").classList.add("hidden");
  togglePoolProxyComposer(false);
  coreMessage();
  renderPoolProviders(); renderCoreProxy(); renderCoreStatus(); renderCoreAccounts();
}
function renderCoreStatus() {
  const s = coreUI.status, running = !!s?.running;
  $("coreVersion").textContent = s?.version || "内置";
  $("coreRuntime").textContent = coreUI.poolID ? running ? "运行中" : s ? "已停止" : "正在连接…" : "创建后启动";
  $("coreRuntime").className = "badge " + (running ? "on" : "off");
  $("coreAutoStart").textContent = s?.auto_start ? "随网关启动" : coreUI.poolID ? "手动启动" : "保存后自动启动";
  $("coreStart").disabled = coreUI.busy || !coreUI.poolID || running;
  $("coreStop").disabled = coreUI.busy || !running;
  const stopped = !!coreUI.poolID && !!s && !running;
  $("poolResumeButton").classList.toggle("hidden", !stopped);
  $("poolResumeButton").disabled = coreUI.busy;
  $("poolAddAccountButton").classList.toggle("hidden", stopped);
  $("coreImportButton").disabled = stopped;
  document.querySelectorAll("#poolProviderCards button, .pool-other-grid button").forEach(button => { button.disabled = stopped; });
  $("coreLicense").href = coreUI.poolID ? poolAPI("license") : "api/cliproxy/license";
}
function proxyOptions(selected, inherit = true) {
  let html = inherit ? '<option value="-1">跟随池子默认代理</option><option value="0">直接连接（不使用代理）</option>' : '<option value="0">系统默认连接</option>';
  proxiesCache.forEach((proxy) => {
    html += '<option value="' + proxy.id + '"' + (!proxy.enabled ? " disabled" : "") + '>' + esc(proxyName(proxy) + (proxy.enabled ? "" : "（已停用）")) + '</option>';
  });
  if (selected > 0 && !proxiesCache.some(p => p.id === selected)) html += '<option value="' + selected + '" disabled>代理不可用 · #' + selected + '</option>';
  return html;
}
function renderCoreProxy() {
  const selected = Number(coreUI.status?.proxy_id ?? coreUI.defaultProxy);
  $("coreProxy").innerHTML = proxyOptions(selected, false); $("coreProxy").value = String(selected);
  const loginProxy = Number($("coreLoginProxy").value || -1);
  $("coreLoginProxy").innerHTML = proxyOptions(loginProxy); $("coreLoginProxy").value = String(loginProxy);
}
async function loadCLIProxy() {
  const id = coreUI.poolID, epoch = coreUI.epoch;
  if (!id) { renderCoreStatus(); renderCoreAccounts(); return; }
  try {
    const [status] = await Promise.all([api(poolAPI("status", id), {cache: "no-store"}), loadProxies(true)]);
    if (id !== coreUI.poolID || epoch !== coreUI.epoch || !editorIsOpen()) return;
    coreUI.status = status; coreUI.defaultProxy = status.proxy_id;
    renderCoreStatus(); renderCoreProxy();
    if (status.error) coreMessage(status.error, true);
    const result = await api(poolAPI("credentials", id), {cache: "no-store"});
    if (id !== coreUI.poolID || epoch !== coreUI.epoch || !editorIsOpen()) return;
    coreUI.credentials = result.files || [];
    coreUI.uptimeError = result.uptime_error || "";
    renderCoreAccounts(); stampRefresh();
  } catch (error) {
    if (id !== coreUI.poolID || epoch !== coreUI.epoch) return;
    coreMessage(error.message, true); renderCoreStatus(); renderCoreAccounts();
  } finally {
    if (id === coreUI.poolID && epoch === coreUI.epoch) schedulePoolUptimeRefresh();
  }
}
async function saveCoreProxy(select) {
  const previous = coreUI.defaultProxy, next = Number(select.value), id = coreUI.poolID;
  if (!id) { coreUI.defaultProxy = next; updateEditorDirty(); renderCoreAccounts(); return; }
  select.disabled = true;
  try {
    await api(poolAPI("proxy", id), {method: "PUT", body: JSON.stringify({proxy_id: next})});
    if (id !== coreUI.poolID) return;
    coreUI.status = {...coreUI.status, proxy_id: next}; coreUI.defaultProxy = next;
    coreUI.credentials.forEach(item => { if (item.proxy_id === -1) item.effective_proxy_id = next; });
    const up = upsCache.find(u => u.id === id); if (up) up.pool_proxy_id = next;
    renderCoreAccounts(); toast("池子默认代理已更新");
  } catch (error) { select.value = String(previous); reportError(error, "代理保存失败"); }
  finally { select.disabled = false; }
}
function renderCoreAccounts() {
  const all = [...coreUI.credentials, ...coreUI.pending.map((item, index) => ({name: item.name, provider: item.provider, pending: true, proxy_id: item.proxy_id, pendingIndex: index}))];
  const query = $("poolAccountSearch").value.trim().toLowerCase();
  const rows = all.filter(item => [item.name, item.provider, item.label, item.email].join(" ").toLowerCase().includes(query));
  const ready = coreUI.credentials.filter(item => !item.disabled && !item.unavailable && !["error","invalid","expired"].includes(item.status)).length;
  const stopped = !!coreUI.poolID && !!coreUI.status && !coreUI.status.running;
  $("poolAccountCount").textContent = stopped ? coreUI.status.account_count || 0 : all.length; $("poolReadyCount").textContent = ready;
  $("poolReadyNote").textContent = coreUI.pending.length ? coreUI.pending.length + " 个账号将在保存时导入" : stopped ? "池子已停止，历史仍可查看" : ready ? "可参与池内调度" : "等待添加账号";
  const explicit = coreUI.credentials.filter(item => item.proxy_id > 0).map(item => item.proxy_id);
  if (coreUI.defaultProxy) explicit.push(coreUI.defaultProxy);
  $("poolProxyCount").textContent = new Set(explicit).size ? new Set(explicit).size + " 个代理" : "默认";
  $("poolFilteredCount").textContent = query ? rows.length + " / " + all.length : all.length;
  renderPoolAccountUptime();
  const tbody = $("coreAccountsTable").querySelector("tbody");
  if (!rows.length) {
    tbody.innerHTML = '<tr><td colspan="5"><div class="pool-empty"><span class="pool-empty-icon">' + icon("layers") + '</span><strong>' + (stopped ? "账号池已停止" : query ? "没有找到匹配的账号" : "这个池子，等待第一个账号") + '</strong><p>' + (stopped ? "启动后即可授权和管理池内账号。" : query ? "试试其他账号名称或提供商。" : "从下方选择提供商授权，或一次导入多个 JSON 凭据。") + '</p>' + (stopped ? '<button type="button" class="btn ghost" onclick="runCoreAction(\'start\',this)">启动当前池</button>' : '') + '</div></td></tr>';
    return;
  }
  tbody.innerHTML = rows.map((item, index) => {
    const unavailable = item.unavailable || ["error","invalid","expired"].includes(item.status);
    const statusName = ({active:"就绪",ready:"就绪",error:"异常",invalid:"凭据无效",expired:"已过期",refreshing:"刷新中",stopped:"账号池已停止"})[item.status] || item.status || "就绪";
    const label = item.pending ? "待保存" : item.disabled ? "已停用" : unavailable && statusName === "就绪" ? "暂不可用" : statusName;
    const state = item.pending || item.disabled || stopped ? "off" : unavailable ? "warn" : "on";
    const proxy = Number(item.proxy_id ?? -1);
    return '<tr><td class="pool-account-identity-cell"><div class="pool-account-identity"><span class="pool-account-icon">' + modelIcon(item.provider === "codex" ? "gpt" : item.provider || "") + '</span><div><strong>' + esc(item.label || item.email || item.name || item.id) + '</strong><small>' + esc(item.provider || "待识别") + (item.label || item.email ? ' · ' + esc(item.name) : '') + '</small></div></div></td><td class="pool-account-state-cell"><span class="badge ' + state + '">' + esc(label) + '</span></td><td class="pool-account-uptime-cell">' + poolAccountUptimeHTML(item, index) + '</td><td class="pool-account-proxy-cell"><select data-account-proxy="' + index + '" aria-label="' + esc(item.name) + '的出站代理"' + (stopped && !item.pending ? ' disabled' : '') + '>' + proxyOptions(proxy) + '</select><small class="pool-effective-proxy">' + esc(proxy === -1 ? "当前：" + (coreUI.defaultProxy ? upstreamProxyLabel(coreUI.defaultProxy) : "系统默认连接") : proxy === 0 ? "绕过池子默认代理" : upstreamProxyLabel(proxy)) + '</small></td><td class="pool-account-actions-cell"><div class="core-row-actions">' + (item.pending ? '<button type="button" class="text-btn danger" data-pending-remove="' + item.pendingIndex + '">移除</button>' : '<button type="button" class="text-btn" data-core-action="toggle" data-index="' + index + '"' + (stopped ? ' disabled' : '') + '>' + (item.disabled ? "启用" : "停用") + '</button>' + (!item.runtime_only ? '<button type="button" class="text-btn" data-core-action="refresh" data-index="' + index + '"' + (stopped ? ' disabled' : '') + '>刷新</button><button type="button" class="icon-btn danger" data-core-action="delete" data-index="' + index + '" aria-label="删除 ' + esc(item.name) + '"' + (stopped ? ' disabled' : '') + '>' + icon("trash") + '</button>' : '')) + '</div></td></tr>';
  }).join("");
  tbody.querySelectorAll("[data-account-proxy]").forEach(select => {
    const item = rows[Number(select.dataset.accountProxy)];
    select.value = String(item.proxy_id ?? -1);
    select.addEventListener("change", async () => {
      if (item.pending) { coreUI.pending[item.pendingIndex].proxy_id = Number(select.value); renderCoreAccounts(); updateEditorDirty(); return; }
      const previous = item.proxy_id, id = coreUI.poolID;
      select.disabled = true;
      try {
        await api(poolAPI("credentials/proxy", id), {method: "PATCH", body: JSON.stringify({name: item.name || item.id, proxy_id: Number(select.value)})});
        item.proxy_id = Number(select.value); renderCoreAccounts(); toast("账号代理已更新");
      } catch (error) { select.value = String(previous); reportError(error); }
      finally { select.disabled = false; }
    });
  });
  tbody.querySelectorAll("[data-core-action]").forEach(button => button.addEventListener("click", () => coreAccountAction(button.dataset.coreAction, rows[Number(button.dataset.index)], button)));
  tbody.querySelectorAll("[data-account-uptime]").forEach(button => button.addEventListener("click", () => showPoolAccountUptime(rows[Number(button.dataset.accountUptime)])));
  tbody.querySelectorAll("[data-pending-remove]").forEach(button => button.addEventListener("click", () => { coreUI.pending.splice(Number(button.dataset.pendingRemove), 1); renderCoreAccounts(); updateEditorDirty(); }));
  labelTableCells("coreAccountsTable");
}
function showPoolAuthorization() { $("poolAuthorization").scrollIntoView({behavior: motionBehavior(), block: "start"}); $("coreLoginProxy").focus({preventScroll: true}); }
async function ensureCorePool() {
  if (coreUI.poolID) return true;
  const result = await saveEditUpstream({preventDefault() {}});
  return !!result && !!coreUI.poolID;
}
async function runCoreAction(action, button) {
  if (coreUI.busy || !coreUI.poolID) return;
  if (action === "stop" && !await askAction({title: "停止当前账号池？", description: "当前池正在处理的请求可能中断。停止后可以在此重新启动。", confirm: "停止账号池"})) return;
  coreUI.busy = true; setBusy(button, true); renderCoreStatus();
  try { await api(poolAPI(action), {method: "POST", body: "{}"}); coreMessage(); toast(action === "start" ? "账号池已启动" : "账号池已停止"); }
  catch (error) { coreMessage(error.message, true); }
  finally { coreUI.busy = false; setBusy(button, false); await loadCLIProxy(); }
}
async function importCoreCredential(input) {
  const files = [...(input.files || [])];
  if (!files.length) return;
  const button = $("coreImportButton"), proxyID = Number($("coreLoginProxy").value), poolID = coreUI.poolID, epoch = coreUI.epoch;
  setBusy(button, true, "读取凭据…");
  let imported = 0;
  try {
    const pending = [];
    if (files.length > 100) throw new Error("每次最多导入 100 个凭据文件");
    for (const file of files) {
      if (file.size > 1024 * 1024) throw new Error(file.name + " 超过 1 MB");
      const credential = JSON.parse(await file.text());
      if (!credential || Array.isArray(credential) || typeof credential !== "object" || !Object.keys(credential).length) throw new Error(file.name + " 必须是非空 JSON 对象");
      if (!/^[^/\\\u0000\r\n]+\.json$/.test(file.name)) throw new Error("凭据文件名必须以 .json 结尾");
      pending.push({name: file.name, credential, proxy_id: proxyID, provider: credential.type === "service_account" ? "vertex" : credential.type || credential.provider || "待识别"});
    }
    if (epoch !== coreUI.epoch || !editorIsOpen()) return;
    if (!poolID) {
      for (const item of pending) {
        const index = coreUI.pending.findIndex(p => p.name === item.name);
        if (index >= 0) coreUI.pending[index] = item; else coreUI.pending.push(item);
      }
      renderCoreAccounts(); updateEditorDirty(); toast("已添加 " + pending.length + " 个凭据，保存时导入");
    } else {
      for (const item of pending) {
        if (epoch !== coreUI.epoch || !editorIsOpen()) return;
        await api(poolAPI("credentials/import", poolID), {method: "POST", body: JSON.stringify(item)});
        imported++;
      }
      await loadCLIProxy(); toast("已导入 " + imported + " 个账号");
    }
  } catch (error) { reportError(error, imported ? "已导入 " + imported + " 个，其余导入中断" : "导入失败"); if (imported) await loadCLIProxy(); }
  finally { input.value = ""; setBusy(button, false); }
}
async function coreAccountAction(action, item, button) {
  if (action === "delete" && !await askAction({title: "删除此账号凭据？", description: item.name + " 将从当前池移除，恢复需要重新授权或导入。", confirm: "删除账号", danger: true})) return;
  setBusy(button, true);
  try {
    const name = item.name || item.id;
    if (action === "delete") await api(poolAPI("credentials?name=" + encodeURIComponent(name)), {method: "DELETE"});
    else await api(poolAPI("credentials/" + (action === "toggle" ? "status" : "refresh")), {method: action === "toggle" ? "PATCH" : "POST", body: JSON.stringify({name, ...(action === "toggle" ? {disabled: !item.disabled} : {})})});
    await loadCLIProxy(); toast("账号已更新");
  } catch (error) { reportError(error); }
  finally { setBusy(button, false); }
}
function cancelCoreLogin(notify = true) {
  const login = coreUI.login;
  coreUI.generation++; clearTimeout(coreUI.timer); coreUI.login = null;
  $("coreLoginChallenge").classList.add("hidden"); $("coreLoginLink").removeAttribute("href"); $("coreCallback").value = "";
  if (notify && login?.state) api(poolAPI("oauth/session?state=" + encodeURIComponent(login.state), login.poolID), {method: "DELETE"}).catch(() => {});
}
function displayPoolLogin(result, provider, generation, poolID) {
  coreUI.login = {...result, poolID, deadline: Date.now() + (result.expires_in || 900) * 1000};
  $("poolLoginProvider").textContent = provider;
  $("coreLoginLink").href = result.url;
  $("coreLoginCode").textContent = result.user_code ? "验证码：" + result.user_code : "";
  $("coreLoginStatus").textContent = result.user_code ? "打开授权页并输入设备码，完成后账号自动加入此池。" : "在授权页完成登录；如停在本地回调地址，可在下方回填。";
  $("poolCallbackDetails").open = false;
  $("poolCallbackDetails").classList.toggle("hidden", !!result.user_code);
  $("coreLoginChallenge").classList.remove("hidden");
  $("coreLoginChallenge").scrollIntoView({behavior: motionBehavior(), block: "nearest"});
  coreUI.timer = setTimeout(() => pollCoreLogin(generation), 2000);
}
async function startCoreLogin(button, provider) {
  if (coreUI.loginBusy) return;
  const proxyID = Number($("coreLoginProxy").value);
  if (!await ensureCorePool()) return;
  cancelCoreLogin();
  const generation = coreUI.generation, poolID = coreUI.poolID;
  coreUI.loginBusy = true; setBusy(button, true, "正在发起授权…");
  try {
    const result = await api(poolAPI("oauth/start", poolID), {method: "POST", body: JSON.stringify({provider, proxy_id: proxyID})});
    if (generation !== coreUI.generation || poolID !== coreUI.poolID) { api(poolAPI("oauth/session?state=" + encodeURIComponent(result.state), poolID), {method: "DELETE"}).catch(() => {}); return; }
    const title = provider === "kimi" ? "Kimi 国内站" : provider === "kimi-ai" ? "Kimi 国际站" : poolProviders.find(p => p.id === provider)?.name || provider;
    displayPoolLogin(result, title + " · 等待授权", generation, poolID);
  } catch (error) { coreMessage(error.message, true); reportError(error, "授权失败"); }
  finally { coreUI.loginBusy = false; setBusy(button, false); }
}
async function pollCoreLogin(generation) {
  const login = coreUI.login;
  if (generation !== coreUI.generation || !login) return;
  if (Date.now() > login.deadline) { cancelCoreLogin(); coreMessage("授权已过期，请重新发起。", true); return; }
  try {
    const result = await api(poolAPI("oauth/status?state=" + encodeURIComponent(login.state), login.poolID));
    if (generation !== coreUI.generation) return;
    if (result.status === "ok") {
      cancelCoreLogin(); coreMessage(); toast(login.flow === "siwc_bind" ? "SIWC 绑定完成" : "授权成功，账号已加入当前池");
      await loadCLIProxy(); return;
    }
    if (result.status === "error") { cancelCoreLogin(); coreMessage(result.error || "授权失败，请重新发起。", true); return; }
  } catch (error) {
    if (generation !== coreUI.generation) return;
    if ([401, 404].includes(error.status) || error.message === "unauthorized") { cancelCoreLogin(false); coreMessage("授权会话已失效，请重新发起。", true); return; }
    $("coreLoginStatus").textContent = "暂时无法读取授权状态，正在重试：" + error.message;
  }
  coreUI.timer = setTimeout(() => pollCoreLogin(generation), 2000);
}
async function submitCoreCallback(event) {
  event.preventDefault();
  const login = coreUI.login;
  if (!login) return false;
  const button = $("coreCallbackSubmit"), redirect = $("coreCallback").value.trim();
  if (!redirect) { $("coreCallback").focus(); return false; }
  setBusy(button, true, "提交中…");
  try {
    await api(poolAPI("oauth/callback", login.poolID), {method: "POST", body: JSON.stringify({state: login.state, redirect_url: redirect})});
    $("coreCallback").value = ""; $("coreLoginStatus").textContent = "回调已提交，正在等待账号就绪…";
  } catch (error) { $("coreLoginStatus").textContent = error.message; }
  finally { setBusy(button, false); }
  return false;
}
async function showSIWCBinding() {
  if (!await ensureCorePool()) return;
  const accounts = coreUI.credentials.filter(item => item.provider === "codex" && !item.runtime_only);
  if (!accounts.length) { toast("请先在当前池添加一个 Codex OAuth 账号", "error"); return; }
  $("poolSIWCTarget").replaceChildren(...accounts.map(item => new Option(item.label || item.name, item.name)));
  $("poolSIWCPicker").classList.remove("hidden"); $("poolSIWCTarget").focus();
}
async function startCoreSIWC(button) {
  if (coreUI.loginBusy) return;
  cancelCoreLogin();
  const generation = coreUI.generation, id = coreUI.poolID;
  coreUI.loginBusy = true; setBusy(button, true, "准备绑定…");
  try {
    const result = await api(poolAPI("oauth/siwc/start", id), {method: "POST", body: JSON.stringify({name: $("poolSIWCTarget").value})});
    if (generation !== coreUI.generation || id !== coreUI.poolID) { api(poolAPI("oauth/session?state=" + encodeURIComponent(result.state), id), {method: "DELETE"}).catch(() => {}); return; }
    displayPoolLogin(result, "SIWC · 等待绑定", generation, id);
  } catch (error) { reportError(error, "绑定失败"); }
  finally { coreUI.loginBusy = false; setBusy(button, false); }
}
function togglePoolProxyComposer(open = true) {
  $("poolProxyComposer").classList.toggle("hidden", !open);
  ["poolProxyName", "poolProxyURL", "poolProxyUser", "poolProxyPassword"].forEach(id => { $(id).value = ""; $(id).setCustomValidity(""); });
  $("poolProxyError").classList.add("hidden");
  if (!open) return;
  const item = proxiesCache.find(p => p.id === Number($("coreProxy").value));
  $("poolProxyName").value = item?.name || ""; $("poolProxyURL").value = item?.url || ""; $("poolProxyUser").value = item?.username || "";
  $("poolProxyUpdate").disabled = !item; $("poolProxyName").focus();
}
async function savePoolInlineProxy(button, create) {
  const id = Number($("coreProxy").value), name = $("poolProxyName").value.trim(), url = $("poolProxyURL").value.trim(), epoch = coreUI.epoch;
  if (!name || !url) { $("poolProxyError").textContent = "请填写代理名称和地址。"; $("poolProxyError").classList.remove("hidden"); return; }
  setBusy(button, true);
  try {
    const body = {name, url, username: $("poolProxyUser").value.trim(), password: $("poolProxyPassword").value};
    const result = await api(create ? "api/proxies" : "api/proxies/" + id, {method: create ? "POST" : "PATCH", body: JSON.stringify(body)});
    await loadProxies(true); renderCoreProxy();
    if (epoch !== coreUI.epoch || !editorIsOpen()) return;
    $("coreProxy").value = String(create ? result.id : id);
    await saveCoreProxy($("coreProxy")); togglePoolProxyComposer(false); toast(create ? "代理已创建" : "代理已更新");
  } catch (error) { $("poolProxyError").textContent = error.message; $("poolProxyError").classList.remove("hidden"); }
  finally { setBusy(button, false); }
}

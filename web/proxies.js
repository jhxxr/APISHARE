/* Saved outbound proxies and the upstream connection selector. */
let proxiesCache = [], proxyEditID = null, proxySaving = false;

async function loadProxies(quiet = false) {
  const version = requestVersions.proxies = (requestVersions.proxies || 0) + 1;
  try {
    const result = await api("api/proxies");
    if (version !== requestVersions.proxies) return true;
    proxiesCache = result || [];
    renderProxies(); renderProxyOptions(); stampRefresh();
    return true;
  } catch (error) {
    errorRow("proxiesTable", 6, error);
    if (!quiet) reportError(error, "代理加载失败");
    return false;
  }
}
function upstreamProxyLabel(id) {
  if (!id) return "默认连接";
  const proxy = proxiesCache.find((item) => item.id === id);
  return proxy ? proxy.name + (proxy.enabled ? "" : "（已停用）") : "代理 #" + id;
}
function renderProxyOptions(selected = Number($("editProxy").value || 0)) {
  const field = $("editProxy");
  field.replaceChildren(new Option("默认连接（跟随系统环境）", "0"));
  proxiesCache.forEach((proxy) => {
    const option = new Option(proxy.name + " · " + proxy.url.split(":")[0].toUpperCase() + (proxy.enabled ? "" : "（已停用）"), String(proxy.id));
    option.disabled = !proxy.enabled;
    field.add(option);
  });
  if (selected && !proxiesCache.some((item) => item.id === selected)) field.add(new Option("代理 #" + selected + "（不可用）", String(selected)));
  field.value = String(selected);
  updateProxyHint();
}
function updateProxyHint() {
  const id = Number($("editProxy").value), proxy = proxiesCache.find((item) => item.id === id);
  $("editProxyHint").textContent = !id ? "沿用系统 HTTP(S)_PROXY / NO_PROXY 设置；未设置时直连。" : proxy?.enabled ? "模型调用、连接测试和模型列表获取均通过此代理。" : "此代理不可用。请启用它或选择其他连接方式。";
}
function renderProxies() {
  const query = $("proxySearch").value.trim().toLowerCase();
  const list = proxiesCache.filter((item) => [item.name, item.url, item.username].join(" ").toLowerCase().includes(query));
  $("proxyListCount").textContent = list.length + (list.length !== proxiesCache.length ? " / " + proxiesCache.length : "");
  $("proxiesTable").querySelector("tbody").innerHTML = list.map((item) => '<tr><td class="cell-stack"><strong>' + esc(item.name) + '</strong><small>' + esc(item.url.split(":")[0].toUpperCase()) + ' · #' + item.id + '</small></td><td class="endpoint-cell"><span>' + esc(item.url) + '</span></td><td>' + (item.username ? esc(item.username) + (item.has_password ? '<span class="cell-stack"><small>密码已保存</small></span>' : '') : '<span class="muted">无需认证</span>') + '</td><td>' + item.upstream_count + ' 个上游</td><td><button class="toggle-btn" role="switch" aria-checked="' + !!item.enabled + '" aria-label="' + (item.enabled ? "停用 " : "启用 ") + esc(item.name) + '" onclick="toggleProxy(' + item.id + ', ' + !item.enabled + ', this)"><span></span></button></td><td><div class="row-actions"><button class="text-btn" onclick="openProxyComposer(' + item.id + ')" aria-label="编辑 ' + esc(item.name) + '">' + icon("edit") + '编辑</button><button class="icon-btn danger" onclick="deleteProxy(' + item.id + ')" aria-label="删除 ' + esc(item.name) + '" title="删除代理">' + icon("trash") + '</button></div></td></tr>').join("") || emptyRow(6, query ? "没有匹配的代理" : "添加你的第一个代理", query ? "调整搜索条件后重试。" : "支持 HTTP、HTTPS、SOCKS5、SOCKS5h，以及用户名和密码认证。", query ? "" : "openProxyComposer()", "添加代理", "link");
  labelTableCells("proxiesTable");
}
function openProxyComposer(id = null) {
  if (proxySaving) return;
  const item = id === null ? null : proxiesCache.find((proxy) => proxy.id === id);
  if (id !== null && !item) { toast("代理不存在，请刷新后重试", "error"); return; }
  proxyEditID = id;
  $("proxyForm").reset();
  $("proxyPassword").disabled = false;
  $("proxyForm").querySelectorAll(".form-error").forEach((error) => error.classList.add("hidden"));
  ["proxyName", "proxyURL", "proxyUsername", "proxyPassword"].forEach((field) => { $(field).setCustomValidity(""); $(field).removeAttribute("aria-invalid"); });
  $("proxyName").value = item?.name || "";
  $("proxyURL").value = item?.url || "";
  $("proxyUsername").value = item?.username || "";
  $("proxyPassword").placeholder = item?.has_password ? "留空保留已保存的密码" : "无需密码时留空";
  $("proxyPasswordHint").textContent = item ? "留空保留原密码；清空用户名会同时移除认证。" : "密码不会在列表中显示。";
  $("proxyClearPassword").closest("label").classList.toggle("hidden", !item?.has_password);
  $("proxyComposerTitle").textContent = item ? "编辑代理" : "添加代理";
  $("proxyFormError").classList.add("hidden");
  $("proxyComposer").classList.remove("hidden");
  $("proxyName").focus();
}
function closeProxyComposer() {
  if (proxySaving) return;
  $("proxyComposer").classList.add("hidden");
  $("proxyPassword").value = "";
  proxyEditID = null;
}
function validateProxyURL() {
  const field = $("proxyURL");
  let message = "";
  try {
    const parsed = new URL(field.value.trim());
    if (!["http:", "https:", "socks5:", "socks5h:"].includes(parsed.protocol) || !parsed.hostname || parsed.username || parsed.password || parsed.search || parsed.hash || (parsed.pathname && parsed.pathname !== "/")) throw new Error();
    // URL normalizes standard HTTP(S) ports away, so inspect the original authority.
    const authority = field.value.trim().split("://")[1]?.replace(/\/$/, "");
    const port = authority?.match(/:(\d+)$/)?.[1];
    if (!port || Number(port) < 1 || Number(port) > 65535) throw new Error();
  } catch { message = "请输入 HTTP、HTTPS、SOCKS5 或 SOCKS5h 地址，例如 socks5://127.0.0.1:1080。"; }
  field.setCustomValidity(message);
  field.setAttribute("aria-invalid", String(!!message));
  if (message) field.reportValidity();
  return !message;
}
async function saveProxy(event) {
  event.preventDefault();
  if (proxySaving) return false;
  if (!validateProxyURL() || !$("proxyForm").reportValidity()) return false;
  const body = { name: $("proxyName").value.trim(), url: $("proxyURL").value.trim(), username: $("proxyUsername").value.trim(), clear_password: $("proxyClearPassword").checked };
  if ($("proxyPassword").value) body.password = $("proxyPassword").value;
  const id = proxyEditID;
  proxySaving = true;
  $("proxyFields").disabled = true;
  $("proxyFormError").classList.add("hidden");
  setBusy($("proxySaveButton"), true, "正在保存…");
  try {
    await api(id === null ? "api/proxies" : "api/proxies/" + id, { method: id === null ? "POST" : "PATCH", body: JSON.stringify(body) });
    proxySaving = false; closeProxyComposer();
    await loadProxies(); renderUpstreams();
    toast(id === null ? "代理已添加，可在上游中选择使用" : "代理配置已保存");
  } catch (error) {
    if (error.message !== "unauthorized") {
      $("proxyFormError").textContent = error.message;
      $("proxyFormError").classList.remove("hidden");
      $("proxyFormError").focus();
    }
  } finally { proxySaving = false; $("proxyFields").disabled = false; setBusy($("proxySaveButton"), false); }
  return false;
}
async function toggleProxy(id, enabled, button) {
  const item = proxiesCache.find((proxy) => proxy.id === id);
  if (!enabled && item?.upstream_count && !await askAction({ title: "停用此代理？", description: "关联的 " + item.upstream_count + " 个上游将无法通过它发起请求。可在上游中选择其他代理。", confirm: "停用代理", danger: true })) return;
  button.disabled = true;
  try {
    await api("api/proxies/" + id, { method: "PATCH", body: JSON.stringify({ enabled }) });
    await loadProxies(); renderUpstreams(); toast(enabled ? "代理已启用" : "代理已停用");
  } catch (error) { reportError(error); } finally { button.disabled = false; }
}
async function deleteProxy(id) {
  const item = proxiesCache.find((proxy) => proxy.id === id);
  if (item?.upstream_count) { toast("此代理仍被 " + item.upstream_count + " 个上游使用，请先解除关联", "error"); return; }
  if (!await askAction({ title: "删除代理？", description: "“" + (item?.name || "此代理") + "”的连接配置将被删除。", confirm: "删除代理", danger: true })) return;
  try { await api("api/proxies/" + id, { method: "DELETE" }); await loadProxies(); toast("代理已删除"); }
  catch (error) { reportError(error); }
}
function initOutboundProxies() {
  $("proxyForm").addEventListener("input", (event) => { event.target.setCustomValidity?.(""); });
  $("proxyClearPassword").addEventListener("change", () => {
    $("proxyPassword").disabled = $("proxyClearPassword").checked;
    if ($("proxyClearPassword").checked) $("proxyPassword").value = "";
  });
  $("editProxy").addEventListener("change", () => { updateProxyHint(); clearProbeResults(); updateEditorDirty(); });
}

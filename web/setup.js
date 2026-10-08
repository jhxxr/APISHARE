/* First-run page. The form is available only until configuration is saved. */
const TOKEN_KEY = "apishare_token";
const $ = (id) => document.getElementById(id);
const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (ch) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[ch]));
const icon = (name) => '<svg class="icon" aria-hidden="true"><use href="#i-' + name + '"/></svg>';
let token = "";
const busyContents = new WeakMap();

function setBusy(button, busy, label = "处理中…") {
  if (busy) {
    if (!busyContents.has(button)) busyContents.set(button, button.innerHTML);
    button.innerHTML = icon("refresh") + "<span>" + esc(label) + "</span>";
  } else if (busyContents.has(button)) {
    button.innerHTML = busyContents.get(button);
    busyContents.delete(button);
  }
  button.disabled = busy;
  button.classList.toggle("is-loading", busy);
  button.setAttribute("aria-busy", String(busy));
}
function toast(message) {
  const item = document.createElement("div");
  item.className = "toast";
  item.innerHTML = icon("check") + "<span>" + esc(message) + "</span>";
  $("toastRegion").append(item);
  setTimeout(() => item.remove(), 4000);
}
function reportError(error) {
  showDeploymentError(error.field, error.message);
  $("setupRetry").classList.remove("hidden");
}
async function copyText(value, message) {
  try {
    if (!navigator.clipboard) throw new Error("当前浏览器不支持复制，请手动选择文本");
    await navigator.clipboard.writeText(value);
    toast(message);
  } catch (error) { showDeploymentError("", error.message); }
}
async function api(path, options = {}) {
  let response;
  try {
    response = await fetch(path, { ...options, headers: { "Content-Type": "application/json" } });
  } catch {
    throw new Error("无法连接服务，请检查网络后重试。");
  }
  if (response.status === 404) {
    clearDeploymentPasswords();
    deploymentConfig = null;
    location.replace("/");
    throw new Error("部署配置已完成，正在返回首页…");
  }
  const data = await response.json().catch(() => ({}));
  if (!response.ok) {
    const error = new Error(data.error || "请求失败，请稍后重试。");
    error.field = data.field || "";
    throw error;
  }
  return data;
}

document.querySelectorAll("svg.icon").forEach((svg) => { svg.setAttribute("aria-hidden", "true"); svg.setAttribute("focusable", "false"); });
window.addEventListener("beforeunload", (event) => { if (deploymentDirty()) { event.preventDefault(); event.returnValue = ""; } });
initDeployment();
loadDeployment();

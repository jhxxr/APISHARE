let deploymentConfig = null, deploymentSaving = false;
const DEPLOYMENT_NOTICE = "apishare_deployment_saved";

/* Deployment setup and administrator access. */
const deploymentFieldIDs = { current_password: "deploymentCurrentPassword", new_password: "deploymentNewPassword", confirm_password: "deploymentConfirmPassword", admin_path: "deploymentAdminPath" };
function clearDeploymentPasswords() {
  ["deploymentCurrentPassword", "deploymentNewPassword", "deploymentConfirmPassword"].forEach((id) => { if ($(id)) { $(id).value = ""; $(id).type = "password"; } });
  $("deploymentReveal").setAttribute("aria-pressed", "false");
  $("deploymentReveal").setAttribute("aria-label", "显示新密码和确认密码");
  $("deploymentReveal").lastElementChild.textContent = "显示密码";
}
function deploymentPathValue() { return $("deploymentAdminPath").value.trim().replace(/^\/+|\/+$/g, ""); }
function deploymentPathError() {
  const path = deploymentPathValue();
  if (!/^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$/.test(path)) return "请输入 3–64 位字母、数字、短横线或下划线，并以字母或数字开头。";
  if (["v1", "v1beta", "api", "public", "healthz", "static", "setup"].includes(path.toLowerCase())) return "此路径由系统使用，请选择其他后台路径。";
  return "";
}
function renderDeploymentPreview() {
  const path = deploymentPathValue(), invalid = deploymentPathError(), changed = !!deploymentConfig && "/" + path !== deploymentConfig.admin_path;
  const pending = changed || !!deploymentConfig?.setup_required;
  $("deploymentOrigin").textContent = location.origin;
  $("deploymentPreviewPath").textContent = path ? "/" + path + "/" : "/你的后台路径/";
  $("deploymentPreviewStatus").textContent = invalid ? "请填写有效的后台路径" : pending ? "预览地址 · 保存后生效" : "当前后台入口";
  $("deploymentPreviewStatus").classList.toggle("pending", pending && !invalid);
  $("deploymentPreviewStatus").classList.toggle("invalid", !!invalid);
  $("deploymentCopy").disabled = !!invalid || !deploymentConfig;
}
function renderDeploymentState() {
  const initial = deploymentConfig.setup_required;
  $("deploymentTitle").textContent = initial ? "完成部署配置" : "部署配置";
  $("deploymentDescription").textContent = initial ? "两项基础设置，让你的 API 工作空间准备就绪。" : "管理登录密码与后台入口，保存后立即生效。";
  $("deploymentWelcome").classList.toggle("hidden", !initial);
  $("deploymentPasswordHint").textContent = initial ? "为管理后台设置一个新的登录密码。" : "更新登录密码；只修改路径时，新密码可以留空。";
  if ($("deploymentCurrentLabel")) $("deploymentCurrentLabel").textContent = "当前管理员密码";
  $("deploymentNewPassword").required = initial;
  if ($("deploymentCurrentPassword")) $("deploymentCurrentPassword").required = !initial;
  $("deploymentConfirmPassword").required = initial || !!$("deploymentNewPassword").value;
  $("deploymentSave").innerHTML = icon("check") + "<span>" + (initial ? "完成配置并进入后台" : "保存配置") + "</span>";
  renderDeploymentPreview();
}
async function loadDeployment() {
  $("deploymentFields").disabled = true;
  try {
    const config = await api("api/deployment");
    const keepPath = deploymentConfig && deploymentPathValue() !== deploymentConfig.admin_path.slice(1);
    deploymentConfig = config;
    if (!keepPath) $("deploymentAdminPath").value = config.admin_path.slice(1);
    renderDeploymentState();
    $("deploymentFields").disabled = false;
    if (sessionStorage.getItem(DEPLOYMENT_NOTICE)) {
      sessionStorage.removeItem(DEPLOYMENT_NOTICE);
      $("deploymentSuccess").classList.remove("hidden");
    }
    if ($("setupRetry")) $("setupRetry").classList.add("hidden");
    if (typeof stampRefresh === "function") stampRefresh();
  } catch (error) { reportError(error, "部署配置加载失败"); }
}
function toggleDeploymentPasswords() {
  const visible = $("deploymentNewPassword").type === "password";
  ["deploymentNewPassword", "deploymentConfirmPassword"].forEach((id) => { $(id).type = visible ? "text" : "password"; });
  $("deploymentReveal").setAttribute("aria-pressed", String(visible));
  $("deploymentReveal").setAttribute("aria-label", visible ? "隐藏新密码和确认密码" : "显示新密码和确认密码");
  $("deploymentReveal").lastElementChild.textContent = visible ? "隐藏密码" : "显示密码";
}
function generateAdminPath() {
  const bytes = new Uint8Array(4);
  crypto.getRandomValues(bytes);
  $("deploymentAdminPath").value = "console-" + [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
  clearDeploymentError("admin_path");
  renderDeploymentPreview();
}
function copyDeploymentURL() { return copyText(location.origin + "/" + deploymentPathValue() + "/", "后台入口地址已复制"); }
function clearDeploymentError(field) {
  const id = deploymentFieldIDs[field];
  if ($(id)) $(id).removeAttribute("aria-invalid");
  if ($(id + "Error")) {
    $(id + "Error").textContent = "";
    $(id + "Error").classList.add("hidden");
  }
  $("deploymentError").classList.add("hidden");
}
function showDeploymentError(field, message) {
  const id = deploymentFieldIDs[field];
  if (id && $(id)) {
    $(id).setAttribute("aria-invalid", "true");
    $(id + "Error").textContent = message;
    $(id + "Error").classList.remove("hidden");
    $(id).focus();
  } else {
    $("deploymentError").textContent = message;
    $("deploymentError").classList.remove("hidden");
    $("deploymentError").focus();
  }
}
async function saveDeployment(event) {
  event.preventDefault();
  if (!deploymentConfig || deploymentSaving) return false;
  Object.keys(deploymentFieldIDs).forEach(clearDeploymentError);
  const password = $("deploymentNewPassword").value, confirmation = $("deploymentConfirmPassword").value;
  if (password && ([...password].length < 12 || new TextEncoder().encode(password).length > 72 || !password.trim())) { showDeploymentError("new_password", "新密码至少 12 个字符，最多 72 字节。"); return false; }
  if (deploymentConfig.setup_required && !password) { showDeploymentError("new_password", "首次配置请设置新的管理员密码。"); return false; }
  if (password !== confirmation) { showDeploymentError("confirm_password", "两次输入的密码不一致。"); return false; }
  const pathError = deploymentPathError();
  if (pathError) { showDeploymentError("admin_path", pathError); return false; }
  const request = { current_password: $("deploymentCurrentPassword")?.value || "", new_password: password, confirm_password: confirmation, admin_path: "/" + deploymentPathValue() };
  deploymentSaving = true;
  setBusy($("deploymentSave"), true, "正在保存…");
  $("deploymentFields").disabled = true;
  try {
    const initial = deploymentConfig.setup_required;
    const result = await api("api/deployment", { method: "PUT", body: JSON.stringify(request) });
    token = result.token;
    localStorage.setItem(TOKEN_KEY, token);
    clearDeploymentPasswords();
    deploymentConfig = result;
    if (initial) {
      sessionStorage.setItem(DEPLOYMENT_NOTICE, "initial");
      location.replace(result.admin_path + "/#dashboard");
    } else if (location.pathname !== result.admin_path + "/") {
      sessionStorage.setItem(DEPLOYMENT_NOTICE, "1");
      location.replace(result.admin_path + "/#deployment");
    } else {
      setBusy($("deploymentSave"), false);
      renderDeploymentState();
      $("deploymentSuccess").classList.remove("hidden");
      toast("部署配置已保存");
    }
  } catch (error) {
    if (error.message !== "unauthorized") {
      $("deploymentFields").disabled = false;
      showDeploymentError(error.field, error.message);
    }
  } finally {
    deploymentSaving = false;
    $("deploymentFields").disabled = !deploymentConfig;
    setBusy($("deploymentSave"), false);
  }
  return false;
}
function initDeployment() {
  Object.entries(deploymentFieldIDs).forEach(([field, id]) => $(id)?.addEventListener("input", () => {
    clearDeploymentError(field);
    $("deploymentSuccess").classList.add("hidden");
    if (field === "admin_path") renderDeploymentPreview();
    if (field === "new_password") $("deploymentConfirmPassword").required = !!$(id).value || !!deploymentConfig?.setup_required;
  }));
}
function deploymentDirty() {
  return !!deploymentConfig && (deploymentPathValue() !== deploymentConfig.admin_path.slice(1) || Object.values(deploymentFieldIDs).slice(0, 3).some((id) => $(id)?.value));
}

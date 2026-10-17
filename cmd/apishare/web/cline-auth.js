/* Browser device authorization for Cline accounts. */
const clineLoginState = { generation: 0, session: "", timer: null, controller: null, expires: 0 };

function cancelClineLogin(clearMessage = true) {
  const session = clineLoginState.session;
  clineLoginState.generation++;
  clearTimeout(clineLoginState.timer);
  clineLoginState.controller?.abort();
  clineLoginState.session = "";
  clineLoginState.controller = null;
  $("clineLoginChallenge").classList.add("hidden");
  $("clineLoginCode").textContent = "";
  $("clineManualAuthorization").value = "";
  $("clineManualAuthorization").setCustomValidity("");
  $("clineManualAuthorization").removeAttribute("aria-invalid");
  $("clineManualFill").classList.add("hidden");
  $("clineLoginLink").removeAttribute("href");
  setBusy($("clineLoginButton"), false);
  setBusy($("clineManualSubmit"), false);
  if (clearMessage) setClineLoginStatus("");
  if (session) api("api/cline/oauth/" + session, { method: "DELETE" }).catch(() => {});
}

function setClineLoginStatus(message, error = false) {
  const status = $("clineLoginStatus");
  status.textContent = message;
  status.classList.toggle("error", error);
}

function updateClineLoginProvider(changed = false) {
  if (changed) cancelClineLogin();
  const cline = editorUpstreamType() === "cline";
  $("clineLoginCard").classList.toggle("hidden", !cline);
  const official = $("editBase").value.trim().replace(/\/+$/, "") === providerURLs.cline;
  $("clineLoginButton").disabled = !official || !!clineLoginState.session;
  $("clineLoginHint").textContent = official ? "无需安装 CLI。可自动获取凭据，也可自行打开授权链接，再手动回填授权码或回调地址。" : "浏览器登录需要使用 Cline 默认地址；自建中转可手动填写凭据。";
}

async function startClineLogin() {
  cancelClineLogin();
  const generation = clineLoginState.generation;
  const controller = clineLoginState.controller = new AbortController();
  setBusy($("clineLoginButton"), true, "正在获取登录链接…");
  setClineLoginStatus("正在通过所选代理连接 Cline 登录服务…");
  try {
    const result = await api("api/cline/oauth/start", {
      method: "POST", signal: controller.signal,
      body: JSON.stringify({ proxy_id: Number($("editProxy").value), base_url: $("editBase").value.trim(), mode: $("clineLoginMode").value }),
    });
    if (generation !== clineLoginState.generation) {
      api("api/cline/oauth/" + result.session_id, { method: "DELETE" }).catch(() => {});
      return;
    }
    clineLoginState.session = result.session_id;
    clineLoginState.expires = Date.parse(result.expires_at);
    const manual = result.mode === "manual";
    $("clineLoginCode").textContent = result.user_code || "";
    $("clineLoginCodeRow").classList.toggle("hidden", manual);
    $("clineManualFill").classList.toggle("hidden", !manual);
    $("clineLoginLink").href = result.verification_uri;
    $("clineLoginChallenge").classList.remove("hidden");
    const instruction = manual ? "请自行打开授权链接登录，再粘贴完整回调地址或授权码并点击「完成回填」。" : "请打开授权页，核对验证码并登录。完成后回到这里，凭据会自动填入。";
    setClineLoginStatus(instruction + "有效至 " + new Date(clineLoginState.expires).toLocaleTimeString("zh-CN", { hour12: false }) + "。");
    if (!manual) clineLoginState.timer = setTimeout(() => pollClineLogin(generation), 2000);
  } catch (error) {
    if (generation === clineLoginState.generation && error.name !== "AbortError" && error.message !== "unauthorized") setClineLoginStatus("发起登录失败：" + error.message, true);
  } finally {
    if (generation === clineLoginState.generation) {
      setBusy($("clineLoginButton"), false);
      updateClineLoginProvider();
    }
  }
}

async function pollClineLogin(generation) {
  if (generation !== clineLoginState.generation || !clineLoginState.session) return;
  if (Date.now() >= clineLoginState.expires + 60000) {
    cancelClineLogin(false);
    setClineLoginStatus("验证码已过期，请重新登录。", true);
    updateClineLoginProvider();
    return;
  }
  try {
    const result = await api("api/cline/oauth/" + clineLoginState.session, { signal: clineLoginState.controller.signal, cache: "no-store" });
    if (generation !== clineLoginState.generation) return;
    if (result.done) {
      finishClineLoginResult(result);
      return;
    }
  } catch (error) {
    if (generation !== clineLoginState.generation || error.name === "AbortError" || error.message === "unauthorized") return;
    if (error.status === 404 || error.status === 410) {
      cancelClineLogin(false);
      setClineLoginStatus(error.message, true);
      updateClineLoginProvider();
      return;
    }
    setClineLoginStatus("暂时无法读取登录状态，将自动重试：" + error.message, true);
  }
  clineLoginState.timer = setTimeout(() => pollClineLogin(generation), 2000);
}

function finishClineLoginResult(result) {
  cancelClineLogin(false);
  if (result.error || !result.refresh_token) {
    setClineLoginStatus(result.error || "登录未返回账号凭据，请重试。", true);
  } else {
    const field = $("editKey");
    field.value = result.refresh_token;
    field.type = "password";
    field.setCustomValidity("");
    field.removeAttribute("aria-invalid");
    $("revealKeyButton").setAttribute("aria-pressed", "false");
    $("revealKeyButton").setAttribute("aria-label", "显示账号凭据");
    clearProbeResults();
    updateEditorDirty();
    setClineLoginStatus("已登录" + (result.email ? " " + result.email : "") + "，账号凭据已填入。请获取模型并保存连接。");
    toast("Cline 登录成功，账号凭据已填入");
  }
  updateClineLoginProvider();
}

async function submitClineManualLogin() {
  if (!clineLoginState.session || $("clineLoginMode").value !== "manual") return;
  const field = $("clineManualAuthorization");
  if (!field.value.trim()) {
    field.setCustomValidity("请粘贴授权码或完整回调地址");
    field.setAttribute("aria-invalid", "true");
    field.reportValidity(); field.focus(); return;
  }
  const generation = clineLoginState.generation;
  setBusy($("clineManualSubmit"), true, "正在完成授权…");
  setClineLoginStatus("正在通过所选代理换取账号凭据…");
  try {
    const result = await api("api/cline/oauth/" + clineLoginState.session + "/complete", {
      method: "POST", signal: clineLoginState.controller.signal,
      body: JSON.stringify({ authorization: field.value.trim() }),
    });
    if (generation === clineLoginState.generation) finishClineLoginResult(result);
  } catch (error) {
    if (generation !== clineLoginState.generation || error.name === "AbortError" || error.message === "unauthorized") return;
    setClineLoginStatus("回填失败：" + error.message, true);
    if (error.status === 404 || error.status === 410) { cancelClineLogin(false); updateClineLoginProvider(); }
  } finally {
    if (generation === clineLoginState.generation) setBusy($("clineManualSubmit"), false);
  }
}

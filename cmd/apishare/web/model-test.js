/* A single-turn test sent directly to a saved upstream. */
const modelTest = { id: null, controller: null, probeController: null };

function openModelTest(id) {
  const upstream = upsCache.find((item) => item.id === id);
  if (!upstream) return;
  modelTest.id = id;
  $("modelTestDescription").textContent = "向「" + upstream.name + "」发送真实模型请求，使用已保存的配置，可能产生上游费用。";
  let mapping = {};
  try { mapping = JSON.parse(upstream.model_map || "{}"); } catch { /* Allow testing legacy configurations. */ }
  const models = [...new Set([...parseAllowlist(upstream.models), ...Object.keys(mapping)])];
  fillModelTestOptions(models);
  $("modelTestModel").value = models[0] || "";
  $("modelTestMessage").value = "hi";
  $("modelTestMaxTokens").value = "4096";
  $("modelTestResult").classList.add("hidden");
  $("modelTestError").classList.add("hidden");
  $("modelTestHint").textContent = models.length ? "可选择已配置模型，也可手动输入其他模型；模型映射会自动生效。" : "请输入模型名称，或点击“获取模型”加载上游模型列表。";
  $("modelTestDialog").showModal(); syncModalLock();
  $("modelTestModel").focus();
}

function fillModelTestOptions(models) {
  $("modelTestOptions").replaceChildren(...models.map((model) => {
    const option = document.createElement("option"); option.value = model; return option;
  }));
}

function closeModelTest() {
  $("modelTestDialog").close();
  modelTest.controller?.abort(); modelTest.probeController?.abort();
  modelTest.controller = null; modelTest.probeController = null;
  $("modelTestFields").disabled = false;
  $("modelTestForm").setAttribute("aria-busy", "false");
  $("modelTestProgress").textContent = "";
  setBusy($("modelTestFetch"), false); setBusy($("modelTestSend"), false);
  syncModalLock();
}

async function fetchModelTestModels() {
  if (modelTest.probeController) return;
  const controller = new AbortController(); modelTest.probeController = controller;
  setBusy($("modelTestFetch"), true, "获取中…");
  try {
    const result = await api("api/upstreams/" + modelTest.id + "/probe", { method: "POST", body: "{}", signal: controller.signal });
    if (controller.signal.aborted) return;
    if (!result.ok) throw new Error(result.error || "获取模型失败");
    const models = [...new Set([...$("modelTestOptions").options].map((option) => option.value).concat(result.models || []))];
    fillModelTestOptions(models);
    if (!$("modelTestModel").value) $("modelTestModel").value = models[0] || "";
    $("modelTestHint").textContent = "已获取 " + (result.models || []).length + " 个上游模型，也可手动输入模型名称。";
  } catch (error) {
    if (!controller.signal.aborted && error.name !== "AbortError") $("modelTestHint").textContent = "获取失败：" + error.message + "。仍可手动输入模型名称。";
  } finally {
    if (modelTest.probeController === controller) {
      modelTest.probeController = null; setBusy($("modelTestFetch"), false);
    }
  }
}

async function submitModelTest(event) {
  event.preventDefault();
  if (modelTest.controller) return;
  const model = $("modelTestModel").value.trim();
  if (!model) { $("modelTestModel").setCustomValidity("请填写模型名称。"); $("modelTestModel").reportValidity(); return; }
  const controller = new AbortController(); modelTest.controller = controller;
  $("modelTestFields").disabled = true;
  $("modelTestResult").classList.add("hidden"); $("modelTestError").classList.add("hidden");
  $("modelTestForm").setAttribute("aria-busy", "true");
  $("modelTestProgress").textContent = "正在等待模型回复，最长等待 90 秒…";
  setBusy($("modelTestSend"), true, "测试中…");
  try {
    const result = await api("api/upstreams/" + modelTest.id + "/model-test", {
      method: "POST", signal: controller.signal,
      body: JSON.stringify({ model, message: $("modelTestMessage").value, max_tokens: Number($("modelTestMaxTokens").value) })
    });
    if (controller.signal.aborted) return;
    if (!result.ok) throw new Error((result.error || "模型测试失败") + "（耗时 " + result.latency_ms + " ms）");
    $("modelTestSummary").textContent = "测试成功 · " + result.latency_ms + " ms · " + result.upstream_model + (result.input_tokens != null ? " · 输入 " + result.input_tokens + " Token" : "") + (result.output_tokens != null ? " · 输出 " + result.output_tokens + " Token" : "");
    $("modelTestReply").textContent = result.reply || "模型未返回文本。结束原因：" + (result.finish_reason || "未知") + "。可增加最大输出 Token 后重试。";
    $("modelTestThinking").textContent = result.thinking || "";
    $("modelTestThinkingDetails").classList.toggle("hidden", !result.thinking);
    $("modelTestThinkingDetails").open = false;
    $("modelTestResult").classList.remove("hidden");
  } catch (error) {
    if (!controller.signal.aborted && error.name !== "AbortError") {
      $("modelTestError").textContent = error.message;
      $("modelTestError").classList.remove("hidden");
    }
  } finally {
    if (modelTest.controller === controller) {
      modelTest.controller = null; $("modelTestFields").disabled = false;
      $("modelTestForm").setAttribute("aria-busy", "false");
      $("modelTestProgress").textContent = ""; setBusy($("modelTestSend"), false);
    }
  }
}

function initModelTest() {
  $("modelTestForm").addEventListener("submit", submitModelTest);
  $("modelTestModel").addEventListener("input", () => $("modelTestModel").setCustomValidity(""));
  $("modelTestDialog").addEventListener("cancel", (event) => { event.preventDefault(); closeModelTest(); });
  $("modelTestDialog").addEventListener("close", syncModalLock);
}

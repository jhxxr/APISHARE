const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const root = path.resolve(__dirname, "..");
const read = (file) => fs.readFileSync(path.join(root, file), "utf8");
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function personalConsole(overrides = {}) {
  const nodes = new Map();
  const $ = (id) => {
    if (!nodes.has(id)) nodes.set(id, {
      value: "", checked: false, disabled: false, innerHTML: "", textContent: "",
      classList: { remove() {}, add() {}, toggle() {} },
      attributes: {}, setAttribute(name, value) { this.attributes[name] = value; },
    });
    return nodes.get(id);
  };
  const view = {
    enabled: true, user_enabled: true, mode: "strict", persona_id: "neutral", revision: 3,
    models: ["alias", '<model & "name">'],
    personas: [{ id: "neutral", name: "中性模板", characters: 10, text: "Original template", source: {} }],
    selected_clients: ["codex"], clients: [{ id: "codex", name: "Codex", available: true }],
    ...overrides,
  };
  const requests = [], notices = [];
  const context = vm.createContext({
    $, esc, fmtInt: String, state: { agentPurge: view },
    $$: (selector) => selector === "[data-purge-client]:checked" ? view.selected_clients.map((id) => ({ dataset: { purgeClient: id } })) : [],
    toast: (value) => notices.push(value),
    fetch: async (url, options) => {
      requests.push({ url, options, payload: JSON.parse(options.body) });
      return { ok: true, json: async () => view };
    },
  });
  const source = read("cmd/apishare/web/public/user.js");
  vm.runInContext(source.slice(source.indexOf("  async function loadAgentPurgeSettings("), source.indexOf("  function bindEvents(")), context);
  vm.runInContext("renderAgentPurgeSettings()", context);
  return { $, context, view, requests, notices };
}

test("personal role settings show a read-only escaped model list without channel checkboxes", () => {
  const { $ } = personalConsole();
  const models = $("#agentPurgeModels").innerHTML;
  assert.match(models, /<li class="agent-purge-model">alias<\/li>/);
  assert.match(models, /&lt;model &amp; &quot;name&quot;&gt;/);
  assert.doesNotMatch(models, /<input|<label|<model/);
  assert.match($("#agentPurgeSelectionNote").textContent, /2 个模型.*无需选择渠道/);
  assert.equal($("#agentPurgeStatus").textContent, "已选择启用");
  const html = read("cmd/apishare/web/public/user.html");
  assert.match(html, /id="agentPurgeModels"/);
  assert.doesNotMatch(html, /id="agentPurgePairs"/);
});

test("saving a role requires no model or upstream choices", async () => {
  const { $, context, requests, notices } = personalConsole();
  await vm.runInContext("saveAgentPurgeSettings()", context);
  assert.equal(requests.length, 1);
  assert.deepEqual(requests[0].payload, { enabled: true, mode: "strict", persona_id: "neutral", revision: 3, clients: ["codex"] });
  assert.equal(requests[0].options.headers["X-APIShare-Request"], "user-console");
  assert.equal($("#saveAgentPurgeUser").attributes["aria-busy"], "false");
  assert.ok(notices.includes("个性化提示词设置已保存"));
});

test("no usable models are explained without requiring users to select nonexistent channels", async () => {
  const { $, context, requests } = personalConsole({ models: [] });
  assert.equal($("#agentPurgeStatus").textContent, "暂无可用模型");
  assert.match($("#agentPurgeModels").innerHTML, /当前没有管理员开放且你有权使用/);
  await vm.runInContext("saveAgentPurgeSettings()", context);
  assert.equal(requests.length, 1);
});

test("missing client selection remains actionable and blocks an ineffective enabled save", async () => {
  const { context, requests, notices } = personalConsole({ selected_clients: [] });
  await vm.runInContext("saveAgentPurgeSettings()", context);
  assert.equal(requests.length, 0);
  assert.deepEqual(notices, ["请至少选择一类适用客户端"]);
});

function adminLogs(log) {
  const body = { innerHTML: "" };
  const table = { querySelector: () => body, querySelectorAll: () => [] };
  const context = vm.createContext({
    $: () => table, esc, logsCache: [log], logPerformance: () => ({}),
    fmtTime: () => "2026-11-21 12:00:00", logTimeLabel: () => "12:00",
    modelIcon: () => "", logClientLabel: () => "Codex", logClientNote: () => "",
    compactUpstreamNumber: String, fmtUSD: () => "$0.00", applyLogColumns() {},
  });
  const source = read("cmd/apishare/web/logs.js");
  vm.runInContext(source.slice(source.indexOf("function logAgentPurgeLabel("), source.indexOf("function applyLogColumns(")), context);
  vm.runInContext("renderLogs()", context);
  return body.innerHTML;
}

test("history displays a persona result only for recorded application and escapes template names", () => {
  const base = { id: 1, status: 200, model: "alias", upstream_name: "approved", billing: {} };
  assert.doesNotMatch(adminLogs(base), /角色.*成功|log-purge-status/);
  for (const mode of ["append", "replace", "strict"]) {
    const html = adminLogs({ ...base, billing: { agent_purge: { mode, name: '<template "unsafe">' } } });
    assert.match(html, mode === "append" ? /角色追加成功/ : /角色替换成功/);
    assert.match(html, /&lt;template &quot;unsafe&quot;&gt;/);
    assert.doesNotMatch(html, /<template "unsafe">/);
  }
});

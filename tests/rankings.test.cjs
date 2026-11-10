const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const root = path.resolve(__dirname, "..");
const read = (file) => fs.readFileSync(path.join(root, file), "utf8");

function element() {
  const children = new Map();
  const classes = new Set();
  return {
    value: "", innerHTML: "", textContent: "", disabled: false, listeners: {},
    classList: {
      add(name) { classes.add(name); },
      remove(name) { classes.delete(name); },
      contains(name) { return classes.has(name); },
      toggle(name, force) { if (force) classes.add(name); else classes.delete(name); },
    },
    addEventListener(name, callback) { this.listeners[name] = callback; },
    setAttribute(name, value) { this[name] = value; },
    querySelector(selector) { if (!children.has(selector)) children.set(selector, element()); return children.get(selector); },
    focus() {},
  };
}

async function rankingsPage(models) {
  const nodes = new Map();
  const get = (id) => { if (!nodes.has(id)) nodes.set(id, element()); return nodes.get(id); };
  get("rankingSort").value = "overall_score";
  const fixture = { models, updated_at: "2026-11-10T15:00:00+08:00", models_loading: false };
  let fail = false;
  const context = vm.createContext({
    document: { getElementById: get }, window: {}, console, AbortController, setTimeout, clearTimeout,
    fetch: async () => {
      if (fail) throw new Error("offline");
      return { ok: true, json: async () => fixture };
    },
  });
  vm.runInContext(read("cmd/apishare/web/public/rankings.js"), context);
  await new Promise(setImmediate);
  return { get, fail: () => { fail = true; } };
}

function model(name, score, extra = {}) {
  return {
    model: name, family: "Example", bandit_calls: 0,
    capabilities: { overall_score: score, coding_swe: score, frontend_ui: score, reasoning_math: score, agentic_tools: score, writing_knowledge: score, long_context_mm: score, benchmark_source: "inferred" },
    ...extra,
  };
}

test("full catalog includes no-sample models and preserves ranks when searching", async () => {
  const page = await rankingsPage([model("weaker", 70), model("stronger", 95)]);
  const { get } = page;
  assert.match(get("rankingResultCount").textContent, /2 \/ 2/);
  assert.ok(get("rankingList").innerHTML.indexOf("stronger") < get("rankingList").innerHTML.indexOf("weaker"));
  assert.match(get("rankingList").innerHTML, /暂无在线样本/);
  get("rankingSearch").value = "weaker";
  get("rankingSearch").listeners.input();
  assert.match(get("rankingResultCount").textContent, /1 \/ 2/);
  assert.match(get("rankingList").innerHTML, /第 2 名/);
  assert.doesNotMatch(get("rankingList").innerHTML, /stronger/);
});

test("latency sorting is ascending and missing measurements sort last", async () => {
  const { get } = await rankingsPage([
    model("unmeasured", 100), model("slow", 90, { bandit_calls: 3, ewma_ttft_ms: 3000 }),
    model("fast", 80, { bandit_calls: 5, ewma_ttft_ms: 200 }),
  ]);
  get("rankingSort").value = "ewma_ttft_ms";
  get("rankingSort").listeners.change();
  const html = get("rankingList").innerHTML;
  assert.ok(html.indexOf(">fast<") < html.indexOf(">slow<"));
  assert.ok(html.indexOf(">slow<") < html.indexOf(">unmeasured<"));
  assert.match(html, /暂无名次/);
  assert.match(html, /200ms/);
  assert.doesNotMatch(get("rankingLeaders").innerHTML, /unmeasured/);
});

test("model names are escaped and an unsuccessful refresh retains prior results", async () => {
  const page = await rankingsPage([model('<img src=x onerror="alert(1)">', 88)]);
  assert.doesNotMatch(page.get("rankingList").innerHTML, /<img/);
  assert.match(page.get("rankingList").innerHTML, /&lt;img/);
  const before = page.get("rankingList").innerHTML;
  page.fail();
  await page.get("rankingRefresh").listeners.click();
  assert.equal(page.get("rankingList").innerHTML, before);
  assert.match(page.get("rankingError").textContent, /保留上次/);
  assert.equal(page.get("rankingRefresh").disabled, false);
  assert.equal(page.get("rankingList")["aria-busy"], "false");
});

test("empty catalogs can be rendered without inventing scores", async () => {
  const { get } = await rankingsPage([]);
  assert.equal(get("rankingList").innerHTML, "");
  assert.equal(get("rankingEmpty").querySelector("h3").textContent, "暂未接入模型");
  assert.equal(get("rankingModelCount").textContent, "0");
});

test("admin reset uses the registered route and capability tags use the API field", async () => {
  let requested;
  const context = vm.createContext({
    esc: String, setBusy() {}, toast() {}, reportError(error) { throw error; },
    api: async (url) => { requested = url; },
  });
  vm.runInContext(read("cmd/apishare/web/router.js"), context);
  vm.runInContext("loadAutoRouter = async () => {};", context);
  await vm.runInContext("resetAutoRouterBanditState({})", context);
  const route = read("cmd/apishare/auto_router.go").match(/auth.POST\("(\/auto-router\/bandit\/reset)"/)[1];
  assert.equal(requested, "api" + route);
  vm.runInContext(`routerConfigCache = {model_health: {example: {capabilities: {coding_swe: 90, frontend_ui: 88, reasoning_math: 80, agentic_tools: 75}, bandit_calls: 5, bandit_win_rate: 0.9}}};`, context);
  const tags = vm.runInContext("capabilityTagsForRouterModel('example')", context);
  assert.match(tags, /SWE 90/);
  assert.match(tags, /胜率 90%/);
});

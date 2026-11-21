const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const source = fs.readFileSync(path.resolve(__dirname, "../cmd/apishare/web/pricing.js"), "utf8");
function pricingContext() {
  const nodes = new Map();
  const $ = (id) => {
    if (!nodes.has(id)) nodes.set(id, { value: "", checked: false, textContent: "", innerHTML: "", disabled: false, classList: { toggle() {} }, addEventListener() {}, setCustomValidity() {}, setAttribute() {} });
    return nodes.get(id);
  };
  const context = vm.createContext({ $, document: { querySelectorAll: () => [] }, requestVersions: {}, number: String, fmtTime: String, stampRefresh() {}, esc: (s) => String(s).replaceAll("<", "&lt;").replaceAll('"', "&quot;"), modelLabel: (s) => String(s).replaceAll("<", "&lt;"), icon: () => "", reportError() {} });
  vm.runInContext(source, context);
  return { context, $, run: (code) => vm.runInContext(code, context) };
}

test("pricing JSON keeps explicit free rates, rejects invalid prices and catches misspelled fields", () => {
  const { run } = pricingContext();
  const parsed = run(`validatePricingJSON('{"default":{"in":0,"out":0},"model":{"in":2,"out":8,"cache_read":0,"reasoning":null}}')`);
  assert.equal(parsed.model.cache_read, 0);
  for (const data of [null, [], {}, { model: { in: -1, out: 1 } }, { model: { in: "1", out: 2 } }, { model: { in: 1 } }, { model: { in: 1, out: 2, typo: 3 } }, { "deepseek-v4-flash": { in: 1, out: 2 } }]) {
    assert.throws(() => run(`validatePricingJSON(${JSON.stringify(JSON.stringify(data))})`));
  }
});

test("model pricing filters and paginates the effective price table", () => {
  const { $, run } = pricingContext();
  run(`pricingCatalog = { pricing: { default: {} }, auto_pricing: { model_count: 120 }, models: Array.from({length: 120}, (_, i) => ({ model: 'model-' + i, pricing_model: 'model-' + i, price: { in: 2, out: 8, cache_read: 0 }, source: i % 2 ? 'official' : 'override', connected: i < 3 })) }; renderPricingTable();`);
  assert.equal(( $("pricingTableBody").innerHTML.match(/<tr>/g) || []).length, 50);
  assert.equal($("pricingPageInfo").textContent, "1–50 / 120");
  assert.match($("pricingTableBody").innerHTML, /\$0<\/td>/);
  run("changePricingPage(1)");
  assert.equal($("pricingPageInfo").textContent, "51–100 / 120");
  $("pricingScopeFilter").value = "connected";
  $("pricingSourceFilter").value = "override";
  run("filterPricing()");
  assert.equal($("pricingTotal").textContent, "2");
  assert.equal($("pricingPrevious").disabled, true);
  $("pricingSearch").value = "missing";
  run("filterPricing()");
  assert.match($("pricingTableBody").innerHTML, /没有匹配的模型/);
});

test("refreshing after row edits preserves unrelated JSON and sync drafts", async () => {
  const { context, $, run } = pricingContext();
  run(`pricingCatalog = { pricing: { default: {in:0,out:0} }, auto_pricing: { enabled: true, url: 'saved-url' } };`);
  $("setPricing").value = '{"draft":{"in":1,"out":2}}';
  $("setAutoPricing").checked = false;
  $("setPricingURL").value = "draft-url";
  context.api = async () => ({ pricing: { default: { in: 0, out: 0 }, saved: { in: 3, out: 4 } }, auto_pricing: { enabled: true, url: "saved-url" }, models: [] });
  await run("loadPricing(true)");
  assert.equal($("setPricing").value, '{"draft":{"in":1,"out":2}}');
  assert.equal($("setPricingURL").value, "draft-url");
  assert.equal($("setAutoPricing").checked, false);
});

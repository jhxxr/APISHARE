const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../cmd/apishare/web/request-redaction.js"), "utf8");

test("redaction script waits for shared app helpers before binding controls", () => {
  const ready = [], handlers = new Map();
  const document = {
    readyState: "loading",
    addEventListener(name, callback, options) {
      assert.equal(name, "DOMContentLoaded");
      assert.equal(options.once, true);
      ready.push(callback);
    },
    querySelector() { return { addEventListener() {} }; },
  };
  const context = vm.createContext({ document });
  // Production loads request-redaction.js before app.js defines $.
  assert.doesNotThrow(() => vm.runInContext(source, context));
  assert.equal(ready.length, 1);
  context.$ = (id) => ({ addEventListener(event, callback) { handlers.set(id + ":" + event, callback); } });
  ready[0]();
  assert.equal(typeof handlers.get("saveRequestRedaction:click"), "function");
  assert.equal(typeof handlers.get("requestRedactionDryRun:click"), "function");
  assert.equal(typeof handlers.get("requestRedactionCategories:change"), "function");
});

test("redaction controls also bind when loaded after DOM readiness", () => {
  const bound = [];
  const context = vm.createContext({
    document: { readyState: "complete", querySelector() { return null; } },
    $: (id) => ({ addEventListener(event) { bound.push(id + ":" + event); } }),
  });
  vm.runInContext(source, context);
  assert.ok(bound.includes("saveRequestRedaction:click"));
  assert.ok(bound.includes("requestRedactionDryRun:click"));
});

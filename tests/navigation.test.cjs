const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const source = fs.readFileSync(path.resolve(__dirname, "../cmd/apishare/web/app.js"), "utf8");
const definitions = source.match(/^const (?:tabNames|validTab) = .*$/gm).join("\n");
const resolver = source.slice(source.indexOf("function tabFromHistory("), source.indexOf("function followHistory("));

test("retired and unknown navigation links resolve to the dashboard without adding history", () => {
  for (const hash of ["#router", "#missing", ""]) {
    const replacements = [];
    const context = vm.createContext({ location: { hash }, history: { replaceState: (...args) => replacements.push(args[2]) } });
    vm.runInContext(definitions + "\n" + resolver, context);
    assert.equal(vm.runInContext("tabFromHistory()", context), "dashboard");
    assert.deepEqual(replacements, hash ? ["#dashboard"] : []);
  }
});

test("supported deep links retain their destination", () => {
  for (const name of ["settings", "pricing", "logs", "models", "upstreams/7"]) {
    const context = vm.createContext({ location: { hash: "#" + name }, history: { replaceState() { throw Error("valid route was replaced"); } } });
    vm.runInContext(definitions + "\n" + resolver, context);
    assert.equal(vm.runInContext("tabFromHistory()", context), name);
  }
});

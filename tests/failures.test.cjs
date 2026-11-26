const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname, "../cmd/apishare/web/failures.js"), "utf8");

function setup() {
  const elements = new Map();
  const $ = (id) => {
    if (!elements.has(id)) elements.set(id, { innerHTML: "", textContent: "", value: "", disabled: false });
    return elements.get(id);
  };
  const esc = (value) => String(value).replace(/[&<>"']/g, (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[char]);
  const context = vm.createContext({ $, esc, failuresPage: 1 });
  vm.runInContext(source, context);
  return { context, $ };
}

function entry(extra = {}) {
  return { request: { model: "alias", redacted: true, truncated: true }, upstream: { failure_kind: "client_disconnected", http_status: 200, summary: "client_disconnected: request canceled or client disconnected (upstream HTTP 200)" }, ...extra };
}

test("redaction omission is separate from the actionable failure", () => {
  const { context, $ } = setup();
  context.renderFailureLogDetail(entry({ content_omitted_reason: "request_redaction_policy" }));
  const html = $("failureLogDetail").innerHTML;
  assert.match(html, /失败摘要.*client_disconnected: request canceled or client disconnected/s);
  assert.match(html, /切换“完整原文”也不会覆盖该策略/);
  assert.match(html, /不代表上游没有返回正文/);
  assert.match(html, /请求头因请求强制脱敏策略未保存/);
  assert.match(html, /failure-badge">未保存/);
  assert.doesNotMatch(html, /failure-badge">已截断/);
  assert.doesNotMatch(html, /请在“保存设置”中切换为完整原文/);
});

test("legacy redaction records show the retained failure kind without inventing details", () => {
  const { context, $ } = setup();
  const summary = "upstream request failed; response omitted by request redaction policy";
  context.renderFailureLogDetail(entry({ upstream: { failure_kind: "client_disconnected", summary } }));
  assert.match($("failureLogDetail").innerHTML, /失败摘要.*client_disconnected（旧记录未保存具体错误信息）/s);
  assert.match($("failureLogDetail").innerHTML, /已省略的内容无法恢复/);
  context.renderFailureLogList({ enabled: true, total: 1, items: [{ name: "legacy.json", summary, failure_kind: "client_disconnected" }] });
  assert.match($("failureLogList").innerHTML, /client_disconnected（旧记录未保存具体错误信息）/);
  assert.doesNotMatch($("failureLogList").innerHTML, /response omitted by request redaction policy/);
});

test("ordinary redacted and full-content records retain their distinct empty-body guidance", () => {
  const { context, $ } = setup();
  context.renderFailureLogDetail(entry());
  assert.match($("failureLogDetail").innerHTML, /若无强制脱敏策略限制/);
  assert.match($("failureLogDetail").innerHTML, /不会恢复已有记录/);
  assert.doesNotMatch($("failureLogDetail").innerHTML, /切换“完整原文”也不会覆盖该策略/);
  context.renderFailureLogDetail(entry({ request: { redacted: false } }));
  assert.match($("failureLogDetail").innerHTML, /该记录没有保存请求正文/);
  assert.doesNotMatch($("failureLogDetail").innerHTML, /脱敏保存模式下未保存请求正文/);
});

test("stored size truncation uses the server body_truncated flag and escapes diagnostics", () => {
  const { context, $ } = setup();
  context.renderFailureLogDetail(entry({ request: { body: { model: "alias" } }, upstream: { summary: "<script>unsafe</script>", body: "<private>", body_truncated: true } }));
  const html = $("failureLogDetail").innerHTML;
  assert.match(html, /上游返回 <span class="failure-badge">已截断/);
  assert.match(html, /&lt;script&gt;unsafe&lt;\/script&gt;/);
  assert.match(html, /&lt;private&gt;/);
  assert.doesNotMatch(html, /<script>unsafe/);
});

test("capture setting warns that request redaction takes precedence", () => {
  const { context, $ } = setup();
  for (const mode of ["true", "false"]) {
    $("failureLogFullContent").value = mode;
    context.warnFullContentScope();
    assert.match($("failureLogSettingsNote").textContent, /强制脱敏/);
    assert.match($("failureLogSettingsNote").textContent, /保留安全的失败原因/);
  }
});

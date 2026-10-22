/* Per-account request outcomes; an idle hour never means 100% availability. */
function poolUptimeRate(value) {
  return Number.isFinite(value) ? new Intl.NumberFormat("zh-CN", {maximumFractionDigits: 2}).format(value) + "%" : "—";
}
function poolUptimeTone(calls, successes) {
  return !calls ? "idle" : successes === calls ? "good" : !successes ? "bad" : "mixed";
}
function poolUptimeHourLabel(hour) {
  return fmtTime(hour.start).slice(5, 16) + " – " + fmtTime(hour.end).slice(5, 16) + " · " +
    (hour.calls ? hour.calls + " 次调用，" + hour.successes + " 次成功，" + hour.failures + " 次失败，Uptime " + poolUptimeRate(hour.uptime_percent) : "无调用");
}
function poolUptimeBars(history) {
  return (history || Array.from({length: 24}, () => ({calls: 0, successes: 0}))).map(hour =>
    '<span class="' + poolUptimeTone(hour.calls, hour.successes) + '" title="' + esc(hour.start ? poolUptimeHourLabel(hour) : "暂无调用") + '"></span>'
  ).join("");
}
function poolAccountUptimeHTML(item, index) {
  const data = item.uptime, key = item.id || item.name, selected = coreUI.uptimeAccount === key;
  if (item.pending || coreUI.uptimeError || !data) return '<div class="pool-uptime-placeholder"><strong>—</strong><small>' + (item.pending ? "导入后开始统计" : coreUI.uptimeError ? "统计暂不可用" : "暂无调用记录") + '</small></div>';
  const rate = poolUptimeRate(data.uptime_percent), tone = poolUptimeTone(data.calls, data.successes);
  const name = item.label || item.email || item.name || item.id;
  const label = name + "，近 24 小时 Uptime " + rate + "，" + data.successes + " 次成功 / " + data.calls + " 次调用，查看每小时明细";
  return '<button type="button" class="pool-uptime-button" data-account-uptime="' + index + '" data-uptime-key="' + esc(key) + '" aria-label="' + esc(label) + '" aria-expanded="' + selected + '" aria-controls="poolAccountUptimeDetail"><span class="pool-uptime-heading"><strong class="' + tone + '">' + rate + '</strong><small>24h</small></span><span class="pool-uptime-bars" aria-hidden="true">' + poolUptimeBars(data.history) + '</span><span class="pool-uptime-caption">' + (data.calls ? number(data.successes) + ' / ' + number(data.calls) + ' 次成功' : '暂无调用') + '</span></button>';
}
function showPoolAccountUptime(item) {
  resetPoolAccountDetails();
  coreUI.uptimeAccount = item.id || item.name;
  renderCoreAccounts();
  const panel = $("poolAccountUptimeDetail");
  panel.scrollIntoView({block: "nearest", behavior: motionBehavior()});
  panel.focus({preventScroll: true});
}
function closePoolAccountUptime() {
  const key = coreUI.uptimeAccount;
  coreUI.uptimeAccount = null;
  renderCoreAccounts();
  [...document.querySelectorAll("[data-uptime-key]")].find(button => button.dataset.uptimeKey === key)?.focus({preventScroll: true});
}
function renderPoolAccountUptime() {
  const note = $("poolUptimeNote");
  note.classList.remove("hidden");
  note.textContent = coreUI.uptimeError || "Uptime = 成功尝试 ÷ 总尝试 · 最近 24 小时 · 每格 1 小时 · 无调用显示灰色 · 每 30 秒更新";
  note.classList.toggle("form-error", !!coreUI.uptimeError);
  const item = coreUI.credentials.find(item => (item.id || item.name) === coreUI.uptimeAccount);
  const data = item?.uptime;
  const panel = $("poolAccountUptimeDetail");
  panel.classList.toggle("hidden", !data || !!coreUI.uptimeError);
  if (!data || coreUI.uptimeError) return;
  $("poolAccountUptimeName").textContent = item.label || item.email || item.name || item.id;
  $("poolUptimeDetailWindow").textContent = fmtTime(data.window_start).slice(5, 16) + " – " + fmtTime(data.updated_at).slice(5, 16) + " · 北京时间";
  $("poolUptimeDetailRate").textContent = poolUptimeRate(data.uptime_percent);
  $("poolUptimeDetailCalls").textContent = number(data.calls);
  $("poolUptimeDetailResults").textContent = number(data.successes) + " / " + number(data.failures);
  $("poolUptimeDetailBars").innerHTML = poolUptimeBars(data.history);
  $("poolUptimeDetailBars").setAttribute("aria-label", "24 小时调用轨迹，成功 " + data.successes + " 次，失败 " + data.failures + " 次");
  $("poolUptimeDetailLast").textContent = data.last_call ? "最近调用：" + fmtTime(data.last_call) : "最近 24 小时没有调用，Uptime 暂无数据。";
  $("poolUptimeDetailRows").innerHTML = [...data.history].reverse().map(hour => '<tr><td>' + esc(fmtTime(hour.start).slice(5, 16) + " – " + fmtTime(hour.end).slice(11, 16)) + '</td><td>' + number(hour.calls) + '</td><td>' + number(hour.successes) + '</td><td>' + number(hour.failures) + '</td><td>' + poolUptimeRate(hour.uptime_percent) + '</td></tr>').join("");
}
function schedulePoolUptimeRefresh() {
  clearTimeout(coreUI.refreshTimer);
  if (!coreUI.poolID || !editorIsOpen()) return;
  const id = coreUI.poolID, epoch = coreUI.epoch;
  coreUI.refreshTimer = setTimeout(() => {
    if (id !== coreUI.poolID || epoch !== coreUI.epoch || !editorIsOpen()) return;
    if (document.hidden || document.activeElement?.closest(".pool-accounts-table")) schedulePoolUptimeRefresh();
    else loadCLIProxy();
  }, 30000);
}

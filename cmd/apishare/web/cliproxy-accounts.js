/* Account diagnosis, pinned tests and resumable authorization. */
const poolAccountPanel = {name: "", busy: false, generation: 0,view:"status",settingsLoaded:"",trace:"",nextID:0};
const poolReasonNames = {
  disabled:"账号已停用",no_models:"尚无可用模型",cooldown:"模型冷却中",quota:"额度耗尽 / 请求受限",
  credential_quota:"账号额度冷却中",unauthorized:"凭据已失效，需要重新授权",invalid_grant:"授权已失效，需要重新登录",
  payment_required:"余额或订阅不可用",forbidden:"账号权限不足",model_not_supported:"账号不支持此模型",
  cloudflare_challenge:"厂商网络验证拦截",transient_error:"厂商服务暂不可用",upstream_unavailable:"厂商服务或网络不可用",
  credential_unavailable:"凭据暂不可用",timeout:"连接或响应超时",canceled:"请求已取消",account_mismatch:"未能固定到指定账号",
  request_failed:"请求失败",unknown:"账号暂不可用"
};
function poolReason(reason) {return poolReasonNames[reason] || "暂不可用";}
function poolCountdown(at) {
  const seconds=Math.max(0,Math.ceil((Date.parse(at)-Date.now())/1000));
  if (!Number.isFinite(seconds)) return "";
  if (!seconds) return "等待恢复";
  if (seconds>=3600) return Math.floor(seconds/3600)+" 小时 "+Math.ceil(seconds%3600/60)+" 分钟";
  if (seconds>=60) return Math.floor(seconds/60)+" 分 "+seconds%60+" 秒";
  return seconds+" 秒";
}
function poolAccountStatusHint(item) {
  if (!item.details) return "";
  const details=item.details;
  if (details.retry_at) return '<small class="pool-account-status-hint" data-retry-at="'+esc(details.retry_at)+'">'+esc(poolCountdown(details.retry_at))+'后重试</small>';
  if (details.reason) return '<small class="pool-account-status-hint">'+esc(poolReason(details.reason))+'</small>';
  return "";
}
function resetPoolAccountDetails() {
  poolAccountPanel.generation++;
  poolAccountPanel.name="";poolAccountPanel.busy=false;
  poolAccountPanel.settingsLoaded="";poolAccountPanel.trace="";
  $("poolAccountSettingsFields").disabled=true;
  $("poolAccountDetails").classList.add("hidden");
  $("poolAccountTestResult").textContent="";
}
function quotaMetricLabel(name) {
  const label=name.replace(/^anthropic-ratelimit-unified-/,"Claude · ").replace(/^x-codex-/,"Codex · ").replace(/^x-ratelimit-/,"限流 · ")
    .replace(/primary/g,"主额度").replace(/secondary/g,"次额度").replace(/used-percent/g,"使用比例 (%)")
    .replace(/window-minutes/g,"窗口 (分钟)").replace(/reset-after-seconds/g,"重置等待 (秒)").replace(/reset-at/g,"重置时间")
    .replace(/credits-balance/g,"剩余额度").replace(/plan-type/g,"套餐").replace(/remaining/g,"剩余").replace(/limit-reached/g,"已达上限").replace(/utilization/g,"使用比例")
    .replace(/retry-after/g,"重试等待 (秒)");
  return label;
}
function renderPoolAccountDetails() {
  const item=coreUI.credentials.find(item=>(item.name||item.id)===poolAccountPanel.name);
  if (!item) {resetPoolAccountDetails();return;}
  const details=item.details;
  syncPoolAccountSettings(item);
  $("poolAccountDetailsTitle").textContent=item.label||item.email||item.name;
  $("poolAccountDetailsState").textContent=details?.ready ? "可参与调度" : coreUI.status?.running ? poolReason(details?.reason) : "账号池已停止";
  $("poolAccountDetailsRefresh").textContent=details?.last_refresh ? fmtTime(details.last_refresh) : "暂无刷新记录";
  $("poolAccountDetailsNext").textContent=details?.next_refresh ? fmtTime(details.next_refresh) : "由核心自动管理";
  const cooldowns=details?.cooldowns||[];
  $("poolAccountCooldowns").innerHTML=cooldowns.length ? cooldowns.map(item=>'<div><span>'+esc(item.scope==="credential"?"整个账号":item.model_key)+'</span><strong>'+esc(poolReason(item.reason))+'</strong><small data-retry-at="'+esc(item.retry_at)+'">'+esc(poolCountdown(item.retry_at))+'后重试</small></div>').join("") : '<p class="muted">当前没有已知的冷却计时。</p>';
  const quota=[...(details?.quota||[]),...(details?.model_quotas||[]).flatMap(group=>group.metrics.map(metric=>({...metric,model:group.model})))];
  $("poolAccountQuota").innerHTML=quota.length ? quota.map(metric=>'<div><span>'+esc((metric.model?metric.model+" · ":"")+quotaMetricLabel(metric.name))+'</span><strong>'+esc(metric.value)+'</strong></div>').join("") : '<p class="muted">厂商尚未返回额度信息。完成调用后，如有额度反馈会显示在这里。</p>';
  $("poolAccountQuotaTime").textContent=details?.quota_observed_at ? "来自最近一次响应 · "+fmtTime(details.quota_observed_at) : "只展示厂商返回的数据";
  const current=$("poolAccountTestModel").value;
  $("poolAccountTestModel").replaceChildren(...(details?.models||[]).map(model=>{
    const option=new Option(model.id+(model.ready?"":" · 暂不可用"),model.id);
    option.disabled=!model.ready;
    return option;
  }));
  if ([...$("poolAccountTestModel").options].some(item=>item.value===current&&!item.disabled)) $("poolAccountTestModel").value=current;
  else $("poolAccountTestModel").value=(details?.models||[]).find(model=>model.ready)?.id||"";
  $("poolAccountModelNote").textContent=details?.models?.length ? details.models.length+" 个已注册模型 · "+details.models.filter(model=>model.ready).length+" 个当前可用" : "当前账号尚无模型，请刷新凭据或检查授权。";
  $("poolAccountRunTest").disabled=poolAccountPanel.busy||!$("poolAccountTestModel").value||!coreUI.status?.running;
  $("poolAccountDetails").classList.remove("hidden");
}
async function showPoolAccountDetails(item) {
  poolAccountPanel.name=item.name||item.id;
  poolAccountPanel.generation++;
  $("poolAccountTestResult").textContent="";
  renderPoolAccountDetails();
  setPoolAccountView("status");
  coreUI.uptimeAccount=null;renderPoolAccountUptime();
  $("poolAccountDetails").scrollIntoView({block:"nearest",behavior:motionBehavior()});
  $("poolAccountDetails").focus({preventScroll:true});
}
async function refreshPoolAccountDetails(button) {
  setBusy(button,true);
  try {await loadCLIProxy();renderPoolAccountDetails();} finally {setBusy(button,false);}
}
async function testPoolAccount(button) {
  if (poolAccountPanel.busy) return;
  const name=poolAccountPanel.name,model=$("poolAccountTestModel").value,id=coreUI.poolID,epoch=coreUI.epoch;
  if (!name||!model) return;
  poolAccountPanel.busy=true;
  setBusy(button,true,"测试中…");
  $("poolAccountTestResult").textContent="正在使用指定账号和它的代理发送测试请求…";
  try {
    const result=await api(poolAPI("credentials/test",id),{method:"POST",body:JSON.stringify({name,model})});
    if (epoch!==coreUI.epoch||name!==poolAccountPanel.name) return;
    $("poolAccountTestResult").textContent=result.ok ? "连接正常 · "+result.latency_ms+" ms · "+result.model : "测试失败："+poolReason(result.reason)+(result.status?" · HTTP "+result.status:"");
    $("poolAccountTestResult").classList.toggle("form-error",!result.ok);
    await loadCLIProxy();
  } catch(error) {
    if (epoch===coreUI.epoch) {$("poolAccountTestResult").textContent=error.message;$("poolAccountTestResult").classList.add("form-error");}
  } finally {poolAccountPanel.busy=false;setBusy(button,false);if(epoch===coreUI.epoch) renderPoolAccountDetails();}
}
async function importPoolCredentialItem(item,poolID) {
  try {return await api(poolAPI("credentials/import",poolID),{method:"POST",body:JSON.stringify(item)});}
  catch(error) {
    if (error.code!=="credential_exists") throw error;
    const confirmed=await askAction({title:"更新已有账号的凭据？",description:"当前池已有“"+error.existingName+"”。确认后更新登录凭据，保留原代理、停用状态与账号配置。",confirm:"更新凭据"});
    if (!confirmed) throw new Error("已取消更新已有账号");
    return api(poolAPI("credentials/import",poolID),{method:"POST",body:JSON.stringify({...item,replace:true})});
  }
}
async function restoreCoreLogin(id,epoch) {
  try {
    const result=await api(poolAPI("oauth/sessions",id),{cache:"no-store"});
    if (id!==coreUI.poolID||epoch!==coreUI.epoch||coreUI.login||!editorIsOpen()) return;
    const pending=result.sessions?.find(session=>["wait","needs_confirmation"].includes(session.status));
    if (pending) {
      const title=poolProviders.find(item=>item.id===pending.provider)?.name||pending.provider;
      displayPoolLogin({...pending,restored:true},title+" · 已恢复授权",coreUI.generation,id);
    } else {
      const completed=result.sessions?.filter(session=>session.status==="ok")||[];
      if (completed.length) {
        coreMessage("后台已完成授权，账号已加入当前池。");
        for (const session of completed) api(poolAPI("oauth/session?state="+encodeURIComponent(session.state),id),{method:"DELETE"}).catch(()=>{});
      }
      const failed=result.sessions?.find(session=>session.status==="error");
      if (failed) {
        coreMessage(failed.error||"上次授权未完成，请重新发起。",true);
        api(poolAPI("oauth/session?state="+encodeURIComponent(failed.state),id),{method:"DELETE"}).catch(()=>{});
      }
    }
  } catch(error) {if (error.status!==404 && id===coreUI.poolID) coreMessage("暂时无法恢复授权会话，请刷新重试。",true);}
}
function renderCoreLoginState(result) {
  $("poolLoginConflict").classList.toggle("hidden",result.status!=="needs_confirmation");
  if (result.status==="needs_confirmation") {
    $("poolLoginConflictNames").textContent=(result.duplicates||[]).join("、");
    $("coreLoginStatus").textContent="授权已完成，确认后更新当前池内的原账号，保留其代理和停用状态。";
  } else if (result.error) {$("coreLoginStatus").textContent=result.error;}
}
async function confirmCoreLogin(button) {
  const login=coreUI.login;
  if (!login) return;
  const epoch=coreUI.epoch;
  setBusy(button,true,"更新中…");
  try {
    const result=await api(poolAPI("oauth/confirm",login.poolID),{method:"POST",body:JSON.stringify({state:login.state})});
    if (epoch!==coreUI.epoch || coreUI.login?.state!==login.state) return;
    renderCoreLoginState(result);
    if (result.status==="ok") {cancelCoreLogin();coreMessage();toast("账号凭据已更新，原配置已保留");await loadCLIProxy();}
  } catch(error) {coreMessage(error.message,true);}
  finally {setBusy(button,false);}
}
setInterval(()=>{
  if (document.hidden) return;
  document.querySelectorAll("[data-retry-at]").forEach(node=>{node.textContent=poolCountdown(node.dataset.retryAt)+"后重试";});
},1000);

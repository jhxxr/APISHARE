/* Shared account-attempt presentation for the pool workspace and log drawer. */
function poolAttemptHTML(item,traceAction="") {
  const success=item.status>=200&&item.status<300;
  const identity=coreUI.poolID===item.pool_id||!item.pool_id?coreUI.credentials.find(account=>account.id===item.auth_id):null;
  const account=identity?.label||identity?.email||item.auth_id;
  return '<article class="pool-attempt-row"><div class="pool-attempt-heading"><span class="badge '+(success?"on":"warn")+'">'+(success?"成功":"HTTP "+item.status)+'</span><strong>'+esc(item.model)+'</strong><small>'+esc(fmtTime(item.created_at))+'</small></div><p>'+esc((item.pool_name?item.pool_name+" · ":"")+account)+' <span>· 第 '+item.attempt+' 次尝试</span></p><div class="pool-attempt-stats"><span>'+item.latency_ms+' ms</span>'+(item.first_token_ms>0?'<span>首 Token '+item.first_token_ms+' ms</span>':'')+'<span>输入 '+number(item.input_tokens)+' · 输出 '+number(item.output_tokens)+' Tokens</span>'+(item.reason?'<span class="form-error">'+esc(poolReason(item.reason))+'</span>':"")+'</div>'+(traceAction?'<button type="button" class="text-btn" data-pool-trace="'+esc(item.trace_id)+'">'+esc(item.trace_id.slice(-10))+' · 查看这次请求 →</button>':'')+'</article>';
}
function setPoolAccountView(view) {
  poolAccountPanel.view=view;
  document.querySelectorAll("[data-account-view]").forEach(button=>button.setAttribute("aria-pressed",String(button.dataset.accountView===view)));
  document.querySelectorAll("[data-account-panel]").forEach(panel=>panel.classList.toggle("hidden",panel.dataset.accountPanel!==view));
  $("poolAccountSettingsFields").disabled=view!=="settings";
  if(view==="logs")loadPoolAccountAttempts(false);
}
function syncPoolAccountSettings(item) {
  const detail=item.details||{};
  if(poolAccountPanel.settingsLoaded!==item.name){
    $("poolAccountPriority").value=detail.priority||0;
    $("poolAccountLabel").value=item.label||"";
    $("poolAccountNote").value=item.note||"";
    $("poolAccountWeight").value=detail.weight>0?detail.weight:1;
    $("poolAccountVertexLocation").value=detail.location||"us-central1";
    $("poolAccountSettingsResult").textContent="";
    poolAccountPanel.settingsLoaded=item.name;
  }
  $("poolAccountVertexField").classList.toggle("hidden",item.provider!=="vertex");
  $("poolAccountSIWC").classList.toggle("hidden",item.provider!=="codex");
  $("poolAccountSIWCState").textContent=({off:"尚未绑定",valid:"已绑定 · 当前有效",invalid:"绑定已失效，请重新绑定"})[detail.siwc_status]||"尚未绑定";
  $("poolAccountSIWCExpiry").textContent=detail.siwc_expires_at?"有效期至 "+fmtTime(detail.siwc_expires_at):"状态来自本地凭据检查";
  renderPoolAccountPriorityHint();
}
async function savePoolAccountSettings(button) {
  const name=poolAccountPanel.name,item=coreUI.credentials.find(item=>item.name===name);
  const id=coreUI.poolID,epoch=coreUI.epoch,generation=poolAccountPanel.generation;
  if(!item)return;
  const priority=Number($("poolAccountPriority").value),weight=Number($("poolAccountWeight").value);
  if(!Number.isInteger(priority)||priority<0||priority>1000||!Number.isInteger(weight)||weight<1||weight>1000000){toast("请输入有效的优先级与权重","error");return;}
  const payload={name,priority,weight,label:$("poolAccountLabel").value,note:$("poolAccountNote").value};
  if(item.provider==="vertex")payload.location=$("poolAccountVertexLocation").value.trim();
  setBusy(button,true,"应用中…");
  try{
    await api(poolAPI("credentials/settings",id),{method:"PATCH",body:JSON.stringify(payload)});
    if(epoch!==coreUI.epoch||generation!==poolAccountPanel.generation)return;
    $("poolAccountSettingsResult").textContent="账号设置已应用";
    await loadCLIProxy();toast("账号设置已更新");
  }catch(error){if(epoch===coreUI.epoch&&generation===poolAccountPanel.generation){$("poolAccountSettingsResult").textContent=error.message;reportError(error);}}
  finally{setBusy(button,false);}
}
async function bindSelectedSIWC(button) {
  const item=coreUI.credentials.find(item=>item.name===poolAccountPanel.name);
  if(!item||item.provider!=="codex")return;
  $("poolSIWCTarget").replaceChildren(new Option(item.label||item.name,item.name));
  setPoolWorkspace("authorization");
  await startCoreSIWC(button);
}
async function loadPoolAccountAttempts(append=false) {
  const id=coreUI.poolID,name=poolAccountPanel.name,generation=poolAccountPanel.generation,request=poolAccountPanel.logRequest=(poolAccountPanel.logRequest||0)+1;
  if(!id||!name)return;
  const account=coreUI.credentials.find(item=>item.name===name);
  const trace=poolAccountPanel.trace||"";
  const params=new URLSearchParams();
  if(trace)params.set("trace_id",trace);else params.set("account",account?.id||name);
  if(append&&poolAccountPanel.nextID)params.set("before",poolAccountPanel.nextID);
  $("poolAccountLogHeading").textContent=trace?"同一请求的账号尝试链":"这个账号的调用记录";
  $("poolAccountLogBack").classList.toggle("hidden",!trace);
  if(!append)$("poolAccountAttemptList").innerHTML='<p class="muted">正在读取实际调用记录…</p>';
  try{
    const result=await api(trace?"api/cliproxy/attempts?"+params:poolAPI("attempts",id)+"?"+params);
    if(generation!==poolAccountPanel.generation||id!==coreUI.poolID||request!==poolAccountPanel.logRequest)return;
    poolAccountPanel.nextID=result.next_id;
    const html=result.items.map(item=>poolAttemptHTML(item,!trace)).join("")||'<p class="muted">暂无调用记录。执行模型测试或收到真实请求后，这里会显示账号、耗时和用量。</p>';
    if(append)$("poolAccountAttemptList").insertAdjacentHTML("beforeend",html);else $("poolAccountAttemptList").innerHTML=html;
    $("poolAccountLogMore").classList.toggle("hidden",!result.next_id||!!trace);
    $("poolAccountAttemptList").querySelectorAll("[data-pool-trace]").forEach(button=>button.onclick=()=>{poolAccountPanel.trace=button.dataset.poolTrace;loadPoolAccountAttempts(false);});
  }catch(error){if(generation!==poolAccountPanel.generation||request!==poolAccountPanel.logRequest)return;$("poolAccountAttemptList").innerHTML='<p class="form-error">'+esc(error.message)+'</p>';}
}
async function renderGatewayAccountAttempts(log) {
  if(!log.trace_id)return;
  const section=document.createElement("section");
  section.className="log-detail-section";section.innerHTML='<h3>实际执行的账号</h3><div class="pool-attempt-list"><p class="muted">正在读取请求尝试链…</p></div>';
  $("logDetailBody").append(section);
  try{
    const result=await api("api/cliproxy/attempts?trace_id="+encodeURIComponent(log.trace_id));
    if(logDetailRecord?.id!==log.id||!section.isConnected)return;
    section.lastElementChild.innerHTML=result.items.map(item=>poolAttemptHTML(item)).join("")||'<p class="muted">暂未查询到账号尝试，记录可能仍在写入或已超过账号日志保留期。</p>';
  }catch(error){if(section.isConnected)section.lastElementChild.innerHTML='<p class="form-error">'+esc(error.message)+'</p>';}
}

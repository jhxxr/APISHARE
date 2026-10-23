/* A compact account face, with a continuous path to the full inspector. */
function poolCardProvider(provider){
  return ({codex:"Codex",claude:"Claude",antigravity:"Antigravity",kimi:"Kimi 国内站","kimi-ai":"Kimi 国际站",meta:"Muse · Meta",xai:"xAI / Grok",devin:"Devin",vertex:"Vertex AI"})[provider]||provider||"待识别";
}
function poolCardState(item){
  if(item.pending)return {label:"待保存",tone:"quiet"};
  if(!coreUI.status?.running)return {label:"池已停止",tone:"quiet"};
  if(item.disabled)return {label:"已停用",tone:"quiet"};
  if(item.details?.ready)return {label:item.details.cooldowns?.length?"部分可用":"可用",tone:"good"};
  if(item.details?.retry_at||item.details?.cooldowns?.length)return {label:"冷却中",tone:"warm"};
  return {label:"暂不可用",tone:"bad"};
}
function poolCardQuotaWindows(item){
  const windows=item.live_quota?.windows||[];
  const primary=windows.filter(window=>!window.scope);
  if(primary.length)return primary.slice(0,2);
  if(windows.length)return windows.slice(0,2);
  const labels=item.provider==="devin"?["今日","本周"]:item.provider==="xai"?["本周","每月"]:["5 小时","本周"];
  return labels.map(label=>({label,remaining_percent:null}));
}
function poolCardQuotaHTML(item,index){
  if(item.pending)return '<div class="account-card-waiting"><span>准备就绪</span><strong>保存后加入当前池</strong></div>';
  if(!supportsPoolLiveQuota(item)){
    return '<button type="button" class="account-card-duo is-service" data-account-details="'+index+'"><span><small>接入方式</small><strong>'+esc(item.provider==="vertex"?"服务账号":"独立凭据")+'</strong></span><span><small>'+(item.provider==="vertex"?"地区":"模型")+'</small><strong>'+esc(item.details?.location||(item.details?.models?.length?item.details.models.length+" 个":"按厂商配置"))+'</strong></span></button>';
  }
  const windows=poolCardQuotaWindows(item);
  const stamp=item.live_quota?.stale?"上次查询":item.live_quota?.observed_at?"更新 "+fmtTime(item.live_quota.observed_at).slice(11,16):"点击查询";
  return '<button type="button" class="account-card-duo" data-card-quota="'+index+'" aria-label="查看 '+esc(item.label||item.email||item.name)+' 的剩余额度">'+windows.map(window=>{
    const known=Number.isFinite(window.remaining_percent)||Number.isFinite(window.remaining);
    const tone=!known?"unknown":window.remaining_percent<=10?"low":window.remaining_percent<=25?"warm":"";
    const label=window.period_seconds===18000?"5 小时":window.period_seconds===604800?"本周":window.period_seconds===86400?"今日":(window.label||"额度").replace(/额度$/,"").trim();
    return '<span class="account-card-quota '+tone+'"><small>'+esc(label)+'</small><strong>'+esc(known?quotaRemainingLabel(window):"—")+'</strong><i aria-hidden="true"><b style="width:'+(Number.isFinite(window.remaining_percent)?Math.max(0,Math.min(100,window.remaining_percent)):0)+'%"></b></i><em>'+esc(known?window.reset_at?poolQuotaResetShort(window.reset_at):"剩余额度":stamp)+'</em></span>';
  }).join("")+'</button>';
}
function poolCardUptimeHTML(item,index){
  const data=item.uptime,available=!item.pending&&!coreUI.uptimeError&&data;
  const rate=available?poolUptimeRate(data.uptime_percent):"—";
  const label=available&&data.calls?data.successes+" / "+data.calls+" 次成功":coreUI.uptimeError?"统计暂不可用":"暂无调用";
  return '<button type="button" class="account-card-uptime" data-account-uptime="'+index+'" data-uptime-key="'+esc(item.id||item.name)+'" aria-label="'+esc((item.label||item.email||item.name)+"，近 24 小时 Uptime "+rate+"，"+label)+'"'+(!available?' disabled':"")+'><span class="account-card-uptime-label"><span>Uptime <small>24h</small></span><strong>'+esc(data?.calls?rate:label)+'</strong></span><span class="pool-uptime-bars" aria-hidden="true">'+poolUptimeBars(available?data.history:null)+'</span></button>';
}
function poolCardMenuHTML(item,index){
  if(item.pending)return '<button type="button" class="account-card-more" data-pending-remove="'+item.pendingIndex+'" aria-label="移除待保存账号 '+esc(item.name)+'">'+icon("close")+'</button>';
  return '<details class="account-card-menu"><summary aria-label="管理 '+esc(item.label||item.email||item.name)+'"><svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="19" cy="12" r="1.6"/></svg></summary><div><button type="button" data-card-settings="'+index+'">名称与备注</button><button type="button" data-core-action="toggle" data-index="'+index+'">'+(item.disabled?"启用账号":"停用账号")+'</button>'+(!item.runtime_only?'<button type="button" data-core-action="refresh" data-index="'+index+'">刷新授权凭据</button><button type="button" class="danger" data-core-action="delete" data-index="'+index+'">删除账号</button>':"")+'</div></details>';
}
function renderPoolAccountCards(rows,all){
  const grid=$("coreAccountsGrid"),stopped=coreUI.poolID&&coreUI.status&&!coreUI.status.running;
  $("poolAccountVisibleCount").textContent=rows.length?"本页 "+rows.length+" 个账号":"";
  if(!rows.length){
    grid.innerHTML='<div class="pool-empty account-grid-empty"><span class="pool-empty-icon">'+icon("layers")+'</span><strong>'+esc(all.length?"没有找到匹配的账号":stopped?"账号池已停止":"这个池子，等待第一个账号")+'</strong><p>'+esc(all.length?"试试清除筛选条件。":"选择提供商授权，或导入 JSON 凭据。")+'</p></div>';
    syncPoolSelection();return;
  }
  grid.innerHTML=rows.map((item,index)=>{
    const title=item.label||item.email||item.name||item.id,state=poolCardState(item);
    const selected=poolManagement.selected.has(item.name),proxy=Number(item.proxy_id??-1);
    return '<article role="listitem" class="account-card'+(selected?" is-selected":"")+'" data-account-card="'+esc(item.name||item.id)+'" aria-label="'+esc(title)+'"><header class="account-card-header"><span class="account-card-brand" aria-hidden="true">'+modelIcon(item.provider==="codex"?"gpt":item.provider||"")+'</span><button type="button" class="account-card-title" '+(item.pending?"disabled":'data-account-details="'+index+'"')+' title="'+esc(title)+'"><strong>'+esc(title)+'</strong><small>'+esc(poolCardProvider(item.provider))+'</small></button><div class="account-card-controls">'+(!item.pending&&!item.runtime_only?'<label class="account-card-select" title="选择此账号"><input type="checkbox" data-pool-select="'+esc(item.name)+'" aria-label="选择 '+esc(item.name)+'"><span class="sr-only">选择账号</span></label>':"")+poolCardMenuHTML(item,index)+'</div></header><div class="account-card-context"><span class="account-card-state '+state.tone+'"><i></i>'+esc(state.label)+'</span><span class="account-card-note" title="'+esc(item.note||item.name)+'">'+esc(item.note||item.name)+'</span></div>'+poolCardQuotaHTML(item,index)+poolCardUptimeHTML(item,index)+'<footer class="account-card-footer"><label class="account-card-egress" title="此账号的出站代理">'+icon("link")+'<select data-account-proxy="'+index+'" aria-label="'+esc(item.name)+'的出站代理"'+(stopped&&!item.pending?' disabled':"")+'>'+proxyOptions(proxy)+'</select></label>'+(item.pending?'<button type="button" class="account-card-open danger" data-pending-remove="'+item.pendingIndex+'">移除</button>':'<button type="button" class="account-card-open" data-account-details="'+index+'">详情 <span aria-hidden="true">↗</span></button>')+'</footer></article>';
  }).join("");
  grid.querySelectorAll("[data-account-proxy]").forEach(select=>{
    const item=rows[Number(select.dataset.accountProxy)];
    select.value=String(item.proxy_id??-1);
    select.addEventListener("change",async()=>{
      if(item.pending){coreUI.pending[item.pendingIndex].proxy_id=Number(select.value);renderCoreAccounts();updateEditorDirty();return;}
      const previous=item.proxy_id,id=coreUI.poolID,epoch=coreUI.epoch;
      select.disabled=true;
      try{
        await api(poolAPI("credentials/proxy",id),{method:"PATCH",body:JSON.stringify({name:item.name||item.id,proxy_id:Number(select.value)})});
        if(epoch!==coreUI.epoch)return;
        item.proxy_id=Number(select.value);renderCoreAccounts();toast("账号代理已更新");
      }catch(error){select.value=String(previous);reportError(error);}
      finally{select.disabled=false;}
    });
  });
  grid.querySelectorAll("[data-core-action]").forEach(button=>button.onclick=()=>{
    button.closest("details").open=false;coreAccountAction(button.dataset.coreAction,rows[Number(button.dataset.index)],button);
  });
  grid.querySelectorAll("[data-account-details]").forEach(button=>button.onclick=()=>showPoolAccountDetails(rows[Number(button.dataset.accountDetails)]));
  grid.querySelectorAll("[data-card-quota]").forEach(button=>button.onclick=async()=>{
    await showPoolAccountDetails(rows[Number(button.dataset.cardQuota)]);loadPoolAccountQuota(true);
  });
  grid.querySelectorAll("[data-card-settings]").forEach(button=>button.onclick=async()=>{
    button.closest("details").open=false;await showPoolAccountDetails(rows[Number(button.dataset.cardSettings)]);setPoolAccountView("settings");
  });
  grid.querySelectorAll("[data-account-uptime]").forEach(button=>button.onclick=()=>showPoolAccountUptime(rows[Number(button.dataset.accountUptime)]));
  grid.querySelectorAll("[data-pending-remove]").forEach(button=>button.onclick=()=>{coreUI.pending.splice(Number(button.dataset.pendingRemove),1);renderCoreAccounts();updateEditorDirty();});
  grid.querySelectorAll("[data-pool-select]").forEach(box=>box.onchange=()=>selectPoolAccount(box.dataset.poolSelect,box.checked));
  grid.querySelectorAll(".account-card-menu").forEach(menu=>menu.addEventListener("toggle",()=>{
    if(menu.open)grid.querySelectorAll(".account-card-menu[open]").forEach(other=>{if(other!==menu)other.open=false;});
  }));
  syncPoolSelection();
}
document.addEventListener("click",event=>{
  if(!event.target.closest(".account-card-menu"))document.querySelectorAll(".account-card-menu[open]").forEach(menu=>menu.open=false);
});
document.addEventListener("keydown",event=>{
  const menus=document.querySelectorAll(".account-card-menu[open]");
  if(event.key==="Escape"&&menus.length){
    event.preventDefault();event.stopPropagation();
    menus.forEach(menu=>{menu.open=false;menu.querySelector("summary").focus();});
  }
},true);

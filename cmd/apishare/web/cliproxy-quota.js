/* Read-only provider quota, queried by the gateway using this account's egress. */
const poolQuotaErrors={
  pool_stopped:"账号池已停止，显示上次查询结果",unsupported_provider:"此类型暂未提供主动额度查询",
  account_not_found:"账号已不在当前池",credential_unavailable:"无法读取此账号的授权信息",
  core_unavailable:"账号池核心暂不可连接",network_error:"厂商额度接口连接失败，请检查账号代理后重试",
  unauthorized:"授权已失效，需要重新授权",forbidden:"厂商拒绝查询额度，请检查账号权限",
  payment_required:"此订阅暂无法获取额度，请检查套餐或余额",model_not_supported:"厂商未提供此额度查询接口",
  quota:"厂商限制查询频率，请稍后重试",quota_unavailable:"厂商未返回可识别的额度窗口",
  invalid_quota_response:"厂商额度响应无法识别",credential_changed:"账号凭据已变更，请重新查询",
  missing_project:"账号缺少 Antigravity 项目 ID",missing_dca_token:"此 Meta 凭据缺少额度查询需要的 DCA 授权",
  timeout:"额度查询超时，请稍后重试",upstream_unavailable:"厂商额度服务暂不可用"
};
function supportsPoolLiveQuota(item){return ["codex","claude","kimi","kimi-ai","antigravity","xai","meta","devin"].includes(item?.provider);}
function quotaPercent(value){return Number.isFinite(value)?new Intl.NumberFormat("zh-CN",{maximumFractionDigits:1}).format(value)+"%":"未获取";}
function quotaRemainingLabel(window){return Number.isFinite(window.remaining_percent)?quotaPercent(window.remaining_percent):Number.isFinite(window.remaining)?quotaAmount(window.remaining,window.unit):"未获取";}
function quotaShortPeriod(window){
  if(window.period_seconds===18000)return "5h";
  if(window.period_seconds===604800)return "周";
  if(window.period_seconds===86400)return "日";
  if(window.period_seconds>=28*86400&&window.period_seconds<=31*86400)return "月";
  return window.label;
}
function poolQuotaSummaryHTML(item,index){
  if(item.pending)return "";
  const quota=item.live_quota,windows=(quota?.windows||[]).filter(window=>!window.scope).slice(0,2);
  const stale=quota?.stale||windows.some(window=>window.reset_at&&Date.parse(window.reset_at)<=Date.now());
  return '<button type="button" class="pool-quota-mini" data-account-details="'+index+'" title="查看账号额度">'+(windows.length?windows.map(window=>'<span>'+esc(quotaShortPeriod(window))+' 余 '+esc(quotaRemainingLabel(window))+'</span>').join("")+(stale?'<small>待刷新</small>':''):supportsPoolLiveQuota(item)?'<span>查询剩余额度 →</span>':'<span>查看厂商额度反馈 →</span>')+'</button>';
}
function poolQuotaResetLabel(at){
  if(!at)return "厂商未提供重置时间";
  if(Date.parse(at)<=Date.now())return "已到重置时间，请刷新确认";
  const hours=Math.ceil((Date.parse(at)-Date.now())/3600000);
  if(hours>=24)return Math.floor(hours/24)+" 天 "+hours%24+" 小时后重置";
  return poolCountdown(at)+"后重置";
}
function quotaAmount(value,unit){
  if(!Number.isFinite(value))return "未获取";
  if(unit==="cents")return "$"+new Intl.NumberFormat("zh-CN",{maximumFractionDigits:2}).format(value/100);
  return new Intl.NumberFormat("zh-CN",{maximumFractionDigits:2}).format(value)+(unit==="USD"?" USD":unit==="credits"?" 额度单位":"");
}
function poolQuotaResetShort(at){
  if(!at)return "重置时间未知";
  const seconds=(Date.parse(at)-Date.now())/1000;
  if(seconds<=0)return "待刷新确认";
  if(seconds>=86400)return Math.ceil(seconds/86400)+" 天后重置";
  if(seconds>=3600)return Math.ceil(seconds/3600)+" 小时后重置";
  return Math.max(1,Math.ceil(seconds/60))+" 分钟后重置";
}
function quotaWindowRowHTML(window){
  const remaining=window.remaining_percent,known=Number.isFinite(remaining);
  const tone=!known?(Number.isFinite(window.remaining)?"":" unknown"):remaining<=10?" low":remaining<=25?" warn":"";
  const label=(window.label||"额度窗口").replace(/额度$/," ").trim();
  return '<article class="pool-quota-window'+tone+'"><div class="pool-quota-window-heading"><strong>'+esc(label)+'</strong>'+(window.scope?'<small>'+esc(window.scope)+'</small>':'')+'</div><div class="pool-quota-remaining"><strong>'+esc(quotaRemainingLabel(window))+'</strong><span class="sr-only">剩余</span></div>'+(known?'<progress value="'+remaining+'" max="100" aria-label="'+esc(label)+'剩余 '+esc(quotaPercent(remaining))+'"></progress>':'<div class="pool-quota-unavailable-bar"></div>')+'<small class="pool-quota-reset"'+(window.reset_at?' data-quota-compact-reset="'+esc(window.reset_at)+'" title="'+esc(fmtTime(window.reset_at))+'"':'')+'>'+esc(poolQuotaResetShort(window.reset_at))+'</small></article>';
}
function renderPoolLiveQuota(item){
  const quota=item.live_quota,supported=supportsPoolLiveQuota(item);
  const busy=poolAccountPanel.quotaBusy&&poolAccountPanel.quotaName===item.name;
  $("poolQuotaRefresh").disabled=busy||!coreUI.status?.running||!supported;
  $("poolQuotaRefresh").textContent=busy?"查询中…":quota?.observed_at?"刷新额度":"查询额度";
  $("poolQuotaRefresh").setAttribute("aria-busy",String(busy));
  $("poolLiveQuotaMessage").classList.toggle("form-error",!!quota?.error);
  $("poolLiveQuotaMessage").textContent=busy?"正在使用此账号的代理查询…":quota?.error?(poolQuotaErrors[quota.error]||"额度暂不可用，请稍后重试")+(quota.http_status?" · HTTP "+quota.http_status:""):!supported?"此类型暂无独立额度查询接口。":item.disabled&&!quota?"账号已停用，可手动查询；查询不会启用账号。":!quota?"尚未获取额度。":"";
  $("poolLiveQuotaTime").textContent=quota?.observed_at?"更新 "+fmtTime(quota.observed_at).slice(11,16)+(quota.stale?" · 上次数据":""):"";
  $("poolLiveQuotaTime").title=quota?.observed_at?fmtTime(quota.observed_at):"";
  $("poolLiveQuotaPlan").textContent=[quota?.plan?"套餐 "+quota.plan:"",quota?.unlimited?"额外额度无限":Number.isFinite(quota?.balance)?"余额 "+number(quota.balance):""].filter(Boolean).join(" · ");
  const windows=quota?.windows||[];
  const placeholder=supported&&(item.provider==="codex"||item.provider==="claude")?[{id:"unfetched-5h",label:"5 小时额度"},{id:"unfetched-week",label:"每周额度"}]:[];
  const rows=windows.length?windows:placeholder;
  $("poolLiveQuotaWindows").innerHTML=rows.slice(0,2).map(quotaWindowRowHTML).join("");
  $("poolQuotaMoreSummary").textContent=rows.length>2?"全部 "+rows.length+" 项额度与重置详情":"用量与重置详情";
  $("poolLiveQuotaDetails").innerHTML=(quota?.observed_at?'<p class="pool-quota-detail-observed">查询时间：'+esc(fmtTime(quota.observed_at))+'</p>':'')+rows.map(window=>'<div><strong>'+esc((window.scope?window.scope+" · ":"")+(window.label||"额度窗口"))+'</strong><p>剩余 '+esc(quotaRemainingLabel(window))+' · '+esc(Number.isFinite(window.used_percent)?"已使用 "+quotaPercent(window.used_percent):"使用比例未获取")+(Number.isFinite(window.remaining)?' · 剩余 '+esc(quotaAmount(window.remaining,window.unit))+(Number.isFinite(window.limit)?' / '+esc(quotaAmount(window.limit,window.unit)):""):"")+'</p><small'+(window.reset_at?' data-quota-reset="'+esc(window.reset_at)+'"':"")+'>'+esc(poolQuotaResetLabel(window.reset_at))+'</small>'+(window.reset_at?'<time>'+esc(fmtTime(window.reset_at))+'</time>':"")+'</div>').join("");
}

async function loadPoolAccountQuota(force=false){
  const name=poolAccountPanel.name,id=coreUI.poolID,epoch=coreUI.epoch,generation=poolAccountPanel.generation;
  const item=coreUI.credentials.find(item=>item.name===name);
  if(!item||!supportsPoolLiveQuota(item)||!coreUI.status?.running)return;
  if(!force&&(item.disabled||item.live_quota&&!item.live_quota.stale))return;
  if(poolAccountPanel.quotaBusy&&poolAccountPanel.quotaName===name)return;
  poolAccountPanel.quotaBusy=true;poolAccountPanel.quotaName=name;renderPoolLiveQuota(item);
  try{
    const result=await api(poolAPI("credentials/quota",id)+"?"+new URLSearchParams({name,...(force?{refresh:"1"}:{})}),{cache:"no-store"});
    if(epoch!==coreUI.epoch||generation!==poolAccountPanel.generation)return;
    item.live_quota=result;
    // A normal list refresh may have replaced the detached UI account object.
    const current=coreUI.credentials.find(account=>account.name===name);if(current)current.live_quota=result;
  }catch(error){
    if(epoch===coreUI.epoch&&generation===poolAccountPanel.generation){
      const current=coreUI.credentials.find(account=>account.name===name);
      if(current)current.live_quota={...(current.live_quota||{windows:[]}),error:"core_unavailable",stale:true};
    }
  }
  finally{
    if(epoch===coreUI.epoch&&generation===poolAccountPanel.generation){
      poolAccountPanel.quotaBusy=false;renderCoreAccounts();
    }
  }
}
setInterval(()=>{
  if(document.hidden)return;
  document.querySelectorAll("[data-quota-reset]").forEach(node=>node.textContent=poolQuotaResetLabel(node.dataset.quotaReset));
  document.querySelectorAll("[data-quota-compact-reset]").forEach(node=>node.textContent=poolQuotaResetShort(node.dataset.quotaCompactReset));
},1000);

/* Site-wide telemetry and separately authenticated, read-only key balances. */
const $ = id => document.getElementById(id);
const esc = value => String(value ?? "").replace(/[&<>"']/g, c => ({ "&":"&amp;", "<":"&lt;", ">":"&gt;", '"':"&quot;", "'":"&#39;" }[c]));
const modelLabel = (model, family = "") => window.APIShareModelIcons.labelHTML(model, family);
const fmt = value => Number(value || 0).toLocaleString("zh-CN");
const compact = value => { value=Number(value||0);return value>=1e8?(value/1e8).toFixed(1)+"亿":value>=1e6?(value/1e6).toFixed(1)+"M":value>=1e4?(value/1e4).toFixed(1)+"万":fmt(value); };
const money = value => {
  const amount=Number(value||0), absolute=Math.abs(amount);
  if(!Number.isFinite(amount))return "—";
  if(absolute>0&&absolute<1e-10)return "$"+amount.toExponential(2);
  const digits=absolute>0&&absolute<0.0001?Math.min(10,Math.max(4,Math.ceil(-Math.log10(absolute))+2)):absolute>0&&absolute<1?4:2;
  return "$"+amount.toLocaleString("en-US",{minimumFractionDigits:2,maximumFractionDigits:digits});
};
const rate = value => value===null||value===undefined?"—":Number(value).toFixed(2)+"%";
const speed = value => value===null||value===undefined?"—":Number(value).toFixed(1)+" tok/s";
const svg = name => '<svg class="icon" aria-hidden="true"><use href="#'+name+'"/></svg>';
const tokens = value => Number(value.prompt_tokens||0)+Number(value.completion_tokens||0);
// The opening counter shows the complete total, with stable digit columns.
function heroTokenAmount(value) { return Math.max(0,Math.round(Number(value)||0)).toLocaleString("en-US"); }
function heroTokenDescription(value) {
 const total=Math.max(0,Number(value)||0);
 let divisor=total>=1e12?1e12:total>=1e8?1e8:total>=1e4?1e4:1;
 let amount=Number((total/divisor).toFixed(divisor===1?0:2));
 if(amount>=1e4&&divisor<1e12){amount/=1e4;divisor*=1e4;}
 const unit=divisor===1e12?"万亿":divisor===1e8?"亿":divisor===1e4?"万":"";
 return "约 "+amount.toLocaleString("zh-CN",{maximumFractionDigits:2})+unit+" Tokens";
}
let heroTarget=0, heroRolling=false, heroRolls=[];
function measureHeroType() {
 const number=$("heroTokens"),heading=$("heroTitle");
 number.style.removeProperty("--hero-token-fit");
 const size=parseFloat(getComputedStyle(number).fontSize),text=number.dataset.value||number.textContent;
 const units=[...text].reduce((sum,char)=>sum+(char===","?.25:char==="—"?.8:.62),0);
 const available=Math.max(1,heading.clientWidth-8);
 number.style.setProperty("--hero-token-fit",Math.min(size,available/Math.max(1,units)).toFixed(2)+"px");
}
function showHeroNumber(value) {
 const number=$("heroTokens"),text=heroTokenAmount(value);number.dataset.value=text;
 number.classList.remove("is-rolling");
 number.innerHTML=[...text].map(char=>'<span class="'+(char===","?'token-separator':'token-digit')+'">'+char+'</span>').join("");
 measureHeroType();
}
function finishHeroRoll() {
 if(!heroRolling)return;heroRolling=false;
 heroRolls.forEach(animation=>animation.cancel());heroRolls=[];showHeroNumber(heroTarget);
}
function rollHeroNumber(value) {
 finishHeroRoll();heroTarget=value;heroRolling=true;
 const number=$("heroTokens"),text=heroTokenAmount(value);number.dataset.value=text;
 number.classList.add("is-rolling");
 number.innerHTML=[...text].map((char,index)=>{
  if(char===",")return '<span class="token-separator">,</span>';
  const steps=10+Number(char)+(index%3===0?10:0);
  const digits=Array.from({length:steps+1},(_,i)=>'<span>'+i%10+'</span>').join("");
  return '<span class="token-digit"><span class="digit-strip" data-steps="'+steps+'">'+digits+'</span></span>';
 }).join("");measureHeroType();
 const strips=[...number.querySelectorAll(".digit-strip")];
 heroRolls=strips.map((strip,index)=>{
  const end='translate3d(0,-'+(Number(strip.dataset.steps)*1.12)+'em,0)';
  strip.style.transform=end;
  return strip.animate([{transform:'translate3d(0,0,0)'},{transform:end}],{duration:1850+index*85,delay:250+index*45,easing:'cubic-bezier(.16,1,.3,1)',fill:'backwards'});
 });
 Promise.all(heroRolls.map(animation=>animation.finished)).then(()=>{if(heroRolling)finishHeroRoll();}).catch(()=>{});
}
window.addEventListener("public-motion-change",()=>{if(!window.pageMotion?.isRunning())finishHeroRoll();});
const statusText = {operational:"运行正常",incident:"最近调用失败",idle:"暂无近期调用"};
let overview=null, metric="calls", historyInterval="hour", historyModel=null, autoUpdate=true, refreshing=false, toastTimer=0, heroAnimated=false;
let announcements=null, announcementVersion="";
function announcementDate(value) {
 return new Date(value).toLocaleString("zh-CN",{timeZone:"Asia/Shanghai",year:"numeric",month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",hourCycle:"h23"});
}
function renderPublicAnnouncements() {
 const items=announcements||[],first=items[0],version=JSON.stringify(items);
 $("announcements").classList.toggle("hidden",!first);
 $("announcementEntry").classList.toggle("hidden",!first);
 if(!first){$("announcementList").replaceChildren();announcementVersion=version;return;}
 $("announcementEntryLabel").textContent=first.pinned?"置顶公告":"最新公告";
 $("announcementEntryTitle").textContent=first.title;
 $("announcementEntry").title=first.title;
 if(version===announcementVersion)return;
 announcementVersion=version;
 const date=item=>'<time datetime="'+esc(item.published_at)+'">'+esc(announcementDate(item.published_at))+' · 北京时间</time>';
 const badge=item=>item.pinned?'<span class="announcement-badge">置顶</span>':"";
 $("announcementList").innerHTML='<article class="announcement-feature"><div class="announcement-meta">'+badge(first)+date(first)+'</div><h3>'+esc(first.title)+'</h3><p class="announcement-excerpt">'+esc(first.content)+'</p><button type="button" class="announcement-read" data-announcement="'+Number(first.id)+'">查看全文 '+svg("arrow")+'</button></article>'+(items.length>1?'<details class="announcement-archive"><summary>更多公告 <span>'+fmt(items.length-1)+' 条</span></summary><div>'+items.slice(1).map(item=>'<button class="announcement-row" type="button" data-announcement="'+Number(item.id)+'"><span>'+badge(item)+'<strong>'+esc(item.title)+'</strong></span>'+date(item)+svg("arrow")+'</button>').join("")+'</div></details>':"");
}
async function refreshAnnouncements(signal) {
 try {
  const response=await fetch("/api/public/announcements",{credentials:"omit",cache:"no-store",signal});
  if(!response.ok)throw new Error();
  const items=await response.json();
  if(!Array.isArray(items))throw new Error();
  announcements=items;renderPublicAnnouncements();$("announcementLoadError").classList.add("hidden");
 } catch {
  $("announcements").classList.remove("hidden");
  $("announcementLoadError").textContent=announcements?.length?"公告更新失败，当前显示上次读取的内容。可点击页尾“刷新”重试。":"暂时无法读取公告，可点击页尾“刷新”重试。";
  $("announcementLoadError").classList.remove("hidden");
 }
}
function showAnnouncement(id) {
 const item=announcements?.find(item=>item.id===id);if(!item)return;
 $("announcementHeading").textContent=item.title;
 $("announcementDialogMeta").textContent=(item.pinned?"置顶公告 · ":"公告 · ")+announcementDate(item.published_at)+" · 北京时间";
 $("announcementBody").textContent=item.content;
 $("announcementDialog").showModal();document.body.classList.add("modal-open");$("closeAnnouncement").focus();
}
function closeAnnouncement() { $("announcementDialog").close();document.body.classList.remove("modal-open"); }
function publicDialogOpen() { return $("historyDialog").open||$("announcementDialog").open; }

function finishIntro() {
 document.documentElement.classList.remove("intro-active");
 document.removeEventListener("keydown",skipIntro);
 document.removeEventListener("pointerdown",skipIntro,true);
}
function skipIntro(event) { if(event.type==="pointerdown"||event.key==="Tab"||event.key==="Escape")finishIntro(); }
if(document.documentElement.classList.contains("intro-active")) {
 setTimeout(finishIntro,3800);
 document.addEventListener("keydown",skipIntro);
 document.addEventListener("pointerdown",skipIntro,true);
}

function notify(text) { $("toast").textContent=text;$("toast").classList.remove("hidden");clearTimeout(toastTimer);toastTimer=setTimeout(()=>$("toast").classList.add("hidden"),5000); }
function relativeTime(value) {
 if(!value)return "尚无调用记录";
 const seconds=Math.max(0,(Date.now()-Date.parse(value))/1000);
 if(seconds<60)return "最近调用：刚刚";
 if(seconds<3600)return "最近调用："+Math.floor(seconds/60)+" 分钟前";
 if(seconds<86400)return "最近调用："+Math.floor(seconds/3600)+" 小时前";
 return "最近调用："+Math.floor(seconds/86400)+" 天前";
}
function renderFirstScreen() {
 const names=overview.models.map(model=>model.model),shown=names.slice(0,4);
 $("todayModels").innerHTML=shown.length?shown.map(model=>modelLabel(model)).join("")+(names.length>4?" 等 "+names.length+" 个模型":""):overview.models_loading?"模型列表更新中":"暂无可用模型";
 $("todayModels").title=names.join("、");
 const total=tokens(overview.summary);
 $("heroTitle").setAttribute("aria-label","全站累计 "+fmt(total)+" Token");
 $("heroTokens").title=fmt(total)+" Tokens";
 $("heroTokenNote").textContent=heroTokenDescription(total);
 finishHeroRoll();
 const box=$("firstScreen").getBoundingClientRect();
 const animate=!heroAnimated&&!document.hidden&&!document.documentElement.classList.contains("motion-paused")&&!matchMedia("(prefers-reduced-motion: reduce)").matches&&box.bottom>0&&box.top<innerHeight&&total>0;
 heroAnimated=true;
 if(animate)rollHeroNumber(total);else showHeroNumber(total);
}
function renderOverview() {
 renderHistoryControls();
 renderFirstScreen();
 const s=overview.summary;
 $("totalTokens").textContent=compact(tokens(s));
 $("totalTokens").setAttribute("aria-label","全站累计 Token 用量："+fmt(tokens(s)));
 $("totalTokens").title=fmt(tokens(s))+" Tokens";
 $("tokenDetail").textContent="输入 "+compact(s.prompt_tokens)+" / 输出 "+compact(s.completion_tokens);
 const total=tokens(s);
 $("inputTokenShare").style.width=(total>0?s.prompt_tokens/total*100:0)+"%";
 $("outputTokenShare").style.width=(total>0?s.completion_tokens/total*100:0)+"%";
 $("totalUptime").textContent=rate(s.uptime_percent);
 $("uptimeShare").style.width=(Number.isFinite(s.uptime_percent)?Math.max(0,Math.min(100,s.uptime_percent)):0)+"%";
 const state=overview.models.some(m=>m.status==="incident")?"incident":overview.models.some(m=>m.status==="operational")?"operational":"idle";
 $("overallStatus").className="status-badge "+state;
 $("overallStatus").innerHTML='<span class="dot"></span>'+({incident:"有模型最近调用异常",operational:"模型服务运行中",idle:"等待真实调用反馈"}[state]);
 $("updatedAt").textContent="更新于 "+new Date(overview.updated_at).toLocaleTimeString("zh-CN",{timeZone:"Asia/Shanghai",hour12:false,hour:"2-digit",minute:"2-digit",second:"2-digit"});
 $("heroDataStatus").textContent=$("updatedAt").textContent;$("heroDataStatus").classList.remove("is-stale");
 $("modelCount").textContent=overview.models.length;
 renderTrend();renderUsage();renderModels();
}
function renderTrend(animate=false) {
 if(!overview)return;
 $("trendCalls").setAttribute("aria-pressed",String(metric==="calls"));
 $("trendTokens").setAttribute("aria-pressed",String(metric==="tokens"));
 const data=overview.daily.slice(-7),values=data.map(d=>metric==="calls"?d.calls:tokens(d)),sum=values.reduce((a,b)=>a+b,0);
 $("weeklyAmount").textContent=compact(sum);
 $("weeklyUnit").textContent=metric==="calls"?"近 7 天调用":"近 7 天 Tokens";
 $("chartLegend").textContent=metric==="calls"?"调用次数":"Token 用量";
 $("chartDates").textContent=data[0].date.slice(5).replace("-","/")+" – "+data.at(-1).date.slice(5).replace("-","/");
 const width=640,height=210,left=38,right=14,top=18,bottom=35,plot=height-top-bottom;
 const highest=Math.max(...values,1),rawStep=highest/3,power=10**Math.floor(Math.log10(rawStep)),step=Math.max(1,Math.ceil(rawStep/power)*power),ceiling=step*3;
 const points=values.map((v,i)=>[left+i*(width-left-right)/6,top+plot-v/ceiling*plot]);
 const path=points.map((p,i)=>(i?"L":"M")+p[0].toFixed(1)+" "+p[1].toFixed(1)).join(" ");
 const base=top+plot,area=path+" L"+points.at(-1)[0]+" "+base+" L"+points[0][0]+" "+base+" Z";
 const label=metric==="calls"?"次调用":"Tokens";
 let drawing='<svg viewBox="0 0 '+width+' '+height+'" role="img" aria-labelledby="trendTitle trendDescription"><title id="trendTitle">最近七天'+(metric==="calls"?"调用次数":"Token 用量")+'</title><desc id="trendDescription">'+esc(data.map((d,i)=>d.date+"："+fmt(values[i])+" "+label).join("；"))+'</desc>';
 for(let i=0;i<4;i++){const y=base-i*plot/3;drawing+='<line class="chart-grid" x1="'+left+'" x2="'+(width-right)+'" y1="'+y+'" y2="'+y+'"/><text class="chart-axis" x="'+(left-9)+'" y="'+(y+4)+'" text-anchor="end">'+compact(step*i)+'</text>';}
 drawing+='<path class="chart-area" d="'+area+'" fill="var(--accent-soft)"/><path class="chart-line" pathLength="1000" d="'+path+'"/>';
 points.forEach((p,i)=>{drawing+='<circle class="chart-point" cx="'+p[0]+'" cy="'+p[1]+'" r="3"><title>'+esc(data[i].date+" · "+fmt(values[i])+" "+label)+'</title></circle><text class="chart-axis" x="'+p[0]+'" y="'+(height-9)+'" text-anchor="middle">'+data[i].date.slice(5).replace("-","/")+'</text>';});
 $("trendChart").innerHTML=drawing+'</svg>';
 window.pageMotion?.drawChart(animate);
}
function renderUsage() {
 const models=new Map(overview.models.map(model=>[model.model,{model:model.model,calls:0,successes:0}]));
 overview.usage.forEach(model=>models.set(model.model,model));
 const list=[...models.values()].sort((a,b)=>b.calls-a.calls||a.model.localeCompare(b.model));
 const highest=Math.max(...list.map(model=>model.calls),1);
 $("usageList").innerHTML=list.length?list.map(model=>'<div class="usage-row"><div class="usage-row-top"><strong>'+modelLabel(model.model)+'</strong><span>'+fmt(model.calls)+' 次</span></div><div class="usage-track" aria-hidden="true"><span style="width:'+(model.calls/highest*100).toFixed(2)+'%"></span></div><p class="usage-row-detail">成功 '+fmt(model.successes)+' 次 · '+compact(tokens(model))+' Tokens</p></div>').join(""):'<p class="loading-note">暂无模型调用记录</p>';
 $("usageRest").textContent=list.length?"全站统计 · 按累计调用次数排序":"";
 window.pageMotion?.drawUsage();
}
function historyTime(value,withDate=true,withSeconds=false) {
 return new Date(value).toLocaleString("zh-CN",{timeZone:"Asia/Shanghai",...(withDate?{month:"2-digit",day:"2-digit"}:{}),hour:"2-digit",minute:"2-digit",...(withSeconds?{second:"2-digit"}:{}),hourCycle:"h23"});
}
function intervalTitle(d) { return historyTime(d.start,true,true)+" – "+historyTime(d.end,true,true)+(d.calls?" · "+fmt(d.calls)+" 次调用 · "+rate(d.successes/d.calls*100):" · 无调用记录"); }
function renderHistoryControls() {
 const minute=(overview?.history_interval||historyInterval)==="minute";
 $("uptimeHour").setAttribute("aria-pressed",String(!minute));
 $("uptimeMinute").setAttribute("aria-pressed",String(minute));
 $("uptimeWindowLabel").textContent="最近 24 小时 · 每格 1 "+(minute?"分钟 · 横向滚动查看":"小时");
}
function drawMinuteHistory(canvas,history) {
 const step=4,height=26,colors=getComputedStyle(document.documentElement),ctx=canvas.getContext("2d");
 canvas.width=history.length*step;canvas.height=height;canvas.style.width=canvas.width+"px";
 if(!ctx)return;
 const palette={idle:colors.getPropertyValue("--history-idle"),success:colors.getPropertyValue("--history-green"),partial:colors.getPropertyValue("--history-amber"),failure:colors.getPropertyValue("--history-red")};
 history.forEach((d,i)=>{const active=!!d.calls,h=active?19:5;ctx.globalAlpha=active?1:.7;ctx.fillStyle=palette[!active?"idle":d.successes===d.calls?"success":!d.successes?"failure":"partial"];ctx.fillRect(i*step,(height-h)/2,3,h);});
 let hovered=-1;
 canvas.addEventListener("pointermove",event=>{const index=Math.floor(event.offsetX/step);if(index===hovered)return;hovered=index;const d=history[index];if(d)canvas.title=intervalTitle(d);});
}
let keyOverview=null,keyBusy=false,keyEpoch=0,keyMessage="完整密钥不会保存在浏览器中。",keyMessageError=false,keyFormError=false;
function setKeyMessage(message,error=false) { keyMessage=message;keyMessageError=error;renderKeyAccount(); }
function renderKeyAccount() {
 $("keyForm").classList.toggle("hidden",!!keyOverview);
 $("keyAccount").classList.toggle("hidden",!keyOverview);
 $("keyInput").disabled=keyBusy;$("verifyKey").disabled=keyBusy;
 $("verifyKey").setAttribute("aria-busy",String(keyBusy));
 $("verifyKey").innerHTML=keyBusy?"正在验证…":"查看额度 "+svg("arrow");
 $("switchKey").disabled=keyBusy;$("logoutKey").disabled=keyBusy;
 $("keyMessage").textContent=keyMessage;$("keyMessage").classList.toggle("is-error",keyMessageError);
 $("keyInput").setAttribute("aria-invalid",String(keyMessageError&&!keyOverview));
 $("quotaNote").textContent=keyOverview?"下方额度仅属于当前密钥。共享额度供多个模型共用，独立额度单独扣费；Uptime 与调用量始终为全站统计。":"验证密钥后，这里会显示对应模型的独立额度余量。";
 if(!keyOverview)return;
 const s=keyOverview.summary;
 $("keyIdentity").textContent="密钥 ···· "+keyOverview.key_suffix;
 $("remainingAmount").innerHTML=!keyOverview.has_pools?'未绑定模型':s.unlimited?'不限额度':money(s.remaining_usd)+'<small>USD</small>';
 $("balanceHint").textContent=!keyOverview.has_pools?"此密钥尚未配置可用模型":s.unlimited?(s.quota_usd>0?"限额部分剩余 "+money(s.remaining_usd)+" · 另含不限额度":"当前额度不限额"):"总额度 "+money(s.quota_usd)+" · 已使用 "+money(s.used_usd);
 const percent=s.quota_usd>0?Math.min(100,Math.max(0,s.remaining_usd/s.quota_usd*100)):0;
 $("balancePercent").textContent=!s.unlimited&&s.quota_usd>0?percent.toFixed(1)+"% 可用":"";
 $("balanceTrack").classList.toggle("hidden",!keyOverview.has_pools||s.unlimited||s.quota_usd<=0);
 $("balanceTrack").setAttribute("aria-valuenow",percent.toFixed(1));$("balanceTrack").firstElementChild.style.width=percent+"%";
 const shared=keyOverview.shared;
 $("sharedBalance").textContent=shared?"其中共享余量："+(shared.unlimited?"不限额度":money(shared.remaining_usd))+" · 多个模型共用":"独立额度明细见下方模型列表。";
}
async function refreshKeyOverview(signal) {
 if(keyBusy)return;
 const epoch=keyEpoch,hadKey=!!keyOverview;
 try {
  const response=await fetch("/api/key/session",{credentials:"same-origin",cache:"no-store",signal});
  if(epoch!==keyEpoch||keyBusy)return;
  if(!response.ok)throw new Error();
  const state=await response.json();if(epoch!==keyEpoch||keyBusy)return;
  if(!state.authenticated){
   keyOverview=null;
   if(hadKey||!keyFormError)setKeyMessage(hadKey?"查询会话已失效，请重新输入密钥。":"完整密钥不会保存在浏览器中。",hadKey);
   renderModels();return;
  }
  const next=state.overview;
  keyOverview=next;keyFormError=false;
  setKeyMessage("额度更新于 "+new Date(next.updated_at).toLocaleTimeString("zh-CN",{timeZone:"Asia/Shanghai",hour12:false})+" · 仅当前密钥可见");
  renderModels();
 } catch {
  if(epoch===keyEpoch&&!keyBusy)setKeyMessage(hadKey?"额度更新失败，当前显示上次数据。请点击页尾刷新重试。":"暂时无法读取查询状态，可输入密钥重试。",true);
 }
}
async function verifyKey(event) {
 event.preventDefault();if(keyBusy)return;
 const key=$("keyInput").value.trim();if(!key){setKeyMessage("请输入 API 密钥。",true);$("keyInput").focus();return;}
 keyBusy=true;keyEpoch++;setKeyMessage("正在验证密钥…");
 const controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),10000);
 try {
  const response=await fetch("/api/key/session",{method:"POST",headers:{"Content-Type":"application/json","X-APIShare-Request":"key-view"},body:JSON.stringify({key}),credentials:"same-origin",cache:"no-store",signal:controller.signal});
  const result=await response.json().catch(()=>({}));
  if(!response.ok)throw new Error(result.error||"暂时无法验证密钥，请稍后重试。");
  keyOverview=result;$("keyInput").value="";keyMessage="已验证 · 查询会话有效期 7 天";keyMessageError=false;keyFormError=false;
  notify("已显示当前密钥额度");
 } catch(error) { keyMessage=error.name==="AbortError"?"查询超时，请稍后重试。":error.message;keyMessageError=true;keyFormError=true; }
 finally {clearTimeout(timeout);keyBusy=false;renderKeyAccount();renderModels();if(keyOverview)$("switchKey").focus({preventScroll:true});else $("keyInput").focus({preventScroll:true});}
}
async function clearKey(switching=false) {
 if(keyBusy)return;keyBusy=true;keyEpoch++;renderKeyAccount();
 const controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),10000);
 try {
  const response=await fetch("/api/key/session",{method:"DELETE",headers:{"X-APIShare-Request":"key-view"},credentials:"same-origin",cache:"no-store",signal:controller.signal});
  if(!response.ok)throw new Error();
  keyEpoch++;keyOverview=null;keyFormError=false;$("keyInput").value="";
  keyMessage=switching?"输入另一个密钥查看对应额度。":"已退出，个人额度已隐藏。";keyMessageError=false;
 } catch {keyMessage="退出失败，请稍后重试。";keyMessageError=true;}
 finally {clearTimeout(timeout);keyBusy=false;renderKeyAccount();renderModels();if(!keyOverview)$("keyInput").focus({preventScroll:true});}
}
function renderModelQuota(model) {
 if(!keyOverview)return "";
 const quota=Object.hasOwn(keyOverview.models,model.model)?keyOverview.models[model.model]:null;
 if(!quota)return '<div class="model-quota"><span class="quota-mode">当前密钥未开通</span></div>';
 if(quota.mode==="shared")return '<div class="model-quota"><span class="quota-mode">使用我的共享额度</span><span class="quota-label">与其他共享模型共用上方余额</span></div>';
 const pool=quota.pool,depleted=!pool.unlimited&&pool.remaining_usd<=0;
 const percent=pool.quota_usd>0?Math.min(100,Math.max(0,pool.remaining_usd/pool.quota_usd*100)):0;
 const track=pool.unlimited?"":'<span class="quota-progress" role="meter" aria-label="'+esc(model.model)+' 的独立限额剩余比例" aria-valuenow="'+percent.toFixed(2)+'" aria-valuemin="0" aria-valuemax="100"><span style="width:'+percent+'%"></span></span>';
 return '<div class="model-quota"><span class="quota-mode is-independent">我的独立额度</span><div class="quota-pool '+(depleted?'is-depleted':'')+'"><span class="quota-label">剩余</span><strong>'+(pool.unlimited?'不限额度':money(pool.remaining_usd))+'</strong>'+(pool.unlimited?'':'<span class="quota-total">/ '+money(pool.quota_usd)+'</span>')+track+(depleted?'<span class="quota-depleted">独立额度已用完</span>':'')+'</div></div>';
}
function renderModels() {
 if(!overview)return;
 const activeModel=document.activeElement?.dataset.model;
 const activeTrack=document.activeElement?.classList.contains("minute-track");
 const scrollPositions=new Map([...$("modelsGrid").querySelectorAll(".minute-track")].map(track=>[track.dataset.model,track.scrollLeft]));
 const minute=overview.history_interval==="minute";
 const query=$("modelSearch").value.trim().toLowerCase(),status=$("statusFilter").value;
 const denied=m=>!!keyOverview&&!Object.hasOwn(keyOverview.models,m.model);
 const recent=m=>(m.history||[]).reduce((sum,d)=>sum+(Number(d.calls)||0),0);
 const list=overview.models.filter(m=>(!query||(m.model+" "+m.family).toLowerCase().includes(query))&&(!status||m.status===status));
 // Heat first: most requested in the last 24 hours on top, idle models sink.
 // When a key is verified, models it cannot use sink below every usable one.
 list.sort((a,b)=>{
  const access=(denied(a)?1:0)-(denied(b)?1:0);
  if(access)return access;
  const heat=recent(b)-recent(a);
  if(heat)return heat;
  if(a.family!==b.family)return a.family<b.family?-1:1;
  return a.model.localeCompare(b.model);
 });
 $("modelsGrid").innerHTML=list.map(m=>{
  const unavailable=denied(m);
  const familyTag=window.APIShareModelIcons.html(m.model,m.family);
  const track=minute?'<div class="minute-track" data-model="'+esc(m.model)+'" tabindex="0" role="region" aria-label="'+esc(m.model)+' 的 24 小时分钟轨迹，可横向滚动"><canvas class="minute-canvas" aria-hidden="true"></canvas></div>':'<div class="uptime-bars" style="--history-buckets:'+m.history.length+'" aria-hidden="true">'+m.history.map(d=>'<span class="interval-bar '+(!d.calls?"":d.successes===d.calls?"success":!d.successes?"failure":"partial")+'" title="'+esc(intervalTitle(d))+'"></span>').join("")+'</div>';
  return '<article class="model-card state-'+m.status+(unavailable?" is-unavailable":"")+'" data-motion-key="'+esc(m.model)+'"><div class="model-identity"><span class="family-symbol" aria-hidden="true">'+familyTag+'</span><div class="model-name"><span class="family">'+esc(m.family)+'</span><h3>'+esc(m.model)+'</h3><span class="model-state"><i class="dot"></i>'+statusText[m.status]+'</span>'+(unavailable?'<span class="unavailable-tag">当前密钥不可用</span>':'')+'</div></div><div class="model-timeline">'+track+'<div class="history-labels"><span>'+esc(historyTime(m.history[0].start))+'</span><span>'+esc(historyTime(m.history.at(-1).end))+'</span></div></div><div class="model-uptime '+(m.uptime_percent===null?'no-results':'has-results')+'"><strong>'+rate(m.uptime_percent)+'</strong><span>24 小时请求成功率</span><span class="model-speed'+(m.avg_tps==null?" is-empty":"")+'" title="'+(m.avg_tps==null?"最近 24 小时没有可测量的输出速度样本":"按输出 Token ÷ 生成时长加权平均")+'">平均 '+speed(m.avg_tps)+'</span></div><div class="model-consumption"><div><strong title="'+fmt(tokens(m))+' Tokens">'+compact(tokens(m))+'</strong><span>Tokens</span></div><div><strong>'+ fmt(m.calls)+'</strong><span>次调用</span></div><span class="last-call">'+relativeTime(m.last_call)+'</span></div><button class="model-history" type="button" data-model="'+esc(m.model)+'" aria-label="查看 '+esc(m.model)+' 的 24 小时调用历史">'+svg("clock")+'<span>调用历史</span></button>'+renderModelQuota(m)+'</article>';
 }).join("")||'<div class="empty-state">'+svg("layers")+'<h3>'+(query||status?"没有匹配的模型":overview.models_loading?"模型列表正在更新":"模型服务即将上线")+'</h3><p>'+(query||status?"试试其他模型名称或状态。":"模型准备就绪后，将在这里展示服务状态。")+'</p></div>';
 if(activeModel){[...$("modelsGrid").querySelectorAll(activeTrack?".minute-track":"button")].find(b=>b.dataset.model===activeModel)?.focus({preventScroll:true});}
 if(minute){const models=new Map(list.map(m=>[m.model,m]));$("modelsGrid").querySelectorAll(".minute-track").forEach(track=>{drawMinuteHistory(track.querySelector("canvas"),models.get(track.dataset.model).history);track.scrollLeft=scrollPositions.get(track.dataset.model)??track.scrollWidth;});}
 window.pageMotion?.observe();
}
function showHistory(model) {
 const m=overview?.models.find(m=>m.model===model);if(!m)return;
 historyModel=model;
 $("historyHeading").innerHTML=modelLabel(m.model,m.family);
 const minute=overview.history_interval==="minute";
 $("historyDescription").textContent="最近 24 小时的真实调用，按"+(minute?"分钟":"小时")+"汇总。灰色时段表示没有调用。"+(minute?"选择时段可查看这 24 小时内任意一小时的分钟记录。":"");
 $("historyHourControl").classList.toggle("hidden",!minute);
 if(minute){$("historyHour").innerHTML=Array.from({length:24},(_,i)=>'<option value="'+i+'">'+esc(historyTime(m.history[i*60].start)+" – "+historyTime(m.history[(i+1)*60-1].end,false))+'</option>').join("");$("historyHour").value="23";}
 renderHistoryRows();
 $("historyDialog").showModal();document.body.classList.add("modal-open");$("closeHistory").focus();
}
function renderHistoryRows() {
 const m=overview?.models.find(m=>m.model===historyModel);if(!m)return;
 const start=Number($("historyHour").value)*60;
 const rows=overview.history_interval==="minute"?m.history.slice(start,start+60):m.history;
 $("historyRows").innerHTML=[...rows].reverse().map(d=>'<tr class="'+(!d.calls?"no-calls":"")+'"><td title="'+esc(intervalTitle(d))+'"><time datetime="'+esc(d.start)+'">'+esc(historyTime(d.start,true,true))+'</time><span class="history-end">– '+esc(historyTime(d.end,false,true))+'</span></td><td>'+fmt(d.calls)+'</td><td>'+fmt(d.successes)+'</td><td>'+(d.calls?rate(d.successes/d.calls*100):"无调用")+'</td></tr>').join("");
 $("historyRows").closest(".history-scroll").scrollTop=0;
}
function closeHistory() { $("historyDialog").close();historyModel=null;document.body.classList.remove("modal-open"); }
function applyPublicBranding(branding) {
 if(!branding)return;
 const title=String(branding.title||"").trim()||"API Share";
 document.documentElement.dataset.siteTitle=title;
 document.querySelectorAll("[data-site-brand-name]").forEach(node=>{node.textContent=title;});
 document.querySelectorAll("[data-site-brand-home]").forEach(node=>node.setAttribute("aria-label",title+" 首页"));
 const icon=branding.icon_url||"";
 document.querySelectorAll("[data-site-brand-logo]").forEach(image=>{
  image.src=icon||image.dataset.defaultSrc||"/public/static/brand/apishare-icon-v2.png";
 });
 document.querySelectorAll("[data-site-brand-icon]").forEach(link=>{
  link.setAttribute("href",icon||link.dataset.defaultHref||"/public/static/brand/apishare-favicon-v2.png");
  link.setAttribute("type",icon?(branding.icon_type||"image/png"):"image/png");
 });
 if(branding.custom_title)document.title=title+" · 用量与模型状态";
}
async function loadPublicBranding() {
 try {
  const response=await fetch("/api/public/branding",{credentials:"omit",cache:"no-store"});
  if(!response.ok)return;
  applyPublicBranding(await response.json());
 } catch {}
}
async function refresh() {
 if(refreshing)return;
 refreshing=true;$("refreshNow").disabled=true;$("refreshNow").setAttribute("aria-busy","true");
 $("uptimeHour").disabled=true;$("uptimeMinute").disabled=true;$("modelsGrid").setAttribute("aria-busy","true");
 if(overview&&overview.history_interval!==historyInterval)$("uptimeWindowLabel").textContent="正在读取"+(historyInterval==="minute"?"分钟":"小时")+"记录…";
 const controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),10000);
 const announcementTask=refreshAnnouncements(controller.signal);
 const keyTask=refreshKeyOverview(controller.signal);
 try {
  const response=await fetch("/api/public/overview?interval="+historyInterval,{credentials:"omit",cache:"no-store",signal:controller.signal});
  if(!response.ok)throw new Error("服务数据暂时无法更新");
  const next=await response.json();
  overview=next;renderOverview();$("errorBanner").classList.add("hidden");
  $("refreshNote").textContent=autoUpdate?"每 30 秒更新":"已暂停自动更新";
 } catch {
  historyInterval=overview?.history_interval||"hour";renderHistoryControls();
  $("heroDataStatus").textContent=overview?"更新失败 · 暂示上次数据":"暂时无法读取全站数据";$("heroDataStatus").classList.add("is-stale");
  if(!overview) {
   $("todayModels").textContent="暂时无法读取模型";
   $("heroTitle").setAttribute("aria-label","累计 Token 用量暂时无法读取");
   $("heroTokenNote").textContent="暂时无法读取 Token 用量";
  }
  $("errorBanner").textContent=overview?"更新失败。当前显示上次读取的数据，点击页尾“刷新”重试。":"暂时无法读取服务数据，点击页尾“刷新”重试。";
  $("errorBanner").classList.remove("hidden");
  $("overallStatus").className="status-badge idle";$("overallStatus").innerHTML='<span class="dot"></span>数据更新异常';
  $("refreshNote").textContent="更新失败";
 } finally { await Promise.allSettled([announcementTask,keyTask]);clearTimeout(timeout);refreshing=false;$("refreshNow").disabled=false;$("refreshNow").setAttribute("aria-busy","false");$("uptimeHour").disabled=false;$("uptimeMinute").disabled=false;$("modelsGrid").setAttribute("aria-busy","false"); }
}
$("keyForm").addEventListener("submit",verifyKey);
$("switchKey").addEventListener("click",()=>clearKey(true));
$("logoutKey").addEventListener("click",()=>clearKey(false));
renderKeyAccount();
$("announcementList").addEventListener("click",event=>{const button=event.target.closest("button[data-announcement]");if(button)showAnnouncement(Number(button.dataset.announcement));});
$("closeAnnouncement").addEventListener("click",closeAnnouncement);
$("announcementDialog").addEventListener("cancel",event=>{event.preventDefault();closeAnnouncement();});
$("announcementEntry").addEventListener("click",()=>$("announcements").focus({preventScroll:true}));
$("modelSearch").addEventListener("input",renderModels);$("statusFilter").addEventListener("change",renderModels);
document.querySelectorAll("[data-metric]").forEach(b=>b.addEventListener("click",()=>{metric=b.dataset.metric;renderTrend(true);}));
document.querySelectorAll("[data-history-interval]").forEach(b=>b.addEventListener("click",()=>{if(refreshing||historyInterval===b.dataset.historyInterval)return;historyInterval=b.dataset.historyInterval;refresh();}));
$("historyHour").addEventListener("change",renderHistoryRows);
$("modelsGrid").addEventListener("click",event=>{const button=event.target.closest("button[data-model]");if(button)showHistory(button.dataset.model);});
$("closeHistory").addEventListener("click",closeHistory);
$("historyDialog").addEventListener("cancel",event=>{event.preventDefault();closeHistory();});
$("historyDialog").addEventListener("click",event=>{if(event.target!==$("historyDialog"))return;const r=event.target.getBoundingClientRect();if(event.clientX<r.left||event.clientX>r.right||event.clientY<r.top||event.clientY>r.bottom)closeHistory();});
$("autoRefresh").addEventListener("click",()=>{autoUpdate=!autoUpdate;$("autoRefresh").setAttribute("aria-pressed",String(autoUpdate));$("autoRefresh").textContent=autoUpdate?"暂停自动更新":"恢复自动更新";$("refreshNote").textContent=autoUpdate?"每 30 秒更新":"已暂停自动更新";if(autoUpdate)refresh();});
$("refreshNow").addEventListener("click",refresh);
$("copyAddress").addEventListener("click",async()=>{const address=location.origin+"/v1";try{await copyToClipboard(address);notify("OpenAI 接入地址已复制");}catch{notify("复制失败，请手动复制接入地址："+address);}});
document.querySelectorAll("svg.icon").forEach(s=>s.setAttribute("aria-hidden","true"));
document.addEventListener("visibilitychange",()=>{if(!document.hidden&&autoUpdate&&!publicDialogOpen())refresh();});
setInterval(()=>{if(autoUpdate&&!document.hidden&&!publicDialogOpen())refresh();},30000);
window.addEventListener("resize",()=>{finishHeroRoll();measureHeroType();},{passive:true});
document.fonts?.ready.then(measureHeroType);
loadPublicBranding();
refresh();

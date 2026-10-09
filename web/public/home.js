/* Public dashboard: only the anonymous, aggregate endpoint is requested. */
const $ = id => document.getElementById(id);
const esc = value => String(value ?? "").replace(/[&<>"']/g, c => ({ "&":"&amp;", "<":"&lt;", ">":"&gt;", '"':"&quot;", "'":"&#39;" }[c]));
const fmt = value => Number(value || 0).toLocaleString("zh-CN");
const compact = value => { value=Number(value||0);return value>=1e8?(value/1e8).toFixed(1)+"亿":value>=1e6?(value/1e6).toFixed(1)+"M":value>=1e4?(value/1e4).toFixed(1)+"万":fmt(value); };
const money = value => "$"+Number(value||0).toLocaleString("en-US",{minimumFractionDigits:2,maximumFractionDigits:Number(value)>0&&Number(value)<1?4:2});
const rate = value => value===null||value===undefined?"—":Number(value).toFixed(2)+"%";
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
 const names=overview.models.map(model=>model.model), shown=names.slice(0,4);
 $("todayModels").textContent=shown.length?shown.join("、")+(names.length>4?" 等 "+names.length+" 个模型":""):overview.models_loading?"模型列表更新中":"暂无可用模型";
 $("todayModels").title=names.join("、");
 const total=tokens(overview.summary),configured=overview.summary.configured;
 $("heroTitle").setAttribute("aria-label",configured?"累计 "+fmt(total)+" Token":"累计 Token 用量尚未发布");
 $("heroTokens").setAttribute("aria-hidden","true");
 $("heroTokens").title=configured?fmt(total)+" Tokens":"尚未发布用量";
 $("heroTokenNote").textContent=configured?heroTokenDescription(total):"尚未发布 Token 用量";
 finishHeroRoll();
 if(!configured){$("heroTokens").textContent="—";delete $("heroTokens").dataset.value;measureHeroType();return;}
 const box=$("firstScreen").getBoundingClientRect();
 const animate=!heroAnimated&&!document.hidden&&!document.documentElement.classList.contains("motion-paused")&&!matchMedia("(prefers-reduced-motion: reduce)").matches&&box.bottom>0&&box.top<innerHeight&&total>0;
 heroAnimated=true;
 if(animate)rollHeroNumber(total);else showHeroNumber(total);
}
function renderOverview() {
 renderHistoryControls();
 renderFirstScreen();
 const s=overview.summary;
 $("totalTokens").textContent=s.configured?compact(tokens(s)):"—";
 $("totalTokens").setAttribute("aria-label",s.configured?"累计 Token 用量："+fmt(tokens(s)):"尚未发布 Token 用量");
 $("totalTokens").title=fmt(tokens(s))+" Tokens";
 $("tokenDetail").textContent=s.configured?"输入 "+compact(s.prompt_tokens)+" / 输出 "+compact(s.completion_tokens):"暂未发布用量";
 $("totalCalls").textContent=s.configured?compact(s.calls):"—";
 $("totalCalls").title=fmt(s.calls)+" 次调用";
 $("callDetail").textContent=s.configured?"成功 "+fmt(s.successes)+" 次":"暂未发布调用统计";
 $("totalUptime").textContent=rate(s.uptime_percent);
 $("remainingAmount").innerHTML=!s.configured?"—":s.unlimited?"不限额度":money(s.remaining_usd)+'<small>USD</small>';
 $("remainingAmount").title=!s.configured?"暂未发布余量":s.unlimited?"已发布额度不限":money(s.remaining_usd)+" USD";
 $("balanceHint").textContent=!s.configured?"暂未发布余量":s.unlimited?"累计消耗 "+money(s.used_usd):"总额度 "+money(s.quota_usd);
 const percent=s.quota_usd>0&&s.remaining_usd!==null?Math.min(100,Math.max(0,s.remaining_usd/s.quota_usd*100)):0;
 $("balancePercent").textContent=s.configured&&!s.unlimited&&s.quota_usd>0?percent.toFixed(1)+"% 可用":"";
 $("balanceTrack").classList.toggle("hidden",!s.configured||s.unlimited||s.quota_usd<=0);
 $("balanceTrack").setAttribute("aria-valuenow",percent.toFixed(1));
 $("balanceTrack").firstElementChild.style.width=percent+"%";
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
 $("weeklyAmount").textContent=overview.summary.configured?compact(sum):"—";
 $("weeklyUnit").textContent=metric==="calls"?"近 7 天调用":"近 7 天 Tokens";
 $("chartLegend").textContent=metric==="calls"?"调用次数":"Token 用量";
 $("chartDates").textContent=data[0].date.slice(5).replace("-","/")+" – "+data.at(-1).date.slice(5).replace("-","/");
 if(!overview.summary.configured){$("trendChart").innerHTML='<p class="loading-note">暂未发布用量数据</p>';return;}
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
 const list=overview.usage.filter(u=>tokens(u)>0),total=list.reduce((sum,u)=>sum+tokens(u),0);
 $("usageList").innerHTML=list.length?list.slice(0,5).map(u=>'<div class="usage-row"><div class="usage-row-top"><strong>'+esc(u.model)+'</strong><span title="'+fmt(tokens(u))+' Tokens">'+compact(tokens(u))+'</span></div><div class="usage-track" aria-hidden="true"><span style="width:'+(tokens(u)/total*100).toFixed(2)+'%"></span></div></div>').join(""):'<p class="loading-note">'+(overview.summary.configured?"暂无 Token 用量":"暂未发布模型用量")+'</p>';
 const rest=list.slice(5),restTokens=rest.reduce((sum,u)=>sum+tokens(u),0);
 $("usageRest").textContent=rest.length?"其他 "+rest.length+" 个模型 · "+compact(restTokens)+" Tokens":list.length?"按累计 Token 用量排序":"";
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
function renderModels() {
 if(!overview)return;
 const activeModel=document.activeElement?.dataset.model;
 const activeTrack=document.activeElement?.classList.contains("minute-track");
 const scrollPositions=new Map([...$("modelsGrid").querySelectorAll(".minute-track")].map(track=>[track.dataset.model,track.scrollLeft]));
 const minute=overview.history_interval==="minute";
 const query=$("modelSearch").value.trim().toLowerCase(),status=$("statusFilter").value;
 const list=overview.models.filter(m=>(!query||(m.model+" "+m.family).toLowerCase().includes(query))&&(!status||m.status===status));
 $("modelsGrid").innerHTML=list.map(m=>{
  const familyTag=({Anthropic:"C",Google:"G",OpenAI:"O",DeepSeek:"D",xAI:"X",Qwen:"Q",GLM:"Z"})[m.family]||"AI";
  const track=minute?'<div class="minute-track" data-model="'+esc(m.model)+'" tabindex="0" role="region" aria-label="'+esc(m.model)+' 的 24 小时分钟轨迹，可横向滚动"><canvas class="minute-canvas" aria-hidden="true"></canvas></div>':'<div class="uptime-bars" style="--history-buckets:'+m.history.length+'" aria-hidden="true">'+m.history.map(d=>'<span class="interval-bar '+(!d.calls?"":d.successes===d.calls?"success":!d.successes?"failure":"partial")+'" title="'+esc(intervalTitle(d))+'"></span>').join("")+'</div>';
  return '<article class="model-card state-'+m.status+'" data-motion-key="'+esc(m.model)+'"><div class="model-identity"><span class="family-symbol" aria-hidden="true">'+familyTag+'</span><div class="model-name"><span class="family">'+esc(m.family)+'</span><h3>'+esc(m.model)+'</h3><span class="model-state"><i class="dot"></i>'+statusText[m.status]+'</span></div></div><div class="model-timeline">'+track+'<div class="history-labels"><span>'+esc(historyTime(m.history[0].start))+'</span><span>'+esc(historyTime(m.history.at(-1).end))+'</span></div></div><div class="model-uptime '+(m.uptime_percent===null?'no-results':'has-results')+'"><strong>'+rate(m.uptime_percent)+'</strong><span>24 小时请求成功率</span></div><div class="model-consumption"><div><strong title="'+fmt(tokens(m))+' Tokens">'+(overview.summary.configured?compact(tokens(m)):"—")+'</strong><span>Tokens</span></div><div><strong>'+ (overview.summary.configured?fmt(m.calls):"—")+'</strong><span>次调用</span></div><span class="last-call">'+relativeTime(m.last_call)+'</span></div><button class="model-history" type="button" data-model="'+esc(m.model)+'" aria-label="查看 '+esc(m.model)+' 的 24 小时调用历史">'+svg("arrow")+'<span>历史</span></button></article>';
 }).join("")||'<div class="empty-state">'+svg("layers")+'<h3>'+(query||status?"没有匹配的模型":overview.models_loading?"模型列表正在更新":"模型服务即将上线")+'</h3><p>'+(query||status?"试试其他模型名称或状态。":"模型准备就绪后，将在这里展示服务状态。")+'</p></div>';
 if(activeModel){[...$("modelsGrid").querySelectorAll(activeTrack?".minute-track":"button")].find(b=>b.dataset.model===activeModel)?.focus({preventScroll:true});}
 if(minute){const models=new Map(list.map(m=>[m.model,m]));$("modelsGrid").querySelectorAll(".minute-track").forEach(track=>{drawMinuteHistory(track.querySelector("canvas"),models.get(track.dataset.model).history);track.scrollLeft=scrollPositions.get(track.dataset.model)??track.scrollWidth;});}
 window.pageMotion?.observe();
}
function showHistory(model) {
 const m=overview?.models.find(m=>m.model===model);if(!m)return;
 historyModel=model;
 $("historyHeading").textContent=m.model;
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
async function refresh() {
 if(refreshing)return;
 refreshing=true;$("refreshNow").disabled=true;$("refreshNow").setAttribute("aria-busy","true");
 $("uptimeHour").disabled=true;$("uptimeMinute").disabled=true;$("modelsGrid").setAttribute("aria-busy","true");
 if(overview&&overview.history_interval!==historyInterval)$("uptimeWindowLabel").textContent="正在读取"+(historyInterval==="minute"?"分钟":"小时")+"记录…";
 const controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),10000);
 try {
  const response=await fetch("/api/public/overview?interval="+historyInterval,{credentials:"omit",cache:"no-store",signal:controller.signal});
  if(!response.ok)throw new Error("服务数据暂时无法更新");
  const next=await response.json();
  overview=next;renderOverview();$("errorBanner").classList.add("hidden");
  $("refreshNote").textContent=autoUpdate?"每 30 秒更新":"已暂停自动更新";
 } catch {
  historyInterval=overview?.history_interval||"hour";renderHistoryControls();
  $("heroDataStatus").textContent=overview?"更新失败 · 暂示上次数据":"暂时无法读取共享数据";$("heroDataStatus").classList.add("is-stale");
  if(!overview) {
   $("todayModels").textContent="暂时无法读取模型";
   $("heroTitle").setAttribute("aria-label","累计 Token 用量暂时无法读取");
   $("heroTokenNote").textContent="暂时无法读取 Token 用量";
  }
  $("errorBanner").textContent=overview?"更新失败。当前显示上次读取的数据，点击页尾“刷新”重试。":"暂时无法读取服务数据，点击页尾“刷新”重试。";
  $("errorBanner").classList.remove("hidden");
  $("overallStatus").className="status-badge idle";$("overallStatus").innerHTML='<span class="dot"></span>数据更新异常';
  $("refreshNote").textContent="更新失败";
 } finally { clearTimeout(timeout);refreshing=false;$("refreshNow").disabled=false;$("refreshNow").setAttribute("aria-busy","false");$("uptimeHour").disabled=false;$("uptimeMinute").disabled=false;$("modelsGrid").setAttribute("aria-busy","false"); }
}
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
$("copyAddress").addEventListener("click",async()=>{const address=location.origin+"/v1";try{await navigator.clipboard.writeText(address);notify("OpenAI 接入地址已复制");}catch{notify("请手动复制接入地址："+address);}});
document.querySelectorAll("svg.icon").forEach(s=>s.setAttribute("aria-hidden","true"));
document.addEventListener("visibilitychange",()=>{if(!document.hidden&&autoUpdate&&!$("historyDialog").open)refresh();});
setInterval(()=>{if(autoUpdate&&!document.hidden&&!$("historyDialog").open)refresh();},30000);
window.addEventListener("resize",()=>{finishHeroRoll();measureHeroType();},{passive:true});
document.fonts?.ready.then(measureHeroType);
refresh();

/* Native scroll choreography; visible content and normal scrolling are the fallback. */
(() => {
 const stage=document.getElementById("heroStage"),screen=document.getElementById("firstScreen"),preference=matchMedia("(prefers-reduced-motion: reduce)");
 const controls=[document.getElementById("heroMotion"),document.getElementById("ambientMotion")];
 const animations=new Set(),registered=new WeakSet(),pending=new Set(),seenModels=new Set();
 const ease="cubic-bezier(.16,1,.3,1)";
 let enabled=true,visible=true,frame=0,progress=0,range=1,start=0,x=0,y=0,targetX=0,targetY=0,chartDrawn=false,usageDrawn=false;
 const allowed=()=>enabled&&!preference.matches&&!document.hidden;

 function paint(value,px=0,py=0) {
  const s=screen.style;
  s.setProperty("--hero-lift",(-value*12).toFixed(2)+"px");
  s.setProperty("--pointer-x",px.toFixed(2)+"px");s.setProperty("--pointer-y",py.toFixed(2)+"px");
 }
 function schedule() { if(!frame&&allowed()&&visible)frame=requestAnimationFrame(tick); }
 function tick() {
  frame=0;if(!allowed()||!visible)return;
  const target=Math.max(0,Math.min(1,(scrollY-start)/range));
  progress+=(target-progress)*.13;x+=(targetX-x)*.1;y+=(targetY-y)*.1;paint(progress,x,y);
  if(Math.abs(target-progress)>.0005||Math.abs(targetX-x)>.02||Math.abs(targetY-y)>.02)schedule();
 }
 function measure() {start=stage.offsetTop;range=Math.max(1,screen.clientHeight*.8);schedule();}
 function sync() {
  const reduced=preference.matches;
  document.documentElement.classList.toggle("motion-reduced",reduced);
  screen.classList.toggle("ambient-paused",!allowed()||!visible);
  document.documentElement.classList.toggle("motion-paused",!allowed());
  controls.forEach(button=>{
   button.setAttribute("aria-pressed",String(enabled&&!reduced));button.disabled=reduced;
   button.textContent=reduced?"已减少动态效果":enabled?(button.id==="heroMotion"?"暂停动画":"暂停页面动画"):(button.id==="heroMotion"?"播放动画":"播放页面动画");
  });
  if(!allowed()) {
   cancelAnimationFrame(frame);frame=0;animations.forEach(animation=>animation.finish());
   if(!enabled||reduced){window.finishIntro?.();window.finishHeroRoll?.();paint(0);}
  } else schedule();
  window.dispatchEvent(new Event("public-motion-change"));
 }
 function play(element,frames,options) {
  if(!allowed()||!element?.animate)return;
  const animation=element.animate(frames,options);animations.add(animation);
  animation.finished.catch(()=>{}).finally(()=>animations.delete(animation));
 }
 function drawChart(force=false) {
  const chart=document.getElementById("trendChart");
  if(!chart.closest(".trend-card").dataset.motionEntered||chartDrawn&&!force)return;
  const line=chart.querySelector(".chart-line");if(!line)return;chartDrawn=true;
  play(line,[{strokeDasharray:"1000",strokeDashoffset:"1000"},{strokeDasharray:"1000",strokeDashoffset:"0"}],{duration:1450,easing:ease});
  play(chart.querySelector(".chart-area"),[{opacity:0},{opacity:.55}],{duration:1300,easing:ease});
  chart.querySelectorAll(".chart-point").forEach((point,index)=>play(point,[{opacity:0},{opacity:1}],{duration:400,delay:200+index*100,fill:"backwards"}));
 }
 function drawUsage() {
  const card=document.querySelector(".usage-card");
  if(usageDrawn||!card.dataset.motionEntered||!card.querySelector(".usage-track"))return;usageDrawn=true;
  card.querySelectorAll(".usage-track > span").forEach((bar,index)=>play(bar,[{transform:"scaleX(0)"},{transform:"scaleX(1)"}],{duration:1100,delay:index*70,easing:ease,fill:"backwards"}));
 }
 function reveal(element) {
  element.dataset.motionEntered="1";
  const key=element.dataset.motionKey;if(key)seenModels.add(key);
  const index=[...element.parentElement.children].indexOf(element);
  const delay=element.classList.contains("metric")?index*80:element.classList.contains("model-card")?Math.min(index,4)*65:element.classList.contains("usage-card")?110:0;
  play(element,[{opacity:0,transform:"translate3d(0,32px,0) scale(.97)"},{opacity:1,transform:"translate3d(0,0,0) scale(1)"}],{duration:850,delay,easing:ease,fill:"backwards"});
  if(element.classList.contains("trend-card"))drawChart();if(element.classList.contains("usage-card"))drawUsage();
  if(element.classList.contains("balance-metric"))play(document.getElementById("balanceTrack").firstElementChild,[{transform:"scaleX(0)"},{transform:"scaleX(1)"}],{duration:1200,easing:ease});
  if(element.classList.contains("model-card"))element.querySelectorAll(".interval-bar").forEach((bar,i)=>play(bar,[{opacity:0,transform:"scaleY(.3)"},{opacity:1,transform:"scaleY(1)"}],{duration:430,delay:delay+120+i*8,easing:ease,fill:"backwards"}));
 }
 const observer="IntersectionObserver" in window?new IntersectionObserver(entries=>{
  entries.forEach(entry=>{if(entry.isIntersecting){observer.unobserve(entry.target);pending.delete(entry.target);reveal(entry.target);}});
 },{threshold:.12}):null;
 function observe() {
  pending.forEach(element=>{if(!element.isConnected){observer?.unobserve(element);pending.delete(element);}});
  document.querySelectorAll(".overview > .hero,.metric,.trend-card,.usage-card,.models-intro,.model-card,.connection-strip").forEach(element=>{
   if(registered.has(element))return;registered.add(element);
   if(element.dataset.motionKey&&seenModels.has(element.dataset.motionKey)){element.dataset.motionEntered="1";return;}
   if(observer){pending.add(element);observer.observe(element);}else element.dataset.motionEntered="1";
  });
 }
 controls.forEach(button=>button.addEventListener("click",()=>{enabled=!enabled;sync();}));
 preference.addEventListener("change",()=>{sync();measure();});
 document.addEventListener("visibilitychange",sync);
 window.addEventListener("scroll",schedule,{passive:true});window.addEventListener("resize",measure,{passive:true});
 screen.addEventListener("pointermove",event=>{if(event.pointerType!=="mouse"||!allowed())return;const box=screen.getBoundingClientRect();targetX=((event.clientX-box.left)/box.width-.5)*16;targetY=((event.clientY-box.top)/box.height-.5)*14;schedule();},{passive:true});
 screen.addEventListener("pointerleave",()=>{targetX=0;targetY=0;schedule();},{passive:true});
 if("IntersectionObserver" in window)new IntersectionObserver(([entry])=>{visible=entry.isIntersecting;sync();}).observe(screen);
 window.pageMotion={observe,drawChart,drawUsage,isRunning:()=>allowed()&&visible,scene:()=>({progress,x,y})};sync();measure();document.fonts?.ready.then(measure);observe();
})();

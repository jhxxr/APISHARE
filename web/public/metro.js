/* Token Metro: the anonymous public overview as a night-time metro station rendered with WebGL.
   Only /api/public/overview is requested; every figure shown in the world comes from it. */
import * as THREE from "three";
import { EffectComposer } from "three/addons/postprocessing/EffectComposer.js";
import { RenderPass } from "three/addons/postprocessing/RenderPass.js";
import { UnrealBloomPass } from "three/addons/postprocessing/UnrealBloomPass.js";
import { ShaderPass } from "three/addons/postprocessing/ShaderPass.js";
import { OutputPass } from "three/addons/postprocessing/OutputPass.js";
import { Reflector } from "three/addons/objects/Reflector.js";

const $ = id => document.getElementById(id);
const fmt = value => Number(value || 0).toLocaleString("zh-CN");
const compact = value => { value=Number(value||0); return value>=1e12?(value/1e12).toFixed(2)+"T":value>=1e9?(value/1e9).toFixed(2)+"B":value>=1e6?(value/1e6).toFixed(2)+"M":value>=1e4?(value/1e3).toFixed(1)+"K":fmt(value); };
const money = value => "$"+Number(value||0).toLocaleString("en-US",{minimumFractionDigits:2,maximumFractionDigits:Number(value)>0&&Number(value)<1?4:2});
const rate = value => value===null||value===undefined||!isFinite(value)?"—":Number(value).toFixed(2)+"%";
const tokens = value => Number(value.prompt_tokens||0)+Number(value.completion_tokens||0);
const clamp = (v,a,b) => Math.max(a,Math.min(b,v));
const lerp = (a,b,t) => a+(b-a)*t;
const rnd = (a=0,b=1) => a+Math.random()*(b-a);
const beijing = (opts={}) => new Date().toLocaleString("zh-CN",{timeZone:"Asia/Shanghai",hour12:false,...opts});
const clockTime = value => new Date(value).toLocaleTimeString("zh-CN",{timeZone:"Asia/Shanghai",hour12:false});

/* ---------- Lines: one model family is one metro line ---------- */
const LINE_META = new Map([
 ["OpenAI",{code:"GP",name:"GPT Line",color:"#2be4a7"}],
 ["Anthropic",{code:"CL",name:"Claude Line",color:"#ff8a4c"}],
 ["Google",{code:"GM",name:"Gemini Line",color:"#4c8dff"}],
 ["DeepSeek",{code:"DS",name:"DeepSeek Line",color:"#8c6bff"}],
 ["Qwen",{code:"QW",name:"Qwen Line",color:"#00d8f0"}],
 ["GLM",{code:"GL",name:"GLM Line",color:"#ff4fb3"}],
 ["xAI",{code:"XA",name:"Grok Line",color:"#d7dce8"}],
 ["OpenAI 兼容",{code:"PX",name:"Proxy Line",color:"#ffc857"}],
]);
const LINE_ORDER = [...LINE_META.keys()];
const metaOf = family => LINE_META.get(family) || {code:String(family||"AI").replace(/[^A-Za-z]/g,"").slice(0,2).toUpperCase()||"AI",name:family+" Line",color:"#9aa6c0"};
const STATUS_TEXT = {operational:"运行正常",incident:"最近调用失败",idle:"暂无近期调用"};
const STATUS_COLOR = {operational:"#36f18b",incident:"#ff5a5f",idle:"#6b7385"};
const DISPLAY = '"Orbitron", "Segoe UI", sans-serif', MONO = '"JetBrains Mono", "Cascadia Code", Consolas, monospace', SANS = '"Space Grotesk", "Microsoft YaHei", "PingFang SC", sans-serif';

/* ---------- World layout (metres). d runs down the tunnel; three.js z = -d ---------- */
const WALL_L=-7, WALL_R=10.5, CEIL=5.6, BED=-1.3, EDGE=1.6, START=-10, END=60, TA=3.6, TB=7.4;
const P = (x,y,d) => new THREE.Vector3(x,y,-d);
const MAP_D0=2.6, MAP_Y0=.45, MAP_Y1=5.3;
// The route map is laid out in its own coordinates and placed this far down the wall, clear of the kiosk.
const MAP_SHIFT=2.6, wd=d=>d+MAP_SHIFT;
const BOARD={x:-1.8,y:3.9,d:7.5,w:6,h:2.1};
const KIOSK={x:-5.3,d:3.4,w:1.8,h:2.3,depth:.75};

let overview=null, lines=[], modelIndex=new Map(), calls24=0, mapEnd=16, mapStep=2;
function buildLines() {
 const groups=new Map();calls24=0;
 for(const m of overview.models){
  let c=0,s=0;for(const h of m.history){c+=h.calls;s+=h.successes;}
  calls24+=c;
  if(!groups.has(m.family))groups.set(m.family,[]);
  groups.get(m.family).push({...m,calls24:c,ok24:s,total:tokens(m)});
 }
 const order=f=>{const i=LINE_ORDER.indexOf(f);return i<0?99:i;};
 lines=[...groups.entries()].sort((a,b)=>order(a[0])-order(b[0])||a[0].localeCompare(b[0])).map(([family,models],index)=>{
  const meta=metaOf(family);
  models.forEach((m,k)=>{m.code=meta.code+"-"+String(k+1).padStart(2,"0");});
  const status=models.some(m=>m.status==="incident")?"incident":models.some(m=>m.status==="operational")?"operational":"idle";
  return {family,meta,models,status,index,calls24:models.reduce((a,m)=>a+m.calls24,0),ok24:models.reduce((a,m)=>a+m.ok24,0),total:models.reduce((a,m)=>a+m.total,0)};
 });
 modelIndex=new Map();
 const n=lines.length,maxStations=Math.max(1,...lines.map(l=>l.models.length));
 mapStep=clamp(14/maxStations,1.2,2.15);
 const top=4.15,bottom=n>5?.95:1.45;
 lines.forEach((line,i)=>{
  line.y=n===1?2.8:top-i*(top-bottom)/(n-1);
  line.models.forEach((m,k)=>{m.y=line.y;m.d=6.4+k*mapStep;m.line=line;m.k=k;modelIndex.set(m.model,m);});
  line.dEnd=6.4+(line.models.length-1)*mapStep;
 });
 mapEnd=Math.max(14,...lines.map(l=>l.dEnd+3.2));
}
const overallStatus = () => lines.some(l=>l.status==="incident")?"incident":lines.some(l=>l.status==="operational")?"operational":"idle";
const flatModels = () => lines.flatMap(l=>l.models);
const energy = () => overview?.summary.configured?clamp(Math.log10(1+tokens(overview.summary))/10,.12,1):.12;

/* ---------- Canvas helpers (textures and in-world screens) ---------- */
function makeCanvas(w,h) { const c=document.createElement("canvas");c.width=w;c.height=h;return [c,c.getContext("2d")]; }
function fitStr(g,s,maxW) { s=String(s);if(g.measureText(s).width<=maxW)return s;while(s.length>2&&g.measureText(s+"…").width>maxW)s=s.slice(0,-1);return s+"…"; }
function text(g,str,x,y,{size=24,font=SANS,weight=500,color="#fff",align="left",base="alphabetic",glow=0,glowColor,maxW,alpha=1}={}) {
 g.font=weight+" "+size+"px "+font;g.textAlign=align;g.textBaseline=base;
 if(maxW)str=fitStr(g,str,maxW);
 g.globalAlpha=alpha;g.fillStyle=color;
 if(glow){g.shadowColor=glowColor||color;g.shadowBlur=glow;}
 g.fillText(str,x,y);g.shadowBlur=0;g.globalAlpha=1;
 return g.measureText(str).width;
}
function rrect(g,x,y,w,h,r) { g.beginPath();g.roundRect(x,y,w,h,r); }
function speckle(g,w,h,n,alpha) { for(let i=0;i<n;i++){const l=Math.random()>.5?255:0;g.fillStyle="rgba("+l+","+l+","+l+","+(Math.random()*alpha)+")";const s=rnd(1,3);g.fillRect(rnd(0,w),rnd(0,h),s,s);} }
function blotches(g,w,h,n,alpha,color="0,0,0") { for(let i=0;i<n;i++){const x=rnd(0,w),y=rnd(0,h),r=rnd(w*.03,w*.16),gr=g.createRadialGradient(x,y,0,x,y,r);gr.addColorStop(0,"rgba("+color+","+alpha+")");gr.addColorStop(1,"rgba("+color+",0)");g.fillStyle=gr;g.fillRect(x-r,y-r,2*r,2*r);} }
function streaks(g,w,h,n,alpha,y0=0) { for(let i=0;i<n;i++){const x=rnd(0,w),len=h*rnd(.15,.8),wd=rnd(1,9),y=y0+rnd(-.05,.25)*h,gr=g.createLinearGradient(0,y,0,y+len);gr.addColorStop(0,"rgba(0,0,0,"+alpha+")");gr.addColorStop(1,"rgba(0,0,0,0)");g.fillStyle=gr;g.fillRect(x,y,wd,len);} }
function scanlines(g,w,h,alpha,step=4) { g.fillStyle="rgba(0,0,0,"+alpha+")";for(let y=0;y<h;y+=step)g.fillRect(0,y,w,1); }
function bucketColor(h) { return !h.calls?"#232b38":h.successes===h.calls?"#36f18b":!h.successes?"#ff5a5f":"#ffc857"; }

async function main() {
 /* ---------- Renderer, camera, post-processing ---------- */
 const canvas=$("metroScene");
 let renderer;
 try { renderer=new THREE.WebGLRenderer({canvas,antialias:false,powerPreference:"high-performance"}); }
 catch { showFallback();return; }
 const mobile=matchMedia("(pointer: coarse)").matches||Math.min(innerWidth,innerHeight)<600;
 let pixelRatio=Math.min(devicePixelRatio||1,mobile?1:1.5);
 renderer.setPixelRatio(pixelRatio);
 renderer.outputColorSpace=THREE.SRGBColorSpace;
 renderer.toneMapping=THREE.ACESFilmicToneMapping;
 renderer.toneMappingExposure=.9;
 const maxAniso=Math.min(8,renderer.capabilities.getMaxAnisotropy());
 const scene=new THREE.Scene();
 scene.background=new THREE.Color(0x020308);
 scene.fog=new THREE.FogExp2(0x04060b,.017);
 const camera=new THREE.PerspectiveCamera(55,innerWidth/innerHeight,.08,420);
 const composer=new EffectComposer(renderer);
 composer.addPass(new RenderPass(scene,camera));
 const bloom=new UnrealBloomPass(new THREE.Vector2(innerWidth,innerHeight),.3,.35,.9);
 composer.addPass(bloom);
 composer.addPass(new OutputPass());
 // Display-space grade: chromatic fringe, teal shadows, grain, vignette.
 const grade=new ShaderPass({
  uniforms:{tDiffuse:{value:null},time:{value:0},res:{value:new THREE.Vector2(innerWidth,innerHeight)},ca:{value:.0014}},
  vertexShader:"varying vec2 vUv;void main(){vUv=uv;gl_Position=projectionMatrix*modelViewMatrix*vec4(position,1.);}",
  fragmentShader:`uniform sampler2D tDiffuse;uniform float time;uniform vec2 res;uniform float ca;varying vec2 vUv;
   float rand(vec2 c){return fract(sin(dot(c,vec2(12.9898,78.233)))*43758.5453);}
   void main(){vec2 d=vUv-.5;float r=dot(d,d);vec2 off=d*ca*(.5+r*3.);
    vec3 col=vec3(texture2D(tDiffuse,vUv+off).r,texture2D(tDiffuse,vUv).g,texture2D(tDiffuse,vUv-off).b);
    float lum=dot(col,vec3(.299,.587,.114));
    col=mix(col,col*vec3(.82,1.,1.16),(1.-smoothstep(0.,.45,lum))*.4);
    col=mix(col,col*vec3(1.08,.97,1.04),smoothstep(.55,1.,lum)*.3);
    col+=(rand(vUv*res+fract(time))-.5)*.032;
    col*=mix(1.,.5,smoothstep(.12,.7,r*1.7));
    gl_FragColor=vec4(col,1.);}`,
 });
 composer.addPass(grade);

 // A dim neon environment so metal and wet surfaces have something to reflect.
 {
  const env=new THREE.Scene(),pm=new THREE.PMREMGenerator(renderer);
  env.add(new THREE.Mesh(new THREE.BoxGeometry(30,10,30),new THREE.MeshBasicMaterial({color:0x05070c,side:THREE.BackSide})));
  for(const [c,x,y,z,w,h] of [[0x00e5ff,-14,1,0,.2,6],[0xff3cac,14,1,-4,.2,5],[0xdfefff,0,4.9,0,10,.2],[0x6a5cff,0,1,-14,8,.2]]){
   const m=new THREE.Mesh(new THREE.BoxGeometry(w,h,w>1?.2:8),new THREE.MeshBasicMaterial({color:new THREE.Color(c).multiplyScalar(3)}));m.position.set(x,y,z);env.add(m);
  }
  scene.environment=pm.fromScene(env,.04).texture;scene.environmentIntensity=.28;
 }

 /* ---------- Procedural materials ---------- */
 const texFrom=(c,repeat,srgb=true)=>{const t=new THREE.CanvasTexture(c);t.wrapS=t.wrapT=THREE.RepeatWrapping;if(repeat)t.repeat.set(repeat[0],repeat[1]);if(srgb)t.colorSpace=THREE.SRGBColorSpace;t.anisotropy=maxAniso;return t;};
 const roughCanvas=(()=>{const [c,g]=makeCanvas(512,512);g.fillStyle="#b4b4b4";g.fillRect(0,0,512,512);speckle(g,512,512,6000,.25);blotches(g,512,512,30,.35,"40,40,40");return c;})();
 function concrete(base,{tiles=false}={}) {
  const [c,g]=makeCanvas(1024,1024);
  g.fillStyle=base;g.fillRect(0,0,1024,1024);
  speckle(g,1024,1024,14000,.06);blotches(g,1024,1024,50,.22);blotches(g,1024,1024,14,.08,"120,140,170");
  if(tiles){
   // Grimy subway tiles on the lower band of the walls.
   const top=760;g.fillStyle="#3a4048";g.fillRect(0,top,1024,264);
   for(let y=top;y<1024;y+=22)for(let x=(y/22)%2?0:22;x<1024;x+=44){g.fillStyle="hsl(210,8%,"+rnd(20,30)+"%)";g.fillRect(x+1,y+1,42,20);}
   g.fillStyle="#13171d";g.fillRect(0,top-10,1024,10);
  }
  streaks(g,1024,1024,70,.28);
  g.strokeStyle="rgba(0,0,0,.4)";g.lineWidth=1;
  for(let i=0;i<10;i++){let x=rnd(0,1024),y=rnd(0,1024);g.beginPath();g.moveTo(x,y);for(let k=0;k<14;k++){x+=rnd(-14,14);y+=rnd(4,20);g.lineTo(x,y);}g.stroke();}
  return c;
 }
 const roughTex=texFrom(roughCanvas,[6,6],false);
 const wallMat=new THREE.MeshStandardMaterial({map:texFrom(concrete("#2a2e35",{tiles:true}),[70/6,1]),roughnessMap:roughTex,roughness:.92,metalness:.05});
 const rightWallMat=new THREE.MeshStandardMaterial({map:texFrom(concrete("#262a31",{tiles:true}),[70/6,1]),roughnessMap:roughTex,roughness:.92,metalness:.05});
 const ceilMat=new THREE.MeshStandardMaterial({map:texFrom(concrete("#17191e"),[3,12]),roughness:.95,color:0x9aa0aa});
 const concreteMat=new THREE.MeshStandardMaterial({map:texFrom(concrete("#1d2026")),roughnessMap:roughTex,roughness:.9,metalness:.05});
 const metalMat=new THREE.MeshStandardMaterial({color:0x2a313b,roughness:.38,metalness:.85});
 const darkMetalMat=new THREE.MeshStandardMaterial({color:0x12161d,roughness:.45,metalness:.8});
 const railMat=new THREE.MeshStandardMaterial({color:0x9aa6b4,roughness:.22,metalness:1});
 const emissive=(hex,k=1)=>new THREE.MeshBasicMaterial({color:new THREE.Color(hex).multiplyScalar(k)});

 // Platform floor: tiles with puddles cut through to a real-time mirror underneath.
 const [floorC,floorG]=makeCanvas(1024,1024),[puddleC,puddleG]=makeCanvas(1024,1024),[floorRC,floorRG]=makeCanvas(1024,1024);
 floorG.fillStyle="#0c0e12";floorG.fillRect(0,0,1024,1024);
 for(let y=0;y<8;y++)for(let x=0;x<8;x++){floorG.fillStyle="hsl(215,7%,"+rnd(15,21)+"%)";floorG.fillRect(x*128+3,y*128+3,122,122);}
 speckle(floorG,1024,1024,16000,.07);blotches(floorG,1024,1024,40,.3);streaks(floorG,1024,1024,12,.12);
 floorRG.fillStyle="#8c8c8c";floorRG.fillRect(0,0,1024,1024);speckle(floorRG,1024,1024,8000,.3);
 puddleG.fillStyle="#d2d2d2";puddleG.fillRect(0,0,1024,1024);
 // Large, irregular puddles built from overlapping soft blobs; everywhere else stays faintly wet.
 for(let i=0;i<14;i++){
  const cx=rnd(0,1024),cy=rnd(0,1024),parts=4+Math.floor(rnd(0,5));
  for(let k=0;k<parts;k++){
   const x=cx+rnd(-110,110),y=cy+rnd(-70,70),rx=rnd(60,180),ry=rx*rnd(.35,.75);
   for(const [g,inner] of [[puddleG,"rgba(24,24,24,.85)"],[floorRG,"rgba(14,14,14,.9)"]]){
    g.save();g.translate(x,y);g.scale(1,ry/rx);const gr=g.createRadialGradient(0,0,0,0,0,rx);gr.addColorStop(0,inner);gr.addColorStop(.45,inner);gr.addColorStop(1,"rgba(0,0,0,0)");g.fillStyle=gr;g.beginPath();g.arc(0,0,rx,0,Math.PI*2);g.fill();g.restore();
   }
  }
 }
 const platformW=EDGE-WALL_L,platformL=END-START,mirrorScale=mobile?.35:.5;
 const floorMat=new THREE.MeshStandardMaterial({map:texFrom(floorC,[platformW/4.8,platformL/4.8]),roughnessMap:texFrom(floorRC,[platformW/4.8,platformL/4.8],false),roughness:.9,metalness:.2,alphaMap:texFrom(puddleC,[1,2],false),transparent:true});
 const mirror=new Reflector(new THREE.PlaneGeometry(platformW,platformL),{textureWidth:innerWidth*pixelRatio*mirrorScale,textureHeight:innerHeight*pixelRatio*mirrorScale,color:0x56606c,clipBias:.003});
 mirror.rotation.x=-Math.PI/2;mirror.position.copy(P((WALL_L+EDGE)/2,-.004,(START+END)/2));
 scene.add(mirror);
 const floor=new THREE.Mesh(new THREE.PlaneGeometry(platformW,platformL),floorMat);
 floor.rotation.x=-Math.PI/2;floor.position.copy(P((WALL_L+EDGE)/2,0,(START+END)/2));scene.add(floor);

 const add=(geo,mat,pos,rot)=>{const m=new THREE.Mesh(geo,mat);m.position.copy(pos);if(rot)m.rotation.set(rot[0]||0,rot[1]||0,rot[2]||0);scene.add(m);return m;};
 const plane=(w,h,mat,pos,rotY=0,rotX=0)=>add(new THREE.PlaneGeometry(w,h),mat,pos,[rotX,rotY,0]);

 /* ---------- Station shell ---------- */
 plane(platformL,CEIL,wallMat,P(WALL_L,CEIL/2,(START+END)/2),Math.PI/2);
 plane(platformL,CEIL-BED,rightWallMat,P(WALL_R,(CEIL+BED)/2,(START+END)/2),-Math.PI/2);
 plane(WALL_R-WALL_L,platformL,ceilMat,P((WALL_L+WALL_R)/2,CEIL,(START+END)/2),0,Math.PI/2);
 plane(platformL,-BED,concreteMat,P(EDGE,BED/2,(START+END)/2),Math.PI/2);
 // Platform lip, tactile strip and lit edge.
 add(new THREE.BoxGeometry(.3,.06,platformL),concreteMat,P(EDGE-.15,.03,(START+END)/2));
 {
  const [c,g]=makeCanvas(64,1024);g.fillStyle="#b88a1a";g.fillRect(0,0,64,1024);
  for(let y=6;y<1024;y+=16)for(let x=6;x<64;x+=16){g.fillStyle="#7d5c0e";g.beginPath();g.arc(x,y,4,0,Math.PI*2);g.fill();}
  blotches(g,64,1024,30,.45);speckle(g,64,1024,2000,.2);
  plane(.42,platformL,new THREE.MeshStandardMaterial({map:texFrom(c,[1,platformL/6]),roughness:.7}),P(1.02,.006,(START+END)/2),0,-Math.PI/2);
  add(new THREE.BoxGeometry(.06,.004,platformL),new THREE.MeshStandardMaterial({color:0xc9a227,roughness:.6}),P(EDGE-.05,.062,(START+END)/2));
 }
 // Track bed, rails, sleepers, conductor rail.
 {
  const [c,g]=makeCanvas(512,512);g.fillStyle="#141518";g.fillRect(0,0,512,512);
  for(let i=0;i<5000;i++){g.fillStyle="hsl(30,4%,"+rnd(10,34)+"%)";g.beginPath();g.arc(rnd(0,512),rnd(0,512),rnd(1,4),0,Math.PI*2);g.fill();}
  blotches(g,512,512,20,.5);
  plane(WALL_R-EDGE,400,new THREE.MeshStandardMaterial({map:texFrom(c,[3,130]),roughness:1}),P((EDGE+WALL_R)/2,BED,190),0,-Math.PI/2);
  const sleepers=new THREE.InstancedMesh(new THREE.BoxGeometry(2.4,.14,.26),concreteMat,400),m=new THREE.Matrix4();
  let k=0;for(const c of [TA,TB])for(let d=START;d<160;d+=.9){m.setPosition(P(c,BED+.07,d));sleepers.setMatrixAt(k++,m);}
  sleepers.count=k;scene.add(sleepers);
  for(const c of [TA,TB]){for(const dx of [-.72,.72])add(new THREE.BoxGeometry(.09,.16,400),railMat,P(c+dx,BED+.22,190));add(new THREE.BoxGeometry(.1,.1,400),darkMetalMat,P(c+1.1,BED+.2,190));}
 }
 // Ceiling beams, cable trays and fluorescent fixtures.
 const fixtures=[];
 {
  const beams=new THREE.InstancedMesh(new THREE.BoxGeometry(WALL_R-WALL_L,.4,.35),concreteMat,13),m=new THREE.Matrix4();
  for(let i=0;i<13;i++){m.setPosition(P((WALL_L+WALL_R)/2,CEIL-.2,START+i*6));beams.setMatrixAt(i,m);}scene.add(beams);
  for(const [x,y] of [[9.6,4.95],[9.95,5.15]])add(new THREE.BoxGeometry(.32,.08,platformL),metalMat,P(x,y,(START+END)/2));
  const positions=[];for(const x of [-4.4,-1.2])for(let d=1;d<END-2;d+=5)positions.push([x,d]);
  const tubes=new THREE.InstancedMesh(new THREE.BoxGeometry(.14,.05,3),new THREE.MeshBasicMaterial({color:0xffffff}),positions.length);
  const housing=new THREE.InstancedMesh(new THREE.BoxGeometry(.3,.08,3.2),darkMetalMat,positions.length);
  positions.forEach(([x,d],i)=>{
   m.setPosition(P(x,5.43,d+1.5));tubes.setMatrixAt(i,m);m.setPosition(P(x,5.5,d+1.5));housing.setMatrixAt(i,m);
   tubes.setColorAt(i,new THREE.Color(1.05,1.18,1.12));
   fixtures.push({i,d:d+1.5,flicker:i===7});
  });
  scene.add(housing);scene.add(tubes);fixtures.mesh=tubes;
  for(let d=4;d<END;d+=10)add(new THREE.SphereGeometry(.05,8,8),emissive(0x6a5cff,1.4),P(6,5.3,d));
 }
 // Pillars between the tracks.
 for(let i=0;i<5;i++){
  const [c,g]=makeCanvas(128,1024);g.fillStyle="#2a2d33";g.fillRect(0,0,128,1024);
  speckle(g,128,1024,3000,.1);blotches(g,128,1024,10,.3);streaks(g,128,1024,10,.35);
  for(let y=930;y<1024;y+=26){g.fillStyle="#d1a21c";g.fillRect(0,y,128,13);}
  g.fillStyle="#d1a21c";g.fillRect(14,430,100,70);text(g,"0"+(i+1),64,480,{size:46,font:DISPLAY,weight:800,color:"#0b0d10",align:"center"});
  const d=6+i*12;add(new THREE.BoxGeometry(.7,CEIL-BED,.7),new THREE.MeshStandardMaterial({map:texFrom(c),roughness:.85}),P(5.5,(CEIL+BED)/2,d));
  add(new THREE.BoxGeometry(.02,4.2,.02),emissive(0x00e5ff,1.25),P(5.14,2.2,d-.36));
 }
 // End wall with the tunnel mouth, the tunnel itself and its signals.
 const signalDots=[];
 {
  plane(EDGE-WALL_L,CEIL-BED,concreteMat,P((WALL_L+EDGE)/2,(CEIL+BED)/2,END));
  plane(WALL_R-EDGE,CEIL-4.8,concreteMat,P((EDGE+WALL_R)/2,(CEIL+4.8)/2,END));
  for(const [w,h,x,y] of [[WALL_R-EDGE-.5,.05,(EDGE+WALL_R-.5)/2,4.8],[.05,6.1,EDGE,1.75],[.05,6.1,WALL_R-.5,1.75]])add(new THREE.BoxGeometry(w,h,.05),emissive(0x00e5ff,.8),P(x,y,END-.02));
  const tunnelMat=new THREE.MeshStandardMaterial({map:texFrom(concrete("#16181d"),[40,1]),roughness:.95});
  plane(330,6.1,tunnelMat,P(EDGE,1.75,END+165),Math.PI/2);
  plane(330,6.1,tunnelMat,P(WALL_R-.5,1.75,END+165),-Math.PI/2);
  plane(WALL_R-.5-EDGE,330,tunnelMat,P((EDGE+WALL_R-.5)/2,4.8,END+165),0,Math.PI/2);
  const ringMat=emissive(0x2f86c0,.6);
  for(let d=END+8;d<380;d+=13){
   add(new THREE.BoxGeometry(WALL_R-.5-EDGE,.04,.04),ringMat,P((EDGE+WALL_R-.5)/2,4.75,d));
   add(new THREE.BoxGeometry(.04,.04,.6),ringMat,P(EDGE+.05,4.4,d));add(new THREE.BoxGeometry(.04,.04,.6),ringMat,P(WALL_R-.55,4.4,d));
  }
  for(let d=END+14;d<330;d+=26)signalDots.push(add(new THREE.SphereGeometry(.07,10,10),new THREE.MeshBasicMaterial({color:0x6b7385}),P(EDGE+.3,.6,d)));
  const [gc,gg]=makeCanvas(128,128),gr=gg.createRadialGradient(64,64,0,64,64,64);gr.addColorStop(0,"rgba(120,230,255,1)");gr.addColorStop(.3,"rgba(80,90,255,.35)");gr.addColorStop(1,"rgba(0,0,0,0)");gg.fillStyle=gr;gg.fillRect(0,0,128,128);
  const glow=new THREE.Sprite(new THREE.SpriteMaterial({map:texFrom(gc),blending:THREE.AdditiveBlending,depthWrite:false,fog:false,opacity:.55}));
  glow.material.opacity=.22;glow.position.copy(P(6,1.6,300));glow.scale.set(60,40,1);scene.add(glow);
 }

 /* ---------- Neon signage ---------- */
 function neonSign(rows,{w=1024,h=256,color="#ff3cac",font=SANS,weight=800,vertical=false}={}) {
  const [c,g]=makeCanvas(w,h),size=vertical?w*.62:h*.5;
  rows.forEach((str,i)=>{
   if(vertical)[...str].forEach((ch,k)=>{for(const blur of [40,16,0])text(g,ch,w/2,h*.12+k*size*1.08+size*.4,{size,font,weight,color:blur?color:"#fff7fd",align:"center",base:"middle",glow:blur,glowColor:color});});
   else for(const blur of [44,18,0])text(g,str,w/2,h/2+(i-(rows.length-1)/2)*size*1.1,{size:i?size*.45:size,font,weight,color:blur?color:"#fbfdff",align:"center",base:"middle",glow:blur,glowColor:color});
  });
  return new THREE.MeshBasicMaterial({map:texFrom(c),transparent:true,blending:THREE.AdditiveBlending,depthWrite:false,opacity:.72});
 }
 plane(7,1.75,neonSign(["TOKEN METRO","地下枢纽 · 02:37 TERMINAL"],{color:"#00e5ff",font:DISPLAY}),P(-2.8,3.9,END-.05));
 plane(.62,2.6,neonSign(["霓虹线"],{w:256,h:1024,vertical:true,color:"#ff3cac"}),P(5.5,3.2,18-.38));
 plane(.62,2.6,neonSign(["二七站"],{w:256,h:1024,vertical:true,color:"#ffc857"}),P(5.5,3.2,42-.38));
 plane(2.4,.6,neonSign(["出口 EXIT →"],{color:"#36f18b"}),P(-5.4,3.2,END-.05));
 {
  const [c,g]=makeCanvas(512,160);g.fillStyle="#050608";g.fillRect(0,0,512,160);
  text(g,"02:37",256,82,{size:110,font:DISPLAY,weight:700,color:"#ff4a3d",align:"center",base:"middle",glow:24});scanlines(g,512,160,.35,3);
  plane(2.6,.8,new THREE.MeshBasicMaterial({map:texFrom(c),color:new THREE.Color(1.4,1.4,1.4)}),P(5.6,5.2,END-.05));
 }

 /* ---------- Lights ---------- */
 scene.add(new THREE.HemisphereLight(0x223048,0x050608,.42));
 const lights=[];
 const addLight=(color,intensity,distance,pos,d)=>{const l=new THREE.PointLight(color,intensity,distance,2);l.position.copy(pos);scene.add(l);lights.push({l,base:intensity,d});return l;};
 for(const d of [2,14,26,38,50])addLight(0xdcefe6,26,0,P(-2.8,5.2,d),d);
 addLight(0xff3cac,2.5,6,P(-4.4,2.8,2.4),2);
 addLight(0x00d8ff,4,9,P(-5,3.2,12),12);
 addLight(0x9a5cff,6,12,P(9.4,2.4,26),26);
 addLight(0x00e5ff,6,14,P(6,1.5,57),57);
 const headlight=new THREE.SpotLight(0xe8f6ff,0,48,.42,.65,1.4);scene.add(headlight);scene.add(headlight.target);
 const cabin=new THREE.PointLight(0xffd8a8,0,9,1.6);scene.add(cabin);
 const expressLight=new THREE.PointLight(0xffffff,0,13,1.6);scene.add(expressLight);

 /* ---------- In-world screens ---------- */
 const screens=[], pickables=[];
 function makeScreen(w,h,draw,{transparent=false}={}) {
  const [c,g]=makeCanvas(w,h),tex=new THREE.CanvasTexture(c);
  tex.colorSpace=THREE.SRGBColorSpace;tex.anisotropy=maxAniso;
  const s={canvas:c,g,tex,w,h,regions:[],dirty:true,draw,transparent};
  s.redraw=()=>{s.regions=[];g.clearRect(0,0,w,h);draw(g,w,h,s);tex.needsUpdate=true;s.dirty=false;};
  s.region=(id,x,y,rw,rh,tip,action)=>{const r={id,x,y,w:rw,h:rh,tip,action};s.regions.push(r);return r;};
  screens.push(s);return s;
 }
 // Screens are lifted a little so they survive the filmic tone curve.
 const screenMat=(s,opts={})=>new THREE.MeshBasicMaterial({map:s.tex,transparent:s.transparent,color:new THREE.Color(.92,.92,.92),...opts});
 function pickable(mesh,info) { mesh.userData.pick=info;pickables.push(mesh);return mesh; }
 let hover={key:"",obj:null,region:null}, selectedModel=null, selectedLine=null, selectT=0, runMetric="calls", ticketType="openai", view="platform";
 const isHover=(scr,id)=>hover.obj?.userData.pick?.screen===scr&&hover.region?.id===id;

 // Departure board hanging over the platform, with a scrolling broadcast strip below it.
 const flips=new Map();
 const flipValue=(key,value)=>{const f=flips.get(key);if(!f||f.value!==value)flips.set(key,{value,t:performance.now()});return flips.get(key);};
 const board=makeScreen(1536,538,(g,w,h)=>{
  const bg=g.createLinearGradient(0,0,0,h);bg.addColorStop(0,"#07090d");bg.addColorStop(1,"#030406");g.fillStyle=bg;g.fillRect(0,0,w,h);
  g.fillStyle="#0c1a24";g.fillRect(0,0,w,92);
  text(g,"TOKEN METRO",34,62,{size:48,font:DISPLAY,weight:800,color:"#00e5ff",glow:18});
  text(g,"DEPARTURES · 到站信息",410,60,{size:26,color:"#7d8ea3"});
  text(g,overview?"更新 "+clockTime(overview.updated_at):"等待信号",w-34,60,{size:24,font:MONO,color:"#56657a",align:"right"});
  const s=overview?.summary,op=overview?overview.models.filter(m=>m.status==="operational").length:0,next=nextTrainInfo();
  const cells=[
   ["在线站点 STATIONS",overview?op+" / "+overview.models.length:"—"],
   ["累计 TOKEN",s?.configured?compact(tokens(s)):"—"],
   ["24H 调用 CALLS",s?.configured?compact(calls24):"—"],
   ["24H 成功率 UPTIME",s?rate(s.uptime_percent):"—"],
   ["剩余额度 QUOTA",!s?.configured?"—":s.unlimited?"不限":money(s.remaining_usd)],
  ];
  const now=performance.now();
  cells.forEach(([label,value],i)=>{
   const x=34+(i%2)*760,y=128+Math.floor(i/2)*134,f=flipValue(label,value),p=clamp((now-f.t)/380,0,1);
   text(g,label,x,y+18,{size:22,color:"#6f7f93"});
   g.save();g.translate(x,y+82);g.scale(1,p<1?Math.max(.05,Math.abs(Math.cos((1-p)*Math.PI))):1);
   text(g,value,0,0,{size:64,font:MONO,weight:700,color:"#ffb547",base:"middle",glow:16,glowColor:"#ff8a00"});g.restore();
   if(i===4&&s?.configured&&!s.unlimited&&s.quota_usd>0){const pct=clamp(s.remaining_usd/s.quota_usd,0,1);g.fillStyle="#1b2230";g.fillRect(x,y+118,620,8);g.fillStyle="#00e5ff";g.shadowColor="#00e5ff";g.shadowBlur=10;g.fillRect(x,y+118,620*pct,8);g.shadowBlur=0;}
  });
  const nx=794,ny=128+2*134;
  text(g,"NEXT ▸ 1 号站台 PLATFORM 1",nx,ny+18,{size:22,color:"#6f7f93"});
  g.fillStyle=next.color;g.fillRect(nx,ny+52,10,58);
  text(g,next.name,nx+28,ny+82,{size:46,font:DISPLAY,weight:700,color:"#f1f6ff",base:"middle",maxW:500});
  text(g,next.eta,w-34,ny+82,{size:56,font:MONO,weight:700,color:"#ffb547",align:"right",base:"middle",glow:14,glowColor:"#ff8a00"});
  // LED matrix texture.
  g.fillStyle="rgba(0,0,0,.32)";for(let y=96;y<h;y+=4)g.fillRect(0,y,w,1.4);for(let x=0;x<w;x+=4)g.fillRect(x,96,1.4,h-96);
 });
 const ticker=makeScreen(2048,88,(g,w,h)=>{
  g.fillStyle="#08040a";g.fillRect(0,0,w,h);
  const str=broadcastText||"TOKEN METRO",size=46;g.font="600 "+size+"px "+MONO;
  const tw=g.measureText(str).width+260,offset=(clock*150)%tw;
  for(let x=w-offset;x>-tw;x-=tw)text(g,str,x,h/2+2,{size,font:MONO,weight:600,color:"#ff4fb3",base:"middle",glow:12});
  scanlines(g,w,h,.4,3);
 });
 const boardGroup=new THREE.Group();scene.add(boardGroup);
 {
  const frame=new THREE.Mesh(new THREE.BoxGeometry(BOARD.w+.24,BOARD.h+.62,.16),darkMetalMat);frame.position.copy(P(BOARD.x,BOARD.y-.18,BOARD.d+.09));boardGroup.add(frame);
  const face=new THREE.Mesh(new THREE.PlaneGeometry(BOARD.w,BOARD.h),screenMat(board));face.position.copy(P(BOARD.x,BOARD.y,BOARD.d));boardGroup.add(face);
  pickable(face,{id:"board",screen:board,tip:()=>"到站信息屏 · 点击查看",action:()=>setView("board")});
  const strip=new THREE.Mesh(new THREE.PlaneGeometry(BOARD.w,.26),screenMat(ticker));strip.position.copy(P(BOARD.x,BOARD.y-BOARD.h/2-.2,BOARD.d));boardGroup.add(strip);
  pickable(strip,{id:"board",tip:()=>"站台广播 · 点击查看到站屏",action:()=>setView("board")});
  for(const dx of [-2.4,2.4]){const rod=new THREE.Mesh(new THREE.CylinderGeometry(.02,.02,CEIL-BOARD.y-BOARD.h/2+.05),metalMat);rod.position.copy(P(BOARD.x+dx,(CEIL+BOARD.y+BOARD.h/2)/2,BOARD.d+.09));boardGroup.add(rod);}
 }

 // Route map wall: the left wall becomes the network diagram.
 let routeScreen=null,routeMesh=null;
 const pulse=new THREE.Sprite(new THREE.SpriteMaterial({map:(()=>{const [c,g]=makeCanvas(64,64),gr=g.createRadialGradient(32,32,0,32,32,32);gr.addColorStop(0,"#fff");gr.addColorStop(.3,"rgba(160,240,255,.8)");gr.addColorStop(1,"rgba(0,0,0,0)");g.fillStyle=gr;g.fillRect(0,0,64,64);return texFrom(c);})(),blending:THREE.AdditiveBlending,depthWrite:false,color:new THREE.Color(1.2,1.2,1.2)}));
 pulse.scale.set(.5,.5,1);pulse.visible=false;scene.add(pulse);
 function drawRoute(g,w,h,scr) {
  const s=w/(mapEnd-MAP_D0),X=d=>(d-MAP_D0)*s,Y=y=>(MAP_Y1-y)*s;
  const bg=g.createLinearGradient(0,0,w,h);bg.addColorStop(0,"#0b1626");bg.addColorStop(1,"#060b14");g.fillStyle=bg;g.fillRect(0,0,w,h);
  g.strokeStyle="rgba(0,229,255,.05)";g.lineWidth=1;for(let x=0;x<w;x+=s/2){g.beginPath();g.moveTo(x,0);g.lineTo(x,h);g.stroke();}for(let y=0;y<h;y+=s/2){g.beginPath();g.moveTo(0,y);g.lineTo(w,y);g.stroke();}
  g.strokeStyle="#00e5ff";g.lineWidth=4;g.shadowColor="#00e5ff";g.shadowBlur=18;g.strokeRect(6,6,w-12,h-12);g.shadowBlur=0;
  text(g,"ROUTE MAP",X(3.05),Y(4.86),{size:.32*s,font:DISPLAY,weight:800,color:"#00e5ff",glow:14});
  text(g,"模型线路图 · 每条线路是一个模型家族，每个站点是一个模型",X(3.05),Y(4.56),{size:.15*s,color:"#8495ab"});
  text(g,"点击站点查看详情 ↘",w-.3*s,Y(4.86),{size:.15*s,color:"#56657a",align:"right"});
  if(!lines.length){text(g,overview?.models_loading?"线路目录更新中…":"线路筹备中",X(3.05),Y(2.8),{size:.3*s,color:"#c8d6e6"});return;}
  const yTop=lines[0].y,yBot=lines.at(-1).y,focus=selectedLine||hover.region?.line;
  g.fillStyle="#e6f4ff";g.shadowColor="#9fe8ff";g.shadowBlur=20;rrect(g,X(4.55),Y(yTop+.32),.6*s,(yTop-yBot+.64)*s,.12*s);g.fill();g.shadowBlur=0;
  text(g,"HUB",X(4.85),Y(yBot-.62),{size:.15*s,font:DISPLAY,weight:700,color:"#e6f4ff",align:"center"});
  for(const ln of lines){
   const c=ln.meta.color,y=Y(ln.y);
   g.globalAlpha=focus&&focus!==ln?.3:1;
   g.strokeStyle=c;g.lineCap="round";g.shadowColor=c;g.shadowBlur=22;g.lineWidth=.1*s;g.beginPath();g.moveTo(X(5.15),y);g.lineTo(X(ln.dEnd+.35),y);g.stroke();g.shadowBlur=0;
   g.fillStyle=c;rrect(g,X(3.05),y-.17*s,1.2*s,.34*s,.06*s);g.fill();
   text(g,ln.meta.code,X(3.65),y+.075*s,{size:.2*s,font:DISPLAY,weight:800,color:"#05080e",align:"center"});
   scr.region("line:"+ln.family,X(3.05),y-.17*s,1.2*s,.34*s,ln.meta.name+" · 点击查看线路",()=>openLine(ln)).line=ln;
   text(g,ln.meta.name,X(ln.dEnd+.75),y+.065*s,{size:.18*s,font:DISPLAY,weight:700,color:c,glow:10});
   for(const m of ln.models){
    const x=X(m.d),hot=isHover(scr,"m:"+m.model)||selectedModel===m,r=(hot?.19:.14)*s;
    g.fillStyle="#0a0f17";g.strokeStyle=c;g.lineWidth=.05*s;g.beginPath();g.arc(x,y,r,0,Math.PI*2);g.fill();g.stroke();
    g.fillStyle=STATUS_COLOR[m.status];g.shadowColor=STATUS_COLOR[m.status];g.shadowBlur=m.status==="idle"?0:14;g.beginPath();g.arc(x,y,.065*s,0,Math.PI*2);g.fill();g.shadowBlur=0;
    if(hot){g.strokeStyle="#ffffff";g.lineWidth=.02*s;g.beginPath();g.arc(x,y,.3*s,0,Math.PI*2);g.stroke();}
    text(g,m.model,x,y+(m.k%2?-.27:.4)*s,{size:.15*s,font:MONO,color:hot?"#ffffff":"#c8d6e6",align:"center",maxW:mapStep*1.9*s});
    scr.region("m:"+m.model,x-.3*s,y-.3*s,.6*s,.6*s,m.code+" · "+m.model+" · "+STATUS_TEXT[m.status],()=>openStation(m.model)).line=ln;
   }
   g.globalAlpha=1;
  }
  scanlines(g,w,h,.12,3);
 }
 function rebuildRoute() {
  const width=mapEnd-MAP_D0,height=MAP_Y1-MAP_Y0,cw=2048,ch=Math.round(cw*height/width);
  if(routeScreen&&routeScreen.h===ch&&routeMesh.geometry.parameters.width===width)return;
  if(routeMesh){scene.remove(routeMesh);routeMesh.geometry.dispose();routeMesh.material.dispose();routeScreen.tex.dispose();screens.splice(screens.indexOf(routeScreen),1);pickables.splice(pickables.indexOf(routeMesh),1);}
  routeScreen=makeScreen(cw,ch,drawRoute);
  routeMesh=new THREE.Mesh(new THREE.PlaneGeometry(width,height),screenMat(routeScreen));
  routeMesh.rotation.y=Math.PI/2;routeMesh.position.copy(P(WALL_L+.03,(MAP_Y0+MAP_Y1)/2,wd(MAP_D0+width/2)));scene.add(routeMesh);
  pickable(routeMesh,{id:"route",screen:routeScreen,tip:()=>view==="lines"?null:"线路图 · 点击进入",action:()=>setView("lines")});
 }

 // Dispatch screen further down the same wall.
 const DISPATCH_W=15,DISPATCH_H=4.7,dispatchD0=()=>wd(mapEnd+1.4);
 const dispatch=makeScreen(2048,642,(g,w,h,scr)=>{
  const bg=g.createLinearGradient(0,0,w,h);bg.addColorStop(0,"#140a1c");bg.addColorStop(1,"#060912");g.fillStyle=bg;g.fillRect(0,0,w,h);
  g.strokeStyle="#ff3cac";g.lineWidth=4;g.shadowColor="#ff3cac";g.shadowBlur=18;g.strokeRect(6,6,w-12,h-12);g.shadowBlur=0;
  text(g,"DISPATCH CENTER",50,74,{size:52,font:DISPLAY,weight:800,color:"#ff4fb3",glow:16});
  text(g,"调度中心 · 全线运营看板",650,72,{size:26,color:"#9b8aa8"});
  if(!overview){text(g,"等待信号…",50,300,{size:40,color:"#c8d6e6"});return;}
  text(g,"列车运行密度 · 最近 7 天",50,160,{size:28,weight:600,color:"#e6f4ff"});
  [["calls","调用"],["tokens","TOKEN"]].forEach(([key,label],i)=>{
   const x=860+i*110,on=runMetric===key,hot=isHover(scr,"run:"+key);
   g.fillStyle=on?"rgba(0,229,255,.22)":hot?"rgba(255,255,255,.08)":"rgba(255,255,255,.03)";rrect(g,x,124,100,46,8);g.fill();
   g.strokeStyle=on?"#00e5ff":"rgba(255,255,255,.15)";g.lineWidth=2;g.stroke();
   text(g,label,x+50,155,{size:22,weight:600,color:on?"#00e5ff":"#9aa6b8",align:"center"});
   scr.region("run:"+key,x,124,100,46,"运行图显示"+(key==="calls"?"调用次数":"Token 用量"),()=>{runMetric=key;dispatch.dirty=true;});
  });
  const data=overview.daily.slice(-7),values=data.map(d=>runMetric==="calls"?d.calls:tokens(d));
  if(!overview.summary.configured)text(g,"暂未发布用量数据",50,380,{size:30,color:"#8495ab"});
  else{
   const raw=Math.max(...values,1)/3,power10=10**Math.floor(Math.log10(raw)),tick=Math.max(1,Math.ceil(raw/power10)*power10),max=tick*3;
   const left=130,right=1060,top=210,bottom=560,step=(right-left)/7,tops=[];
   for(let i=0;i<=3;i++){const y=bottom-i*(bottom-top)/3;g.strokeStyle="rgba(255,255,255,.07)";g.lineWidth=1;g.beginPath();g.moveTo(left,y);g.lineTo(right,y);g.stroke();text(g,compact(tick*i),left-14,y+7,{size:20,font:MONO,color:"#56657a",align:"right"});}
   values.forEach((v,i)=>{
    const bh=v/max*(bottom-top),x=left+i*step+step*.2,bw=step*.6,y=bottom-bh;tops.push([x+bw/2,y]);
    const gr=g.createLinearGradient(0,y,0,bottom);gr.addColorStop(0,"#00e5ff");gr.addColorStop(1,"rgba(106,92,255,.15)");
    g.fillStyle=gr;g.shadowColor="#00e5ff";g.shadowBlur=14;g.fillRect(x,y,bw,Math.max(bh,2));g.shadowBlur=0;
    if(v>0)text(g,compact(v),x+bw/2,y-12,{size:22,font:MONO,weight:700,color:"#ffb547",align:"center"});
    text(g,data[i].date.slice(5).replace("-","/"),x+bw/2,bottom+34,{size:20,font:MONO,color:"#6f7f93",align:"center"});
   });
   g.strokeStyle="#ff3cac";g.lineWidth=2;g.setLineDash([8,8]);g.beginPath();tops.forEach((p,i)=>i?g.lineTo(...p):g.moveTo(...p));g.stroke();g.setLineDash([]);
  }
  const tx=1160,tr=1990;
  text(g,"线路运营 · LINE STATUS",tx,160,{size:28,weight:600,color:"#e6f4ff"});
  text(g,"线路",tx,208,{size:18,color:"#6f7f93"});text(g,"累计 TOKEN",1770,208,{size:18,color:"#6f7f93",align:"right"});text(g,"24H 调用",1880,208,{size:18,color:"#6f7f93",align:"right"});text(g,"成功率",tr,208,{size:18,color:"#6f7f93",align:"right"});
  const ranked=[...lines].sort((a,b)=>b.total-a.total),topTotal=Math.max(1,ranked[0]?.total||0),rowH=Math.min(46,330/Math.max(1,ranked.length));
  ranked.forEach((l,i)=>{
   const y=246+i*rowH;
   g.fillStyle=l.meta.color;rrect(g,tx,y-2,58,28,5);g.fill();text(g,l.meta.code,tx+29,y+19,{size:16,font:DISPLAY,weight:800,color:"#05080e",align:"center"});
   text(g,l.meta.name,tx+74,y+19,{size:21,color:"#d8e4f0",maxW:200});
   g.fillStyle="rgba(255,255,255,.07)";g.fillRect(1460,y+8,180,8);g.fillStyle=l.meta.color;g.fillRect(1460,y+8,180*l.total/topTotal,8);
   text(g,overview.summary.configured?compact(l.total):"—",1770,y+19,{size:21,font:MONO,color:"#c8d6e6",align:"right"});
   text(g,l.calls24?fmt(l.calls24):"—",1880,y+19,{size:21,font:MONO,color:"#8495ab",align:"right"});
   text(g,l.calls24?rate(l.ok24/l.calls24*100):"—",tr,y+19,{size:21,font:MONO,weight:700,color:STATUS_COLOR[l.status],align:"right"});
  });
  const s=overview.summary;
  text(g,"全线 24H 成功率 "+rate(s.uptime_percent)+"   ·   在线站点 "+overview.models.filter(m=>m.status==="operational").length+" / "+overview.models.length+"   ·   更新 "+clockTime(overview.updated_at),tx,606,{size:21,font:MONO,color:"#7d8ea3",maxW:tr-tx});
  scanlines(g,w,h,.12,3);
 });
 const dispatchMesh=new THREE.Mesh(new THREE.PlaneGeometry(DISPATCH_W,DISPATCH_H),screenMat(dispatch));
 dispatchMesh.rotation.y=Math.PI/2;scene.add(dispatchMesh);
 pickable(dispatchMesh,{id:"ops",screen:dispatch,tip:()=>view==="ops"?null:"调度屏 · 点击查看",action:()=>setView("ops")});
 const placeDispatch=()=>dispatchMesh.position.copy(P(WALL_L+.03,.55+DISPATCH_H/2,dispatchD0()+DISPATCH_W/2));

 // Ticket kiosk standing on the platform.
 const TICKETS={
  openai:{label:"OpenAI",line:"GPT / 兼容格式",note:"Base URL · OpenAI SDK 兼容",url:()=>location.origin+"/v1"},
  anthropic:{label:"Anthropic",line:"Claude 格式",note:"Base URL · Anthropic SDK",url:()=>location.origin},
  gemini:{label:"Gemini",line:"Gemini 格式",note:"Base URL · Gemini (v1beta)",url:()=>location.origin},
 };
 let printedAt=-99, printed=null;
 const kioskScreen=makeScreen(512,352,(g,w,h,scr)=>{
  const bg=g.createLinearGradient(0,0,0,h);bg.addColorStop(0,"#16081a");bg.addColorStop(1,"#05070c");g.fillStyle=bg;g.fillRect(0,0,w,h);
  text(g,"ACCESS PASS · 接入通行证",24,40,{size:22,font:DISPLAY,weight:700,color:"#00e5ff",glow:8});
  Object.entries(TICKETS).forEach(([key,t],i)=>{
   const x=24+i*158,on=ticketType===key,hot=isHover(scr,"t:"+key);
   g.fillStyle=on?"rgba(255,60,172,.35)":hot?"rgba(255,255,255,.1)":"rgba(255,255,255,.04)";rrect(g,x,60,148,44,8);g.fill();
   g.strokeStyle=on?"#ff3cac":"rgba(255,255,255,.18)";g.lineWidth=2;g.stroke();
   text(g,t.label,x+74,90,{size:20,weight:600,color:on?"#fff":"#9aa6b8",align:"center"});
   scr.region("t:"+key,x,60,148,44,"切换为 "+t.label+" 接入格式",()=>{ticketType=key;kioskScreen.dirty=true;});
  });
  const t=TICKETS[ticketType];
  text(g,"ENDPOINT",24,138,{size:15,font:DISPLAY,color:"#6f7f93"});
  text(g,t.url(),24,166,{size:21,font:MONO,weight:600,color:"#e6f4ff",maxW:464});
  text(g,t.note,24,194,{size:16,color:"#8495ab"});
  const hot=isHover(scr,"buy"),gr=g.createLinearGradient(0,214,0,298);gr.addColorStop(0,hot?"#ff62c0":"#ff3cac");gr.addColorStop(1,hot?"#8a76ff":"#6a5cff");
  g.fillStyle=gr;g.shadowColor="#ff3cac";g.shadowBlur=hot?26:14;rrect(g,24,214,464,84,12);g.fill();g.shadowBlur=0;
  text(g,"取票 · 复制接入地址",256,266,{size:30,weight:700,color:"#fff",align:"center"});
  scr.region("buy",24,214,464,84,"复制 "+t.label+" 接入地址",issueTicket);
  const recent=clock-printedAt<4;
  text(g,recent?"STATUS · COPIED ✓":"STATUS · READY",24,334,{size:17,font:MONO,weight:600,color:recent?"#36f18b":"#56657a"});
  scanlines(g,w,h,.18,3);
 });
 const ticketScreen=makeScreen(256,330,(g,w,h)=>{
  g.fillStyle="#eef7fc";g.fillRect(0,0,w,h);speckle(g,w,h,600,.05);
  const t=printed||{...TICKETS.openai,at:""};
  text(g,"TOKEN METRO",18,40,{size:20,font:DISPLAY,weight:800,color:"#0b1018"});text(g,"PASS",w-18,40,{size:18,font:DISPLAY,weight:800,color:"#ff3cac",align:"right"});
  g.strokeStyle="#0b101855";g.setLineDash([5,5]);g.beginPath();g.moveTo(14,56);g.lineTo(w-14,56);g.stroke();g.setLineDash([]);
  [["LINE",t.line],["ENDPOINT",t.url()],["ISSUED",t.at],["STATUS","COPIED ✓"]].forEach(([k,v],i)=>{
   text(g,k,18,92+i*56,{size:13,font:MONO,color:"#5a6a7c"});
   text(g,v,18,114+i*56,{size:16,font:MONO,weight:700,color:k==="STATUS"?"#0a8a4a":"#0b1018",maxW:w-36});
  });
  g.fillStyle="#0b1018";for(let x=18;x<w-18;x+=rnd(3,7))g.fillRect(x,h-38,rnd(1,3),24);
 });
 const kioskGroup=new THREE.Group();scene.add(kioskGroup);
 let ticketMesh, gateLight;
 {
  const {x,d,w,h,depth}=KIOSK;
  const body=new THREE.Mesh(new THREE.BoxGeometry(w,h,depth),new THREE.MeshStandardMaterial({color:0x1a1f28,roughness:.35,metalness:.8}));
  body.position.copy(P(x,h/2,d+depth/2));kioskGroup.add(body);
  pickable(body,{id:"kiosk",tip:()=>"售票机 · 点击使用",action:()=>setView("kiosk")});
  const face=new THREE.Mesh(new THREE.PlaneGeometry(1.5,1.03),screenMat(kioskScreen));face.position.copy(P(x,1.52,d-.006));kioskGroup.add(face);
  pickable(face,{id:"kiosk",screen:kioskScreen,tip:()=>"售票机 · 点击使用",action:()=>setView("kiosk")});
  const sign=new THREE.Mesh(new THREE.PlaneGeometry(1.5,.36),neonSign(["TICKETS · 取票"],{color:"#ff3cac"}));sign.position.copy(P(x,h+.24,d+.2));kioskGroup.add(sign);
  const slot=new THREE.Mesh(new THREE.BoxGeometry(.8,.05,.06),new THREE.MeshBasicMaterial({color:0x000000}));slot.position.copy(P(x,.92,d-.01));kioskGroup.add(slot);
  gateLight=new THREE.Mesh(new THREE.SphereGeometry(.035,12,12),new THREE.MeshBasicMaterial({color:0xff5a5f}));gateLight.position.copy(P(x+.65,.92,d-.02));kioskGroup.add(gateLight);
  const strip=new THREE.Mesh(new THREE.BoxGeometry(w,.02,.02),emissive(0xff3cac,1.1));strip.position.copy(P(x,.04,d-.01));kioskGroup.add(strip);
  ticketMesh=new THREE.Mesh(new THREE.PlaneGeometry(.62,.8),new THREE.MeshBasicMaterial({map:ticketScreen.tex,transparent:true,color:new THREE.Color(.62,.62,.62)}));ticketMesh.visible=false;kioskGroup.add(ticketMesh);
 }

 // Holographic panel that opens beside a station or line on the route wall.
 const holo=makeScreen(1024,724,(g,w,h,scr)=>{
  const line=selectedModel?selectedModel.line:selectedLine;if(!line)return;
  g.fillStyle="#050e1a";rrect(g,4,4,w-8,h-8,22);g.fill();
  g.strokeStyle="rgba(0,229,255,.7)";g.lineWidth=3;g.shadowColor="#00e5ff";g.shadowBlur=20;g.stroke();g.shadowBlur=0;
  g.strokeStyle="#00e5ff";g.lineWidth=6;for(const [x,y,dx,dy] of [[14,14,1,1],[w-14,14,-1,1],[14,h-14,1,-1],[w-14,h-14,-1,-1]]){g.beginPath();g.moveTo(x+dx*40,y);g.lineTo(x,y);g.lineTo(x,y+dy*40);g.stroke();}
  const hotClose=isHover(scr,"close");g.fillStyle=hotClose?"rgba(255,255,255,.14)":"rgba(255,255,255,.05)";rrect(g,w-86,30,56,56,10);g.fill();text(g,"×",w-58,72,{size:40,color:"#c8d6e6",align:"center"});
  scr.region("close",w-86,30,56,56,"关闭",()=>closeSelection());
  const c=line.meta.color,configured=overview?.summary.configured;
  g.fillStyle=c;rrect(g,40,40,76,40,7);g.fill();text(g,line.meta.code,78,68,{size:20,font:DISPLAY,weight:800,color:"#05080e",align:"center"});
  const stats=(rows)=>rows.forEach(([k,v],i)=>{const x=40+i*236;g.fillStyle="rgba(255,255,255,.05)";rrect(g,x,226,220,104,10);g.fill();text(g,k,x+16,260,{size:19,color:"#7d8ea3"});text(g,v,x+16,308,{size:34,font:MONO,weight:700,color:"#e6f4ff",maxW:192});});
  if(selectedModel){
   const m=selectedModel;
   text(g,m.code+" · "+line.meta.name.toUpperCase(),132,68,{size:22,font:DISPLAY,color:"#8fa3b8"});
   text(g,m.model,40,150,{size:52,font:MONO,weight:700,color:"#f4fbff",maxW:w-80,glow:10,glowColor:"#00e5ff"});
   g.fillStyle=STATUS_COLOR[m.status];g.beginPath();g.arc(50,186,8,0,Math.PI*2);g.fill();
   text(g,STATUS_TEXT[m.status]+" · "+relativeTime(m.last_call),68,194,{size:24,color:"#b5c4d6"});
   stats([["24H 成功率",rate(m.uptime_percent)],["24H 调用",fmt(m.calls24)],["累计 TOKEN",configured?compact(m.total):"—"],["累计调用",configured?fmt(m.calls):"—"]]);
   text(g,"24 小时信号记录",40,380,{size:20,font:DISPLAY,color:"#7d8ea3"});
   const bw=(w-80)/m.history.length;m.history.forEach((b,i)=>{g.fillStyle=bucketColor(b);const bh=b.calls?52:14;g.fillRect(40+i*bw+2,452-bh,bw-4,bh);});
   text(g,"24 小时前",40,482,{size:16,font:MONO,color:"#56657a"});text(g,"现在",w-40,482,{size:16,font:MONO,color:"#56657a",align:"right"});
   [["copy-model","复制模型名",()=>copy(m.model,"模型名")],["copy-endpoint","复制接入地址",()=>copy(location.origin+"/v1","OpenAI 接入地址")]].forEach(([id,label,action],i)=>{
    const x=40+i*482,hot=isHover(scr,id);
    g.fillStyle=hot?"rgba(0,229,255,.26)":"rgba(0,229,255,.1)";rrect(g,x,540,462,96,12);g.fill();g.strokeStyle="rgba(0,229,255,.6)";g.lineWidth=2;g.stroke();
    text(g,label,x+231,600,{size:30,weight:600,color:"#e6f4ff",align:"center"});
    scr.region(id,x,540,462,96,label,action);
   });
   text(g,"Esc 返回 · ← → 切换站点 · C 复制模型名",40,690,{size:18,color:"#56657a"});
  } else {
   const l=selectedLine;
   text(g,l.family,132,68,{size:22,font:DISPLAY,color:"#8fa3b8"});
   text(g,l.meta.name,40,150,{size:56,font:DISPLAY,weight:800,color:c,glow:16});
   g.fillStyle=STATUS_COLOR[l.status];g.beginPath();g.arc(50,186,8,0,Math.PI*2);g.fill();
   text(g,STATUS_TEXT[l.status],68,194,{size:24,color:"#b5c4d6"});
   stats([["站点",String(l.models.length)],["24H 调用",fmt(l.calls24)],["24H 成功率",l.calls24?rate(l.ok24/l.calls24*100):"—"],["累计 TOKEN",configured?compact(l.total):"—"]]);
   l.models.slice(0,6).forEach((m,i)=>{
    const y=356+i*56,hot=isHover(scr,"row:"+m.model);
    g.fillStyle=hot?"rgba(255,255,255,.1)":"rgba(255,255,255,.035)";rrect(g,40,y,w-80,48,8);g.fill();
    g.fillStyle=STATUS_COLOR[m.status];g.beginPath();g.arc(66,y+24,7,0,Math.PI*2);g.fill();
    text(g,m.model,88,y+32,{size:22,font:MONO,color:"#e6f4ff",maxW:w-320});
    text(g,rate(m.uptime_percent),w-60,y+32,{size:22,font:MONO,color:"#8495ab",align:"right"});
    scr.region("row:"+m.model,40,y,w-80,48,"查看 "+m.model,()=>openStation(m.model));
   });
   if(l.models.length>6)text(g,"另有 "+(l.models.length-6)+" 个站点",40,712,{size:18,color:"#56657a"});
  }
  scanlines(g,w,h,.1,4);
 },{transparent:true});
 const holoMesh=new THREE.Mesh(new THREE.PlaneGeometry(4.6,3.25),new THREE.MeshBasicMaterial({map:holo.tex,alphaTest:.5,color:new THREE.Color(1.3,1.3,1.3)}));
 holoMesh.rotation.y=Math.PI/2;holoMesh.visible=false;scene.add(holoMesh);
 pickable(holoMesh,{id:"holo",screen:holo,tip:()=>null,action:()=>{}});
 const holoLink=new THREE.Line(new THREE.BufferGeometry().setFromPoints([new THREE.Vector3(),new THREE.Vector3()]),new THREE.LineBasicMaterial({color:new THREE.Color(0x00e5ff)}));
 holoLink.visible=false;scene.add(holoLink);
 function holoLayout() {
  const anchor=selectedModel||selectedLine?.models.at(-1);if(!anchor)return null;
  const w=4.6,h=3.25,d0=wd(anchor.d)+.6,y1=clamp(anchor.y+h/2,MAP_Y0+h,MAP_Y1+.2);
  return {anchor,d0,d1:d0+w,y0:y1-h,y1,x:WALL_L+.8};
 }
 function placeHolo() {
  const L=holoLayout();holoMesh.visible=!!L;holoLink.visible=!!L;if(!L)return;
  holoMesh.position.copy(P(L.x,(L.y0+L.y1)/2,(L.d0+L.d1)/2));
  holoLink.geometry.setFromPoints([P(WALL_L+.05,L.anchor.y,wd(L.anchor.d)),P(L.x,L.anchor.y,L.d0)]);
  holo.dirty=true;
 }

 // Light boxes on the far wall.
 const adScreens=[0,1,2].map(i=>makeScreen(1024,346,(g,w,h)=>{
  const top=[...lines].sort((a,b)=>b.total-a.total)[0],s=overview?.summary;
  const ad=[
   {color:top?.meta.color||"#00e5ff",kicker:"HOT LINE · 热门线路",title:top?top.meta.name.toUpperCase():"TOKEN METRO",sub:top&&s?.configured?compact(top.total)+" TOKENS CARRIED":"ALL MODELS IN SERVICE"},
   {color:"#ff3cac",kicker:"ONE API · ALL LINES",title:"/v1 · /v1beta",sub:"OPENAI · ANTHROPIC · GEMINI"},
   {color:"#8c6bff",kicker:"NETWORK STATUS · 网络状态",title:(overview?overview.models.filter(m=>m.status==="operational").length:0)+" / "+(overview?.models.length||0)+" STATIONS",sub:"24H UPTIME "+(s?rate(s.uptime_percent):"—")},
  ][i];
  const bg=g.createLinearGradient(0,0,w,h);bg.addColorStop(0,"#0b0f1a");bg.addColorStop(1,"#14081c");g.fillStyle=bg;g.fillRect(0,0,w,h);
  g.globalAlpha=.25;const glow=g.createRadialGradient(w*.8,h*.3,0,w*.8,h*.3,w*.6);glow.addColorStop(0,ad.color);glow.addColorStop(1,"transparent");g.fillStyle=glow;g.fillRect(0,0,w,h);g.globalAlpha=1;
  g.strokeStyle=ad.color;g.lineWidth=6;g.shadowColor=ad.color;g.shadowBlur=24;g.strokeRect(8,8,w-16,h-16);g.shadowBlur=0;
  text(g,ad.kicker,48,80,{size:30,font:DISPLAY,weight:600,color:ad.color,glow:10});
  text(g,ad.title,48,190,{size:76,font:DISPLAY,weight:800,color:"#f2fbff",maxW:w-96,glow:18,glowColor:ad.color});
  text(g,ad.sub,48,270,{size:30,font:MONO,color:"#c3d2e2",maxW:w-96});
  scanlines(g,w,h,.15,3);
 }));
 adScreens.forEach((s,i)=>plane(8,2.7,screenMat(s),P(WALL_R-.04,2.05,17.5+i*16),-Math.PI/2));

 /* ---------- Trains ---------- */
 const trainTextures=new Map();
 function trainTex(line,kind) {
  const key=(line?.family||"none")+kind;if(trainTextures.has(key))return trainTextures.get(key);
  const color=line?.meta.color||"#7d8799";
  const [c,g]=makeCanvas(2048,256),[e,eg]=makeCanvas(2048,256);
  g.fillStyle="#1d222b";g.fillRect(0,0,2048,256);speckle(g,2048,256,9000,.08);streaks(g,2048,256,40,.3,40);
  eg.fillStyle="#000";eg.fillRect(0,0,2048,256);
  for(let car=0;car<3;car++){
   const x0=car*2048/3;
   g.fillStyle="#0b0d11";g.fillRect(x0,0,5,256);
   for(let wi=0;wi<4;wi++){
    const wx=x0+70+wi*150,ww=110,people=[0,1,2].filter(()=>Math.random()>.35).map(()=>({px:wx+rnd(10,ww-24),ph:rnd(34,60),pw:rnd(16,24)}));
    for(const [ctx,lit] of [[g,false],[eg,true]]){
     const gr=ctx.createLinearGradient(0,70,0,150);gr.addColorStop(0,lit?"#ffe6c0":"#e9d3b4");gr.addColorStop(1,lit?"#ffb878":"#c69a72");ctx.fillStyle=gr;rrect(ctx,wx,70,ww,80,10);ctx.fill();
     // Passengers silhouetted against the cabin light.
     ctx.fillStyle="rgba(10,8,8,.82)";for(const p of people){rrect(ctx,p.px,150-p.ph,p.pw,p.ph,8);ctx.fill();ctx.beginPath();ctx.arc(p.px+p.pw/2,150-p.ph-6,9,0,Math.PI*2);ctx.fill();}
    }
   }
   for(const dx of [620,640]){g.strokeStyle="#0b0d11";g.lineWidth=3;g.strokeRect(x0+dx-30,40,60,200);}
   text(g,line?line.meta.code+" "+(car+1):"",x0+20,40,{size:22,font:DISPLAY,weight:700,color:"#8b97a8"});
  }
  for(const ctx of [g,eg]){ctx.fillStyle=color;ctx.fillRect(0,186,2048,14);ctx.fillRect(0,26,2048,4);}
  text(g,"TOKEN METRO",1500,238,{size:24,font:DISPLAY,weight:800,color:"#9aa6b8"});
  const [f,fg]=makeCanvas(512,700),[fe,feg]=makeCanvas(512,700);
  fg.fillStyle="#20262f";fg.fillRect(0,0,512,700);speckle(fg,512,700,3000,.08);
  feg.fillStyle="#000";feg.fillRect(0,0,512,700);
  {const gr=fg.createLinearGradient(0,170,0,420);gr.addColorStop(0,"#1c3247");gr.addColorStop(1,"#05080d");fg.fillStyle=gr;rrect(fg,48,170,416,250,22);fg.fill();}
  for(const ctx of [fg,feg]){
   ctx.fillStyle="#05070b";ctx.fillRect(70,60,372,80);
   text(ctx,line?line.meta.name.toUpperCase():"OUT OF SERVICE",256,112,{size:40,font:DISPLAY,weight:700,color:"#ffb547",align:"center",maxW:350,glow:10,glowColor:"#ff8a00"});
   ctx.fillStyle=color;ctx.fillRect(0,470,512,18);
   // Head lamps on the local service; tail lamps on expresses seen from behind.
   const lamp=kind==="express"?"#ff3b3b":"#f4fbff";
   for(const x of [100,412]){ctx.fillStyle=lamp;ctx.shadowColor=lamp;ctx.shadowBlur=30;ctx.beginPath();ctx.arc(x,560,34,0,Math.PI*2);ctx.fill();ctx.shadowBlur=0;}
  }
  const result={side:texFrom(c),sideE:texFrom(e),front:texFrom(f),frontE:texFrom(fe)};
  trainTextures.set(key,result);return result;
 }
 function makeTrain(kind,extra={}) {
  const len=kind==="local"?33:30,group=new THREE.Group();
  const body=new THREE.Mesh(new THREE.BoxGeometry(2.6,3.5,len),new THREE.MeshStandardMaterial({color:0x1b2029,roughness:.32,metalness:.85}));body.position.set(0,.78,-len/2);group.add(body);
  const sideMat=new THREE.MeshStandardMaterial({roughness:.35,metalness:.55,emissive:0xffffff,emissiveIntensity:1.4});
  const side=new THREE.Mesh(new THREE.PlaneGeometry(len,3.3),sideMat);side.rotation.y=-Math.PI/2;side.position.set(-1.31,.82,-len/2);group.add(side);
  const frontMat=new THREE.MeshStandardMaterial({roughness:.3,metalness:.6,emissive:0xffffff,emissiveIntensity:.9});
  const front=new THREE.Mesh(new THREE.PlaneGeometry(2.6,3.5),frontMat);front.position.set(0,.78,.01);group.add(front);
  let trail=null;
  if(kind==="express"){
   const [c,g]=makeCanvas(256,8),gr=g.createLinearGradient(0,0,256,0);gr.addColorStop(0,"rgba(255,255,255,1)");gr.addColorStop(1,"rgba(255,255,255,0)");g.fillStyle=gr;g.fillRect(0,0,256,8);
   const trailMat=new THREE.MeshBasicMaterial({map:texFrom(c),transparent:true,blending:THREE.AdditiveBlending,depthWrite:false,side:THREE.DoubleSide});
   trail=new THREE.Group();trail.material=trailMat;
   for(const [x,y] of [[-1.2,.25],[1.2,.25],[0,2.4]]){const t=new THREE.Mesh(new THREE.PlaneGeometry(22,.22),trailMat);t.rotation.y=Math.PI/2;t.position.set(x,y,11);trail.add(t);}
   group.add(trail);
  }
  group.visible=false;scene.add(group);
  const train=Object.assign({kind,len,group,sideMat,frontMat,trail,line:undefined,d:999,v:0},extra);
  for(const mesh of [body,side,front])pickable(mesh,{id:"train",tip:()=>train.line?train.line.meta.name+" · 点击查看线路":null,action:()=>train.line&&openLine(train.line)});
  return train;
 }
 function dressTrain(t,line) {
  if(t.line===line)return;t.line=line;
  const tex=trainTex(line,t.kind);
  t.sideMat.map=tex.side;t.sideMat.emissiveMap=tex.sideE;t.frontMat.map=tex.front;t.frontMat.emissiveMap=tex.frontE;t.sideMat.needsUpdate=t.frontMat.needsUpdate=true;
  if(t.trail)t.trail.material.color.set(line?.meta.color||"#7d8799").multiplyScalar(.9);
 }
 const local=makeTrain("local",{state:"away",timer:3,t:0,T:9,stopD:-8.5,from:230,cursor:0});
 const expresses=[makeTrain("express"),makeTrain("express"),makeTrain("express")];
 let expressTimer=2.5;
 function pickLine(byTraffic) {
  if(!lines.length)return null;
  const pool=byTraffic?lines.filter(l=>l.calls24>0):lines;
  if(!pool.length)return lines[Math.floor(Math.random()*lines.length)];
  let r=Math.random()*pool.reduce((s,l)=>s+l.calls24,0);
  for(const l of pool){r-=l.calls24;if(r<=0)return l;}
  return pool[0];
 }
 const expressInterval=()=>lines.length?clamp(16/(1+Math.log10(1+calls24)),3.2,16):22;
 function spawnExpress(line=pickLine(true)) { const t=expresses.find(e=>!e.group.visible);if(!t)return;dressTrain(t,line);t.d=-75;t.v=50+rnd(0,12);t.group.visible=true; }
 function nextLocalLine() { if(!lines.length)return null;const active=lines.filter(l=>l.status!=="idle"),pool=active.length?active:lines;local.cursor=(local.cursor+1)%pool.length;return pool[local.cursor]; }
 function nextTrainInfo() {
  if(!lines.length)return {name:"Out of service",eta:"--",color:"#56657a"};
  const label={arriving:"进站",dwell:"停靠",departing:"发车"}[local.state];
  if(label)return {name:local.line?.meta.name||"Out of service",eta:label,color:local.line?.meta.color||"#56657a"};
  const pool=lines.filter(l=>l.status!=="idle"),p=pool.length?pool:lines,next=p[(local.cursor+1)%p.length];
  return {name:next.meta.name,eta:Math.ceil(Math.max(0,local.timer)+(next.status==="incident"?12.5:9))+"s",color:next.meta.color};
 }
 // With motion off, a train waits at platform 1 so the still frame still shows a line.
 function parkLocal() { if(motionOn()||!lines.length||local.state==="dwell")return;dressTrain(local,local.line||lines[0]);local.state="dwell";local.d=local.stopD;local.t=0; }

 /* ---------- Particles: dust, drips, ripples, steam, energy pulses ---------- */
 const dotTex=(()=>{const [c,g]=makeCanvas(32,32),gr=g.createRadialGradient(16,16,0,16,16,16);gr.addColorStop(0,"rgba(255,255,255,1)");gr.addColorStop(1,"rgba(255,255,255,0)");g.fillStyle=gr;g.fillRect(0,0,32,32);return texFrom(c);})();
 const dustCount=mobile?220:520,dustPos=new Float32Array(dustCount*3),dustSeed=new Float32Array(dustCount);
 for(let i=0;i<dustCount;i++){dustPos[i*3]=rnd(WALL_L,WALL_R);dustPos[i*3+1]=rnd(0,CEIL);dustPos[i*3+2]=-rnd(-6,END);dustSeed[i]=rnd(0,6);}
 const dustGeo=new THREE.BufferGeometry();dustGeo.setAttribute("position",new THREE.BufferAttribute(dustPos,3));
 scene.add(new THREE.Points(dustGeo,new THREE.PointsMaterial({size:.045,map:dotTex,transparent:true,depthWrite:false,blending:THREE.AdditiveBlending,color:0x9fc9ff,opacity:.28})));
 const leaks=[[-5.3,9],[-2.7,20],[-4.6,34],[-1.5,46],[-3.4,4]].map(([x,d])=>{const m=new THREE.Mesh(new THREE.BoxGeometry(.012,.16,.012),emissive(0xbff6ff,.9));m.visible=false;scene.add(m);return {x,d,y:CEIL,vy:0,falling:false,wait:rnd(0,3),mesh:m};});
 const ripples=Array.from({length:8},()=>{const m=new THREE.Mesh(new THREE.RingGeometry(.9,1,40),new THREE.MeshBasicMaterial({color:0xbff6ff,transparent:true,opacity:0,blending:THREE.AdditiveBlending,depthWrite:false}));m.rotation.x=-Math.PI/2;m.visible=false;scene.add(m);return {mesh:m,age:9};});
 const smokeTex=(()=>{const [c,g]=makeCanvas(128,128);for(let i=0;i<12;i++){const x=rnd(30,98),y=rnd(30,98),r=rnd(20,45),gr=g.createRadialGradient(x,y,0,x,y,r);gr.addColorStop(0,"rgba(255,255,255,.35)");gr.addColorStop(1,"rgba(255,255,255,0)");g.fillStyle=gr;g.fillRect(0,0,128,128);}return texFrom(c);})();
 const steam=Array.from({length:mobile?6:12},(_,i)=>{const s=new THREE.Sprite(new THREE.SpriteMaterial({map:smokeTex,transparent:true,depthWrite:false,opacity:0,color:0x9fb6cc}));scene.add(s);return {s,age:rnd(0,6),life:rnd(5,8),x:rnd(EDGE+.4,EDGE+1.6),d:i<4?rnd(8,14):rnd(24,52)};});
 const pulses=new THREE.InstancedMesh(new THREE.BoxGeometry(.04,.04,1.3),emissive(0x00e5ff,1.5),40);scene.add(pulses);
 const railPulses=new THREE.InstancedMesh(new THREE.BoxGeometry(.05,.02,2),emissive(0x00e5ff,1.1),30);scene.add(railPulses);

 /* ---------- Camera rig and views ---------- */
 const camPos=new THREE.Vector3(),camTarget=new THREE.Vector3();let camFov=55;
 const look={dragYaw:0,dragPitch:0,px:0,py:0,tx:0,ty:0};
 const narrow=()=>innerWidth<820, tanHalf=Math.tan(27.5*Math.PI/180);
 function fitWall(d0,d1,y0,y1,front=0) {
  const F=innerHeight/2/tanHalf,uw=innerWidth*.94,uh=Math.max(200,innerHeight-150);
  const need=Math.max((d1-d0)/2/(uw/2/F),(y1-y0)/2/(uh/2/F))*1.08,dist=clamp(need,2.2,7.4),zoom=clamp(dist/need,.42,1);
  const dc=(d0+d1)/2,yc=(y0+y1)/2;
  return {pos:P(WALL_L+front+dist,yc+.05,dc),target:P(WALL_L,yc,dc),fov:2*Math.atan(tanHalf/zoom)*180/Math.PI};
 }
 function fitFront(xc,yc,d,w,h) {
  const F=innerHeight/2/tanHalf,need=Math.max(w/2/(innerWidth*.92/2/F),h/2/(Math.max(200,innerHeight-150)/2/F))*1.05;
  return {pos:P(xc+.35,yc-.12,d-need),target:P(xc,yc,d),fov:55};
 }
 function viewGoal() {
  if(view==="lines"&&(selectedModel||selectedLine)){
   const L=holoLayout();
   return fitWall(Math.min(wd(L.anchor.d)-.6,L.d0),L.d1+.2,Math.min(L.y0,L.anchor.y-.3),Math.max(L.y1,L.anchor.y+.3),L.x-WALL_L);
  }
  if(view==="lines")return fitWall(wd(MAP_D0),wd(mapEnd),MAP_Y0,MAP_Y1);
  if(view==="ops")return fitWall(dispatchD0(),dispatchD0()+DISPATCH_W,.55,.55+DISPATCH_H);
  if(view==="kiosk")return fitFront(KIOSK.x,1.45,KIOSK.d,2.3,2.8);
  if(view==="board")return fitFront(BOARD.x,BOARD.y-.15,BOARD.d,BOARD.w+.4,BOARD.h+.7);
  // Portrait screens keep a usable horizontal field of view instead of a sliver of the platform.
  const aspect=innerWidth/innerHeight,fov=aspect<1?clamp(2*Math.atan(Math.tan(36*Math.PI/180)/aspect)*180/Math.PI,55,100):55;
  return narrow()?{pos:P(-1.2,2.1,-6),target:P(1.4,2.3,30),fov}:{pos:P(-1.4,1.75,-4.5),target:P(3.2,2.1,30),fov};
 }
 const introFrom={pos:P(4.2,4.3,-18),target:P(3.8,1.3,90),fov:55};

 /* ---------- Motion, intro, loop ---------- */
 const reduced=matchMedia("(prefers-reduced-motion: reduce)");
 let motionEnabled=!reduced.matches, clock=0, introT=99, introActive=false, introExpress=false, raf=0, last=0, slow=0, watchdog=0, dragging=false, downAt=null;
 const motionOn=()=>motionEnabled&&!reduced.matches;
 function finishIntro() {
  if(!introActive&&!document.documentElement.classList.contains("intro-active"))return;
  introActive=false;introT=99;document.documentElement.classList.remove("intro-active");
  try{sessionStorage.setItem("metro-intro-seen","1");}catch{}
  document.removeEventListener("keydown",skipIntro);document.removeEventListener("pointerdown",skipIntro,true);
 }
 function skipIntro(event) { if(event.type==="pointerdown"||["Escape","Enter"," ","Tab"].includes(event.key))finishIntro(); }
 if(document.documentElement.classList.contains("intro-active")&&motionOn()){
  introActive=true;introT=0;
  document.addEventListener("keydown",skipIntro);document.addEventListener("pointerdown",skipIntro,true);
  setTimeout(finishIntro,9000);
 } else document.documentElement.classList.remove("intro-active");
 const power=d=>!introActive?1:clamp((introT-1.0-(d-START)*.02)*2.4,0,1);

 const tmpM=new THREE.Matrix4(),tmpC=new THREE.Color(),camDir=new THREE.Vector3();
 function update(dt) {
  clock+=dt;
  if(introActive){introT+=dt;if(!introExpress&&introT>1.4){introExpress=true;spawnExpress(pickLine(true));}if(introT>5.8)finishIntro();}
  switch(local.state){
   case "away": local.timer-=dt;if(local.timer<=0&&!introActive){dressTrain(local,nextLocalLine());local.state="arriving";local.t=0;local.T=local.line?.status==="incident"?12.5:9;}break;
   case "arriving": {local.t+=dt;const p=Math.min(1,local.t/local.T);local.d=local.stopD+(local.from-local.stopD)*(1-p)**2;if(p>=1){local.state="dwell";local.t=0;}break;}
   case "dwell": local.t+=dt;if(local.t>=6.5){local.state="departing";local.t=0;}break;
   case "departing": {local.t+=dt;const p=local.t/6;local.d=local.stopD-95*p*p;if(p>=1){local.state="away";local.d=999;local.timer=4+rnd(0,5);}break;}
  }
  if(!introActive){expressTimer-=dt;if(expressTimer<=0){spawnExpress();expressTimer=expressInterval()*rnd(.75,1.25);}}
  for(const t of expresses)if(t.group.visible){t.d+=t.v*dt;if(t.d>380)t.group.visible=false;}
  const pos=dustGeo.attributes.position.array;
  for(let i=0;i<dustCount;i++){pos[i*3+2]+=dt*.18;pos[i*3+1]+=Math.sin(clock*.3+dustSeed[i])*.0015;if(pos[i*3+2]>6)pos[i*3+2]=-END;}
  dustGeo.attributes.position.needsUpdate=true;
  for(const l of leaks){
   if(!l.falling){l.wait-=dt;if(l.wait<=0){l.falling=true;l.y=CEIL-.05;l.vy=0;}}
   else{l.vy+=9.8*dt;l.y-=l.vy*dt;if(l.y<=0){l.falling=false;l.wait=rnd(1.2,4.5);const r=ripples.find(r=>r.age>1.6);if(r){r.age=0;r.mesh.position.copy(P(l.x,.01,l.d));}}}
   l.mesh.visible=l.falling;l.mesh.position.copy(P(l.x,l.y,l.d));
  }
  for(const r of ripples){r.age+=dt;const p=r.age/1.6;r.mesh.visible=p<1;if(p<1){const s=.05+p*.6;r.mesh.scale.set(s,s,s);r.mesh.material.opacity=.5*(1-p);}}
  for(const s of steam){s.age+=dt;if(s.age>s.life){s.age=0;s.x=rnd(EDGE+.4,EDGE+1.6);}const p=s.age/s.life;s.s.position.copy(P(s.x+p*.3,BED+.2+p*3.2,s.d));const k=.8+p*3;s.s.scale.set(k,k,1);s.s.material.opacity=.11*Math.sin(p*Math.PI);}
 }
 function updateWorld() {
  for(const t of [local,...expresses]){
   const visible=t===local?local.d<500:t.group.visible;
   t.group.visible=visible;if(!visible)continue;
   t.group.position.copy(P(t===local?TA:TB,0,t.d));
   const flick=t.line?.status==="incident"?.6+.4*Math.abs(Math.sin(clock*9+t.d)):1;
   t.sideMat.emissiveIntensity=(t===local&&local.state==="dwell"?1.15:.8)*flick;
  }
  const localOn=local.d<500;
  headlight.intensity=localOn?55:0;
  if(localOn){headlight.position.copy(P(TA,-.2,local.d-.2));headlight.target.position.copy(P(TA,BED,local.d-26));}
  cabin.intensity=localOn&&local.state==="dwell"?3.5:0;cabin.position.copy(P(EDGE+.4,1.4,local.d+10));
  const lead=expresses.filter(t=>t.group.visible).sort((a,b)=>Math.abs(a.d-12)-Math.abs(b.d-12))[0];
  expressLight.intensity=lead?7:0;if(lead){expressLight.color.set(lead.line?.meta.color||"#ffffff");expressLight.position.copy(P(TB-1.6,1,lead.d+6));}
  // Fixtures power on along the platform during the intro; one tube misbehaves.
  for(const f of fixtures){
   let k=power(f.d);
   if(f.flicker&&motionOn())k*=Math.sin(clock*23)>.2||Math.sin(clock*1.7)>-.6?1:.12;
   fixtures.mesh.setColorAt(f.i,tmpC.setRGB(1.05*k,1.18*k,1.12*k));
  }
  fixtures.mesh.instanceColor.needsUpdate=true;
  for(const L of lights)L.l.intensity=L.base*power(L.d);
  // Energy pulses: cumulative tokens drive their density and speed.
  const e=energy(),n=Math.round(6+e*30),speed=6+e*16;
  for(let i=0;i<40;i++){if(i<n)tmpM.setPosition(P(9.6,5.01,END-((clock*speed+i*(70/n))%70)));else tmpM.setPosition(0,-99,0);pulses.setMatrixAt(i,tmpM);}
  pulses.instanceMatrix.needsUpdate=true;
  const rn=Math.round(4+e*24),rs=8+e*22;
  for(let i=0;i<30;i++){if(i<rn)tmpM.setPosition(P((i%2?TA:TB)+1.1,BED+.27,((clock*rs+i*(260/rn))%260)-10));else tmpM.setPosition(0,-99,0);railPulses.setMatrixAt(i,tmpM);}
  railPulses.instanceMatrix.needsUpdate=true;
  const sig=new THREE.Color(STATUS_COLOR[overallStatus()]).multiplyScalar(1.6);signalDots.forEach(s=>s.material.color.copy(sig));
  if(selectedLine&&view==="lines"){const span=selectedLine.dEnd-4.85,p=((clock-selectT)*.55)%1.25;pulse.visible=p<1;pulse.position.copy(P(WALL_L+.08,selectedLine.y,wd(4.85+span*Math.min(p,1))));}else pulse.visible=false;
  const holoAge=motionOn()?clamp((clock-selectT)/.45,0,1):1;
  // The panel unfolds from its centre line and hums faintly while open.
  holoMesh.scale.set(1,Math.max(.02,1-(1-holoAge)**3),1);
  holoMesh.material.color.setScalar(.95+.04*Math.sin(clock*40));
  // The hanging board would cut across the view when turned toward the wall.
  camera.getWorldDirection(camDir);boardGroup.visible=camDir.x>-.75;
  const show=clock-printedAt;
  ticketMesh.visible=show<4.2;
  if(ticketMesh.visible){const p=clamp(show/.7,0,1),grow=1-(1-p)**3;ticketMesh.scale.set(1,Math.max(.001,grow),1);ticketMesh.position.copy(P(KIOSK.x,.92-.4*grow,KIOSK.d-.03));ticketMesh.material.opacity=show>3.6?clamp((4.2-show)/.6,0,1):1;}
  gateLight.material.color.set(show<4.2?0x36f18b:0xff5a5f).multiplyScalar(1.6);
 }
 function stepCamera(dt,snap) {
  const g=viewGoal();
  if(introActive){
   const p=clamp((introT-.6)/4.8,0,1),e=p<.5?4*p*p*p:1-(-2*p+2)**3/2;
   camPos.lerpVectors(introFrom.pos,g.pos,e);camTarget.lerpVectors(introFrom.target,g.target,e);camFov=lerp(introFrom.fov,g.fov,e);
  } else {
   const k=snap?1:1-Math.exp(-dt*2.3);
   camPos.lerp(g.pos,k);camTarget.lerp(g.target,k);camFov+=(g.fov-camFov)*k;
  }
  const kp=snap?1:1-Math.exp(-dt*3);
  look.px+=(look.tx-look.px)*kp;look.py+=(look.ty-look.py)*kp;
  if(!dragging&&!snap){const decay=Math.exp(-dt*.35);look.dragYaw*=decay;look.dragPitch*=decay;}
  const breathe=motionOn()?Math.sin(clock*.35)*.006:0;
  camera.position.copy(camPos);camera.lookAt(camTarget);
  camera.rotateY(-look.px*.04-look.dragYaw+breathe);camera.rotateX(-look.py*.025+look.dragPitch+breathe*.4);
  if(Math.abs(camera.fov-camFov)>.01){camera.fov=camFov;camera.updateProjectionMatrix();}
 }
 let boardTick=0,tickerTick=0;
 function frame(ts,fromTimer=false) {
  raf=0;const now=ts/1000,dt=last?Math.min(.05,now-last):.016;last=now;
  const running=motionOn()&&!document.hidden;
  if(running)update(dt);
  updateWorld();stepCamera(dt,!running);
  boardTick-=dt;tickerTick-=dt;
  const flipping=[...flips.values()].some(f=>performance.now()-f.t<400);
  if(board.dirty||boardTick<=0||flipping){board.redraw();boardTick=.5;}
  if((running&&tickerTick<=0)||ticker.dirty){ticker.redraw();tickerTick=1/30;}
  if(clock-printedAt>4&&clock-printedAt<4.2)kioskScreen.dirty=true;
  for(const s of screens)if(s.dirty&&s!==board&&s!==ticker&&!(s===holo&&!holoMesh.visible))s.redraw();
  grade.uniforms.time.value=clock;
  composer.render(dt);
  if(running){
   if(fromTimer){}else if(dt>.032){if(++slow>90&&pixelRatio>.75){pixelRatio=Math.max(.75,pixelRatio-.25);slow=0;resize();}}else slow=Math.max(0,slow-1);
   requestFrame();
  } else last=0;
 }
 // Some embedded views pause requestAnimationFrame while still "visible"; a slow timer keeps the scene alive.
 function requestFrame() {
  if(raf)return;raf=requestAnimationFrame(frame);
  clearTimeout(watchdog);watchdog=setTimeout(()=>{if(raf){cancelAnimationFrame(raf);raf=0;frame(performance.now(),true);}},250);
 }
 function resize() {
  const w=innerWidth,h=innerHeight;
  renderer.setPixelRatio(pixelRatio);renderer.setSize(w,h,false);composer.setPixelRatio(pixelRatio);composer.setSize(w,h);
  grade.uniforms.res.value.set(w*pixelRatio,h*pixelRatio);
  mirror.getRenderTarget().setSize(Math.round(w*pixelRatio*mirrorScale),Math.round(h*pixelRatio*mirrorScale));
  camera.aspect=w/h;camera.updateProjectionMatrix();requestFrame();
 }
 function setMotion(on) {
  motionEnabled=on;
  document.documentElement.classList.toggle("motion-off",!motionOn());
  const b=$("motionToggle");b.setAttribute("aria-pressed",String(motionOn()));b.disabled=reduced.matches;
  b.textContent=reduced.matches?"已减少动态效果":motionOn()?"暂停动画":"播放动画";
  if(!motionOn()){finishIntro();parkLocal();}
  requestFrame();
 }

 /* ---------- Views, selection, actions ---------- */
 const HINTS={
  platform:"点击场景中的物体探索：左墙「线路图」· 远处「调度屏」· 头顶「到站屏」· 左前方「售票机」｜ 拖动环视",
  lines:"点击站点打开全息信息 · ← → 切换站点 · Esc 返回站台",
  ops:"点击屏幕上的 调用 / TOKEN 切换运行图 · Esc 返回站台",
  kiosk:"选择接入格式后点击「取票」· ← → 切换格式 · Enter 取票 · Esc 返回",
  board:"实时数据每 30 秒刷新 · Esc 返回站台",
 };
 function setView(next) {
  if(next!=="lines"){selectedModel=null;selectedLine=null;placeHolo();}
  view=next;document.body.dataset.view=next;
  $("backButton").hidden=next==="platform";
  $("sceneHint").textContent=narrow()&&next==="platform"?"点击场景中的物体探索 · 拖动环视":HINTS[next];
  if(routeScreen)routeScreen.dirty=true;requestFrame();
 }
 function openStation(name) {
  const m=modelIndex.get(name);if(!m)return;
  setView("lines");selectedModel=m;selectedLine=m.line;selectT=clock;placeHolo();routeScreen.dirty=true;
  announce(m.code+" "+m.model+"，"+STATUS_TEXT[m.status]+"，24 小时成功率 "+rate(m.uptime_percent)+"，24 小时调用 "+fmt(m.calls24)+" 次。按 C 复制模型名。");
 }
 function openLine(line) {
  if(!line)return;
  setView("lines");selectedModel=null;selectedLine=line;selectT=clock;placeHolo();routeScreen.dirty=true;
  announce(line.meta.name+"，"+STATUS_TEXT[line.status]+"，"+line.models.length+" 个站点。");
 }
 function closeSelection() { const had=!!(selectedModel||selectedLine);selectedModel=null;selectedLine=null;placeHolo();if(routeScreen)routeScreen.dirty=true;requestFrame();return had; }
 function relativeTime(value) {
  if(!value)return "尚无调用记录";
  const s=Math.max(0,(Date.now()-Date.parse(value))/1000);
  return "最近调用 "+(s<60?"刚刚":s<3600?Math.floor(s/60)+" 分钟前":s<86400?Math.floor(s/3600)+" 小时前":Math.floor(s/86400)+" 天前");
 }
 let toastTimer=0;
 function notify(msg) { const t=$("toast");t.textContent=msg;t.hidden=false;clearTimeout(toastTimer);toastTimer=setTimeout(()=>t.hidden=true,3200); }
 function announce(msg) { $("sceneSummary").dataset.focus=msg;renderSummary();$("announcer").textContent=msg; }
 let audio=null,soundOn=true;
 try{soundOn=localStorage.getItem("metro-sound")!=="off";}catch{}
 function chime() {
  if(!soundOn)return;
  try{audio=audio||new (window.AudioContext||window.webkitAudioContext)();const t=audio.currentTime;
   [[1320,0],[1760,.09]].forEach(([f,d])=>{const o=audio.createOscillator(),g=audio.createGain();o.type="sine";o.frequency.value=f;g.gain.setValueAtTime(0,t+d);g.gain.linearRampToValueAtTime(.05,t+d+.01);g.gain.exponentialRampToValueAtTime(.0001,t+d+.16);o.connect(g).connect(audio.destination);o.start(t+d);o.stop(t+d+.18);});}catch{}
 }
 function renderSound() { $("soundToggle").setAttribute("aria-pressed",String(soundOn));$("soundToggle").textContent=soundOn?"提示音 开":"提示音 关"; }
 async function copy(value,label) {
  try{await copyToClipboard(value);chime();notify(label+"已复制："+value);}
  catch{notify("复制失败，请手动复制："+value);}
 }
 async function issueTicket() {
  const t=TICKETS[ticketType],url=t.url();
  try{await copyToClipboard(url);}catch{notify("复制失败，请手动复制："+url);return;}
  chime();printed={...t,at:beijing({month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",second:"2-digit"})};
  ticketScreen.redraw();printedAt=clock;kioskScreen.dirty=true;
  if(!motionOn()){printedAt=clock-1;setTimeout(()=>{printedAt=-99;kioskScreen.dirty=true;requestFrame();},4000);}
  notify(t.label+" 接入地址已复制："+url);requestFrame();
 }

 /* ---------- Data ---------- */
 let broadcastText="";
 function renderBroadcast() {
  const msgs=[];
  if(!lines.length)msgs.push(overview.models_loading?"线路目录正在更新，请稍候":"线路筹备中，暂无列车运行");
  const incident=lines.filter(l=>l.status==="incident");
  incident.forEach(l=>{const bad=l.models.filter(m=>m.status==="incident").map(m=>m.model);msgs.push("MINOR DELAY ON "+l.meta.name.toUpperCase()+" · "+bad.slice(0,2).join("、")+(bad.length>2?" 等":"")+" 最近一次调用失败");});
  if(!incident.length&&lines.some(l=>l.status==="operational"))msgs.push("ALL LINES OPERATING NORMALLY · 全线运行正常");
  const busiest=[...lines].sort((a,b)=>b.calls24-a.calls24)[0];
  if(busiest?.calls24>0)msgs.push("HEAVY TRAFFIC ON "+busiest.meta.name.toUpperCase()+" · 近 24 小时 "+fmt(busiest.calls24)+" 次调用");
  const idle=lines.filter(l=>l.status==="idle");
  if(idle.length&&idle.length<lines.length)msgs.push(idle.map(l=>l.meta.name).join("、")+" 暂无近期列车");
  msgs.push("请站在黄色安全线以内候车 · MIND THE GAP BETWEEN THE PROMPT AND THE TOKEN");
  broadcastText=msgs.join("   ◆   ");
 }
 function renderSummary() {
  if(!overview){$("sceneSummary").textContent="正在读取线路信号。";return;}
  const s=overview.summary,op=overview.models.filter(m=>m.status==="operational").length;
  $("sceneSummary").textContent=[$("sceneSummary").dataset.focus||"",
   "在线站点 "+op+" / "+overview.models.length+"。",
   s.configured?"累计 Token "+fmt(tokens(s))+"，近 24 小时调用 "+fmt(calls24)+" 次，成功率 "+rate(s.uptime_percent)+"，剩余额度 "+(s.unlimited?"不限":money(s.remaining_usd))+"。":"尚未发布用量。",
   lines.map(l=>l.meta.name+" "+STATUS_TEXT[l.status]+"："+l.models.map(m=>m.model+" "+STATUS_TEXT[m.status]).join("，")).join("；")+"。",
   broadcastText].join(" ");
 }
 let refreshing=false;
 async function refresh() {
  if(refreshing)return;refreshing=true;
  const controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),10000);
  try{
   const response=await fetch("/api/public/overview?interval=hour",{credentials:"omit",cache:"no-store",signal:controller.signal});
   if(!response.ok)throw new Error("unavailable");
   overview=await response.json();
   const keepModel=selectedModel?.model,keepLine=selectedLine?.family;
   buildLines();
   selectedModel=keepModel?modelIndex.get(keepModel)||null:null;
   selectedLine=keepLine?lines.find(l=>l.family===keepLine)||null:null;
   rebuildRoute();placeDispatch();placeHolo();renderBroadcast();renderSummary();parkLocal();
   for(const s of screens)s.dirty=true;
  }catch{
   broadcastText=overview?"信号中断：更新失败，当前显示上次数据":"信号中断：暂时无法读取服务数据，稍后自动重试";
   ticker.dirty=true;renderSummary();
  }finally{clearTimeout(timeout);refreshing=false;requestFrame();}
 }

 /* ---------- Pointer and keyboard ---------- */
 const raycaster=new THREE.Raycaster(),ndc=new THREE.Vector2();
 function pick(x,y) {
  if(introActive)return null;
  ndc.set(x/innerWidth*2-1,-(y/innerHeight)*2+1);raycaster.setFromCamera(ndc,camera);
  const hit=raycaster.intersectObjects(pickables.filter(m=>m.visible&&m.parent.visible),false)[0];
  if(!hit)return null;
  const info=hit.object.userData.pick;let region=null;
  if(info.screen&&hit.uv){const px=hit.uv.x*info.screen.w,py=(1-hit.uv.y)*info.screen.h;region=[...info.screen.regions].reverse().find(r=>px>=r.x&&px<=r.x+r.w&&py>=r.y&&py<=r.y+r.h)||null;}
  return {obj:hit.object,info,region};
 }
 function setHover(h) {
  const key=h?(h.info.id+":"+(h.region?.id||"")):"";
  if(key===hover.key)return;
  const prev=hover.obj?.userData.pick?.screen;
  hover={key,obj:h?.obj||null,region:h?.region||null};
  if(prev)prev.dirty=true;if(h?.info.screen)h.info.screen.dirty=true;
  canvas.classList.toggle("is-pointing",!!(h&&(h.region||h.info.tip?.())));
  requestFrame();
 }
 canvas.addEventListener("pointerdown",e=>{downAt={x:e.clientX,y:e.clientY,yaw:look.dragYaw,pitch:look.dragPitch};dragging=false;});
 canvas.addEventListener("pointermove",e=>{
  if(e.pointerType==="mouse"){look.tx=(e.clientX/innerWidth-.5)*2;look.ty=(e.clientY/innerHeight-.5)*2;}
  if(downAt&&(e.buttons&1||e.pointerType!=="mouse")){
   const dx=e.clientX-downAt.x,dy=e.clientY-downAt.y;
   if(dragging||Math.hypot(dx,dy)>6){dragging=true;canvas.classList.add("is-dragging");look.dragYaw=clamp(downAt.yaw+dx*.0035,-.9,.9);look.dragPitch=clamp(downAt.pitch+dy*.0025,-.35,.35);$("sceneTip").hidden=true;requestFrame();return;}
  }
  const h=pick(e.clientX,e.clientY);setHover(h);
  const tip=$("sceneTip"),label=h?(h.region?.tip||h.info.tip?.()):null;
  if(label&&e.pointerType==="mouse"){tip.textContent=label;tip.style.left=e.clientX+"px";tip.style.top=e.clientY+"px";tip.hidden=false;}else tip.hidden=true;
  requestFrame();
 });
 addEventListener("pointerup",e=>{
  if(!downAt)return;
  const wasDrag=dragging;dragging=false;downAt=null;canvas.classList.remove("is-dragging");
  if(wasDrag||introActive||e.target!==canvas)return;
  const h=pick(e.clientX,e.clientY);
  if(h)(h.region?.action||h.info.action)?.();
  else if(!closeSelection()&&view!=="platform")setView("platform");
  requestFrame();
 });
 canvas.addEventListener("pointerleave",()=>{look.tx=0;look.ty=0;if(!downAt){setHover(null);$("sceneTip").hidden=true;}});
 document.addEventListener("keydown",e=>{
  if(e.altKey||e.ctrlKey||e.metaKey||introActive)return;
  if(e.target.closest?.("button,a")&&(e.key==="Enter"||e.key===" "))return;
  const views={"1":"platform","2":"lines","3":"ops","4":"kiosk","5":"board"};
  if(views[e.key]){setView(views[e.key]);announce(HINTS[views[e.key]]);return;}
  if(e.key==="Escape"){if(!closeSelection()&&view!=="platform")setView("platform");return;}
  if(view==="lines"&&(e.key==="ArrowRight"||e.key==="ArrowLeft")){
   const all=flatModels();if(!all.length)return;e.preventDefault();
   const i=selectedModel?all.indexOf(selectedModel):-1,next=all[(i+(e.key==="ArrowRight"?1:-1)+all.length)%all.length];openStation(next.model);return;
  }
  if(view==="kiosk"&&(e.key==="ArrowRight"||e.key==="ArrowLeft")){
   const keys=Object.keys(TICKETS),i=keys.indexOf(ticketType);ticketType=keys[(i+(e.key==="ArrowRight"?1:-1)+keys.length)%keys.length];kioskScreen.dirty=true;announce("接入格式："+TICKETS[ticketType].label);requestFrame();e.preventDefault();return;
  }
  if(view==="kiosk"&&e.key==="Enter"){issueTicket();e.preventDefault();return;}
  if(selectedModel&&(e.key==="c"||e.key==="C"))copy(selectedModel.model,"模型名");
 });
 $("backButton").addEventListener("click",()=>{closeSelection();setView("platform");});
 $("soundToggle").addEventListener("click",()=>{soundOn=!soundOn;try{localStorage.setItem("metro-sound",soundOn?"on":"off");}catch{}renderSound();});
 $("motionToggle").addEventListener("click",()=>setMotion(!motionEnabled));
 addEventListener("resize",resize,{passive:true});
 reduced.addEventListener("change",()=>setMotion(motionEnabled));
 document.addEventListener("visibilitychange",()=>{if(!document.hidden){refresh();requestFrame();}});
 setInterval(()=>{if(!document.hidden)refresh();},30000);
 document.fonts?.ready.then(()=>{for(const s of screens)s.dirty=true;requestFrame();});

 rebuildRoute();placeDispatch();
 const start=viewGoal();camPos.copy(start.pos);camTarget.copy(start.target);
 renderSound();setMotion(motionEnabled);setView("platform");renderSummary();resize();refresh();
}
function showFallback() { document.documentElement.classList.remove("intro-active");$("fallback").hidden=false; }
main().catch(error=>{console.error(error);showFallback();});

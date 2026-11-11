const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs"), path = require("node:path"), vm = require("node:vm");
const read = (file) => fs.readFileSync(path.resolve(__dirname, "..", file), "utf8");
function element() {
 const children = new Map(), classes = new Set();
 return {value:"", innerHTML:"", textContent:"", disabled:false, listeners:{}, dataset:{},
  classList:{add(n){classes.add(n)},remove(n){classes.delete(n)},contains(n){return classes.has(n)},toggle(n,on){if(on)classes.add(n);else classes.delete(n)}},
  addEventListener(n,fn){this.listeners[n]=fn},setAttribute(n,v){this[n]=v},querySelector(s){if(!children.has(s))children.set(s,element());return children.get(s)},focus(){},showModal(){this.open=true},close(){this.open=false}};
}
const model=(name,rank,score,site=false)=>({model:name,family:"Example",rank,score,site_available:site,site_models:site?[name]:[],votes:1200,rating_lower:score-5,rating_upper:score+5});
const data=(models,board="text")=>({models,board,boards:[{id:"text"},{id:"agent"}],board_label:board==="text"?"Text":"Agent",score_kind:board==="agent"?"ips":"arena",score_label:board==="agent"?"Agent IPS Score":"Arena Score",sample_label:"对战样本",published_at:"2026-11-09",fetched_at:"2026-11-10T04:00:00+08:00"});
async function page(models,customFetch){
 const nodes=new Map(),urls=[],copied=[];let fail=false;
 const get=id=>{if(!nodes.has(id))nodes.set(id,element());return nodes.get(id)};get("rankingBoard").value="text";get("rankingPageSize").value="25";
 const tabs=["text","vision","webdev","agent"].map(board=>{const el=element();el.dataset.board=board;return el});
 const context=vm.createContext({document:{getElementById:get,querySelectorAll:()=>tabs},window:{},console,AbortController,setTimeout,clearTimeout,copyToClipboard:async(value)=>copied.push(value),
  fetch:async(url,opts)=>{urls.push(url);if(fail)throw Error("offline");if(customFetch)return customFetch(url,opts);return{ok:true,json:async()=>data(models,new URL(url,"https://example.invalid").searchParams.get("board"))}}});
 vm.runInContext(read("cmd/apishare/web/public/rankings.js"),context);await new Promise(setImmediate);
 return{get,urls,copied,tabs,fail(){fail=true}};
}
test("shows ALL official models, preserves ties and original scores",async()=>{
 const p=await page([model("external winner",1,1525.95),model("site model",2,1470.25,true),model("tied",2,1469)]);
 const html=p.get("rankingList").innerHTML;
 assert.ok(html.indexOf("external winner")<html.indexOf("site model"));assert.match(html,/1525.95/);assert.match(html,/已接入/);assert.equal((html.match(/源榜第 2 名/g)||[]).length,2);
 assert.equal(p.get("rankingSiteCount").textContent,"1");assert.match(p.get("rankingPublication").textContent,/2026-11-09/);
});
test("site-only, provider and search filters do not renumber or fetch externally",async()=>{
 const p=await page([model("external",1,1500),model("local",7,1400,true)]);
 p.get("rankingScopeSite").listeners.click();let html=p.get("rankingList").innerHTML;
 assert.match(html,/源榜第 7 名/);assert.doesNotMatch(html,/external/);assert.match(p.get("rankingResultCount").textContent,/全榜 2/);
 p.get("rankingSearch").value="nothing";p.get("rankingSearch").listeners.input();assert.equal(p.get("rankingList").innerHTML,"");assert.equal(p.urls.length,1);
});
test("board change reads only local public API; IPS score keeps its scale",async()=>{
 const p=await page([model("agent",3,0.199556,true)]);p.get("rankingBoard").value="agent";await p.get("rankingBoard").listeners.change();
 assert.match(p.get("rankingList").innerHTML,/0.1996/);assert.match(p.get("rankingScoreHeading").textContent,/Agent IPS Score/);assert.ok(p.urls.every(u=>u.startsWith("/api/public/rankings?board=")));
});
test("empty official cache NEVER invents models or scores",async()=>{
 const p=await page([]);assert.equal(p.get("rankingList").innerHTML,"");assert.equal(p.get("rankingEmpty").querySelector("h3").textContent,"暂无官方榜单缓存");assert.match(p.get("rankingEmpty").querySelector("p").textContent,/不生成推断排名/);
});
test("escapes external names and keeps good snapshot if local request fails",async()=>{
 const p=await page([model('<img src=x onerror="alert(1)">',1,1500)]);const before=p.get("rankingList").innerHTML;assert.doesNotMatch(before,/<img/);assert.match(before,/&lt;img/);
 p.fail();await p.get("rankingRefresh").listeners.click();assert.equal(p.get("rankingList").innerHTML,before);assert.match(p.get("rankingError").textContent,/保留已显示/);assert.equal(p.get("rankingRefresh").disabled,false);
});
test("slow old board response cannot overwrite newly selected board",async()=>{
 let resolveFirst;let count=0;
 const p=await page([],async(url)=>{const board=new URL(url,"https://example.invalid").searchParams.get("board");if(++count===1)return new Promise(resolve=>{resolveFirst=resolve});return{ok:true,json:async()=>data([model("new agent",1,0.2)],board)}});
 p.get("rankingBoard").value="agent";await p.get("rankingBoard").listeners.change();resolveFirst({ok:true,json:async()=>data([model("old text",1,1500)])});await new Promise(setImmediate);
 assert.match(p.get("rankingList").innerHTML,/new agent/);assert.doesNotMatch(p.get("rankingList").innerHTML,/old text/);
});
test("pagination limits DOM size, preserves source ranks and resets when filtering",async()=>{
 const models=Array.from({length:80},(_,i)=>model("model-"+(i+1),i+1,1600-i,i%10===0));
 const p=await page(models);
 assert.equal((p.get("rankingList").innerHTML.match(/class="ranking-row/g)||[]).length,25);
 assert.equal(p.get("rankingPrev").disabled,true);
 p.get("rankingNext").listeners.click();
 assert.match(p.get("rankingList").innerHTML,/源榜第 26 名/);assert.doesNotMatch(p.get("rankingList").innerHTML,/源榜第 1 名/);
 assert.equal(p.get("rankingPage").value,"2");
 p.get("rankingPageSize").value="50";p.get("rankingPageSize").listeners.change();
 assert.equal((p.get("rankingList").innerHTML.match(/class="ranking-row/g)||[]).length,50);
 p.get("rankingScopeSite").listeners.click();assert.equal(p.get("rankingPage").value,"1");assert.equal(p.get("rankingNext").disabled,true);
 assert.equal(p.urls.length,1);
});
test("details use original row index after paging and copy the actual site alias",async()=>{
 const models=Array.from({length:30},(_,i)=>model("entry-"+(i+1),i+1,1600-i,true));models[25].site_models=["real-request-alias"];
 const p=await page(models);p.get("rankingNext").listeners.click();
 p.get("rankingList").listeners.click({target:{closest:()=>({dataset:{modelIndex:"25"}})}});
 assert.equal(p.get("rankingDetailTitle").textContent,"entry-26");assert.equal(p.get("rankingDetail").open,true);
 assert.match(p.get("rankingDetailContent").innerHTML,/real-request-alias/);
 await p.get("rankingDetailContent").listeners.click({target:{closest:()=>({dataset:{copyIndex:"0"}})}});
 assert.deepEqual(p.copied,["real-request-alias"]);p.get("rankingDetailClose").listeners.click();assert.equal(p.get("rankingDetail").open,false);
});
test("version-mismatched DeepSeek is not presented as scored 1017 and unranked models stay visible",async()=>{
 const source=model("DeepSeek-V4.1-Flash-20260910 (Max)",8,1450);
 source.related_site_models=["deepseek-flash"];
 source.site_match_note='本站是 1017，不能套用 0910；<unsafe>';
 const response={...data([source]),unranked_site_models:[{model:"deepseek-flash",versions:["V4.1-Flash-1017"],note:"官方 2026-10-17 版本"}]};
 const p=await page([],async()=>({ok:true,json:async()=>response}));
 assert.match(p.get("rankingList").innerHTML,/本站版本不同/);
 assert.doesNotMatch(p.get("rankingList").innerHTML,/is-connected/);
 assert.equal(p.get("rankingSiteCount").textContent,"0");
 assert.equal(p.get("rankingUnrankedCount").textContent,"1");
 assert.match(p.get("rankingUnrankedList").innerHTML,/1017/);
 p.get("rankingList").listeners.click({target:{closest:()=>({dataset:{modelIndex:"0"}})}});
 const detail=p.get("rankingDetailContent").innerHTML;
 assert.match(detail,/不同日期版本/);assert.match(detail,/deepseek-flash/);
 assert.match(detail,/&lt;unsafe&gt;/);assert.doesNotMatch(detail,/<unsafe>/);
 p.get("rankingScopeSite").listeners.click();
 assert.equal(p.get("rankingList").innerHTML,"");
 assert.equal(p.get("rankingUnranked").classList.contains("hidden"),false);
});
test("thinking-level connection notes do not change the source label or score",async()=>{
 const m=model("GPT-5.2 (High)",9,1456.78,true);m.site_models=["gpt-5.2"];m.site_match_note="思考配置可能不同";
 const p=await page([m]);
 assert.match(p.get("rankingList").innerHTML,/GPT-5.2 \(High\)/);
 assert.match(p.get("rankingList").innerHTML,/1456.78/);
 assert.match(p.get("rankingList").innerHTML,/思考配置可能不同/);
});
test("admin reset and capability metadata regressions stay fixed",async()=>{
 let requested;const ctx=vm.createContext({esc:String,setBusy(){},toast(){},reportError(e){throw e},api:async(url)=>{requested=url}});
 vm.runInContext(read("cmd/apishare/web/router.js"),ctx);vm.runInContext("loadAutoRouter=async()=>{}",ctx);await vm.runInContext("resetAutoRouterBanditState({})",ctx);
 const route=read("cmd/apishare/auto_router.go").match(/auth.POST\("(\/auto-router\/bandit\/reset)"/)[1];assert.equal(requested,"api"+route);
 vm.runInContext("routerConfigCache={model_health:{example:{capabilities:{coding_swe:90,frontend_ui:88},bandit_calls:5,bandit_win_rate:0.9}}}",ctx);
 assert.match(vm.runInContext("capabilityTagsForRouterModel('example')",ctx),/SWE 90/);
});

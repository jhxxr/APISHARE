const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const root = path.resolve(__dirname, "..");
const read = (file) => fs.readFileSync(path.join(root,file),"utf8");
const esc = (v) => String(v??"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));

function consoleContext() {
  const nodes = new Map();
  const $ = (id) => {
    if (!nodes.has(id)) nodes.set(id,{value:"",checked:false,innerHTML:"",classList:{remove(){},add(){},toggle(){}}});
    return nodes.get(id);
  };
  const context=vm.createContext({$,esc,console,modelIcon:()=>"",modelLabel:(v)=>"<code>"+esc(v)+"</code>"});
  vm.runInContext(read("cmd/apishare/web/router.js"),context);
  return {$,context};
}

test("older site configurations default to observation and preserve explicit live/off",()=>{
  const {context}=consoleContext();
  assert.equal(vm.runInContext("cloneRouterDraftFromConfig({}).arena_mode",context),"shadow");
  for(const mode of ["live","off","shadow"]) {
    assert.equal(vm.runInContext(`cloneRouterDraftFromConfig({arena_mode:'${mode}'}).arena_mode`,context),mode);
  }
});

test("saving the site form sends the explicitly selected Arena mode",()=>{
  const {$,context}=consoleContext();
  vm.runInContext("routerDraft=cloneRouterDraftFromConfig({arena_mode:'shadow'})",context);
  $("routerArenaMode").value="live";
  const payload=vm.runInContext("collectRouterDraftFromInputs()",context);
  assert.equal(payload.arena_mode,"live");
});

test("dry run exposes source evidence, shadow proposal and actual transfer separately",()=>{
  const {$,context}=consoleContext();
  context.result={decision:{
    tier:"COMPLEX",selected_model:"actual-model",
    arena:{mode:"shadow",board:"agent",published_at:"2026-11-10",baseline_model:"actual-model",
      proposed_model:"<unsafe-proposal>",actual_model:"actual-model",general_reference_only:false,reason:"source_quality_then_strategy"},
    steps:[{priority:1,status:"selected",model:"actual-model",source_tier:"COMPLEX",arena:{status:"matched",rank:2,score:0.199556,identity:"actual-model"}}],
  }};
  vm.runInContext("renderRouterDryRunResult(result)",context);
  // Read the actual result target from the implementation to avoid pretending
  // that an unused stub node proved the visible renderer worked.
  const source=read("cmd/apishare/web/router.js");
  const id=source.slice(source.indexOf("function renderRouterDryRunResult")).match(/\$\("([^"]+)"\)/)[1];
  assert.ok(read("cmd/apishare/web/index.html").includes('id="'+id+'"'));
  const html=$(id).innerHTML;
  assert.match(html,/观察对照，不改变实际转发/);
  assert.match(html,/0\.1996/);
  assert.match(html,/&lt;unsafe-proposal&gt;/);
  assert.doesNotMatch(html,/<unsafe-proposal>/);
  assert.match(html,/实际 actual-model/);
});

test("both site and personal pages expose all three modes and personal payload persists it",()=>{
  for(const [file,id] of [["cmd/apishare/web/index.html","routerArenaMode"],["cmd/apishare/web/public/user.html","userRouterArenaMode"]]){
    const html=read(file),select=html.slice(html.indexOf('id="'+id+'"')).split("</select>")[0];
    for(const mode of ["shadow","live","off"])assert.ok(select.includes('value="'+mode+'"'));
  }
  assert.match(read("cmd/apishare/web/public/user.js"),/arena_mode:\s*\$\("#userRouterArenaMode"\)/);
});

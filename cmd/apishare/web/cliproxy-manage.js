/* User workflows for selection, batch results, retry and pool scheduling. */
const poolManagement={selected:new Set(),report:[],busy:false,cancel:false,retry:null,view:"accounts",routing:null,jobID:null,jobs:[],jobTimer:null,serverBusy:false};
function setPoolWorkspace(view) {
  poolManagement.view=["accounts","authorization","routing"].includes(view)?view:"accounts";
  document.querySelectorAll("[data-pool-workspace]").forEach(node=>{
    const active=node.dataset.poolWorkspace===poolManagement.view;
    node.setAttribute("aria-pressed",String(active));
  });
  document.querySelectorAll("[data-pool-workspace-panel]").forEach(node=>node.classList.toggle("hidden",node.dataset.poolWorkspacePanel!==poolManagement.view));
}
function resetPoolManagement() {
  clearTimeout(poolManagement.jobTimer);poolManagement.jobID=null;poolManagement.jobs=[];poolManagement.serverBusy=false;poolManagement.jobsLoading=false;poolManagement.dismissedJobID=null;
  $("poolJobsButton").classList.add("hidden");
  poolManagement.selected.clear();poolManagement.report=[];poolManagement.busy=false;poolManagement.cancel=false;poolManagement.retry=null;poolManagement.routing=null;
  $("poolRoutingStrategy").value="round-robin";
  $("poolRoutingAffinity").checked=true;
  setPoolWorkspace("accounts");
  renderPoolReport();syncPoolSelection();
}
function selectPoolAccount(name,selected) {
  if(poolManagement.busy)return;
  if(selected)poolManagement.selected.add(name);else poolManagement.selected.delete(name);
  syncPoolSelection();
}
function selectAllPoolAccounts(selected) {
  if(poolManagement.busy)return;
  document.querySelectorAll("[data-pool-select]").forEach(box=>{if(selected)poolManagement.selected.add(box.dataset.poolSelect);else poolManagement.selected.delete(box.dataset.poolSelect);box.checked=selected;});
  syncPoolSelection();
}
function syncPoolSelection() {
  const locked=poolManagement.busy||poolManagement.serverBusy;
  const existing=new Set(coreUI.credentials.map(item=>item.name));
  for(const name of poolManagement.selected)if(!existing.has(name))poolManagement.selected.delete(name);
  const boxes=[...document.querySelectorAll("[data-pool-select]")];
  const count=boxes.filter(box=>poolManagement.selected.has(box.dataset.poolSelect)).length;
  $("poolSelectAll").checked=!!boxes.length&&count===boxes.length;
  $("poolSelectAll").indeterminate=count>0&&count<boxes.length;
  $("poolSelectAll").disabled=!boxes.length||locked;
  boxes.forEach(box=>{
    box.checked=poolManagement.selected.has(box.dataset.poolSelect);box.disabled=locked;
    box.closest(".account-card")?.classList.toggle("is-selected",box.checked);
  });
  $("poolBulkBar").classList.toggle("hidden",!poolManagement.selected.size);
  $("poolSelectedCount").textContent="已选 "+poolManagement.selected.size+" 个账号";
  $("poolBulkFields").disabled=locked;
  $("poolAddAccountButton").disabled=locked;
  const stopped=coreUI.poolID>0&&coreUI.status&&!coreUI.status.running;
  $("coreImportButton").disabled=locked||stopped;
  document.querySelectorAll("#poolProviderCards button,.pool-other-grid button").forEach(node=>node.disabled=locked||stopped);
  document.querySelectorAll("[data-core-action],[data-account-proxy],[data-pending-remove]").forEach(node=>{
    node.disabled=locked||(!coreUI.status?.running&&!node.hasAttribute("data-pending-remove")&&coreUI.poolID>0);
  });
}
async function openPoolBatch(action) {
  if(!poolManagement.selected.size||poolManagement.busy)return;
  $("poolBulkAction").value=action||"enable";
  $("poolBulkProxy").innerHTML=proxyOptions(-1);
  $("poolBulkProxy").value="-1";
  $("poolBulkComposer").classList.remove("hidden");
  updatePoolBatchFields();
  const id=coreUI.poolID,epoch=coreUI.epoch;
  {
    try{
      const result=await api(poolAPI("pools",id));
      if(epoch!==coreUI.epoch)return;
      $("poolBulkTarget").replaceChildren(...result.pools.map(pool=>{
        const option=new Option(pool.name+(pool.running?"":" · 已停止"),pool.id);option.disabled=!pool.running;return option;
      }));
    }catch(error){reportError(error);}
  }
  $("poolBulkComposer").scrollIntoView({block:"nearest",behavior:motionBehavior()});
}
function updatePoolBatchFields() {
  const action=$("poolBulkAction").value;
  $("poolBulkProxyField").classList.toggle("hidden",action!=="proxy");
  $("poolBulkTargetField").classList.toggle("hidden",action!=="move");
  $("poolBatchHint").textContent=action==="move"?"目标池存在重复账号时保留源账号。配置与代理选择随迁移保留；跟随默认代理的账号将使用目标池默认代理。":"操作完成后逐条显示结果，可以只重试失败的账号。";
}
function renderPoolReport() {
  const items=poolManagement.report;
  $("poolOperationReport").classList.toggle("hidden",!items.length);
  if(!items.length)return;
  const done=items.filter(item=>item.status==="ok").length;
  const failures=items.filter(item=>item.status==="error"||item.status==="canceled").length;
  const confirmations=items.filter(item=>item.status==="confirmation").length;
  const running=poolManagement.serverBusy||poolManagement.busy;
  const settled=items.filter(item=>["ok","error","canceled","confirmation"].includes(item.status)).length;
  const staged=items.filter(item=>item.status==="staged").length;
  $("poolReportSummary").textContent=running?"后台处理中 · 已完成 "+settled+" / "+items.length:staged?staged+" 个凭据待保存"+(failures?" · "+failures+" 项失败":""):"处理完成 · "+done+" 项成功"+(failures?" · "+failures+" 项未完成":"")+(confirmations?" · "+confirmations+" 项待确认":"");
  $("poolReportList").innerHTML=items.map(item=>'<div class="pool-result-row"><span class="badge '+(item.status==="ok"?"on":["error","confirmation","recovering"].includes(item.status)?"warn":"off")+'">'+({ok:"成功",error:"失败",pending:"等待",running:"处理中",staged:"待保存",canceled:"已取消",confirmation:"待确认",recovering:"恢复中"})[item.status]+'</span><strong>'+esc(item.name)+'</strong><small>'+esc(item.message||"")+(item.status==="confirmation"?'<button type="button" class="text-btn" onclick="confirmBackgroundPoolImport('+item.position+')">更新原账号凭据 →</button>':"")+'</small></div>').join("");
  $("poolRetryFailed").disabled=running||!failures||!poolManagement.retry;
  $("poolStopRemaining").classList.toggle("hidden",!running);
  $("poolJobNote").textContent=poolManagement.jobID?"任务已保存在网关。刷新、关闭页面后继续执行；取消仅影响尚未开始的项。":"";
  $("poolJobProgress").max=items.length;$("poolJobProgress").value=settled;
}
async function executePoolBatch(request) {
  await createBackgroundPoolJob(request);
}
async function submitPoolBatch() {
  const action=$("poolBulkAction").value,names=[...poolManagement.selected];
  if(!names.length)return;
  const request={action,names,proxy_id:Number($("poolBulkProxy").value),target_pool_id:Number($("poolBulkTarget").value)};
  if(action==="move"&&!request.target_pool_id){toast("请选择一个正在运行的目标池","error");return;}
  const label={enable:"启用",disable:"停用",refresh:"刷新",proxy:"更换代理",move:"迁移"};
  if(!await askAction({title:"批量"+label[action]+" "+names.length+" 个账号？",description:action==="move"?"目标池确认写入成功后，再从当前池移除。已有账号不会自动覆盖。":"将逐个处理所选账号，并保留每项的执行结果。",confirm:"开始"+label[action]}))return;
  executePoolBatch(request);
}
async function readPoolImportFile(file) {
  if(!file||file.size>1024*1024)throw new Error("文件超过 1 MB");
  if(file.name.length<6||file.name.length>=240||!file.name.endsWith(".json")||/[/\\:*?"<>|\x00\r\n]/.test(file.name))throw new Error("请使用有效的 .json 文件名");
  let credential;
  try{credential=JSON.parse(await file.text());}catch{throw new Error("JSON 格式无效，请检查文件内容");}
  if(!credential||Array.isArray(credential)||typeof credential!=="object"||!Object.keys(credential).length)throw new Error("凭据必须是非空 JSON 对象");
  if(credential.type==="service_account"||credential.type==="vertex"){
    if($("coreVertexOverride").checked||!String(credential.location||"").trim())credential.location=$("coreVertexLocation").value.trim()||"us-central1";
  }
  return {name:file.name,credential,proxy_id:Number($("coreLoginProxy").value),provider:credential.type==="service_account"?"vertex":credential.type||"待识别"};
}
async function stagePoolImport(payload) {
  const existing=coreUI.pending.findIndex(entry=>entry.name.toLowerCase()===payload.name.toLowerCase());
  if(existing>=0){
    if(!await askAction({title:"更新待保存的凭据？",description:payload.name+" 已在待保存列表中。",confirm:"更新此文件"}))throw new Error("已保留原待保存文件");
    coreUI.pending[existing]=payload;
  }else coreUI.pending.push(payload);
}
async function importPoolFiles(files) {
  if(poolManagement.busy||poolManagement.serverBusy)return;
  if(files.length>100){toast("每次最多导入 100 个文件","error");return;}
  const id=coreUI.poolID,epoch=coreUI.epoch;
  poolManagement.jobID=null;poolManagement.busy=true;poolManagement.cancel=false;
  poolManagement.report=files.map(file=>({name:file.name,file,status:"pending"}));
  syncPoolSelection();renderPoolReport();
  for(const item of poolManagement.report){
    if(epoch!==coreUI.epoch)return;
    if(poolManagement.cancel){item.status="error";item.message="尚未上传";continue;}
    try{
      item.payload=await readPoolImportFile(item.file);
      if(!id){await stagePoolImport(item.payload);item.status="staged";item.message="保存账号池后上传并后台导入";}
      else item.status="pending";
    }catch(error){item.status="error";item.message=error.message;}
    renderPoolReport();
  }
  poolManagement.busy=false;
  poolManagement.retry=()=>retryPoolImports();
  if(id)await submitBackgroundPoolImports(poolManagement.report,id);
  else{renderCoreAccounts();renderPoolReport();updateEditorDirty();}
}
async function retryPoolImports() {
  if(poolManagement.jobID){await retryBackgroundPoolJob();return;}
  const failed=poolManagement.report.filter(item=>item.status==="error");
  if(!failed.length||poolManagement.busy)return;
  await importPoolFiles(failed.map(item=>item.file).filter(Boolean));
}
async function loadPoolRouting() {
  if(!coreUI.poolID)return;
  const id=coreUI.poolID,epoch=coreUI.epoch;
  try{
    const result=await api(poolAPI("routing",id));
    if(epoch!==coreUI.epoch)return;
    poolManagement.routing=result;
    $("poolRoutingStrategy").value=result.strategy||"round-robin";
    $("poolRoutingAffinity").checked=result.session_affinity;
  }catch(error){reportError(error);}
}
async function savePoolRouting(button) {
  const settings={strategy:$("poolRoutingStrategy").value,session_affinity:$("poolRoutingAffinity").checked};
  if(!await ensureCorePool())return;
  setBusy(button,true,"应用中…");
  try{
    await api(poolAPI("routing"),{method:"PUT",body:JSON.stringify(settings)});
    toast("池内调度已更新");await loadPoolRouting();setPoolWorkspace("routing");
  }catch(error){reportError(error);}
  finally{setBusy(button,false);}
}
async function completeStagedPoolImports(items,poolID,previousReport=[]) {
  const names=new Set(items.map(item=>item.name));
  const report=items.map(item=>({name:item.name,payload:item,status:"pending"}));
  report.push(...previousReport.filter(item=>item.status==="error"&&(item.file||item.payload)&&!names.has(item.name)));
  coreUI.pending=[];
  await submitBackgroundPoolImports(report,poolID);
  setPoolWorkspace("accounts");
}

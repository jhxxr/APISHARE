/* Durable operations stay on the gateway; polling only presents progress. */
function poolJobActive(job){return ["queued","running","recovering"].includes(job.status);}
function poolJobAction(action){return ({enable:"批量启用",disable:"批量停用",refresh:"刷新凭据",proxy:"绑定代理",move:"跨池迁移",import:"导入凭据"})[action]||"账号任务";}
function applyBackgroundPoolJob(job){
  poolManagement.jobID=job.id;
  poolManagement.serverBusy=poolJobActive(job);
  poolManagement.report=job.items;
  poolManagement.retry=()=>retryBackgroundPoolJob();
  for(const item of job.items)if(item.status==="ok")poolManagement.selected.delete(item.name);
  renderPoolReport();syncPoolSelection();
}
function renderPoolJobHistory(){
  const jobs=poolManagement.jobs;
  $("poolJobsButton").classList.toggle("hidden",!jobs.length);
  $("poolJobsButton").textContent="任务记录"+(jobs.length?" · "+jobs.length:"");
  $("poolJobHistoryField").classList.toggle("hidden",!jobs.length);
  $("poolJobHistory").replaceChildren(...jobs.map(job=>new Option(poolJobAction(job.action)+" · "+fmtTime(job.created_at)+(poolJobActive(job)?" · 执行中":""),job.id)));
  if(poolManagement.jobID)$("poolJobHistory").value=poolManagement.jobID;
}
async function loadBackgroundPoolJobs(){
  if(poolManagement.jobsLoading)return;
  clearTimeout(poolManagement.jobTimer);
  const id=coreUI.poolID,epoch=coreUI.epoch;
  if(!id||!editorIsOpen())return;
  poolManagement.jobsLoading=true;
  try{
    const result=await api(poolAPI("jobs",id),{cache:"no-store"});
    if(epoch!==coreUI.epoch||!editorIsOpen())return;
    poolManagement.jobs=result.jobs;
    const previousActive=poolManagement.serverBusy;
    const selected=result.jobs.find(job=>job.id===poolManagement.jobID)||result.jobs[0];
    if(selected&&!poolManagement.busy&&(poolManagement.dismissedJobID!==selected.id||poolJobActive(selected))){
      applyBackgroundPoolJob(selected);
      if(previousActive&&!poolJobActive(selected)){await loadCLIProxy();toast("账号任务处理完成");}
    }
    renderPoolJobHistory();
  }catch(error){if(epoch===coreUI.epoch)coreMessage("任务进度暂时无法读取，后台任务会继续执行。",true);}
  finally{
    if(epoch===coreUI.epoch)poolManagement.jobsLoading=false;
    if(epoch===coreUI.epoch&&editorIsOpen()&&poolManagement.jobs.some(poolJobActive)){
      poolManagement.jobTimer=setTimeout(loadBackgroundPoolJobs,1600);
    }
  }
}
async function selectBackgroundPoolJob(id){
  poolManagement.dismissedJobID=null;
  const job=poolManagement.jobs.find(item=>item.id===id);
  if(job)applyBackgroundPoolJob(job);
  await loadBackgroundPoolJobs();
}
async function createBackgroundPoolJob(request){
  if(poolManagement.busy||poolManagement.serverBusy)return;
  const id=coreUI.poolID,epoch=coreUI.epoch;
  if(!id)return;
  poolManagement.busy=true;syncPoolSelection();
  poolManagement.dismissedJobID=null;
  try{
    const job=await api(poolAPI("jobs",id),{method:"POST",body:JSON.stringify(request)});
    if(epoch!==coreUI.epoch)return;
    poolManagement.busy=false;
    $("poolBulkComposer").classList.add("hidden");
    applyBackgroundPoolJob(job);
    setPoolWorkspace("accounts");
    await loadBackgroundPoolJobs();
    $("poolOperationReport").scrollIntoView({block:"nearest",behavior:motionBehavior()});
  }catch(error){if(epoch===coreUI.epoch)reportError(error);}
  finally{if(epoch===coreUI.epoch){poolManagement.busy=false;syncPoolSelection();}}
}
function dismissPoolReport(){
  if(poolManagement.busy||poolManagement.serverBusy)return;
  poolManagement.dismissedJobID=poolManagement.jobID;
  poolManagement.report=[];renderPoolReport();
}
function showPoolJobHistory(){
  poolManagement.dismissedJobID=null;
  const job=poolManagement.jobs.find(item=>item.id===poolManagement.jobID)||poolManagement.jobs[0];
  if(job){applyBackgroundPoolJob(job);$("poolOperationReport").scrollIntoView({block:"nearest",behavior:motionBehavior()});}
}
async function submitBackgroundPoolImports(report,id){
  poolManagement.report=report;
  const items=report.map(item=>item.payload?{name:item.name,credential:item.payload.credential,proxy_id:item.payload.proxy_id}:{name:item.name,validation_error:"invalid"});
  await createBackgroundPoolJob({action:"import",items});
}
async function retryBackgroundPoolJob(confirm=[]){
  if(!poolManagement.jobID||poolManagement.busy||poolManagement.serverBusy)return;
  const id=coreUI.poolID,job=poolManagement.jobID,epoch=coreUI.epoch;
  poolManagement.busy=true;syncPoolSelection();
  try{
    await api(poolAPI("jobs/"+job+"/retry",id),{method:"POST",body:JSON.stringify({confirm})});
    if(epoch===coreUI.epoch){poolManagement.busy=false;await loadBackgroundPoolJobs();}
  }catch(error){if(epoch===coreUI.epoch)reportError(error);}
  finally{if(epoch===coreUI.epoch){poolManagement.busy=false;syncPoolSelection();}}
}
async function confirmBackgroundPoolImport(position){
  const item=poolManagement.report.find(item=>item.position===position);
  if(!item||!await askAction({title:"更新已有账号凭据？",description:"确认更新“"+(item.existing_name||item.name)+"”，保留它的代理、启停、备注和调度设置。",confirm:"更新凭据"}))return;
  await retryBackgroundPoolJob([position]);
}
async function cancelPoolRemaining(){
  if(!poolManagement.jobID){poolManagement.cancel=true;return;}
  const id=coreUI.poolID,epoch=coreUI.epoch;
  try{
    await api(poolAPI("jobs/"+poolManagement.jobID+"/cancel",id),{method:"POST",body:"{}"});
    if(epoch===coreUI.epoch)await loadBackgroundPoolJobs();
  }catch(error){reportError(error);}
}

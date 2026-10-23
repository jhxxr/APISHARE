const poolAccountList={page:1,size:50};
function resetPoolAccountPage(){poolAccountList.page=1;renderCoreAccounts();}
function clearPoolAccountFilters(){
  ["poolAccountSearch","poolStatusFilter","poolProviderFilter","poolProxyFilter"].forEach(id=>$(id).value="");
  $("poolOnlyErrors").checked=false;$("poolAccountSort").value="name";resetPoolAccountPage();
}
function changePoolAccountPage(delta){poolAccountList.page=Math.max(1,poolAccountList.page+delta);renderCoreAccounts();$("coreAccountsGrid").scrollIntoView({block:"start",behavior:motionBehavior()});}
function poolAccountCategory(item){
  if(item.pending)return "pending";
  if(item.disabled)return "disabled";
  if(item.details?.retry_at||item.details?.cooldowns?.length)return "cooldown";
  if(item.details?.ready)return "ready";
  return "error";
}
function syncPoolAccountFilterOptions(items){
  const provider=$("poolProviderFilter").value,proxy=$("poolProxyFilter").value;
  $("poolProviderFilter").replaceChildren(new Option("全部提供商",""),...[...new Set(items.map(item=>item.provider).filter(Boolean))].sort().map(name=>new Option(name,name)));
  $("poolProviderFilter").value=[...$("poolProviderFilter").options].some(option=>option.value===provider)?provider:"";
  $("poolProxyFilter").innerHTML='<option value="">全部代理</option>'+proxyOptions(-1);
  $("poolProxyFilter").value=[...$("poolProxyFilter").options].some(option=>option.value===proxy)?proxy:"";
}
function filteredPoolAccounts(items){
  syncPoolAccountFilterOptions(items);
  const query=$("poolAccountSearch").value.trim().toLowerCase(),status=$("poolStatusFilter").value,provider=$("poolProviderFilter").value,proxy=$("poolProxyFilter").value;
  const rows=items.filter(item=>{
    const category=poolAccountCategory(item);
    return [item.name,item.provider,item.email,item.label,item.note].join(" ").toLowerCase().includes(query)&&
      (!status||(status==="ready"?!item.disabled&&item.details?.ready:category===status))&&(!provider||item.provider===provider)&&(!proxy||Number(item.proxy_id??-1)===Number(proxy))&&
      (!$("poolOnlyErrors").checked||category==="error"||category==="cooldown");
  });
  const sort=$("poolAccountSort").value,name=item=>(item.label||item.email||item.name||"");
  rows.sort((a,b)=>{
    let result=0;
    if(sort==="ready")result=Number(poolAccountCategory(b)==="ready")-Number(poolAccountCategory(a)==="ready");
    if(sort==="priority")result=(b.details?.priority||0)-(a.details?.priority||0);
    if(sort==="uptime")result=(a.uptime?.uptime_percent??101)-(b.uptime?.uptime_percent??101);
    if(sort==="failures")result=(b.uptime?.failures||0)-(a.uptime?.failures||0);
    if(sort==="recent")result=(Date.parse(b.uptime?.last_call)||0)-(Date.parse(a.uptime?.last_call)||0);
    return result||name(a).localeCompare(name(b),"zh-CN",{numeric:true});
  });
  const pages=Math.max(1,Math.ceil(rows.length/poolAccountList.size));
  poolAccountList.page=Math.min(poolAccountList.page,pages);
  $("poolAccountPagination").classList.toggle("hidden",pages===1);
  $("poolAccountPageLabel").textContent="第 "+poolAccountList.page+" / "+pages+" 页 · 共 "+rows.length+" 个账号";
  $("poolAccountPrev").disabled=poolAccountList.page<=1;$("poolAccountNext").disabled=poolAccountList.page>=pages;
  $("poolFilteredCount").textContent=rows.length===items.length?items.length:rows.length+" / "+items.length;
  const offset=(poolAccountList.page-1)*poolAccountList.size;
  return rows.slice(offset,offset+poolAccountList.size);
}
function renderPoolRoutingPreview(){
  const select=$("poolPreviewModel"),previous=select.value;
  const models=[...new Set(coreUI.credentials.flatMap(item=>(item.details?.models||[]).map(model=>model.id)))].sort();
  if(select.options.length!==models.length||models.some((id,index)=>select.options[index]?.value!==id)){
    select.replaceChildren(...models.map(id=>new Option(id,id)));if(models.includes(previous))select.value=previous;
  }
  const accounts=coreUI.credentials.filter(item=>!item.disabled&&item.details?.models?.some(model=>model.id===select.value&&model.ready)&&(item.details.weight??1)>0);
  const priority=Math.max(...accounts.map(item=>item.details.priority||0));
  const tier=accounts.filter(item=>(item.details.priority||0)===priority).sort((a,b)=>a.id.localeCompare(b.id));
  const strategy=$("poolRoutingStrategy").value,total=tier.reduce((sum,item)=>sum+(item.details.weight||1),0);
  $("poolRoutingPreviewRows").innerHTML=tier.length?tier.map((item,index)=>{
    const share=strategy==="weighted-round-robin"?(item.details.weight||1)/total:strategy==="fill-first"?(index===0?1:0):1/tier.length;
    return '<div><strong>'+esc(item.label||item.email||item.name)+'</strong><span>账号优先级 '+priority+' · 权重 '+(item.details.weight||1)+'</span><b>'+Math.round(share*1000)/10+'%</b><progress max="1" value="'+share+'" aria-label="'+esc(item.name)+'的预览比例"></progress></div>';
  }).join(""):'<p class="muted">当前没有可用于预览的账号或模型。</p>';
}
function renderPoolAccountPriorityHint(){
  const priority=Number($("poolAccountPriority").value),current=poolAccountPanel.name;
  const peers=coreUI.credentials.filter(item=>item.name!==current&&!item.disabled);
  const above=peers.filter(item=>(item.details?.priority||0)>priority).length;
  const same=peers.filter(item=>(item.details?.priority||0)===priority).length;
  $("poolAccountPriorityHint").textContent="账号数值越大越优先。"+(above?above+" 个未停用账号的优先级更高；它们支持同一模型且可用时优先选择。":"当前没有未停用账号的优先级高于此值。")+(same?" 同级还有 "+same+" 个账号，按池内策略分配。":"");
}

/* The account face stays in place while its complete detail surface opens. */
function openPoolInspector(mode,name){
  const dialog=$("poolInspectorDialog"),panel=$(mode==="uptime"?"poolAccountUptimeDetail":"poolAccountDetails");
  $("poolInspectorContent").append(panel);
  dialog.dataset.mode=mode;dialog.dataset.focusName=name||"";dialog.dataset.poolID=String(coreUI.poolID||"");
  dialog.setAttribute("aria-labelledby",mode==="uptime"?"poolAccountUptimeName":"poolAccountDetailsTitle");
  if(!dialog.open)dialog.showModal();
  dialog.scrollTop=0;syncModalLock();
}
function closePoolInspector(){
  const dialog=$("poolInspectorDialog");
  if(dialog.open)dialog.close();
}
const poolInspectorElement=document.getElementById("poolInspectorDialog");
poolInspectorElement.addEventListener("cancel",event=>{event.preventDefault();closePoolInspector();});
poolInspectorElement.addEventListener("click",event=>{
  if(event.target!==poolInspectorElement)return;
  const rect=poolInspectorElement.getBoundingClientRect();
  if(event.clientX<rect.left||event.clientX>rect.right||event.clientY<rect.top||event.clientY>rect.bottom)closePoolInspector();
});
poolInspectorElement.addEventListener("close",()=>{
  if(poolInspectorElement.open)return;
  const name=poolInspectorElement.dataset.focusName,id=poolInspectorElement.dataset.poolID;
  if(poolInspectorElement.dataset.mode==="uptime"){coreUI.uptimeAccount=null;renderCoreAccounts();}
  else resetPoolAccountDetails();
  syncModalLock();
  if(id===String(coreUI.poolID||"")){
    const card=[...document.querySelectorAll("[data-account-card]")].find(node=>node.dataset.accountCard===name);
    card?.querySelector(".account-card-open")?.focus({preventScroll:true});
  }
});

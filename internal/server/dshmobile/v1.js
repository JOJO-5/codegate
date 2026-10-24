/* CodeGate-owned, version-gated viewing adaptation; never reads prompts,
 * credentials, cookies or models and never sends requests or CLI commands. */
(() => {
 'use strict';
 if(document.getElementById('cg-dsh-mobile-bar'))return;
 const root=document.documentElement, supported=document.currentScript?.dataset.supported==='true';
 const bar=document.createElement('div');bar.id='cg-dsh-mobile-bar';bar.setAttribute('role','region');bar.setAttribute('aria-label','CodeGate 手机布局');
 const label=document.createElement('span'),toggle=document.createElement('button');toggle.type='button';bar.append(label,toggle);document.body.append(bar);
 if(!supported){label.textContent='此 DSH 版本未验证手机适配，保留原版布局';toggle.textContent='关闭提示';toggle.onclick=()=>bar.remove();return}
 const pref='codegate.dsh.mobile.disabled';let enabled=true;
 try{enabled=localStorage.getItem(pref)!=='true'}catch{}
 const media=matchMedia('(max-width:700px),(max-width:950px) and (max-height:500px)');let previousFocus=null,drawer=null,close=null,queued=false,drawerAttrs=null,contractExpired=false;
 const altered=new Map();
 function apply(){root.toggleAttribute('data-cg-dsh-mobile',enabled);label.textContent=enabled?'CodeGate 手机布局':'DSH 原版布局';toggle.textContent=enabled?'使用原版':'启用手机布局';toggle.setAttribute('aria-pressed',String(enabled));sync()}
 toggle.onclick=()=>{enabled=!enabled;try{localStorage.setItem(pref,String(!enabled))}catch{};apply()};
 function viewport(){const v=window.visualViewport;root.style.setProperty('--cg-dsh-vh',(v?.height??innerHeight)+'px');root.style.setProperty('--cg-dsh-top',(v?.offsetTop??0)+'px')}
 function nativeToggle(){const frame=document.querySelector('.pI_x6G_frame');return frame?.querySelector('button[aria-label="收起侧边栏"],button[aria-label="打开侧边栏"],button[aria-label="Close sidebar"],button[aria-label="Open sidebar"]')}
 function closeDrawer(){nativeToggle()?.click()}
 function sync(){
  queued=false;
  if(contractExpired)return;
  const frame=document.querySelector('.pI_x6G_frame');if(!frame)return;
  for(const node of altered.keys())if(!node.isConnected)altered.delete(node);
  root.setAttribute('data-cg-dsh-installed','');
  const sidebar=frame.querySelector('.pI_x6G_sidebarCol');
  const expanded=enabled&&media.matches&&!frame.hasAttribute('data-sidebar-collapsed');
  if(expanded&&sidebar&&!drawer){
   previousFocus=document.activeElement;drawer=sidebar;
   drawerAttrs=['role','aria-modal','aria-label'].map(name=>[name,drawer.getAttribute(name)]);drawer.setAttribute('role','dialog');drawer.setAttribute('aria-modal','true');drawer.setAttribute('aria-label','会话与工作区');
   close=document.createElement('button');close.id='cg-dsh-drawer-close';close.type='button';close.textContent='返回对话';close.onclick=closeDrawer;drawer.prepend(close);close.focus({preventScroll:true});
  }else if(!expanded&&drawer){close?.remove();for(const [name,value] of drawerAttrs??[]){if(value===null)drawer.removeAttribute(name);else drawer.setAttribute(name,value)}drawerAttrs=null;drawer=null;close=null;if(previousFocus?.isConnected)previousFocus.focus({preventScroll:true});previousFocus=null}
  for(const button of frame.querySelectorAll('button[aria-label="选择工作区"]')){
   const walker=document.createTreeWalker(button,NodeFilter.SHOW_TEXT);let node;
   while(node=walker.nextNode()){
    if(enabled&&node.nodeValue.trim()==='createMode'){altered.set(node,node.nodeValue);node.nodeValue='选择工作区'}
   }
  }
  if(!enabled){for(const [node,text] of altered){if(node.isConnected&&node.nodeValue==='选择工作区')node.nodeValue=text}altered.clear()}
 }
 function schedule(){if(!queued){queued=true;requestAnimationFrame(sync)}}
 const observer=new MutationObserver(schedule);observer.observe(document.body,{childList:true,subtree:true,attributes:true,attributeFilter:['data-sidebar-collapsed']});
 document.addEventListener('click',event=>{if(drawer&&event.target?.classList?.contains('pI_x6G_frame'))closeDrawer()});
 document.addEventListener('keydown',event=>{
  if(!drawer)return;
  if([...document.querySelectorAll('[role=dialog]')].some(el=>el!==drawer&&el.getClientRects().length))return;
  if(event.key==='Escape'){event.preventDefault();event.stopPropagation();closeDrawer()}
  if(event.key==='Tab'){
   const items=[...drawer.querySelectorAll('button,a[href],input,select,textarea,[tabindex="0"]')].filter(el=>!el.disabled&&el.getClientRects().length);
   const first=items[0],last=items.at(-1);
   if(event.shiftKey&&document.activeElement===first){event.preventDefault();last?.focus()}else if(!event.shiftKey&&document.activeElement===last){event.preventDefault();first?.focus()}
  }
 },true);
 viewport();window.visualViewport?.addEventListener('resize',viewport);window.visualViewport?.addEventListener('scroll',viewport);window.addEventListener('resize',viewport);media.addEventListener('change',schedule);apply();
 const contractTimer=setTimeout(()=>{if(!document.querySelector('.pI_x6G_frame')){contractExpired=true;observer.disconnect();root.removeAttribute('data-cg-dsh-mobile');label.textContent='尚未检测到兼容界面，保留原版布局';toggle.textContent='关闭提示';toggle.onclick=()=>bar.remove()}},20000);
 window.addEventListener('pagehide',event=>{if(event.persisted)return;clearTimeout(contractTimer);observer.disconnect();window.visualViewport?.removeEventListener('resize',viewport);window.visualViewport?.removeEventListener('scroll',viewport);window.removeEventListener('resize',viewport)});
})();

async page => {
 const results=[],errors=[];page.on('pageerror',e=>errors.push(e.message));
 const assert=(condition,name,detail)=>{if(!condition)throw Error(name+': '+JSON.stringify(detail));results.push({name,detail})};
 const measure=()=>page.evaluate(()=>{const e=document.querySelector('[contenteditable=true]'),b=document.querySelector('button[aria-label="打开侧边栏"]');const rect=x=>{const r=x?.getBoundingClientRect();return r?{x:r.x,y:r.y,width:r.width,height:r.height}:null};return{width:innerWidth,scroll:document.documentElement.scrollWidth,input:rect(e),font:e&&getComputedStyle(e).fontSize,button:rect(b),mobile:document.documentElement.hasAttribute('data-cg-dsh-mobile')}});
 await page.setViewportSize({width:390,height:844});await page.reload();
 await page.getByRole('button',{name:'稍后配置',exact:true}).click();
 const initialRight=page.getByRole('button',{name:'收起右侧边栏',exact:true});if(await initialRight.isVisible())await initialRight.click();
 const input=page.locator('[contenteditable=true]').first();await input.waitFor();
 for(const [width,height] of [[320,640],[360,800],[390,844],[430,932],[844,390]]){
  await page.setViewportSize({width,height});await page.waitForTimeout(150);const m=await measure();
  assert(m.scroll<=width&&m.input.x>=0&&m.input.x+m.input.width<=width+1,'viewport '+width+'x'+height,m);
  if(width<=700||(width<=950&&height<=500))assert(m.font==='16px'&&m.button.width>=44&&m.button.height>=44&&m.button.x+m.button.width<=48.5,'touch/input '+width,m);
 }
 await page.setViewportSize({width:390,height:844});
 const opener=page.getByRole('button',{name:'打开侧边栏',exact:true});await opener.click();
 const close=page.getByRole('button',{name:'返回对话',exact:true});await close.waitFor();
 assert(await close.evaluate(e=>document.activeElement===e),'drawer focus');
 await page.keyboard.press('Shift+Tab');assert(await page.evaluate(()=>document.querySelector('.pI_x6G_sidebarCol').contains(document.activeElement)),'drawer tab trap');
 await page.keyboard.press('Escape');await close.waitFor({state:'detached'});
 assert(await opener.evaluate(e=>document.activeElement===e),'drawer return focus');
 await page.getByRole('button',{name:'打开右侧边栏',exact:true}).click();
 const right=page.locator('[data-sidebar-right-panel][data-sidebar-right-open=true]');await right.waitFor();
 assert(await right.evaluate(e=>{const r=e.getBoundingClientRect();return r.y>=43&&r.bottom<=innerHeight+1&&r.width<=innerWidth+1}),'right panel viewport');
 await page.getByRole('button',{name:'收起右侧边栏',exact:true}).click();
 await page.getByRole('button',{name:'设置',exact:true}).click();const settings=page.getByRole('dialog',{name:'设置',exact:true});await settings.waitFor();
 for(const name of ['通用设置','模型','内置插件','Agent 预设']){
  const tab=settings.getByRole('button',{name,exact:true});if(!await tab.count())continue;await tab.click();
  assert(await settings.evaluate(e=>e.getBoundingClientRect().width<=innerWidth-15),'settings '+name);
 }
 await settings.getByRole('button',{name:'通用设置',exact:true}).click();
 await page.screenshot({path:'dsh-mobile-settings-final-390.png'});
 await settings.getByRole('button',{name:'关闭',exact:true}).click();
 await input.fill('手机草稿测试，不发送');await page.getByRole('button',{name:'使用原版',exact:true}).click();
 assert(await input.evaluate(e=>e.textContent)==='手机草稿测试，不发送','draft original');
 await page.getByRole('button',{name:'启用手机布局',exact:true}).click();
 assert(await input.evaluate(e=>e.textContent)==='手机草稿测试，不发送','draft adapted');
 await page.setViewportSize({width:390,height:400});await input.focus();let m=await measure();
 assert(m.input.y>=44&&m.input.y+m.input.height<=400,'keyboard-height simulation',m);
 await page.setViewportSize({width:390,height:844});await page.emulateMedia({colorScheme:'dark'});
 await page.waitForFunction(()=>getComputedStyle(document.getElementById('cg-dsh-mobile-bar')).backgroundColor===getComputedStyle(document.querySelector('.pI_x6G_frame')).backgroundColor);
 assert(await page.locator('body').getAttribute('data-ds-dark-theme')!==null,'dark theme settled');await page.screenshot({path:'dsh-mobile-dark-final-390.png'});
 await page.setViewportSize({width:1280,height:800});
 assert(await page.locator('#cg-dsh-mobile-bar').isHidden(),'desktop no toolbar');
 await page.context().request.get('http://127.0.0.1:4190/_fixture/mode?value=unsupported');await page.setViewportSize({width:390,height:844});await page.reload();
 await page.getByRole('button',{name:'稍后配置',exact:true}).click();await input.waitFor();
 assert(await page.locator('#cg-dsh-mobile-bar').textContent().then(t=>t.includes('保留原版布局')),'unknown version notice');
 assert(!await page.evaluate(()=>document.documentElement.hasAttribute('data-cg-dsh-mobile')),'unknown native layout');
 await page.getByRole('button',{name:'关闭提示',exact:true}).click();assert(await page.locator('#cg-dsh-mobile-bar').count()===0,'unknown dismiss');
 assert(errors.length===0,'runtime errors',errors);
 await page.context().request.get('http://127.0.0.1:4190/_fixture/mode?value=adapted');
 return{checks:results.length,results,modelCalls:0};
}

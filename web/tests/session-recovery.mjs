import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { parse, compileScript } from '@vue/compiler-sfc'
import ts from 'typescript'
import { ref, computed, watch, nextTick, reactive } from 'vue'

const url = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const actualModule=async path=>import(url(ts.transpileModule(await readFile(new URL(path,import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText))
const {classifyCommand}=await actualModule('../src/lib/commands.ts')
const {nativeCLI}=await actualModule('../src/lib/sessionQuota.ts')
const {inProject}=await actualModule('../src/lib/projectWork.ts')
process.on('uncaughtException', error => { console.error(error.name + ': ' + error.message.replace(/data:text\/javascript;base64,[A-Za-z0-9+/=]+/g,'[compiled component]')); process.exitCode=1 })
const ended = { session_id:'ended', device_id:'device', name:'Closed Codex', command:'codex', cwd:'/work', status:'terminated', recovery:{command_id:'codex',native_id:'native'}, cols:80,rows:24 }
const active = {...ended,session_id:'active',status:'running'}
let mounted=[],unmounted=[],attachCalls=0
const records=reactive({device:[ended,active]})
const sessions={byDevice:records,forDevice:id=>records[id]??[],load:async()=>{},takeDraft:()=>'',patch(id,changes){for(const list of Object.values(records)){const item=list.find(s=>s.session_id===id);if(item)Object.assign(item,changes)}},setFor(id,list){records[id]=list}}
const conn=reactive({isOpen:true,attachTerminal:async()=>{attachCalls++;throw Object.assign(Error('会话已关闭'),{code:'session_not_found'})},detachTerminal:async()=>{},request:async()=>{},onFrame:()=>()=>{},onControl:()=>()=>{}})
const routes=[],emitted=[]
globalThis.__sessionRecovery={ref,computed,watch,nextTick,conn,sessions,classifyCommand,nativeCLI,inProject,
  api:{session:async()=>ended,deviceGit:async()=>({branch:'main'})},
  router:{push:async route=>routes.push(route)},
  devices:{items:[{id:'device',online:true}],byId:()=>({online:true}),load:async()=>{}},
  drafts:{get:()=>'',set:()=>{},owner:'owner'},preferences:{fontSize:14,load:async()=>{},projects:[]},route:reactive({query:{}}),
  onMounted:fn=>mounted.push(fn),onUnmounted:fn=>unmounted.push(fn),
}
const shim=url(`const t=globalThis.__sessionRecovery;export const {ref,computed,watch,nextTick,onMounted,onUnmounted}=t;
export const onBeforeUnmount=t.onUnmounted,defineComponent=x=>x;
export const api=t.api,useConnStore=()=>t.conn,useSessionsStore=()=>t.sessions,useDevicesStore=()=>t.devices,usePreferencesStore=()=>t.preferences,useDraftsStore=()=>t.drafts,useRouter=()=>t.router,useRoute=()=>t.route;
export const humanizeError=e=>e.message,statusKind=()=>'',statusLabel=s=>s,classifyCommand=t.classifyCommand,nativeCLI=t.nativeCLI,inProject=t.inProject,sessionQuotaProvider=s=>s==='codex'?'codex':'',canRecoverConversation=s=>s.command==='codex',liveSession=s=>['starting','running','detached'].includes(s.status);
export const MessageType={SessionSignal:'session.signal',ConversationList:'conversation.list',ConversationRestore:'conversation.restore',SessionExit:'session.exit',SessionClosed:'session.closed'},FrameFlag={},FrameType={},TASK_TEMPLATES=[],RouterLink={};
export const estimateTerminalSize=()=>({cols:80,rows:24}),absoluteTime=String,workspaceFilePath=()=>'',terminalTextSnapshot=()=>{},captureImagePaste=()=>{};
export class Terminal{};export class FitAddon{};export class WebLinksAddon{};export class SearchAddon{};export class Unicode11Addon{};export class WebglAddon{};export default {};`)
async function component(path){
 const script=compileScript(parse(await readFile(new URL(path,import.meta.url),'utf8')).descriptor,{id:path}).content
 let code=ts.transpileModule(script,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
 code=code.replace(/from (['"])[^'"\n]+\1/g,`from '${shim}'`)
 return (await import(url(code))).default
}
const view=await component('../src/views/SessionView.vue')
const ctx=view.setup({id:'ended'},{expose:()=>{},emit:()=>{}})
assert.equal(ctx.summary.value?.session_id,'ended','closed record supplies context before any successful attach')
assert.equal(ctx.siblingSessions.value.length,2,'failed/closed terminal must retain the switcher')
assert.equal(ctx.currentQuotaProvider.value,'','closed terminal cannot poll a session-scoped quota endpoint')
assert.equal(typeof ctx.loadContext,'function','cold refresh loads metadata without attaching a dead terminal')
await ctx.loadContext()
ctx.terminalReady.value=true
await nextTick()
assert.equal(attachCalls,0,'known closed terminal is not retried as a live process')
assert.equal(ctx.canRestore.value,true,'closed native conversation offers recovery in the session')
__sessionRecovery.preferences.projects=[{key:'project.demo',device_id:'device',cwd:'/work'}]
__sessionRecovery.route.query.project='project.demo'
ctx.goBack()
assert.equal(routes.at(-1).name,'project','a terminal opened from a project returns to its workspace')
__sessionRecovery.route.query.project='removed-project'
ctx.goBack()
assert.equal(routes.at(-1).name,'device','removed or mismatched project context falls back safely')
delete __sessionRecovery.route.query.project
__sessionRecovery.preferences.projects=[];routes.length=0

// A stale running snapshot must become an ended conversation after not-found.
globalThis.__sessionRecovery.api.session=async()=>({...active,session_id:'stale'})
const stale=view.setup({id:'stale'},{expose:()=>{},emit:()=>{}})
await stale.loadContext();stale.terminalReady.value=true
await nextTick();await nextTick()
assert.equal(stale.isLive.value,false,'attach not-found reconciles stale running metadata')
assert.equal(stale.canRestore.value,true)
assert.ok(stale.siblingSessions.value.some(s=>s.session_id==='active'))

let finishMetadata
__sessionRecovery.api.session=()=>new Promise(resolve=>{finishMetadata=resolve})
const racedMetadata=view.setup({id:'active'},{expose:()=>{},emit:()=>{}})
const loadingMetadata=racedMetadata.loadContext()
racedMetadata.onControl({type:'session.exit',payload:{session_id:'active',exit_code:0}})
finishMetadata({...active,status:'running'});await loadingMetadata
assert.equal(racedMetadata.isLive.value,false,'older metadata cannot undo a live exit notification')

__sessionRecovery.api.session=async()=>({...active,status:'running',session_id:'raced-attach'})
let finishAttach
conn.attachTerminal=()=>new Promise(resolve=>{finishAttach=resolve})
const racedAttach=view.setup({id:'raced-attach'},{expose:()=>{},emit:()=>{}})
await racedAttach.loadContext();racedAttach.terminalReady.value=true
await nextTick()
racedAttach.onControl({type:'session.closed',payload:{session_id:'raced-attach'}})
finishAttach({payload:{session:{...active,status:'running',session_id:'raced-attach'},role:'controller',seq_to:0,seq_from:0}})
await nextTick();await nextTick()
assert.equal(racedAttach.isLive.value,false,'late attach response cannot resurrect a closed terminal')

const cachedLive={...active,session_id:'cached-live',status:'running'}
records.device.push(cachedLive)
__sessionRecovery.api.session=()=>new Promise(resolve=>{finishMetadata=resolve})
conn.attachTerminal=async()=>({payload:{session:cachedLive,role:'controller',seq_to:0,seq_from:0}})
const cachedView=view.setup({id:'cached-live'},{expose:()=>{},emit:()=>{}})
const pendingSnapshot=cachedView.loadContext();cachedView.terminalReady.value=true
await nextTick();await nextTick()
assert.equal(cachedView.attachmentReady.value,true,'known live context can attach while metadata is slow')
assert.equal(cachedView.currentQuotaProvider.value,'codex','live terminal retains its quota panel')
finishMetadata({...cachedLive,status:'terminated'});await pendingSnapshot
assert.equal(cachedView.isLive.value,true,'late cache snapshot cannot overwrite confirmed live attachment')

const staleCached={...active,session_id:'cached-closed',status:'terminated'}
records.device.push(staleCached)
__sessionRecovery.api.session=async()=>({...staleCached,status:'running'})
conn.attachTerminal=async()=>({payload:{session:{...staleCached,status:'running'},role:'controller',seq_to:0,seq_from:0}})
const corrected=view.setup({id:staleCached.session_id},{expose:()=>{},emit:()=>{}})
corrected.terminalReady.value=true
await corrected.loadContext();await nextTick();await nextTick()
assert.equal(corrected.attachmentReady.value,true,'new running metadata can correct a stale closed snapshot')

// Virtual keys must send only the requested key and preserve unsent text.
const keyFrames=[]
conn.sendFrame=(type,flags,id,bytes)=>keyFrames.push({id,bytes:[...bytes]})
assert.equal(typeof corrected.sendTerminalKey,'function','mobile terminal keys have a guarded dispatch path')
corrected.mobileDraft.value='unsent question'
corrected.sendTerminalKey('\x1b[1;2D')
assert.deepEqual(keyFrames.at(-1),{id:staleCached.session_id,bytes:[27,91,49,59,50,68]})
assert.equal(corrected.mobileDraft.value,'unsent question','a selection key never submits or clears the composer')
const acceptedKeys=keyFrames.length
conn.isOpen=false;corrected.sendTerminalKey('\r');conn.isOpen=true
corrected.role.value='viewer';corrected.sendTerminalKey('\r');corrected.role.value='controller'
corrected.attachmentReady.value=false;corrected.sendTerminalKey('\r');corrected.attachmentReady.value=true
corrected.summary.value={...corrected.summary.value,status:'terminated'};corrected.sendTerminalKey('\r')
assert.equal(keyFrames.length,acceptedKeys,'offline, viewer, reconnecting and closed states never send terminal keys')
console.log('PASS mobile terminal key dispatch preserves drafts and rejects unavailable input')

const signals=[]
conn.request=async(...args)=>{signals.push(args);return {payload:{}}}
for(const entry of [
 {command:'codex',name:'Codex',blocked:true},
 {command:'claude',name:'Shell',blocked:true},
 {command:'opencode',blocked:true},
 {command:'dsh',blocked:true},
 {command:'cmd.exe',args:['/c','C:\\tools\\opencode.cmd'],blocked:true},
 {command:'node.exe',recovery:{command_id:'codex',native_id:'bound'},blocked:true},
 {command:'cmd.exe',name:'Codex',blocked:false},
 {command:'C:\\Program Files\\PowerShell\\7\\pwsh.exe',blocked:false},
 {command:'powershell.exe',blocked:false},
 {command:'/bin/bash',blocked:false},
]){
 const input=view.setup({id:'interrupt-test'},{expose:()=>{},emit:()=>{}})
 input.summary.value={...active,recovery:undefined,...entry,status:'running',session_id:'interrupt-test'}
 input.role.value='controller';input.attachmentReady.value=true
 input.mobileDraft.value='preserve this'
 let count=signals.length
 await input.sendInterrupt()
 assert.equal(signals.length,count+(entry.blocked?0:1),`Ctrl+C policy for ${entry.command}`)
 if(!entry.blocked)assert.deepEqual(signals.at(-1),['session.signal',{session_id:'interrupt-test',signal:'int'},'interrupt-test'])
 assert.equal(input.mobileDraft.value,'preserve this')
 assert.equal(typeof input.onTerminalData,'function')
 count=keyFrames.length;input.onTerminalData('\x03')
 assert.equal(keyFrames.length,count+(entry.blocked?0:1),`physical Ctrl+C policy for ${entry.command}`)
 if(entry.blocked){input.onTerminalData('x');assert.equal(keyFrames.length,count+1,'TUI normal typing remains enabled')}
 for(const mode of ['offline','viewer','unattached','closed']){
  conn.isOpen=mode!=='offline';input.role.value=mode==='viewer'?'viewer':'controller';input.attachmentReady.value=mode!=='unattached';input.summary.value.status=mode==='closed'?'terminated':'running'
  const requests=signals.length,frames=keyFrames.length
  await input.sendInterrupt();input.onTerminalData('\x03')
  assert.equal(signals.length,requests,`${mode} cannot interrupt`);assert.equal(keyFrames.length,frames,`${mode} cannot type`)
 }
 conn.isOpen=true
}
console.log('PASS Ctrl+C protects programming TUIs, enables shells and ignores session labels')
const lateInterrupt=view.setup({id:'late-interrupt'},{expose:()=>{},emit:()=>{}})
lateInterrupt.summary.value={...active,session_id:'late-interrupt',status:'running',command:'cmd.exe',recovery:undefined}
lateInterrupt.role.value='controller';lateInterrupt.attachmentReady.value=true
let failInterrupt
conn.request=()=>new Promise((_,reject)=>{failInterrupt=reject})
lateInterrupt.sendInterrupt()
lateInterrupt.onControl({type:'session.closed',payload:{session_id:'late-interrupt'}})
lateInterrupt.errorText.value='new connection state'
failInterrupt(Error('old interrupt failure'));await nextTick()
assert.equal(lateInterrupt.errorText.value,'new connection state','late interrupt failure cannot overwrite a closed session state')
const draftInput=view.setup({id:'draft-input'},{expose:()=>{},emit:()=>{}})
draftInput.summary.value={...active,status:'running',session_id:'draft-input'}
draftInput.role.value='controller';draftInput.attachmentReady.value=true
draftInput.mobileDraft.value='text with \x03 control'
let draftFrames=keyFrames.length
draftInput.submitDraft()
assert.equal(keyFrames.length,draftFrames,'composer cannot bypass programming TUI Ctrl+C protection')
assert.equal(draftInput.mobileDraft.value,'text with \x03 control','blocked composer text remains editable')
draftInput.mobileDraft.value='not attached';draftInput.attachmentReady.value=false
draftInput.submitDraft()
assert.equal(keyFrames.length,draftFrames,'composer cannot send before attachment is ready')
assert.equal(draftInput.mobileDraft.value,'not attached')
draftInput.attachmentReady.value=true;draftInput.mobileDraft.value='normal question';draftInput.submitDraft()
assert.equal(keyFrames.length,draftFrames+1,'ready composer sends normally')
assert.equal(draftInput.mobileDraft.value,'')

// Clipboard permissions may resolve after editing, cancelling or leaving.
let finishClipboard, failClipboard, clipboardReads=0
globalThis.document={activeElement:null}
globalThis.HTMLElement=class {}
Object.defineProperty(globalThis,'navigator',{configurable:true,value:{clipboard:{readText:()=>{clipboardReads++;return new Promise((resolve,reject)=>{finishClipboard=resolve;failClipboard=reject})}}}})
const pasteView=view.setup({id:'paste-test'},{expose:()=>{},emit:()=>{}})
const pendingPaste=pasteView.readClipboard()
pasteView.pasteDraft.value='manually pasted text'
finishClipboard('old clipboard');await pendingPaste
assert.equal(pasteView.pasteDraft.value,'manually pasted text','late clipboard read must not overwrite manual editing')
assert.equal(pasteView.clipboardReading.value,false)
pasteView.closePaste()
const readCount=clipboardReads
await pasteView.readClipboard()
assert.equal(clipboardReads,readCount,'reopening preserves an unsent preview instead of replacing it')
assert.equal(pasteView.pasteDraft.value,'manually pasted text')
pasteView.closePaste();pasteView.pasteDraft.value=''
const cancelledPaste=pasteView.readClipboard()
const duplicatePaste=pasteView.readClipboard()
assert.equal(clipboardReads,readCount+1,'only one clipboard permission request can be pending')
pasteView.closePaste();finishClipboard('cancelled content');await cancelledPaste;await duplicatePaste
assert.equal(pasteView.pasteOpen.value,false)
assert.equal(pasteView.pasteDraft.value,'','cancelled clipboard read cannot update hidden preview')
const deniedPaste=pasteView.readClipboard();failClipboard(Error('permission denied'));await deniedPaste
assert.match(pasteView.clipboardStatus.value,/手动粘贴/,'denied permission leaves a usable manual recovery path')
assert.equal(pasteView.pasteOpen.value,true)
assert.equal(pasteView.clipboardReading.value,false)
const emptyPaste=pasteView.readClipboard();finishClipboard('');await emptyPaste
assert.match(pasteView.clipboardStatus.value,/没有文字/,'empty clipboard is distinct from permission failure')
const oldPaste=pasteView.readClipboard(),failOld=failClipboard;pasteView.closePaste()
const newPaste=pasteView.readClipboard()
const finishNew=finishClipboard
// An old rejection cannot clear the new request's busy state or show an error.
// Keep separate resolvers because each request belongs to its own preview.
failOld(Error('old permission failure'));await oldPaste
assert.equal(pasteView.clipboardReading.value,true,'old failure cannot clear a newer read')
assert.equal(pasteView.clipboardStatus.value,'')
finishNew('new clipboard');await newPaste
assert.equal(pasteView.pasteDraft.value,'new clipboard')
globalThis.window={removeEventListener:()=>{}}
const leaving=view.setup({id:'leaving-paste'},{expose:()=>{},emit:()=>{}})
const unmountLeaving=unmounted.at(-1),leavingPaste=leaving.readClipboard()
unmountLeaving();finishClipboard('after navigation');await leavingPaste
assert.equal(leaving.pasteDraft.value,'','unmounted view ignores clipboard completion')
console.log('PASS clipboard editing, cancel, permission recovery and navigation guards')

const recovery=await component('../src/components/ConversationRecovery.vue')
mounted=[];unmounted=[]
let resolveRestore,restoreCalls=0
conn.request=async type=>{
 if(type==='conversation.restore'){restoreCalls++;return new Promise(resolve=>{resolveRestore=resolve})}
 return {payload:{conversations:[{id:'chosen',title:'Saved conversation',updated_at:1}]}}
}
const props=reactive({source:ended})
const panel=recovery.setup(props,{expose:()=>{},emit:name=>emitted.push(name)})
mounted.forEach(fn=>fn());await nextTick()
assert.equal(restoreCalls,1)
await panel.restore()
assert.equal(restoreCalls,1,'duplicate recovery clicks cannot create another request')
// Source changes while an old restore is pending must not navigate to old work.
props.source={...ended,session_id:'other',recovery:undefined}
await nextTick();await nextTick()
resolveRestore({payload:{session:active}})
await nextTick();await nextTick()
assert.equal(routes.length,0,'late old recovery cannot navigate away from another source')
assert.equal(emitted.length,0,'late recovery cannot close a different dialog')

conn.request=async()=>{throw Error('device offline')}
await panel.restore('chosen')
assert.match(panel.error.value,/device offline/)
assert.equal(panel.busy.value,false,'failed recovery releases UI for retry/close')
assert.ok(panel.conversations.value.length,'failed restore keeps history options available')

// The session metadata request may discover its native binding after the
// recovery dialog has already begun reading legacy history for the same ID.
mounted=[]
let finishLegacyList,finishBoundRestore,boundRestoreCalls=0
conn.request=async type=>type==='conversation.list'
 ? new Promise(resolve=>{finishLegacyList=resolve})
 : (boundRestoreCalls++,new Promise(resolve=>{finishBoundRestore=resolve}))
const lateProps=reactive({source:{...ended,session_id:'late-binding',recovery:undefined}})
const latePanel=recovery.setup(lateProps,{expose:()=>{},emit:()=>{}})
mounted.forEach(fn=>fn());await nextTick()
lateProps.source={...lateProps.source,recovery:{command_id:'codex',native_id:'native-late'}}
await nextTick();await nextTick()
assert.equal(boundRestoreCalls,1,'late binding for the same source must advance recovery instead of hiding all actions')
finishLegacyList({payload:{conversations:[{id:'obsolete',title:'Old history'}]}})
await nextTick();await nextTick()
assert.equal(latePanel.conversations.value.length,0,'old legacy history cannot overwrite the newly bound recovery')
finishBoundRestore({payload:{session:active}});await nextTick();await nextTick()
assert.ok(routes.some(r=>r.params?.id===active.session_id),'late binding recovery reaches the resumed conversation')
console.log('PASS closed-session context, cold refresh, switching, stale live status, recovery duplicate/stale/error guards')

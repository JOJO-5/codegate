import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { parse, compileScript } from '@vue/compiler-sfc'
import ts from 'typescript'
import { ref, computed, watch, nextTick, reactive } from 'vue'

const url = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
process.on('uncaughtException', error => { console.error(error.name + ': ' + error.message.replace(/data:text\/javascript;base64,[A-Za-z0-9+/=]+/g,'[compiled component]')); process.exitCode=1 })
const ended = { session_id:'ended', device_id:'device', name:'Closed Codex', command:'codex', cwd:'/work', status:'terminated', recovery:{command_id:'codex',native_id:'native'}, cols:80,rows:24 }
const active = {...ended,session_id:'active',status:'running'}
let mounted=[],unmounted=[],attachCalls=0
const records=reactive({device:[ended,active]})
const sessions={byDevice:records,forDevice:id=>records[id]??[],load:async()=>{},takeDraft:()=>'',patch(id,changes){for(const list of Object.values(records)){const item=list.find(s=>s.session_id===id);if(item)Object.assign(item,changes)}},setFor(id,list){records[id]=list}}
const conn=reactive({isOpen:true,attachTerminal:async()=>{attachCalls++;throw Object.assign(Error('会话已关闭'),{code:'session_not_found'})},detachTerminal:async()=>{},request:async()=>{},onFrame:()=>()=>{},onControl:()=>()=>{}})
const routes=[],emitted=[]
globalThis.__sessionRecovery={ref,computed,watch,nextTick,conn,sessions,
  api:{session:async()=>ended,deviceGit:async()=>({branch:'main'})},
  router:{push:async route=>routes.push(route)},
  devices:{items:[{id:'device',online:true}],byId:()=>({online:true}),load:async()=>{}},
  drafts:{get:()=>'',set:()=>{},owner:'owner'},preferences:{fontSize:14,load:async()=>{}},
  onMounted:fn=>mounted.push(fn),onUnmounted:fn=>unmounted.push(fn),
}
const shim=url(`const t=globalThis.__sessionRecovery;export const {ref,computed,watch,nextTick,onMounted,onUnmounted}=t;
export const onBeforeUnmount=t.onUnmounted,defineComponent=x=>x;
export const api=t.api,useConnStore=()=>t.conn,useSessionsStore=()=>t.sessions,useDevicesStore=()=>t.devices,usePreferencesStore=()=>t.preferences,useDraftsStore=()=>t.drafts,useRouter=()=>t.router;
export const humanizeError=e=>e.message,statusKind=()=>'',statusLabel=s=>s,classifyCommand=()=> 'tui',nativeCLI=()=> 'codex',sessionQuotaProvider=s=>s==='codex'?'codex':'',canRecoverConversation=s=>s.command==='codex',liveSession=s=>['starting','running','detached'].includes(s.status);
export const MessageType={ConversationList:'conversation.list',ConversationRestore:'conversation.restore',SessionExit:'session.exit',SessionClosed:'session.closed'},FrameFlag={},FrameType={},TASK_TEMPLATES=[],RouterLink={};
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

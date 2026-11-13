import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import {parse,compileScript} from '@vue/compiler-sfc'
import ts from 'typescript'
import {ref,computed,watch,reactive,nextTick} from 'vue'
const url=code=>`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
process.on('uncaughtException',e=>{console.error(e.message.replace(/data:text\/javascript;base64,[A-Za-z0-9+/=]+/g,'[compiled]'));process.exitCode=1})
const saved=new Map(),drafts=reactive({owner:'one',storageUnavailable:false,get:key=>saved.get(key)||'',set:(key,value)=>value?saved.set(key,value):saved.delete(key)})
const calls=[],routes=[]
let createReply=async()=>{throw Object.assign(Error('lost receipt'),{code:'request_outcome_unknown'})}
let checkReply=async()=>({state:'pending'})
const sessions={create:async(device,opts)=>{calls.push(opts);return createReply()},setDraft:()=>{},byDevice:{},setFor:()=>{}}
globalThis.__launch={ref,computed,watch,nextTick,onMounted:()=>{},onBeforeUnmount:()=>{},drafts,sessions,
 repositories:{commands:{device:[{id:'codex',label:'Codex',kind:'tui'}]},load:async()=>{}},devices:{byId:()=>({online:true}),onlineState:()=> 'online',load:async()=>{}},conn:{isOpen:true},router:{push:async r=>routes.push(r)},api:{sessionCreation:()=>checkReply()},
 launchModeIssue:()=>'',humanizeError:e=>e.message,estimateTerminalSize:()=>({cols:80,rows:24}),TASK_TEMPLATES:[]}
const shim=url('const t=globalThis.__launch;export const {ref,computed,watch,nextTick,onMounted,onBeforeUnmount,api,launchModeIssue,humanizeError,estimateTerminalSize,TASK_TEMPLATES}=t;export const defineComponent=x=>x,useDraftsStore=()=>t.drafts,useSessionsStore=()=>t.sessions,useRepositoriesStore=()=>t.repositories,useDevicesStore=()=>t.devices,useConnStore=()=>t.conn,useRouter=()=>t.router;export default {};')
const script=compileScript(parse(await readFile(new URL('../src/components/TaskLaunch.vue',import.meta.url),'utf8')).descriptor,{id:'launch'}).content
const code=ts.transpileModule(script,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText.replace(/from (['"])[^'"\n]+\1/g,`from '${shim}'`)
const view=(await import(url(code))).default
const setup=()=>view.setup({deviceId:'device',cwd:'/demo',label:'Demo',commandId:'codex',projectKey:'project'},{expose:()=>{},emit:()=>{}})
let ctx=setup()
ctx.name.value='keep my task';ctx.launchMode.value='codex.full-access';await nextTick()
let reopened=setup()
assert.equal(reopened.name.value,'keep my task','closing/reopening preserves the task draft')
assert.equal(reopened.launchMode.value,'','full access is never a saved default')
drafts.storageUnavailable=true
await ctx.launch()
assert.equal(calls.length,0,'storage failure cannot dispatch an untrackable intention')
assert.equal([...saved.keys()].some(k=>k.startsWith('launch-pending:')),false,'undispatched local failure is not a phantom pending launch')
drafts.storageUnavailable=false
await ctx.launch()
assert.equal(ctx.uncertain.value,true,'lost acknowledgement locks dispatch')
await ctx.launch();assert.equal(calls.length,1,'unknown result cannot blindly resend')
const nonce=calls[0].nonce
reopened=setup()
assert.equal(reopened.uncertain.value,true,'pending intention survives component recreation/refresh')
await reopened.launch();assert.equal(calls.length,1)
await reopened.checkResult();assert.equal(calls.length,1,'checking never executes')
assert.equal(routes.length,0,'pending is not reported as success')
checkReply=async()=>({state:'not_received'})
await reopened.checkResult()
assert.equal(reopened.retryAllowed.value,true)
reopened.retryConfirmed.value=true
createReply=async()=>{throw Object.assign(Error('offline on resubmission'),{code:'device_offline'})}
await reopened.retry()
assert.equal(reopened.uncertain.value,true,'a resubmission failure cannot discard an earlier unknown intention')
await reopened.checkResult();reopened.retryConfirmed.value=true
createReply=async()=>({session_id:'created',command:'codex'})
await reopened.retry()
assert.equal(calls[2].nonce,nonce,'explicit resubmission keeps the same durable identity')
assert.equal(calls[2].launchMode,'codex.full-access','resubmission is the explicitly confirmed original request')
assert.equal(routes.at(-1).params.id,'created')
ctx=setup();assert.equal(ctx.launchMode.value,'');assert.equal(ctx.uncertain.value,false)
createReply=async()=>{throw Object.assign(Error('lost'),{code:'timeout'})}
await ctx.launch()
let finishCheck;checkReply=()=>new Promise(resolve=>finishCheck=resolve)
const pending=ctx.checkResult();drafts.owner='two'
finishCheck({state:'resolved',result:{type:'session.created',payload:{session:{session_id:'old-account'}}}});await pending
assert.equal(routes.length,1,'late old-account receipt never navigates')
console.log('PASS launch draft, explicit native permissions, durable unknown result, safe check/retry and account races')

import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import ts from 'typescript'
import {ref,computed,watch,reactive} from 'vue'
import {defineStore,createPinia,setActivePinia} from 'pinia'
const url=code=>`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const compile=code=>ts.transpileModule(code,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const {workItems,canRemoveWork}=await import(url(compile(await readFile(new URL('../src/lib/projectWork.ts',import.meta.url),'utf8'))))
const {mergeWorkflows}=await import(url(compile(await readFile(new URL('../src/lib/workflows.ts',import.meta.url),'utf8'))))
const auth=reactive({user:{id:'first'}})
let continuationReply=async()=>({tasks:[]}),workflowReply=async()=>({workflows:[]}),cancelReply=async()=>({...row,state:'cancelling'})
let removeReply=async(id)=>({...row,id,state:'completed',deleted_at:1}),restoreReply=async(id)=>({...row,id,state:'completed',deleted_at:0})
const api={continuations:()=>continuationReply(),workflows:()=>workflowReply(),cancelContinuation:()=>cancelReply(),cancelWorkflow:()=>cancelReply(),deleteContinuation:(_,id)=>removeReply(id),restoreContinuation:(_,id)=>restoreReply(id)}
const drafts=new Map()
globalThis.__workCatalog={ref,computed,watch,defineStore,api,workItems,canRemoveWork,mergeWorkflows,useAuthStore:()=>auth,useDraftsStore:()=>({get:k=>drafts.get(k)||'',set:(k,v)=>drafts.set(k,v)}),humanizeError:e=>e.message}
const shim=url('export const {ref,computed,watch,defineStore,api,workItems,canRemoveWork,mergeWorkflows,useAuthStore,useDraftsStore,humanizeError}=globalThis.__workCatalog')
const source=compile(await readFile(new URL('../src/stores/workCatalog.ts',import.meta.url),'utf8')).replace(/from (['"])[^'"\n]+\1/g,`from '${shim}'`)
setActivePinia(createPinia())
const {useWorkCatalogStore}=await import(url(source)),store=useWorkCatalogStore()
const row={id:'task',state:'running',source:{name:'test',cwd:'/demo'},updated_at:1}
continuationReply=async()=>({tasks:[row]});workflowReply=async()=>{throw Error('workflow unavailable')}
await store.load('device')
assert.equal(store.items.length,1,'partial failure preserves the successful source')
assert.match(store.errors.device,/workflow unavailable/)
assert.equal(store.snapshots.device.workflowLoaded,false,'unknown source is not an empty result')
let oldReply
continuationReply=()=>new Promise(resolve=>{oldReply=resolve})
const oldLoad=store.load('device')
await store.cancel(store.items[0])
oldReply({tasks:[row]});await oldLoad
assert.equal(store.items[0].state,'cancelling','older poll cannot undo cancellation acknowledgement')
assert.equal(store.loading.device,false)
cancelReply=async()=>{throw Error('connection interrupted')}
continuationReply=async()=>({tasks:[row]});await store.load('device')
await store.cancel(store.items[0])
assert.match(store.notices[store.items[0].key],/尚未确认/)
assert.equal(store.items[0].state,'running','transport failure is not confirmed cancellation')
workflowReply=async()=>({})
await store.load('device')
assert.match(store.errors.device,/格式无法识别/,'malformed data is not reported as an empty source')
continuationReply=async()=>({tasks:[{...row,state:'completed'},{...row,id:'second',state:'completed'}]});workflowReply=async()=>({workflows:[]})
await store.load('device')
let staleHistory
continuationReply=()=>new Promise(resolve=>staleHistory=resolve)
const oldHistory=store.load('device')
removeReply=async id=>{if(id==='second')throw Error('network interrupted');return {...row,id,state:'completed',deleted_at:1}}
await store.remove(store.items)
staleHistory({tasks:[{...row,state:'completed'},{...row,id:'second',state:'completed'}]});await oldHistory
assert.equal(store.items.some(t=>t.id==='task'),false,'older list cannot resurrect removed record')
assert.equal(store.items.some(t=>t.id==='second'),true,'unconfirmed removal stays visible')
assert.equal(store.undoEntries.length,2,'uncertain removals also retain a reversible identity')
assert.match(store.historyNotice,/1/)
await store.undoRemoval()
assert.equal(store.items.length,2,'undo restores records without restarting tasks')
assert.equal(store.undoEntries.length,0)
let removalCalls=0;removeReply=async()=>{removalCalls++;return row}
await store.remove([{...store.items[0],state:'running'}])
assert.equal(removalCalls,0,'active/unknown tasks cannot be hidden via history controls')
let afterLogout
continuationReply=()=>new Promise(resolve=>{afterLogout=resolve})
const pending=store.load('device');auth.user={id:'second'}
afterLogout({tasks:[row]});await pending
assert.equal(store.items.length,0,'late old-account replies stay discarded')
console.log('PASS task catalog partial failure, cancel races, uncertain acknowledgement and account reset')

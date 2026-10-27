import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import ts from 'typescript'
import {ref,computed,watch,reactive} from 'vue'
import {defineStore,createPinia,setActivePinia} from 'pinia'
const url=code=>`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const compile=code=>ts.transpileModule(code,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const {workItems}=await import(url(compile(await readFile(new URL('../src/lib/projectWork.ts',import.meta.url),'utf8'))))
const {mergeWorkflows}=await import(url(compile(await readFile(new URL('../src/lib/workflows.ts',import.meta.url),'utf8'))))
const auth=reactive({user:{id:'first'}})
let continuationReply=async()=>({tasks:[]}),workflowReply=async()=>({workflows:[]}),cancelReply=async()=>({...row,state:'cancelling'})
const api={continuations:()=>continuationReply(),workflows:()=>workflowReply(),cancelContinuation:()=>cancelReply(),cancelWorkflow:()=>cancelReply()}
globalThis.__workCatalog={ref,computed,watch,defineStore,api,workItems,mergeWorkflows,useAuthStore:()=>auth,humanizeError:e=>e.message}
const shim=url('export const {ref,computed,watch,defineStore,api,workItems,mergeWorkflows,useAuthStore,humanizeError}=globalThis.__workCatalog')
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
let afterLogout
continuationReply=()=>new Promise(resolve=>{afterLogout=resolve})
const pending=store.load('device');auth.user={id:'second'}
afterLogout({tasks:[row]});await pending
assert.equal(store.items.length,0,'late old-account replies stay discarded')
console.log('PASS task catalog partial failure, cancel races, uncertain acknowledgement and account reset')

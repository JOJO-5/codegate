import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import ts from 'typescript'
import {ref,computed,watch,reactive} from 'vue'
import {defineStore,createPinia,setActivePinia} from 'pinia'
const url=code=>`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const auth=reactive({user:{id:'one'}}),conn=reactive({isOpen:true,request:async()=>({payload:{sessions:[live]}})})
const live={session_id:'live',status:'running',command:'codex',cwd:'/demo'}
let deviceReply=async()=>[{id:'device',online:true}],savedReply=async()=>[live]
const api={listDevices:()=>deviceReply(),deviceSessions:()=>savedReply()}
globalThis.__refresh={ref,computed,watch,defineStore,api,useAuthStore:()=>auth,useConnStore:()=>conn,humanizeError:e=>e.message,groupConversationSessions:s=>s,conversationRecords:s=>s,liveSession:s=>s.status==='running',MessageType:{SessionList:'session.list'}}
const shim=url('export const {ref,computed,watch,defineStore,api,useAuthStore,useConnStore,humanizeError,groupConversationSessions,conversationRecords,liveSession,MessageType}=globalThis.__refresh')
async function store(file,name){const source=ts.transpileModule(await readFile(new URL(file,import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText.replace(/from (['"])[^'"\n]+\1/g,`from '${shim}'`);return (await import(url(source)))[name]()}
setActivePinia(createPinia())
const devices=await store('../src/stores/devices.ts','useDevicesStore')
await devices.load()
let finishPresence
deviceReply=()=>new Promise(resolve=>finishPresence=resolve)
const oldPresence=devices.load(true)
devices.patch('device',{online:false})
finishPresence([{id:'device',online:true}]);await oldPresence
assert.equal(devices.byId('device').online,false,'older device response cannot undo a newer presence observation')
deviceReply=async()=>[{id:'device',online:false}]
await devices.load(true)
assert.equal(devices.byId('device').online,false)
deviceReply=async()=>{throw Error('network')}
await devices.load(true)
assert.equal(devices.onlineState('device'),'unknown','failed presence verification cannot claim online/offline as current')
let deviceDone
deviceReply=()=>new Promise(resolve=>deviceDone=resolve)
const pendingDevice=devices.load(true)
auth.user={id:'two'}
deviceDone([{id:'old-account',online:true}]);await pendingDevice
assert.equal(devices.items.length,0,'late old-account presence discarded')
const sessions=await store('../src/stores/sessions.ts','useSessionsStore')
await sessions.load('device')
let finishLive
conn.request=()=>new Promise(resolve=>finishLive=resolve)
const refresh=sessions.load('device')
await Promise.resolve();await Promise.resolve()
assert.equal(sessions.sources.device,'live','background refresh retains last confirmed live state')
assert.equal(sessions.byDevice.device[0].status,'running')
assert.equal(sessions.loadStates.device.refreshing,true)
finishLive({payload:{sessions:[live]}});await refresh
assert.ok(sessions.loadStates.device.checkedAt>0)
conn.request=()=>new Promise(resolve=>finishLive=resolve)
const oldLive=sessions.load('device')
await Promise.resolve();await Promise.resolve()
sessions.patch('live',{status:'terminated'})
finishLive({payload:{sessions:[live]}});await oldLive
assert.equal(sessions.byDevice.device[0].status,'terminated','older list cannot undo a confirmed process-exit notification')
conn.request=async()=>{throw Error('device_offline')}
await sessions.load('device')
assert.equal(sessions.sources.device,'cache','failed live refresh is clearly stale')
conn.request=async()=>({payload:{sessions:[live]}});await sessions.load('device')
conn.isOpen=false
assert.equal(sessions.sources.device,'cache','socket loss immediately invalidates live action eligibility')
conn.isOpen=true
let afterLogout
savedReply=()=>new Promise(resolve=>afterLogout=resolve)
const old=sessions.load('device');auth.user={id:'three'}
afterLogout([live]);await old
assert.equal(sessions.byDevice.device,undefined)
console.log('PASS per-device background refresh, offline/unknown state, disconnect and account races')

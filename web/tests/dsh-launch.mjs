import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import ts from 'typescript'
import {ref} from 'vue'

const data=code=>`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const compile=code=>ts.transpileModule(code,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText
const source=await readFile(new URL('../src/views/DeviceDetailView.vue',import.meta.url),'utf8')
const open=source.match(/(?:async )?function openDSHWeb\(\): (?:Promise<void>|void) \{[\s\S]*?\n\}/)?.[0]
assert.ok(open)
const windows=[]
const context={props:{id:'device-a'},router:{resolve:()=>({href:'/devices/device-a/dsh'})},window:{open:(url)=>{windows.push(url);return {opener:null,location:{}}}},dshOpenError:ref(null),dshOpenURL:ref(null),dshNotice:ref(''),api:{startDSHWeb:async()=>({url:'https://dsh.example/'})},humanizeError:()=>''}
await new Function(...Object.keys(context),compile(open).replace('export {};','')+'; return openDSHWeb()')(...Object.values(context))
assert.equal(windows[0],'/devices/device-a/dsh','the user gesture opens an actual waiting route, not an unexplained blank tab')

const shim=data('export const ref=globalThis.__dshRef')
globalThis.__dshRef=ref
const controllerSource=await readFile(new URL('../src/lib/dsh-launch.ts',import.meta.url),'utf8')
const {createDSHLauncher}=await import(data(compile(controllerSource).replace("from 'vue'",`from '${shim}'`)))
const deferred=()=>{let resolve,reject;const promise=new Promise((a,b)=>{resolve=a;reject=b});return {promise,resolve,reject}}
let starts=0,lookups=0,navigations=[]
let pending=deferred()
const deps={status:async()=>{lookups++;return {online:true,state:'not_open'}},start:()=>{starts++;return pending.promise},navigate:url=>navigations.push(url),describe:error=>error.message}
let launcher=createDSHLauncher(deps)
const first=launcher.open('a');await Promise.resolve();await Promise.resolve()
await launcher.open('a')
assert.equal(starts,1,'duplicate click shares waiting state without another dispatch')
assert.equal(launcher.busy.value,true)
launcher.invalidate()
pending.resolve({url:'https://dsh.example/?cg_ticket=fixture'})
await first
assert.equal(navigations.length,0,'leaving the waiting page ignores a late launch reply')

pending=deferred();launcher=createDSHLauncher(deps)
const old=launcher.open('a');await Promise.resolve();await Promise.resolve()
const next=launcher.open('b');await Promise.resolve();await Promise.resolve()
pending.resolve({url:'https://dsh.example/?cg_ticket=fixture'})
await Promise.all([old,next])
assert.equal(navigations.length,1,'only the newest device can navigate')

launcher=createDSHLauncher({...deps,status:async()=>({online:false,state:'offline'})})
const calls=starts;await launcher.open('offline')
assert.equal(starts,calls,'offline status never dispatches start')
assert.match(launcher.error.value,/离线/)
launcher=createDSHLauncher({...deps,status:async()=>({online:true,state:'stopping'})})
await launcher.open('stopping');assert.equal(starts,calls,'stop confirmation cannot race a new start')

launcher=createDSHLauncher({...deps,start:async()=>{throw Error('端口已占用')}})
await launcher.open('failed');assert.match(launcher.error.value,/占用/);assert.equal(launcher.busy.value,false)
await launcher.inspect('failed');assert.equal(starts,calls,'status inspection does not dispatch a task')
assert.equal(launcher.observed.value,'not_open')

launcher=createDSHLauncher({...deps,start:async()=>({url:'javascript:alert(1)'})})
await launcher.open('invalid-url');assert.equal(navigations.length,1,'invalid handoff URL is never navigated')
assert.ok(launcher.error.value)
console.log('PASS DSH waiting page, duplicate dispatch, late responses, offline/stop guards and read-only recovery')

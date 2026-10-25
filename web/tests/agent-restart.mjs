import assert from 'node:assert/strict'
import {readFile} from 'node:fs/promises'
import ts from 'typescript'
import {ref} from 'vue'
const data=s=>`data:text/javascript;base64,${Buffer.from(s).toString('base64')}`
globalThis.__restartRef=ref
const shim=data('export const ref=globalThis.__restartRef')
const source=await readFile(new URL('../src/lib/agent-restart.ts',import.meta.url),'utf8')
const code=ts.transpileModule(source,{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText.replace("from 'vue'",`from '${shim}'`)
const {createImmediateRestart}=await import(data(code))
let status={online:true,agent_version:'v1.0.0',update:{managed:true,can_restart_now:true,instance_started_at:100,state:'waiting'}}
let calls=0,resolve,record=null
const deps={current:()=>status,submit:()=>{calls++;return new Promise(r=>resolve=r)},read:()=>record,write:r=>record=r,describe:e=>e.message}
let controller=createImmediateRestart(deps)
controller.open();assert.equal(calls,0,'opening confirmation never stops work')
controller.close();assert.equal(calls,0,'cancel confirmation has no effects')
controller.open();status={...status,update:{...status.update,instance_started_at:101}}
await controller.confirm();assert.equal(calls,0,'stale confirmation cannot restart a newer Agent')
controller.close();controller.open()
const pending=controller.confirm();await Promise.resolve();await controller.confirm()
assert.equal(calls,1,'duplicate confirm does not dispatch twice')
assert.equal(record.instance,101,'intent persisted before dispatch')
resolve();await pending
assert.notEqual(controller.phase.value,'complete','202 is not proof of restart')
const refreshed=createImmediateRestart(deps);assert.equal(calls,1,'refresh never replays command')
assert.equal(refreshed.phase.value,'unknown')
refreshed.observe({...status,online:false});assert.notEqual(refreshed.phase.value,'complete')
refreshed.observe({...status,update:{...status.update,instance_started_at:99}});assert.notEqual(refreshed.phase.value,'complete','older cached instance is not proof of restart')
refreshed.observe({...status,agent_version:'v1.0.1',update:{...status.update,instance_started_at:102}})
assert.equal(refreshed.phase.value,'complete');assert.equal(record,null)
status={...status,update:{...status.update,instance_started_at:103}}
controller=createImmediateRestart({...deps,submit:async()=>{throw Error('connection lost')}})
controller.open();await controller.confirm()
assert.equal(controller.phase.value,'unknown');assert.ok(record,'uncertain dispatch survives refresh')
assert.equal(calls,1,'uncertain result never automatically retries')
status={...status,update:{...status.update,can_restart_now:false}}
controller=createImmediateRestart(deps);controller.open();await controller.confirm()
assert.equal(calls,1,'unsupported Agent never receives destructive request')
record={instance:103,version:'v1.0.0',target:'v1.1.0',checkedAt:0}
controller=createImmediateRestart(deps)
controller.observe({...status,agent_version:'v1.0.0',update:{...status.update,instance_started_at:104}})
assert.equal(controller.phase.value,'error','restart into old version must not be called an upgrade success')
console.log('Agent immediate restart confirmation, stale instance, duplicate, uncertain dispatch and refresh recovery passed')

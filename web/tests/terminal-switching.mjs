import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
import { ref, computed, shallowRef, watch } from 'vue'
import { createPinia, setActivePinia, defineStore } from 'pinia'
const url=s=>`data:text/javascript;base64,${Buffer.from(s).toString('base64')}`
const sent=[]
let detachError
class WSClient {
 async request(type,payload,id){sent.push([type,id]);if(type==='session.detach'&&detachError)throw detachError;return {payload:{}}}
}
globalThis.__terminalSwitch={ref,computed,shallowRef,watch,defineStore,WSClient}
const shim=url(`export const {ref,computed,shallowRef,watch,defineStore,WSClient}=globalThis.__terminalSwitch;export const FrameType={},MessageType={SessionAttach:'session.attach',SessionDetach:'session.detach'};`)
let code=ts.transpileModule(await readFile(new URL('../src/stores/conn.ts',import.meta.url),'utf8'),{compilerOptions:{target:ts.ScriptTarget.ES2022,module:ts.ModuleKind.ESNext}}).outputText.replace(/from (['"])[^'"\n]+\1/g,`from '${shim}'`)
const {useConnStore}=await import(url(code))
setActivePinia(createPinia())
const conn=useConnStore();conn.state='open'
await conn.attachTerminal('closed',{})
detachError=Object.assign(Error('会话已关闭'),{code:'session_not_found'})
await assert.doesNotReject(conn.detachTerminal('closed'),'closed terminal detach must release the old view')
await conn.attachTerminal('other',{})
detachError=Object.assign(Error('会话不存在'),{code:'session_not_found'})
await assert.doesNotReject(conn.attachTerminal('third',{}),'switching can recover even if an old page never detached')
assert.deepEqual(sent.at(-1),['session.attach','third'])
detachError=Object.assign(Error('offline'),{code:'device_offline'})
await assert.rejects(conn.attachTerminal('blocked',{}),/offline/,'uncertain detach must not pretend it succeeded')
assert.notDeepEqual(sent.at(-1),['session.attach','blocked'])
detachError=undefined
await conn.attachTerminal('after-retry',{})
assert.deepEqual(sent.at(-1),['session.attach','after-retry'],'transport failure remains retryable')
console.log('PASS terminal switching after close, missing detach acknowledgement, transport failure and retry')

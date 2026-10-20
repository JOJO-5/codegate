import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'

// Compile the actual browser helpers, retaining their real protocol constants.
const modules = new Map()
async function load(name) {
  if (modules.has(name)) return modules.get(name)
  const source = await readFile(new URL(`../src/lib/${name}.ts`, import.meta.url), 'utf8')
  let code = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
  for (const dependency of ['protocol', 'transferPacing']) {
    if (code.includes(`from './${dependency}'`)) code = code.replaceAll(`from './${dependency}'`, `from '${await load(dependency)}'`)
  }
  const url = `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
  modules.set(name, url)
  return url
}
const { sessionQuotaProvider } = await import(await load('sessionQuota'))
for (const [command, args, expected] of [
  ['/usr/bin/codex', [], 'codex'], ['C:\\cli\\codex.exe', [], 'codex'],
  ['C:\\Windows\\cmd.exe', ['/d', '/c', 'C:\\npm\\claude.cmd'], 'claude'],
  ['opencode', [], 'opencode'], ['bash', ['codex'], ''], ['dsh', [], ''],
]) assert.equal(sessionQuotaProvider(command, args), expected)
const { uploadFile } = await import(await load('uploads'))
const { MessageType } = await import(await load('protocol'))
const originalPerformance = globalThis.performance
let now = 0
Object.defineProperty(globalThis, 'performance', { configurable: true, value: { now: () => now } })
try {
  for (const delay of [30, 800]) {
    const bytes = Buffer.from(Array.from({ length: 256 * 1024 + 19 }, (_, i) => i % 251))
    const chunks = [], progress = []
    let offset = 0
    const conn = { isOpen: true, async request(type, payload) {
      assert.equal(type, MessageType.FileWrite)
      assert.equal(payload.offset, offset)
      const chunk = Buffer.from(payload.data, 'base64');chunks.push(chunk);offset += chunk.length;now += delay
      return { type: payload.final ? MessageType.FileWriteDone : MessageType.FileWriteReady, payload: { path: payload.path, size: bytes.length } }
    } }
    await uploadFile(conn, 'session', new File([bytes], 'file'), 'file', { progress: p => progress.push(p) })
    assert.deepEqual(Buffer.concat(chunks), bytes)
    assert.equal(chunks[0].length, 32768)
    assert.ok(chunks.every(chunk=>chunk.length<=32768),'individual messages remain bounded for interactive traffic')
    assert.equal(progress.at(-1), 100)
    assert.ok(progress.every((p, i) => i === 0 || p >= progress[i - 1]))
  }
  // Model a delayed acknowledgement batch, not a slower disk or lower bandwidth.
  // Multiple ordered requests can cross the network during the same round trip.
  async function latencyUpload(delay) {
    const bytes = Buffer.alloc(1024 * 1024 + 7, 173)
    const queued = [], received = [], progress = []
    let scheduled = false, active = 0, maxActive = 0, sent = 0
    const started = now
    const conn = { isOpen: true, request(type, payload) {
      assert.equal(type, MessageType.FileWrite)
      assert.equal(payload.offset, sent, 'requests reach the Agent in byte order')
      const data = Buffer.from(payload.data, 'base64');received.push(data);sent += data.length
      if (payload.final) assert.equal(active, 0, 'publish only after every preceding block is acknowledged')
      active++;maxActive=Math.max(maxActive,active)
      return new Promise(resolve => {
        queued.push(() => {active--;resolve({ type: payload.final ? MessageType.FileWriteDone : MessageType.FileWriteReady, payload: { path: payload.path, size: bytes.length } })})
        if (!scheduled) {
          scheduled = true
          setImmediate(() => {scheduled=false;now+=delay;for(const reply of queued.splice(0)) reply()})
        }
      })
    } }
    await uploadFile(conn, 'session', new File([bytes], 'latency.bin'), 'latency.bin', { progress: p => progress.push(p) })
    assert.deepEqual(Buffer.concat(received), bytes)
    assert.equal(progress.at(-1),100)
    return { delay, elapsed: now-started, requests: received.length, maxActive, early100: progress.slice(0,-1).includes(100) }
  }
  const highLatency=await latencyUpload(800)
  console.log('LATENCY_UPLOAD',JSON.stringify(highLatency))
  assert.ok(highLatency.elapsed<=16000,'1 MiB on an 800 ms RTT must not turn into over a minute of stop-and-wait traffic')
  assert.ok(highLatency.maxActive>1&&highLatency.maxActive<=4,'a small bounded upload window overlaps acknowledgements')
  assert.equal(highLatency.early100,false,'100% requires the final publish acknowledgement')
  // An out-of-order failure must stop refill and never publish the final block.
  for (const cancel of [false,true]) {
    const controller=new AbortController(), requests=[], waiting=[]
    let failed=false
    const transfer=uploadFile({isOpen:true,request(type,payload){
      requests.push({type,...payload})
      if(type===MessageType.FileCancel)return Promise.resolve({})
      return new Promise((resolve,reject)=>waiting.push({resolve,reject,payload}))
    }},'session',new File([new Uint8Array(160000)],'pending'),'pending',{signal:controller.signal})
    const caught=transfer.catch(error=>{failed=true;return error})
    while(waiting.length<4)await new Promise(setImmediate)
    if(cancel)controller.abort()
    else waiting[3].reject(Error('later block failed'))
    // A later failed reply must not wait for an earlier reply that may be lost.
    await new Promise(setImmediate)
    assert.equal(requests.at(-1).type,MessageType.FileCancel,'any failed reply triggers cleanup without waiting for earlier receipts')
    const error=await caught
    assert.ok(failed)
    assert.match(error.message,cancel?/取消|aborted/:/later block failed/)
    assert.equal(requests.filter(r=>r.type===MessageType.FileWrite).length,4)
    assert.ok(!requests.some(r=>r.final),'failed/cancelled uploads have no publishing block')
    assert.equal(requests.at(-1).type,MessageType.FileCancel,'cancel follows all already-queued writes')
    // Old rejections are observed and cannot restart sending or leak promises.
    for(const pending of waiting)pending.reject(Error('late receipt'))
    await new Promise(setImmediate)
  }
  for(const size of [0,1,32768,32769]){
    let writes=0
    await uploadFile({isOpen:true,async request(type,payload){
      writes++
      return {type:payload.final?MessageType.FileWriteDone:MessageType.FileWriteReady,payload:{path:payload.path,size}}
    }},'session',new File([new Uint8Array(size)],'edge'),'edge')
    assert.equal(writes,Math.max(1,Math.ceil(size/32768)))
  }
  let progress100=false
  await assert.rejects(uploadFile({isOpen:true,async request(type,payload){
    if(type===MessageType.FileCancel)return {}
    return {type:MessageType.FileWriteDone,payload:{path:payload.path,size:999}}
  }},'session',new File(['x'],'bad'),'bad',{progress:p=>{if(p===100)progress100=true}}),/确认不匹配/)
  assert.equal(progress100,false,'wrong final size cannot report success')
  const controller = new AbortController();const types = []
  await assert.rejects(uploadFile({ isOpen: true, async request(type, payload) {
    types.push(type)
    if (type === MessageType.FileWrite) { controller.abort();return { type: MessageType.FileWriteReady, payload: { path: payload.path } } }
    return {}
  } }, 'session', new File([new Uint8Array(100000)], 'cancel'), 'cancel', { signal: controller.signal }), { name: 'AbortError' })
  assert.deepEqual(types, [MessageType.FileWrite, MessageType.FileCancel])
  console.log('PASS bounded pipelining preserves bytes, offsets, publish barrier, progress and cancellation')
} finally { Object.defineProperty(globalThis, 'performance', { configurable: true, value: originalPerformance }) }

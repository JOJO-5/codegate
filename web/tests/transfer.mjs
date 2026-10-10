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
    assert.equal(chunks[1].length, delay < 120 ? 65536 : 16384)
    if (delay > 600) assert.equal(chunks[2].length, 8192)
    assert.equal(progress.at(-1), 100)
    assert.ok(progress.every((p, i) => i === 0 || p >= progress[i - 1]))
  }
  const controller = new AbortController();const types = []
  await assert.rejects(uploadFile({ isOpen: true, async request(type, payload) {
    types.push(type)
    if (type === MessageType.FileWrite) { controller.abort();return { type: MessageType.FileWriteReady, payload: { path: payload.path } } }
    return {}
  } }, 'session', new File([new Uint8Array(100000)], 'cancel'), 'cancel', { signal: controller.signal }), { name: 'AbortError' })
  assert.deepEqual(types, [MessageType.FileWrite, MessageType.FileCancel])
  console.log('PASS adaptive fast/slow chunks preserve bytes, offsets and progress; cancellation sends no further chunks')
} finally { Object.defineProperty(globalThis, 'performance', { configurable: true, value: originalPerformance }) }

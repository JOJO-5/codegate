import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
import { ref, watch, reactive } from 'vue'
import { createPinia, setActivePinia, defineStore } from 'pinia'

const moduleURL = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const saved = new Map()
globalThis.window = { sessionStorage: {
  getItem: key => saved.get(key) ?? null,
  setItem: (key, value) => saved.set(key, value),
  removeItem: key => saved.delete(key),
} }
const auth = reactive({ user: { id: 'owner-a' } })
globalThis.__draftTest = { ref, watch, defineStore, auth }
const shim = moduleURL('const t=globalThis.__draftTest;export const {ref,watch,defineStore}=t;export const useAuthStore=()=>t.auth;')
let code = ts.transpileModule(await readFile(new URL('../src/stores/drafts.ts', import.meta.url), 'utf8'), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
}).outputText
for (const name of ['vue', 'pinia', './auth']) code = code.replaceAll(`from '${name}'`, `from '${shim}'`)
const { useDraftsStore } = await import(moduleURL(code))
function reload() { setActivePinia(createPinia()); return useDraftsStore() }

let store = reload()
store.set('terminal:one', '未发送的长提示词')
store.set('terminal:two', '另一个会话')
store.set('file:one:src', JSON.stringify({ name: 'main.py', content: 'print("hello")' }))
assert.equal(store.get('terminal:one'), '未发送的长提示词')
assert.equal(store.get('terminal:two'), '另一个会话')
store.$dispose()
store = reload() // A refresh has new Pinia state but retains tab sessionStorage.
assert.equal(store.get('terminal:one'), '未发送的长提示词')
assert.equal(JSON.parse(store.get('file:one:src')).content, 'print("hello")')
store.set('terminal:one', '')
assert.equal(store.get('terminal:one'), '')
assert.equal(store.get('terminal:two'), '另一个会话')
auth.user = null
assert.equal(store.get('terminal:two'), '')
assert.equal(saved.has('codegate.drafts.v1.owner-a'), false)
auth.user = { id: 'owner-b' }
assert.equal(store.get('terminal:two'), '')
store.set('terminal:two', 'B 的草稿')
auth.user = { id: 'owner-a' }
assert.equal(store.get('terminal:two'), '')
assert.equal(saved.has('codegate.drafts.v1.owner-b'), false)
store.$dispose()

saved.set('codegate.drafts.v1.owner-a', '{"terminal:invalid":42}')
store = reload()
assert.equal(store.get('terminal:invalid'), '')
store.$dispose()
Object.defineProperty(window, 'sessionStorage', { get() { throw new Error('storage denied') } })
store = reload()
store.set('terminal:one', '内存草稿')
assert.equal(store.get('terminal:one'), '内存草稿')
assert.equal(store.storageUnavailable, true)
auth.user = null
assert.equal(store.get('terminal:one'), '')
store.$dispose()
console.log('PASS 草稿跨刷新恢复、会话隔离、退出清除、账号隔离及存储失败降级')

import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { parse, compileScript } from '@vue/compiler-sfc'
import ts from 'typescript'
import { ref, computed, watch, reactive } from 'vue'

const url = s => `data:text/javascript;base64,${Buffer.from(s).toString('base64')}`
let list
const saved = new Map()
const drafts = reactive({ owner: 'fixture', get: key => saved.get(key) ?? '', set: (key, value) => saved.set(key, value) })
const blocked = { id: 'blocked', state: 'needs_review', mode: 'live_quota', session_id: 'session' }
const cancelled = { ...blocked, state: 'cancelled' }
globalThis.document = { hidden: false }
globalThis.__continuationTest = { ref, computed, watch, drafts, api: {
  continuations: () => list,
  cancelContinuation: async () => cancelled,
} }
const shim = url(`const t=globalThis.__continuationTest;
export const {ref,computed,watch,api}=t;
export const defineComponent=x=>x,onMounted=()=>{},onBeforeUnmount=()=>{};
export const useDraftsStore=()=>t.drafts,useConnStore=()=>({}),nativeCLI=()=> 'codex';
export const continuationLabels={},absoluteTime=x=>String(x),humanizeError=e=>e.message;
export const MessageType={};export default {};`)
const source = await readFile(new URL('../src/components/ContinuationPanel.vue', import.meta.url), 'utf8')
const script = compileScript(parse(source).descriptor, { id: 'continuation-regression' }).content
let code = ts.transpileModule(script, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
code = code.replace(/from (['"])[^'"\n]+\1/g, `from '${shim}'`)
const component = (await import(url(code))).default
const setup = () => component.setup({ deviceId: 'device', source: null, compact: false }, { expose: () => {}, emit: () => {} })
const ctx = setup()
list = Promise.resolve({ tasks: [blocked] })
await ctx.load()
assert.equal(ctx.visibleRows.value[0].state, 'needs_review')
let finishOldList
list = new Promise(resolve => { finishOldList = resolve })
const oldLoad = ctx.load()
await ctx.cancel(blocked)
assert.equal(ctx.visibleRows.value[0].state, 'cancelled')
finishOldList({ tasks: [blocked] })
await oldLoad
assert.equal(ctx.visibleRows.value[0].state, 'cancelled', 'late old list must not undo cancellation')
ctx.dismiss(cancelled)
assert.equal(ctx.visibleRows.value.length, 0)
const refreshed = setup()
list = Promise.resolve({ tasks: [cancelled] })
await refreshed.load()
assert.equal(refreshed.visibleRows.value.length, 0, 'hidden history survives page recreation')
refreshed.showDismissed()
assert.equal(refreshed.visibleRows.value.length, 1)
ctx.dismiss({ ...blocked, state: 'running' })
assert.equal(saved.get('continuations:hidden:device'), '', 'running task cannot be hidden')
console.log('PASS 取消立即显示，旧响应不覆盖，已结束记录可收起恢复，运行中任务不隐藏')

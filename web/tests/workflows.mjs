import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { parse, compileScript } from '@vue/compiler-sfc'
import ts from 'typescript'
import { ref, computed, watch } from 'vue'

const url = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const transpile = code => ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext } }).outputText
const helpers = await import(url(transpile(await readFile(new URL('../src/lib/workflows.ts', import.meta.url), 'utf8'))))
const row = { id: 'workflow', cwd: '/project', goal: 'goal', state: 'running', revision: 2, updated_at: 2, steps: [] }
assert.equal(helpers.mergeWorkflows([{ ...row, state: 'cancelled', revision: 5 }], [row])[0].state, 'cancelled')
assert.equal(helpers.workflowTerminal('cancelling'), false)
assert.equal(helpers.workflowTerminal('ready_for_review'), true)
globalThis.document = { hidden: false }
let list = Promise.resolve({ workflows: [row] }), failCreate = true
const requests = [], saved = new Map()
class ApiError extends Error {}
const state = {
  ref, computed, watch, ...helpers, ApiError,
  drafts: { owner: 'owner', get: key => saved.get(key) || '', set: (key, value) => saved.set(key, value) },
  api: {
    workflows: () => list,
    workflowProfiles: async () => ({ profiles: [{ id: 'mimo', label: 'OpenCode · 免费', free: true }] }),
    createWorkflow: async (_, body) => { requests.push(JSON.parse(JSON.stringify(body))); if (failCreate) throw Error('network lost after submission'); return { ...row, state: 'queued', revision: 1 } },
    cancelWorkflow: async () => ({ ...row, state: 'cancelling', revision: 3 }),
  },
}
globalThis.__workflow = state
const shim = url(`const t=globalThis.__workflow;
export const {ref,computed,watch,api,ApiError,mergeWorkflows,workflowLabels,workflowTerminal}=t;
export const defineComponent=x=>x,onMounted=()=>{},onBeforeUnmount=()=>{},useDraftsStore=()=>t.drafts;
export const humanizeError=e=>e.message,absoluteTime=String;`)
const source = await readFile(new URL('../src/components/WorkflowPanel.vue', import.meta.url), 'utf8')
const script = compileScript(parse(source).descriptor, { id: 'workflow-regression' }).content
const component = (await import(url(transpile(script).replace(/from (['"])[^'"\n]+\1/g, `from '${shim}'`)))).default
const setup = () => component.setup({ deviceId: 'device', cwd: '/project' }, { expose: () => {} })
const ctx = setup()
await ctx.loadProfiles(); ctx.master.value = 'mimo'; ctx.goal.value = 'Implement goal'
await ctx.create()
assert.equal(ctx.pending.value.nonce, requests[0].nonce)
const restored = setup()
assert.equal(restored.goal.value, 'Implement goal', 'failed submission retains goal across recreation')
assert.equal(restored.pending.value.nonce, requests[0].nonce, 'ambiguous request identity persists')
failCreate = false
await restored.create()
assert.deepEqual(requests[0], requests[1], 'retry uses identical model, target and nonce')
assert.equal(restored.pending.value, null)
await restored.load()
let resolveOld
list = new Promise(resolve => { resolveOld = resolve })
const old = restored.load()
await restored.cancel(row)
resolveOld({ workflows: [row] }); await old
assert.equal(restored.rows.value[0].state, 'cancelling', 'late list must not undo cancellation')
list = Promise.reject(Error('offline')); await restored.load()
assert.equal(restored.rows.value[0].state, 'cancelling', 'failed refresh retains last known status')
assert.match(restored.error.value, /offline/)
assert.match(source, /取消调度/)
assert.match(source, /已交付|待你验收|workflowLabels/)
console.log('PASS 主 Agent 单选、提交幂等恢复、取消旧响应防护及离线保留证据')

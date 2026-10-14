import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import ts from 'typescript'
import { ref, watch } from 'vue'
import { createPinia, setActivePinia, defineStore } from 'pinia'
const moduleURL = code => `data:text/javascript;base64,${Buffer.from(code).toString('base64')}`
const compile = async path => ts.transpileModule(await readFile(new URL(path, import.meta.url), 'utf8'), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
}).outputText
const cliURL = moduleURL(await compile('../src/lib/sessionQuota.ts'))
const { validProjectName } = await import(moduleURL(await compile('../src/lib/projectNames.ts')))
for (const name of ['main.py','你好.md','file with space.txt']) assert.ok(validProjectName(name))
for (const name of ['..','../escape','a\\b','CON.txt','COM¹.txt','tail.','C:escape']) assert.equal(validProjectName(name),false)
const helperURL = moduleURL((await compile('../src/lib/conversationSessions.ts')).replaceAll("from './sessionQuota'", `from '${cliURL}'`))
const { groupConversationSessions: group, conversationRecords: members } = await import(helperURL)
const { canRecoverConversation } = await import(helperURL)
const row = (id, changes = {}) => ({ session_id: id, device_id: 'device-a', name: 'Codex', command: 'codex', cwd: '/work', status: 'terminated', created_at: 10, recovery: { command_id: 'codex', native_id: 'native-a' }, ...changes })
assert.equal(canRecoverConversation(row('native', { recovery: undefined })), true)
assert.equal(canRecoverConversation(row('shim', { recovery: undefined, command: 'C:\\Windows\\System32\\cmd.exe', args: ['/c', 'C:\\tools\\codex.cmd'] })), true)
assert.equal(canRecoverConversation(row('shell', { recovery: undefined, command: 'powershell.exe' })), false)
assert.equal(canRecoverConversation(row('custom', { command: 'custom-agent.exe' })), true)
const old = row('old', { archived: true }), resumed = row('resumed', { created_at: 20 }), live = row('live', { status: 'running', created_at: 5 })
assert.equal(group([old, resumed, live]).length, 1)
assert.equal(group([old, resumed, live])[0].session_id, 'live')
assert.equal(group([old, resumed])[0].session_id, 'resumed')
assert.equal(group([old, resumed])[0].archived, false)
assert.equal(group([old, { ...resumed, archived: true }])[0].archived, true)
assert.equal(group([old, row('different', { recovery: { command_id: 'codex', native_id: 'native-b' } })]).length, 2)
assert.equal(group([old, row('device', { device_id: 'device-b' })]).length, 2)
assert.equal(group([old, row('alias', { recovery: { ...old.recovery, command_id: 'codex-custom' } })]).length, 1)
assert.equal(group([old, row('other-tool', { command: 'claude', recovery: { ...old.recovery, command_id: 'claude' } })]).length, 2)
for (const field of ['store_key', 'native_root']) assert.equal(group([old, row('namespace', { recovery: { ...old.recovery, [field]: 'different' } })]).length, 2)
assert.equal(group([row('unbound-a', { recovery: undefined }), row('unbound-b', { recovery: undefined })]).length, 2)
assert.deepEqual(members([old, resumed], 'resumed').map(s => s.session_id), ['old', 'resumed'])
assert.deepEqual(members([old], 'missing'), [])
assert.equal(old.archived, true) // Grouping never mutates source records.

// Exercise the actual Pinia store, with only transport/auth dependencies stubbed.
const calls = [], saved = []
globalThis.__conversationTest = { ref, watch, defineStore, auth: { user: { id: 'owner' } },
  api: { async archiveSession(device, id, value) { calls.push(['archive', id, value]) },
    async deleteSession(device, id) { calls.push(['delete', id]) }, async deviceSessions() { return saved } },
  conn: { isOpen: false }, }
const shim = moduleURL(`const t=globalThis.__conversationTest; export const {ref,watch,defineStore}=t; export const api=t.api; export const useAuthStore=()=>t.auth; export const useConnStore=()=>t.conn; export const humanizeError=e=>String(e); export const MessageType={};`)
let code = await compile('../src/stores/sessions.ts')
for (const name of ['vue', 'pinia', './auth', '../lib/api', '../lib/protocol', '../lib/format', './conn']) code = code.replaceAll(`from '${name}'`, `from '${shim}'`)
code = code.replaceAll("from '../lib/conversationSessions'", `from '${helperURL}'`)
setActivePinia(createPinia())
const { useSessionsStore } = await import(moduleURL(code))
const store = useSessionsStore()
store.setFor('device-a', [old, resumed])
assert.equal(store.forDevice('device-a').length, 1)
assert.equal(store.byDevice['device-a'].length, 2)
await store.archive('device-a', 'resumed', true)
assert.deepEqual(calls, [['archive', 'old', true], ['archive', 'resumed', true]])
assert.equal(store.forDevice('device-a')[0].archived, true)
store.setFor('device-a', [old, resumed, live]);calls.length=0
await assert.rejects(store.remove('device-a', 'resumed'), /所有远端进程/)
assert.deepEqual(calls, [])
store.setFor('device-a', [old, resumed]);await store.remove('device-a', 'resumed')
assert.deepEqual(calls, [['delete', 'old'], ['delete', 'resumed']])
assert.equal(store.forDevice('device-a').length, 0)
store.setFor('device-a', [old, resumed]);saved.push(old, resumed);calls.length=0
let count=0
__conversationTest.api.archiveSession=async()=>{if(++count===2)throw new Error('fixture failure')}
await assert.rejects(store.archive('device-a', 'resumed', false), /fixture failure/)
assert.equal(store.byDevice['device-a'].length, 2) // Refreshed after partial failure.
// Creating a terminal must retain hidden process records for subsequent group actions.
__conversationTest.conn.request=async()=>({payload:{session:row('new', {created_at:30,status:'running'})}})
await store.create('device-a', {cwd:'/work',cols:80,rows:24,commandId:'codex'})
assert.equal(store.byDevice['device-a'].length, 3)
assert.equal(store.forDevice('device-a').length, 1)
assert.equal(store.forDevice('device-a')[0].session_id, 'new')
store.setFor('device-a', [old, resumed]);let writes=0
__conversationTest.api.archiveSession=async()=>{writes++;store.reset()}
await store.archive('device-a', 'resumed', true)
assert.equal(writes, 1)
assert.equal(store.forDevice('device-a').length, 0)
console.log('PASS native conversation identity, live/latest selection, namespaces, unknown identities, grouped archive/delete and failed-write reconciliation')

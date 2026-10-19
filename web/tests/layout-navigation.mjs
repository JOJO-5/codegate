import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { parse } from '@vue/compiler-sfc'
import { compile, parse as parseTemplate } from '@vue/compiler-dom'
import ts from 'typescript'
import * as Vue from 'vue'
import { renderToString } from '@vue/server-renderer'

async function render(file, state, region) {
  const source = await readFile(new URL(file, import.meta.url), 'utf8')
  let template = parse(source).descriptor.template.content
  if (region) {
    const matches = node => node.props?.some(p => p.name === 'class' && p.value?.content.split(' ').includes(region.startsWith('heading:') ? 'card' : region)) && (!region.startsWith('heading:') || node.loc.source.includes(`<h2>${region.slice(8)}</h2>`))
    const find = node => matches(node) ? node : node.children?.map(find).find(Boolean)
    template = find(parseTemplate(template))?.loc.source
    assert.ok(template, `missing ${region} region`)
  }
  const compiled = compile(template, { mode: 'function', expressionPlugins: ['typescript'] }).code
  const js = ts.transpileModule(compiled, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText
  const app = Vue.createSSRApp({ setup: () => state, render: new Function('Vue', js)(Vue) })
  app.component('RouterLink', { template: '<a><slot /></a>' })
  for (const name of ['RouterView', 'AddProject', 'WorkSummary', 'TaskLaunch', 'ConversationRecovery', 'WorkflowPanel']) app.component(name, { render: () => null })
  app.config.warnHandler = () => {}
  return renderToString(app)
}

const noop = () => {}
const project = { key: 'project.demo', label: 'Demo', device_id: 'device', command_id: 'codex', cwd: '/demo' }
const projectState = {
  preferences: { projects: [project], loading: false }, error: '', removed: null, launch: null, query: '', recent: [], visible: [project],
  devices: { items: [], onlineCount: 1, byId: () => ({ online: true, name: '电脑' }) },
  sessions: { loading: false, forDevice: () => [], sources: {} }, conn: { isOpen: true },
  repositories: { loading: {}, results: {}, errors: {}, commands: {} }, discovered: [], scanBusy: false, scanFailed: false,
  expanded: '', editing: '', recoveringSession: null, addingProject: false, unavailable: () => '',
  refresh: noop, loadRecent: noop, loadRepositories: noop,
}

const projects = await render('../src/views/ProjectsView.vue', projectState)
assert.match(projects, /设备在线/)
assert.match(projects, /<details[^>]*class="project-settings"[^>]*>[\s\S]*?<summary[^>]*>项目设置<\/summary>[\s\S]*?移除入口[\s\S]*?<\/details>/, 'project administration has a discoverable disclosure rather than competing with task actions')
assert.match(projects, /<details[^>]*class="repository-browser"[^>]*>[\s\S]*?<summary/, 'repository discovery can be disclosed independently of saved work')
const scanningFailure = await render('../src/views/ProjectsView.vue', { ...projectState, scanFailed: true })
assert.match(scanningFailure, /<summary[^>]*>[\s\S]*?扫描有问题[\s\S]*?<\/summary>/, 'collapsed discovery still signals a scan failure')
const projectSessionsFailed = await render('../src/views/ProjectsView.vue', {
  ...projectState, expanded: project.key,
  sessions: { ...projectState.sessions, error: '读取会话失败' },
})
assert.ok(!projectSessionsFailed.includes('此目录还没有会话'), 'failed project session reads must not claim an empty directory')
const projectSessionsEmpty = await render('../src/views/ProjectsView.vue', { ...projectState, expanded: project.key })
assert.match(projectSessionsEmpty, /此目录还没有会话/)
assert.ok(!projectSessionsFailed.includes('还没有最近会话'), 'recent session failure is not an empty state')
const savedProjectsFailed = await render('../src/views/ProjectsView.vue', {
  ...projectState, visible: [], preferences: { projects: [], loading: false, error: '项目读取失败' },
})
assert.ok(!savedProjectsFailed.includes('还没有保存的项目'), 'project loading failure is not an empty state')

const launch = await render('../src/components/TaskLaunch.vue', {
  label: 'Demo', deviceId: 'device', mode: 'terminal', busy: false, tools: [], command: '', cwd: '/demo', name: '', isolated: false,
  task: '', selected: null, reason: '请选择已安装并授权的工具', error: '', devices: { byId: () => ({ name: '电脑' }) },
  TASK_TEMPLATES: [], launch: noop, emit: noop,
})
assert.equal((launch.match(/type="radio"/g) ?? []).length, 2, 'both execution methods are visible keyboard-operable radio choices')
assert.match(launch, /<legend[^>]*>执行方式<\/legend>/)
assert.match(launch, /主 Agent 自主调度/)
assert.match(launch, /请选择已安装并授权的工具/)

const shell = await render('../src/App.vue', {
  showChrome: true, dark: false, conn: { health: 'connected', healthText: '已连接' }, auth: { isLoggedIn: true },
  notifications: { events: [] }, toggleTheme: noop, onLogout: noop,
  route: { fullPath: '/projects', path: '/projects' },
})
assert.match(shell, /href="#main-content"[^>]*>跳到主要内容/)
assert.match(shell, /id="main-content"[^>]*tabindex="-1"/)

const sessionState = {
  sessions: { source: null, error: '', loading: true }, sessionActionError: '', showArchived: false, archivedCount: 0,
  sessionQuery: '', visibleSessions: [], isLiveStatus: () => true, canRecoverConversation: () => false,
}
const loading = await render('../src/views/DeviceDetailView.vue', sessionState, 'device-sessions')
assert.match(loading, /正在读取会话/)
assert.ok(!loading.includes('还没有会话'), 'initial loading does not claim no conversations exist')
const failed = await render('../src/views/DeviceDetailView.vue', { ...sessionState, sessions: { source: null, error: '设备无法读取', loading: false } }, 'device-sessions')
assert.match(failed, /暂时无法确认会话列表/)
assert.ok(!failed.includes('还没有会话'), 'failed loading is distinct from empty data')
const cached = await render('../src/views/DeviceDetailView.vue', {
  ...sessionState, sessions: { source: 'cache', error: '', loading: false },
  visibleSessions: [{ session_id: 'cached', name: 'Codex', status: 'running', cwd: '/demo', command: 'codex' }],
  device: { online: true }, conn: { isOpen: true }, canOpen: () => false, pinnedIds: [],
  statusKind: () => 'ok', statusLabel: () => '运行中', describeSession: () => '', createdText: () => '',
}, 'device-sessions')
assert.ok(!cached.includes('离线，暂不可接回'), 'a connected device with cached sessions is not labelled offline')
assert.match(cached, /状态待确认/)

const deviceSource = await readFile(new URL('../src/views/DeviceDetailView.vue', import.meta.url), 'utf8')
const deviceScript = parse(deviceSource).descriptor.scriptSetup.content
const program = ts.createSourceFile('device.ts', deviceScript, ts.ScriptTarget.Latest, true)
const gate = program.statements.find(n => ts.isFunctionDeclaration(n) && n.name?.text === 'canOpen')
assert.ok(gate)
const gateJs = ts.transpileModule(gate.getText(program), { compilerOptions: { target: ts.ScriptTarget.ES2022 } }).outputText
for (const [online, connected, source, expected] of [[true, true, 'live', true], [true, false, 'live', false], [false, true, 'live', false], [true, true, 'cache', false]]) {
  const canOpen = new Function('isLiveStatus', 'device', 'conn', 'sessions', 'props', `${gateJs}; return canOpen`) (
    s => s === 'running', { value: { online } }, { isOpen: connected }, { sources: { device: source } }, { id: 'device' },
  )
  assert.equal(canOpen({ status: 'running' }), expected, `${online}/${connected}/${source}: attach requires a current live view`)
}
console.log('PASS 主要内容跳转、项目管理渐进展开、扫描失败可见和双执行方式入口')
console.log('PASS 会话加载/失败/空数据分离与缓存/断线接回门控')

const devicesFailed = await render('../src/views/DevicesView.vue', {
  devices: { items: [], loading: false, error: '设备列表无法读取' }, showAddDevice: false,
})
assert.ok(!devicesFailed.includes('还没有绑定任何设备'), 'device failure must not claim no devices are bound')
const auditFailed = await render('../src/views/SettingsView.vue', {
  auditItems: [], auditLoading: false, auditError: '审计记录无法读取', auditDone: true,
}, 'heading:审计日志')
assert.ok(!auditFailed.includes('还没有记录'), 'audit failure must not claim the audit log is empty')
console.log('PASS 设备和审计首次读取失败保留真实失败状态')

import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { parse } from '@vue/compiler-sfc'
import { compile } from '@vue/compiler-dom'
import ts from 'typescript'
import * as Vue from 'vue'
import { renderToString } from '@vue/server-renderer'

async function render(path, state) {
  const source = await readFile(new URL(path, import.meta.url), 'utf8')
  const template = parse(source).descriptor.template.content
  const code = compile(template, { mode: 'function', expressionPlugins: ['typescript'] }).code
  const js = ts.transpileModule(code, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText
  const render = new Function('Vue', js)(Vue)
  const app = Vue.createSSRApp({ setup: () => state, render })
  app.component('RouterLink', { template: '<a><slot /></a>' })
  app.config.warnHandler = () => {}
  return renderToString(app)
}
const noop = () => {}
const checks = [
  ['摘要保留失败原因与缓存结果，不把进程退出等同于交付完成', async () => {
    const html = await render('../src/components/WorkSummary.vue', {
      records: [{ status: 'exited' }], latest: { session_id: 'ended', status: 'exited' },
      deviceId: 'device', online: true, sessions: { sources: { device: 'cache' } },
      git: { branch: 'main', files: [], sync_known: false }, tasks: [], busy: false,
      error: '设备读取失败', checked: 123, refresh: noop, absoluteTime: () => '测试时间',
      statusLabel: () => '已退出', continuationLabels: {},
    })
    assert.ok(html.includes('设备读取失败'))
    assert.ok(html.includes('缓存快照'))
    assert.ok(html.includes('上游同步状态未知'))
    assert.ok(html.includes('最近读取尝试'))
    assert.ok(html.includes('任务结果需人工核对'))
    assert.ok(!html.includes('暂无后台续跑任务'))
  }],
  ['首次加载后台续跑失败仍显示错误和重试', async () => {
    const html = await render('../src/components/ContinuationPanel.vue', {
      source: null, visibleRows: [], compact: false, error: '续跑读取失败', listError: '续跑读取失败',
      busy: false, loading: false, loaded: false, load: noop,
    })
    assert.ok(html.includes('续跑读取失败'))
    assert.ok(html.includes('重试'))
  }],
  ['文件列表读取失败不显示空目录', async () => {
    const html = await render('../src/views/FilesView.vue', {
      id: 'audit', embedded: false, router: { push: noop }, busy: false, uploading: false, conn: { isOpen: false },
      newTextOpen: false, transferLabel: '', transferDone: '', cwd: '/demo', folderLabel: '工作区根目录', folder: '',
      error: '文件列表读取失败', entries: [], previewName: '', selected: null, overwrite: false, picker: null,
      listed: false, list: noop, up: noop, upload: noop,
    })
    assert.ok(html.includes('文件列表读取失败'))
    assert.ok(!html.includes('目录为空'))
  }],
  ['真实空续跑列表给出入口说明', async () => {
    const html = await render('../src/components/ContinuationPanel.vue', {
      source: null, visibleRows: [], compact: false, error: '', listError: '', busy: false, loading: false, loaded: true, load: noop,
    })
    assert.ok(html.includes('暂无后台任务'))
    assert.ok(!html.includes('重试读取'))
  }],
  ['项目最近会话提供原对话恢复入口', async () => {
    const ended = { session_id: 'ended', device_id: 'device', name: '项目 · Codex', command: 'codex', cwd: '/demo', status: 'terminated', online: true }
    const html = await render('../src/views/ProjectsView.vue', {
      error: '', preferences: { projects: [], loading: false }, removed: null, launch: null, query: '', visible: [],
      recent: [ended], sessions: { loading: false, sources: { device: 'live' } }, conn: { isOpen: true },
      devices: { items: [], onlineCount: 0, byId: () => ({ online: true, name: '电脑' }) },
      repositories: { loading: {}, results: {}, errors: {}, commands: {} }, discovered: [], scanBusy: false, scanFailed: false,
      statusLabel: () => '已终止', isLiveStatus: () => false, canRecoverConversation: () => true,
      unavailable: () => '', refresh: noop, loadRecent: noop, loadRepositories: noop, recoveringSession: null, addingProject: false,
    })
    assert.ok(html.includes('恢复原对话'))
  }],
]
let failures = 0
for (const [name, check] of checks) {
  try { await check(); console.log(`PASS ${name}`) }
  catch (error) { failures++; console.error(`FAIL ${name}: ${error.message}`) }
}
assert.equal(failures, 0, 'UX continuity regressions')

<script setup lang="ts">
import { computed, nextTick, onMounted, onBeforeUnmount, ref, watch } from 'vue'
import WorkflowPanel from './WorkflowPanel.vue'
import NativeLaunchModePicker from './NativeLaunchModePicker.vue'
import { launchModeIssue } from '../lib/nativeLaunchModes'
const mode = ref('terminal')
import { useRouter } from 'vue-router'
import { useRepositoriesStore } from '../stores/repositories'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { TASK_TEMPLATES } from '../lib/tasks'
import { estimateTerminalSize } from '../lib/viewport'
import { humanizeError } from '../lib/format'
import { api } from '../lib/api'
import { useDraftsStore } from '../stores/drafts'
import type { SessionSummary } from '../lib/protocol'
const props = defineProps<{ deviceId: string; cwd: string; label: string; commandId: string; projectKey?: string; intentKey?: string }>()
const emit = defineEmits<{ close: []; busy: [value: boolean] }>()
const repositories = useRepositoriesStore()
const devices = useDevicesStore()
const sessions = useSessionsStore()
const conn = useConnStore()
const router = useRouter()
const drafts = useDraftsStore()
const draftKey = `launch-draft:${props.deviceId}:${props.intentKey || props.projectKey || props.cwd}`
const pendingKey = `launch-pending:${props.deviceId}:${props.intentKey || props.projectKey || props.cwd}`
let saved: { command?: string; cwd?: string; name?: string; isolated?: boolean; task?: string } = {}
try { saved = JSON.parse(drafts.get(draftKey) || '{}') ?? {} } catch { /* Keep opening a damaged draft recoverable. */ }
const command = ref(typeof saved.command === 'string' ? saved.command : props.commandId)
const launchMode = ref('')
watch(command, () => { launchMode.value = '' }, { flush: 'sync' })
const cwd = ref(typeof saved.cwd === 'string' ? saved.cwd : props.cwd)
const name = ref(typeof saved.name === 'string' ? saved.name : '')
const isolated = ref(saved.isolated === true)
const task = ref(typeof saved.task === 'string' ? saved.task : '')
const busy = ref(false)
watch(busy, value => emit('busy', value), { flush: 'sync' })
const error = ref('')
type LaunchOptions = Parameters<typeof sessions.create>[1]
const pending = ref<{ nonce: string; options: LaunchOptions; task: string; prompt?: string } | null>(null)
const uncertain = ref(false), retryAllowed = ref(false), retryConfirmed = ref(false)
const showOriginal = ref(false)
const resultButton = ref<HTMLButtonElement | null>(null)
let disposed = false, generation = 0
try {
  const raw = drafts.get(pendingKey)
  if (raw) {
    uncertain.value = true
    const value = JSON.parse(raw)
    if (typeof value.nonce === 'string' && value.options && typeof value.options.cwd === 'string') pending.value = value
    error.value = '上次启动结果待确认，请先核对启动结果；不会重复创建终端或分支。'
  }
} catch { uncertain.value = true; error.value = '启动标识无法读取，请在设备会话和工作区中核对；暂不重新派发。' }
if (pending.value) {
  const options = pending.value.options
  command.value = options.commandId || ''; cwd.value = options.cwd; name.value = options.name || ''; isolated.value = !!options.worktree
  launchMode.value = options.launchMode || ''; task.value = pending.value.task
}
watch([command, cwd, name, isolated, task], () => {
  if (!uncertain.value && !disposed) drafts.set(draftKey, JSON.stringify({ command: command.value, cwd: cwd.value, name: name.value, isolated: isolated.value, task: task.value }))
}, { flush: 'sync' })
watch(() => drafts.owner, () => { generation++; disposed = true; busy.value = false; pending.value = null; uncertain.value = true; error.value = '账号已切换，请重新打开启动面板。' }, { flush: 'sync' })
onBeforeUnmount(() => { disposed = true; generation++ })
const tools = computed(() => repositories.commands[props.deviceId] ?? [])
const selected = computed(() => tools.value.find(t => t.id === command.value))
const permissionLabel = computed(() => selected.value?.native_launch_modes?.find(m => m.id === launchMode.value)?.label || launchMode.value || '继承 CLI 本机设置')
const reason = computed(() => devices.onlineState(props.deviceId) === 'unknown' ? '设备状态待确认，请刷新设备状态后重试。' : !devices.byId(props.deviceId)?.online ? '设备离线，上线后才能新建任务。' : !conn.isOpen ? '连接正在恢复，请稍候。' : !selected.value ? '请选择已安装并授权的工具；可在设备页管理授权。' : !cwd.value.trim() ? '请选择工作目录。' : launchModeIssue(selected.value.native_launch_modes, launchMode.value))
onMounted(async () => {
  await repositories.load(props.deviceId)
  if (!selected.value && !uncertain.value) command.value = tools.value[0]?.id ?? ''
})
async function launch(): Promise<void> {
  if (reason.value || busy.value || uncertain.value || disposed) return
  const current = generation
  busy.value = true; error.value = ''
  await devices.load(true)
  if (disposed || current !== generation) return
  busy.value = false
  if (devices.onlineState(props.deviceId) !== 'online' || !conn.isOpen) { error.value = '尚未派发：设备或连接状态未确认，请刷新后重试。'; return }
  const options = { commandId: command.value, launchMode: launchMode.value, cwd: cwd.value.trim(), name: name.value.trim(), worktree: isolated.value, ...estimateTerminalSize() }
  pending.value = { nonce: crypto.randomUUID(), options, task: task.value }
  drafts.set(pendingKey, JSON.stringify(pending.value))
  if (drafts.storageUnavailable) { drafts.set(pendingKey, ''); pending.value = null; error.value = '无法保存启动标识，请允许此标签页存储后重试；尚未派发。'; return }
  await dispatch()
}
async function openSession(session: SessionSummary, current: number): Promise<void> {
  const owner = drafts.owner
  const template = TASK_TEMPLATES.find(t => t.id === pending.value?.task)
  const prompt = pending.value?.prompt || template?.prompt
  if (prompt && selected.value?.kind === 'tui') sessions.setDraft(session.session_id, prompt)
  sessions.setFor(props.deviceId, [session, ...(sessions.byDevice[props.deviceId] ?? []).filter(s => s.session_id !== session.session_id)])
  await router.push({ name: 'session', params: { id: session.session_id }, query: props.projectKey ? { project: props.projectKey } : {} })
  if (owner === drafts.owner) { drafts.set(pendingKey, ''); drafts.set(draftKey, '') }
  if (!disposed && current === generation) { pending.value = null; uncertain.value = false; launchMode.value = '' }
}
async function dispatch(): Promise<void> {
  if (!pending.value || disposed) return
  const current = generation, intent = pending.value, retrying = uncertain.value
  busy.value = true; error.value = ''
  try {
    const session = await sessions.create(props.deviceId, { ...intent.options, nonce: intent.nonce })
    if (!disposed && current === generation) await openSession(session, current)
  } catch (e) {
    if (disposed || current !== generation) return
    const code = e && typeof e === 'object' && 'code' in e ? e.code : ''
    uncertain.value = retrying || !code || ['timeout', 'request_outcome_unknown', 'internal'].includes(String(code))
    if (!uncertain.value) { drafts.set(pendingKey, ''); pending.value = null }
    error.value = uncertain.value ? '启动结果待确认，请先核对启动结果；不会重复创建终端或独立分支。' : humanizeError(e)
  } finally {
    if (!disposed && current === generation) {
      busy.value = false
      if (uncertain.value) { await nextTick(); if (!disposed) resultButton.value?.focus() }
    }
  }
}
async function checkResult(): Promise<void> {
  if (busy.value || !pending.value || disposed) return
  const current = generation
  busy.value = true; error.value = ''; retryAllowed.value = false; retryConfirmed.value = false
  try {
    const receipt = await api.sessionCreation(props.deviceId, pending.value.nonce)
    if (disposed || current !== generation) return
    if (receipt.state === 'resolved' && receipt.result?.type === 'session.created' && receipt.result.payload?.session) {
      await openSession(receipt.result.payload.session, current)
    } else if (receipt.state === 'resolved' && receipt.result?.type === 'error' && receipt.result.payload?.code && receipt.result.payload.code !== 'request_outcome_unknown') {
      error.value = receipt.result.payload?.message || '启动未成功，请检查参数后重试。'
      drafts.set(pendingKey, ''); pending.value = null; uncertain.value = false
    } else {
      retryAllowed.value = receipt.state === 'not_received'
      error.value = retryAllowed.value ? '服务端尚未登记这次启动；可确认后按原参数重新提交，同一标识最多派发一次。' : '启动已派发，尚未收到结果。请稍后核对，也可在设备会话与工作区查看；不会重新派发。'
    }
  } catch (e) { if (!disposed && current === generation) error.value = `核对失败：${humanizeError(e)}。原启动标识已保留，请稍后重试。` }
  finally { if (!disposed && current === generation) busy.value = false }
}
async function retry(): Promise<void> {
  if (busy.value || disposed || !retryAllowed.value || !retryConfirmed.value || !conn.isOpen) return
  retryAllowed.value = false; retryConfirmed.value = false
  await dispatch()
}
</script>
<template>
  <section class="card stack task-launch" aria-labelledby="launch-title">
    <div class="row row--between"><h2 id="launch-title" tabindex="-1">开始工作 · {{ label }}</h2><button class="btn btn--ghost" :disabled="busy" @click="emit('close')">关闭启动面板</button></div>
    <fieldset v-if="!uncertain" class="launch-modes" :disabled="busy">
      <legend>执行方式</legend>
      <label class="launch-mode"><input v-model="mode" type="radio" name="task-execution" value="terminal" /><span><strong>打开 CLI，由我对话</strong><small>自己与编程工具交流、控制每一步。</small></span></label>
      <label class="launch-mode"><input v-model="mode" type="radio" name="task-execution" value="workflow" /><span><strong>主 Agent 自主调度</strong><small>交给它目标，由它分工、检查与返工。</small></span></label>
    </fieldset>
    <WorkflowPanel v-if="mode === 'workflow'" :key="deviceId + cwd" :device-id="deviceId" :cwd="cwd" :label="label" />
    <form v-else class="stack" @submit.prevent="launch">
      <section v-if="uncertain" class="stack" aria-labelledby="pending-launch-title">
        <h3 id="pending-launch-title">上次启动待核对</h3>
        <p class="small dim">{{ devices.byId(deviceId)?.name }} · {{ selected?.label || command }} · {{ isolated ? '独立 Git 工作区' : '原目录' }}<br />{{ cwd }}</p>
        <p class="small dim">原启动权限：{{ permissionLabel }}。此设置只属于这一次启动。</p>
        <p v-if="error" class="notice notice--warn" role="alert">{{ error }}</p>
        <button ref="resultButton" type="button" class="btn btn--primary" :disabled="busy || !pending" @click="checkResult">{{ busy ? '正在核对…' : '核对启动结果' }}</button>
        <template v-if="retryAllowed"><label class="check"><input v-model="retryConfirmed" type="checkbox" :disabled="busy" />确认按原参数与原权限重新提交这一次启动</label><button type="button" class="btn" :disabled="busy || !retryConfirmed || !conn.isOpen" @click="retry">按原标识重新提交</button></template>
        <p class="small dim">关闭面板和刷新页面会保留待核对标识；关闭面板不会停止远端启动。</p>
        <button type="button" class="btn btn--ghost btn--sm" :aria-expanded="showOriginal" @click="showOriginal = !showOriginal">{{ showOriginal ? '收起启动参数' : '查看这次启动参数与权限' }}</button>
      </section>
      <fieldset v-show="!uncertain || showOriginal" class="stack launch-fields" :disabled="busy || uncertain">
      <div class="field"><label for="launch-cli">编程工具</label><select id="launch-cli" v-model="command" :disabled="busy"><option value="">选择已授权工具</option><option v-for="tool in tools" :key="tool.id" :value="tool.id">{{ tool.label }}</option></select></div>
      <div class="field"><label for="launch-cwd">任务工作目录</label><input id="launch-cwd" v-model="cwd" :disabled="busy" autocapitalize="none" spellcheck="false" /></div>
      <div class="field"><label for="launch-name">任务名称</label><input id="launch-name" v-model="name" :disabled="busy" maxlength="160" placeholder="留空自动使用项目目录和工具名，同名加编号" /></div>
      <NativeLaunchModePicker v-if="selected?.kind === 'tui'" id="launch-native-mode" v-model="launchMode" :modes="selected?.native_launch_modes" :note="selected?.native_launch_note" :disabled="busy" />
      <div class="field"><label for="launch-mode">工作方式</label><select id="launch-mode" v-model="isolated" :disabled="busy"><option :value="false">使用原目录</option><option :value="true">创建独立 Git 工作区</option></select><span class="field__hint">{{ isolated ? '从当前提交创建新分支；原目录的未提交改动不会带入。启动后显示实际分支和目录。' : '新对话直接修改原目录，与该目录的其他会话共享文件。' }}</span></div>
      <div v-if="selected?.kind === 'tui'" class="field"><label for="launch-template">任务模板</label><select id="launch-template" v-model="task" :disabled="busy"><option value="">自由对话</option><option v-for="item in TASK_TEMPLATES" :key="item.id" :value="item.id">{{ item.label }}</option></select><span class="field__hint">只预填终端输入框，由你编辑并发送。</span></div>
      </fieldset>
      <p v-if="!uncertain" class="small dim launch-summary">{{ devices.byId(deviceId)?.name }} · {{ selected?.label || '未选择工具' }} · {{ isolated ? '新建独立分支' : '原目录' }}<br />{{ cwd }}</p>
      <p v-if="reason" class="notice notice--warn" role="status">{{ reason }}</p>
      <p v-if="error && !uncertain" class="notice notice--err" role="alert">{{ error }}</p>
      <button v-if="!uncertain" class="btn btn--primary" :disabled="busy || !!reason">{{ busy ? '正在启动任务…' : '启动任务' }}</button>
    </form>
  </section>
</template>

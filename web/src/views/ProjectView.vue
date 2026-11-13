<script setup lang="ts">
import { computed, nextTick, onMounted, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { usePreferencesStore } from '../stores/preferences'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { useDraftsStore } from '../stores/drafts'
import { canRecoverConversation } from '../lib/conversationSessions'
import { inProject } from '../lib/projectWork'
import { isLiveStatus, type SessionSummary } from '../lib/protocol'
import { statusLabel, absoluteTime } from '../lib/format'
import { nativeCLI } from '../lib/sessionQuota'
import ConversationRecovery from '../components/ConversationRecovery.vue'
import WorkSummary from '../components/WorkSummary.vue'
import TaskLaunch from '../components/TaskLaunch.vue'
import ModalDialog from '../components/ModalDialog.vue'

const props = defineProps<{ projectKey: string }>()
const preferences = usePreferencesStore(), devices = useDevicesStore(), sessions = useSessionsStore(), conn = useConnStore(), drafts = useDraftsStore()
const project = computed(() => preferences.projects.find(p => p.key === props.projectKey))
const device = computed(() => project.value ? devices.byId(project.value.device_id) : undefined)
const viewKey = `project-view:${props.projectKey}`
const sections = [{ id: 'sessions', label: '对话与终端' }, { id: 'changes', label: '改动与核对' }, { id: 'settings', label: '项目设置' }]
const saved = drafts.get(viewKey)
const section = ref(sections.some(s => s.id === saved) ? saved : 'sessions')
watch(section, value => drafts.set(viewKey, value))
const search = ref(''), launch = ref(false), recovering = ref<SessionSummary | null>(null), label = ref('')
const removed = ref(false)
const launchBusy = ref(false)
const undoButton = ref<HTMLButtonElement | null>(null)
const records = computed(() => project.value ? sessions.forDevice(project.value.device_id).filter(s => !s.archived && inProject(s.cwd, project.value!.cwd, device.value?.platform)).sort((a,b) => (b.last_attached_at || b.created_at) - (a.last_attached_at || a.created_at)) : [])
const shown = computed(() => records.value.filter(s => `${s.name} ${s.command}`.toLocaleLowerCase().includes(search.value.trim().toLocaleLowerCase())))
const now = ref(Date.now())
const presence = computed(() => project.value && conn.isOpen ? devices.onlineState(project.value.device_id, now.value) : 'unknown')
const reason = computed(() => !device.value ? '原设备不可用，请检查设备绑定。' : !conn.isOpen ? '连接正在恢复，请稍候。' : presence.value === 'unknown' ? '设备状态待确认，请刷新核对后开始或恢复对话。' : presence.value === 'offline' ? '设备离线，上线后可开始或恢复对话。' : '')
const listState = computed(() => project.value ? sessions.loadStates[project.value.device_id] : undefined)
const liveConfirmed = computed(() => !!project.value && sessions.sources[project.value.device_id] === 'live' && !!listState.value?.checkedAt && now.value - listState.value.checkedAt < 45000)
function toolName(s: SessionSummary): string {
  const tool = nativeCLI(s.command, s.args)
  return ({ codex: 'Codex CLI', claude: 'Claude Code', opencode: 'OpenCode', dsh: 'DeepSeek Harness' } as Record<string,string>)[tool] || s.command.replace(/\\/g, '/').split('/').at(-1) || '终端'
}
const projectSnapshot = ref(project.value)
let timer: ReturnType<typeof setInterval> | undefined
async function refresh(): Promise<void> {
  await Promise.all([preferences.load(), devices.load(true)])
  now.value = Date.now()
  if (project.value) { projectSnapshot.value = project.value; await sessions.load(project.value.device_id, true); now.value = Date.now() }
}
function onVisible(): void { if (!document.hidden) { now.value = Date.now(); void refresh() } }
watch(() => conn.isOpen, open => { now.value = Date.now(); if (open) void refresh() })
onMounted(() => { void refresh(); timer = setInterval(() => { now.value = Date.now(); if (!document.hidden) void refresh() }, 15000); document.addEventListener('visibilitychange', onVisible) })
onBeforeUnmount(() => { clearInterval(timer); document.removeEventListener('visibilitychange', onVisible) })
async function rename(): Promise<void> {
  if (!project.value || !label.value.trim()) return
  const { key, ...value } = project.value
  try { await preferences.change(key, { ...value, label: label.value.trim() }); label.value = '' } catch { /* Store retains error. */ }
}
async function remove(): Promise<void> {
  if (!project.value) return
  projectSnapshot.value = project.value
  try { await preferences.change(project.value.key); removed.value = true; await nextTick(); undoButton.value?.focus() } catch { /* Store retains error. */ }
}
async function undo(): Promise<void> {
  if (!projectSnapshot.value) return
  const { key, ...value } = projectSnapshot.value
  try { await preferences.change(key, value); removed.value = false } catch { /* Store retains error. */ }
}
</script>
<template>
  <main class="page page--wide project-workspace">
    <RouterLink class="workspace-back" :to="{ name: 'projects' }">← 所有项目</RouterLink>
    <p v-if="preferences.error" class="notice notice--err" role="alert">{{ preferences.error }} <button class="btn btn--sm" @click="preferences.load(true)">重新读取项目</button></p>
    <section v-if="removed" class="empty" role="status"><h1>已移除项目入口</h1><p>原生对话和项目文件保留。</p><button ref="undoButton" class="btn" :disabled="preferences.saving" @click="undo">撤销移除</button></section>
    <p v-else-if="!project && preferences.loading" role="status">正在读取项目…</p>
    <section v-else-if="!project" class="empty"><h1>项目入口不可用</h1><p>{{ preferences.error ? '读取失败，请重试；无法确认项目是否存在。' : '入口可能已被移除，请返回项目列表重新选择。' }}</p></section>
    <template v-else>
      <header class="workspace-heading">
        <div><p class="projects-eyebrow">项目工作空间</p><h1>{{ project.label }}</h1><p class="dim small">{{ device?.name || '原设备不可用' }} · {{ presence === 'unknown' ? '设备状态待确认' : presence === 'online' ? '设备在线' : '设备离线' }} · {{ project.command_id }}</p></div>
        <div class="row"><RouterLink class="btn" :to="{ name: 'tasks', query: { device: project.device_id, cwd: project.cwd } }">后台任务</RouterLink><button class="btn btn--primary" :disabled="!!reason" @click="launch = true">开始工作</button></div>
      </header>
      <p v-if="reason" class="notice small" role="status">{{ reason }} 已有记录仍可查看。</p>
      <details class="workspace-context"><summary>工作目录与设备</summary><p class="mono">{{ project.cwd }}</p><RouterLink :to="{ name: 'device', params: { id: project.device_id } }">管理设备与工具</RouterLink></details>
      <nav class="workspace-sections" aria-label="项目内容"><button v-for="s in sections" :key="s.id" class="btn btn--ghost" :aria-pressed="section === s.id" @click="section = s.id">{{ s.label }}</button></nav>
      <section v-if="section === 'sessions'" aria-labelledby="project-sessions-heading" class="workspace-content">
        <div class="row row--between"><h2 id="project-sessions-heading">对话与终端 <span class="projects-count">{{ records.length }}</span></h2><button class="btn btn--ghost btn--sm" :disabled="listState?.refreshing || devices.loading" @click="refresh">{{ listState?.refreshing || devices.loading ? '正在核对…' : '刷新记录' }}</button></div>
        <div class="field"><label for="project-session-search">查找项目内会话</label><input id="project-session-search" v-model="search" type="search" placeholder="搜索名称或工具…" /></div>
        <p v-if="listState?.error || devices.error" class="notice notice--err" role="alert">{{ listState?.error || devices.error }}。请刷新重试，下方保留已有记录。</p>
        <p v-if="listState?.checkedAt" class="small dim" role="status">最近确认：{{ absoluteTime(listState.checkedAt) }}{{ listState.refreshing ? ' · 正在后台核对' : '' }}{{ !liveConfirmed ? ' · 当前为缓存快照，请刷新核对实时状态' : '' }}</p>
        <p v-else-if="sessions.sources[project.device_id] === 'cache'" class="small dim" role="status">当前为缓存快照；实时状态确认后才能接回运行中的终端。</p>
        <p v-if="(listState?.refreshing || devices.loading || !sessions.sources[project.device_id]) && !records.length" class="empty" role="status">正在读取项目会话…</p>
        <p v-else-if="!shown.length && !listState?.error && !devices.error" class="empty">{{ search.trim() ? '没有匹配的会话。' : '此目录还没有会话，选择“开始工作”创建。' }}</p>
        <article v-for="s in shown" :key="s.session_id" class="workspace-session">
          <div class="workspace-session__summary"><h3>{{ s.name || toolName(s) }}</h3><p class="small dim">{{ toolName(s) }} · {{ statusLabel(s.status) }}{{ isLiveStatus(s.status) && !liveConfirmed ? '（上次记录）' : '' }}</p><details class="session-command"><summary>启动命令</summary><p class="mono small">{{ s.command }}</p></details></div>
          <div class="workspace-session__actions" :class="{ 'workspace-session__actions--notice': isLiveStatus(s.status) && (!liveConfirmed || !!reason) }">
          <RouterLink v-if="isLiveStatus(s.status) && !reason && liveConfirmed" class="btn" :to="{ name: 'session', params: { id: s.session_id }, query: { project: project.key } }">接回终端</RouterLink>
          <button v-else-if="!isLiveStatus(s.status) && canRecoverConversation(s)" class="btn" :disabled="!!reason" @click="recovering = s">恢复原对话</button>
          <RouterLink v-else-if="!isLiveStatus(s.status)" class="btn btn--ghost" :to="{ name: 'session', params: { id: s.session_id }, query: { project: project.key } }">查看记录</RouterLink>
          <span v-else class="small dim">{{ reason || '状态待确认，请刷新' }}</span>
          </div>
        </article>
      </section>
      <section v-else-if="section === 'changes'" class="workspace-content"><h2>改动与核对</h2><p class="small dim">执行结束与检查通过分别核对，结果由你验收。</p><WorkSummary :device-id="project.device_id" :cwd="project.cwd" :online="!!device?.online && conn.isOpen" :project-key="project.key" /></section>
      <section v-else class="workspace-content stack" aria-labelledby="project-settings-heading"><h2 id="project-settings-heading">项目设置</h2><form class="stack" @submit.prevent="rename"><label class="field">项目名称<input v-model="label" :placeholder="project.label" maxlength="160" :disabled="preferences.saving" /></label><button class="btn" :disabled="preferences.saving || !label.trim()">保存名称</button></form><div class="row"><RouterLink class="btn" :to="{ name: 'workspaces', params: { id: project.device_id }, query: { path: project.cwd } }">管理独立工作区</RouterLink><button class="btn btn--ghost" :disabled="preferences.saving" @click="remove">移除项目入口</button></div><p class="small dim">移除入口保留文件与对话，并可撤销。</p></section>
      <ModalDialog v-if="launch" title-id="launch-title" :busy="launchBusy" @close="launch = false"><TaskLaunch :device-id="project.device_id" :cwd="project.cwd" :label="project.label" :command-id="project.command_id" :project-key="project.key" @busy="launchBusy = $event" @close="launch = false" /></ModalDialog>
      <ConversationRecovery v-if="recovering" :source="recovering" :project-key="project.key" @close="recovering = null" />
    </template>
  </main>
</template>

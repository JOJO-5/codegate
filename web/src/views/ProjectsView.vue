<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { usePreferencesStore } from '../stores/preferences'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { estimateTerminalSize } from '../lib/viewport'
import { humanizeError, statusLabel } from '../lib/format'
import { isLiveStatus } from '../lib/protocol'
import type { ProjectPreference } from '../lib/api'

const preferences = usePreferencesStore()
const devices = useDevicesStore()
const sessions = useSessionsStore()
const conn = useConnStore()
const router = useRouter()
const query = ref('')
const opening = ref('')
const expanded = ref('')
const error = ref('')
const editing = ref('')
const label = ref('')
const confirmRemove = ref('')
const visible = computed(() => preferences.projects.filter(p =>
  [p.label, p.cwd, devices.byId(p.device_id)?.name].some(v => v?.toLocaleLowerCase().includes(query.value.trim().toLocaleLowerCase()))))
async function refresh(): Promise<void> {
  await Promise.all([preferences.load(true), devices.load(true)])
}
onMounted(refresh)
async function showSessions(project: ProjectPreference & { key: string }): Promise<void> {
  expanded.value = expanded.value === project.key ? '' : project.key
  if (expanded.value) await sessions.load(project.device_id, true)
}
async function create(project: ProjectPreference & { key: string }): Promise<void> {
  error.value = ''
  if (!conn.isOpen) { error.value = '连接尚未就绪，请稍后重试。'; return }
  opening.value = project.key
  try {
    const size = estimateTerminalSize()
    const session = await sessions.create(project.device_id, {
      commandId: project.command_id, cwd: project.cwd, name: project.name || project.label,
      cols: size.cols, rows: size.rows,
    })
    await router.push({ name: 'session', params: { id: session.session_id } })
  } catch (e) { error.value = humanizeError(e) }
  finally { opening.value = '' }
}
async function rename(project: ProjectPreference & { key: string }): Promise<void> {
  if (!label.value.trim()) return
  try {
    await preferences.change(project.key, { label: label.value.trim(), device_id: project.device_id, command_id: project.command_id, cwd: project.cwd, name: project.name })
    editing.value = ''
  } catch { /* The store exposes the sync error. */ }
}
async function remove(key: string): Promise<void> {
  if (confirmRemove.value !== key) { confirmRemove.value = key; return }
  try { await preferences.change(key); confirmRemove.value = '' }
  catch { /* The store exposes the sync error. */ }
}
</script>

<template>
  <main class="page">
    <div class="row row--between"><h1>项目</h1><div class="row"><RouterLink class="btn btn--primary btn--sm" to="/devices">从设备添加项目</RouterLink><button class="btn btn--sm" type="button" :disabled="preferences.loading" @click="refresh">刷新</button></div></div>
    <p class="dim">选择设备、工具和工作目录，保存后可在手机与电脑上使用。</p>
    <div v-if="error || preferences.error" class="notice notice--err" role="alert">{{ error || preferences.error }}</div>
    <div class="field"><label for="project-query">查找项目</label><input id="project-query" v-model="query" type="search" placeholder="项目名、目录或设备名" /></div>
    <div v-if="preferences.loading" class="empty"><span class="spinner" />正在加载项目…</div>
    <div v-else-if="visible.length === 0" class="empty">{{ query.trim() ? '没有匹配的项目。' : '还没有项目。在设备页填写工具和工作目录，点击“保存为项目”。' }}</div>
    <div class="project-grid">
      <section v-for="project in visible" :key="project.key" class="card project-card">
        <div class="card__title"><h2>{{ project.label }}</h2><span class="badge" :class="devices.byId(project.device_id)?.online ? 'badge--ok' : 'badge--idle'">{{ devices.byId(project.device_id)?.online ? '在线' : devices.byId(project.device_id) ? '离线' : '设备已解绑' }}</span></div>
        <p class="dim small">{{ devices.byId(project.device_id)?.name || '原设备不可用' }} · {{ project.command_id }}</p>
        <p class="mono project-path">{{ project.cwd }}</p>
        <div class="row">
          <button class="btn btn--primary" type="button" :disabled="!!opening || !conn.isOpen || !devices.byId(project.device_id)?.online" @click="create(project)">{{ opening === project.key ? '正在新建…' : '新建独立会话' }}</button>
          <button class="btn" type="button" :aria-expanded="expanded === project.key" @click="showSessions(project)">接回已有会话</button>
        </div>
        <div v-if="expanded === project.key" class="project-sessions">
          <span v-if="sessions.loading" class="spinner" />
          <div v-if="sessions.error" class="notice notice--err">{{ sessions.error }}</div>
          <p class="small dim">此目录下的会话；结束的进程无法接回，新建会启动新的对话。</p>
          <div v-for="session in sessions.forDevice(project.device_id).filter(s => s.cwd === project.cwd && !s.archived)" :key="session.session_id" class="row row--between">
            <span>{{ session.name || session.command }} <small class="dim">{{ statusLabel(session.status) }}</small></span>
            <RouterLink v-if="isLiveStatus(session.status) && devices.byId(project.device_id)?.online" class="btn btn--sm" :to="{ name: 'session', params: { id: session.session_id } }">接回</RouterLink>
          </div>
          <p v-if="!sessions.loading && !sessions.forDevice(project.device_id).some(s => s.cwd === project.cwd && !s.archived)" class="small dim">此目录还没有会话。</p>
        </div>
        <div class="row project-management">
          <RouterLink v-if="devices.byId(project.device_id)" class="btn btn--ghost btn--sm" :to="{ name: 'device', params: { id: project.device_id } }">设备与工具</RouterLink>
          <button class="btn btn--ghost btn--sm" type="button" @click="editing = editing === project.key ? '' : project.key; label = project.label">{{ editing === project.key ? '取消改名' : '改名' }}</button>
          <button class="btn btn--ghost btn--sm" type="button" :disabled="preferences.saving" @click="remove(project.key)">{{ confirmRemove === project.key ? '确认移除项目' : '移除' }}</button>
          <button v-if="confirmRemove === project.key" class="btn btn--ghost btn--sm" type="button" @click="confirmRemove = ''">取消</button>
        </div>
        <form v-if="editing === project.key" class="row" @submit.prevent="rename(project)"><input v-model="label" aria-label="项目名称" maxlength="160" /><button class="btn btn--sm" :disabled="preferences.saving || !label.trim()">保存</button></form>
        <p v-if="confirmRemove === project.key" class="small dim">只移除项目入口，文件和正在运行的会话会保留。</p>
      </section>
    </div>
  </main>
</template>

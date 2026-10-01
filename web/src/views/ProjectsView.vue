<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { useRepositoriesStore } from '../stores/repositories'
import { usePreferencesStore } from '../stores/preferences'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { estimateTerminalSize } from '../lib/viewport'
import { humanizeError, statusLabel } from '../lib/format'
import { isLiveStatus } from '../lib/protocol'
import type { ProjectPreference } from '../lib/api'

const repositories = useRepositoriesStore()
const repoCommands = ref<Record<string, string>>({})
let repositoryTimer: ReturnType<typeof setInterval> | undefined
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
const discovered = computed(() => devices.items.flatMap(device => (repositories.results[device.id]?.repositories ?? []).map(repo => ({ ...repo, device_id: device.id, device_name: device.name }))).filter(repo => [repo.name, repo.path, repo.device_name].some(value => value.toLocaleLowerCase().includes(query.value.trim().toLocaleLowerCase()))))
function commandFor(id: string): string { return (repositories.commands[id] ?? []).some(c => c.id === repoCommands.value[id]) ? repoCommands.value[id]! : repositories.commands[id]?.[0]?.id ?? '' }
async function loadRepositories(rescan = false): Promise<void> {
  await Promise.all(devices.items.filter(device => device.online).map(device => repositories.load(device.id, rescan)))
}
async function refresh(): Promise<void> {
  await Promise.all([preferences.load(true), devices.load(true)])
  await loadRepositories()
}
onMounted(() => { void refresh(); repositoryTimer = setInterval(() => { void loadRepositories() }, 5000) })
onUnmounted(() => { if (repositoryTimer) clearInterval(repositoryTimer) })
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
  <main class="page page--wide projects-page">
    <div class="projects-heading">
      <div><p class="projects-eyebrow">你的工作区</p><h1>项目 <span class="projects-count">{{ preferences.projects.length }}</span></h1><p class="dim">从熟悉的目录开始，随时开启或接回对话。</p></div>
      <div class="projects-heading__actions"><button class="btn btn--ghost btn--sm" type="button" :disabled="preferences.loading" @click="refresh">刷新</button><RouterLink class="btn btn--primary" to="/devices">＋ 添加项目</RouterLink></div>
    </div>
    <div v-if="error || preferences.error" class="notice notice--err" role="alert">{{ error || preferences.error }}</div>
    <div class="field projects-search"><label for="project-query">查找项目</label><input id="project-query" v-model="query" type="search" placeholder="搜索项目、目录或设备…" /></div>
    <h2 class="projects-section-heading">保存的项目</h2>
    <div v-if="preferences.loading" class="empty"><span class="spinner" />正在加载项目…</div>
    <div v-else-if="visible.length === 0" class="empty">{{ query.trim() ? '没有匹配的项目。' : '还没有保存的项目。可以从下方发现的仓库开始，或在设备页保存常用入口。' }}</div>
    <div class="project-grid">
      <section v-for="project in visible" :key="project.key" class="card project-card">
        <div class="card__title"><div class="project-identity"><span class="project-avatar" aria-hidden="true">{{ project.label.slice(0, 1).toLocaleUpperCase() }}</span><h2>{{ project.label }}</h2></div><span class="badge" :class="devices.byId(project.device_id)?.online ? 'badge--ok' : 'badge--idle'">{{ devices.byId(project.device_id)?.online ? '在线' : devices.byId(project.device_id) ? '离线' : '设备已解绑' }}</span></div>
        <p class="dim small">{{ devices.byId(project.device_id)?.name || '原设备不可用' }} · {{ project.command_id }}</p>
        <p class="mono project-path">{{ project.cwd }}</p>
        <div class="row project-launch">
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
    <section class="repository-discovery" aria-labelledby="repositories-heading">
      <div class="row row--between"><h2 id="repositories-heading">发现的 Git 仓库 <span class="projects-count">{{ discovered.length }}</span></h2><button class="btn btn--sm" type="button" :disabled="Object.values(repositories.loading).some(Boolean) || devices.items.some(d => d.online && repositories.results[d.id]?.state === 'scanning') || !devices.onlineCount" @click="loadRepositories(true)">重新扫描</button></div>
      <p class="dim small">自动发现在线设备允许目录内的仓库。选择工具，在仓库目录中开启新对话。</p>
      <div v-for="device in devices.items.filter(d => d.online)" :key="device.id" class="repository-status">
        <p v-if="repositories.errors[device.id]" class="notice notice--warn" role="status">{{ device.name }}：{{ repositories.errors[device.id] }}</p>
        <p v-else-if="repositories.results[device.id]?.state === 'scanning' || !repositories.results[device.id]" class="dim small" role="status">{{ device.name }}：正在扫描允许的工作目录…</p>
        <details v-if="repositories.results[device.id]?.partial" class="notice notice--warn"><summary>{{ device.name }}：扫描未完成，共 {{ repositories.results[device.id]?.error_count }} 处问题</summary><p v-for="warning in repositories.results[device.id]?.warnings" :key="warning" class="small repository-warning">{{ warning }}</p></details>
      </div>
      <div v-if="!discovered.length && !Object.values(repositories.loading).some(Boolean)" class="empty">{{ query.trim() ? '没有匹配的仓库。' : devices.onlineCount ? '尚未发现 Git 仓库。请确认仓库位于目标设备允许的目录内，并查看上方扫描状态。' : '连接设备后自动发现其允许目录下的 Git 仓库。' }}</div>
      <div class="project-grid">
        <section v-for="repo in discovered" :key="`${repo.device_id}:${repo.path}`" class="card project-card repository-card">
          <div class="card__title"><div class="project-identity"><span class="project-avatar" aria-hidden="true">{{ repo.name.slice(0, 1).toLocaleUpperCase() }}</span><h3>{{ repo.name }}</h3></div><span class="badge">Git</span></div>
          <p class="dim small">{{ repo.device_name }}{{ devices.byId(repo.device_id)?.online ? '' : ' · 离线' }}</p>
          <p class="mono project-path">{{ repo.path }}</p>
          <div class="field"><label :for="`repo-cli-${repo.device_id}-${repo.path}`">编程 CLI</label><select :id="`repo-cli-${repo.device_id}-${repo.path}`" :value="commandFor(repo.device_id)" :disabled="!devices.byId(repo.device_id)?.online" @change="repoCommands[repo.device_id] = ($event.target as HTMLSelectElement).value"><option v-if="!repositories.commands[repo.device_id]?.length" value="">请先在设备页授权已安装的工具</option><option v-for="tool in repositories.commands[repo.device_id]" :key="tool.id" :value="tool.id">{{ tool.label }}</option></select></div>
          <div class="row project-launch"><button class="btn btn--primary" type="button" :disabled="!!opening || !conn.isOpen || !devices.byId(repo.device_id)?.online || !commandFor(repo.device_id)" @click="create({ key: `${repo.device_id}:${repo.path}`, label: repo.name, name: repo.name, device_id: repo.device_id, command_id: commandFor(repo.device_id), cwd: repo.path })">{{ opening === `${repo.device_id}:${repo.path}` ? '正在新建…' : '新建对话' }}</button><RouterLink class="btn btn--ghost" :to="{ name: 'device', params: { id: repo.device_id } }">设备与工具</RouterLink></div>
        </section>
      </div>
    </section>
  </main>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useRepositoriesStore } from '../stores/repositories'
import { usePreferencesStore } from '../stores/preferences'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { statusLabel } from '../lib/format'
import { isLiveStatus } from '../lib/protocol'
import AddProject from '../components/AddProject.vue'
import TaskLaunch from '../components/TaskLaunch.vue'
import ConversationRecovery from '../components/ConversationRecovery.vue'
import { canRecoverConversation } from '../lib/conversationSessions'
import type { SessionSummary } from '../lib/protocol'
import type { ProjectPreference } from '../lib/api'

const repositories = useRepositoriesStore()
const repoCommands = ref<Record<string, string>>({})
let repositoryTimer: ReturnType<typeof setInterval> | undefined
const preferences = usePreferencesStore()
const devices = useDevicesStore()
const sessions = useSessionsStore()
const conn = useConnStore()
const addingProject = ref(false)
const adding = ref(false)
const discoveryStarted = ref(false)
const showAllRecent = ref(false)
const query = ref('')
const error = ref('')
const launch = ref<(ProjectPreference & { key: string }) | null>(null)
const launchHost = ref<HTMLElement | null>(null)
let launchTrigger: HTMLElement | null = null
const recoveringSession = ref<SessionSummary | null>(null)
const scanBusy = computed(() => devices.items.some(d => d.online && (repositories.loading[d.id] || !repositories.results[d.id] || repositories.results[d.id]?.state === 'scanning')))
const scanFailed = computed(() => devices.items.some(d => d.online && !!repositories.errors[d.id]))
const recent = computed(() => devices.items.flatMap(d => sessions.forDevice(d.id).filter(s => !s.archived).map(s => ({ ...s, device_id: d.id, online: d.online }))).sort((a,b) => (b.last_attached_at || b.created_at) - (a.last_attached_at || a.created_at)))
const shownRecent = computed(() => recent.value.slice(0, showAllRecent.value ? 15 : 3))
function unavailable(id: string): string { return !devices.byId(id)?.online ? '设备离线，上线后可新建或接回。' : !conn.isOpen ? '连接正在恢复，请稍候。' : '' }
async function loadRecent(): Promise<void> { await Promise.all(devices.items.map(d => sessions.load(d.id))) }

const visible = computed(() => preferences.projects.filter(p =>
  [p.label, p.cwd, devices.byId(p.device_id)?.name].some(v => v?.toLocaleLowerCase().includes(query.value.trim().toLocaleLowerCase()))))
const discovered = computed(() => devices.items.flatMap(device => (repositories.results[device.id]?.repositories ?? []).map(repo => ({ ...repo, device_id: device.id, device_name: device.name }))).filter(repo => [repo.name, repo.path, repo.device_name].some(value => value.toLocaleLowerCase().includes(query.value.trim().toLocaleLowerCase()))))
function commandFor(id: string, path: string): string {
  const key = JSON.stringify([id, path])
  return (repositories.commands[id] ?? []).some(c => c.id === repoCommands.value[key]) ? repoCommands.value[key]! : repositories.commands[id]?.[0]?.id ?? '' }
async function loadRepositories(rescan = false): Promise<void> {
  await Promise.all(devices.items.filter(device => device.online).map(device => repositories.load(device.id, rescan)))
}
async function refresh(): Promise<void> {
  await Promise.all([preferences.load(true), devices.load(true)])
  await loadRecent()
  if (adding.value && discoveryStarted.value) await loadRepositories()
}
onMounted(() => { void refresh(); repositoryTimer = setInterval(() => { if (!document.hidden) { if (adding.value && discoveryStarted.value) void loadRepositories(); void loadRecent() } }, 15000) })
onUnmounted(() => { if (repositoryTimer) clearInterval(repositoryTimer) })
async function toggleAdding(): Promise<void> {
  adding.value = !adding.value
  await nextTick()
  document.getElementById(adding.value ? 'add-project-heading' : 'add-project-button')?.focus()
}
async function create(project: ProjectPreference & { key: string }): Promise<void> {
  launchTrigger = document.activeElement instanceof HTMLElement ? document.activeElement : null
  launch.value = project
  await nextTick()
  launchHost.value?.querySelector<HTMLElement>('#launch-title')?.focus({ preventScroll: true })
  launchHost.value?.scrollIntoView({ block: 'start' })
}
async function closeLaunch(): Promise<void> {
  const trigger = launchTrigger
  launchTrigger = null
  launch.value = null
  await nextTick()
  if (!launch.value && trigger?.isConnected) trigger.focus()
}
async function saveRepository(repo: { device_id: string; path: string; name: string }): Promise<void> {
  try { await preferences.saveProject({ label: repo.name, name: '', device_id: repo.device_id, cwd: repo.path, command_id: commandFor(repo.device_id, repo.path) }) }
  catch { /* The store exposes the sync error. */ }
}
</script>

<template>
  <main class="page page--wide projects-page">
    <AddProject v-if="addingProject" @close="addingProject = false" />
    <ConversationRecovery v-if="recoveringSession" :source="recoveringSession" @close="recoveringSession = null" />
    <div class="projects-heading">
      <div><p class="projects-eyebrow">工作从这里继续</p><h1>项目 <span class="projects-count">{{ preferences.projects.length }}</span></h1><p class="dim">进入一个项目，集中处理对话、任务与改动。</p></div>
      <div class="projects-heading__actions"><button class="btn btn--ghost btn--sm" type="button" :disabled="preferences.loading" @click="refresh">{{ preferences.loading ? '刷新中…' : '刷新' }}</button><button id="add-project-button" class="btn btn--primary" type="button" :aria-expanded="adding" aria-controls="add-project-area" @click="toggleAdding">＋ 添加项目</button></div>
    </div>
    <div v-if="error || preferences.error" class="notice notice--err" role="alert">{{ error || preferences.error }}</div>
    <section class="card stack recent-work" :class="{ 'recent-work--expanded': showAllRecent }" aria-labelledby="recent-heading">
      <div class="row row--between"><h2 id="recent-heading">最近工作</h2><button v-if="recent.length > 1" class="btn btn--ghost btn--sm" :class="{ 'recent-toggle--mobile': recent.length <= 3 }" :aria-expanded="showAllRecent" @click="showAllRecent = !showAllRecent">{{ showAllRecent ? '收起' : '查看更多' }}</button></div>
      <p v-if="sessions.loading && !recent.length" class="small dim" role="status">正在读取最近会话…</p>
      <p v-else-if="sessions.error || devices.error" class="notice notice--err" role="alert">{{ sessions.error || devices.error }}。请刷新重试，已有会话不会因此被删除。</p>
      <p v-else-if="!recent.length" class="small dim">还没有最近会话，进入项目后开始工作。</p>
      <p v-if="showAllRecent && recent.length > 15" class="small dim">这里展示最近 15 项；更早记录可在项目内查看。</p>
      <div v-for="item in shownRecent" :key="item.session_id" class="recent-work__item">
        <div><strong>{{ item.name || item.command }}</strong><p class="small dim">{{ devices.byId(item.device_id)?.name }} · {{ statusLabel(item.status) }}{{ sessions.sources[item.device_id] === 'cache' ? ' · 缓存快照' : '' }}</p></div>
        <RouterLink v-if="item.online && conn.isOpen && sessions.sources[item.device_id] === 'live' && isLiveStatus(item.status)" class="btn btn--sm" :to="{ name: 'session', params: { id: item.session_id } }">继续工作</RouterLink>
        <button v-else-if="!isLiveStatus(item.status) && canRecoverConversation(item)" class="btn btn--sm" type="button" :disabled="!item.online || !conn.isOpen" :title="unavailable(item.device_id)" @click="recoveringSession = item">恢复原对话</button>
        <span v-else class="small dim">{{ !item.online ? '设备离线' : !conn.isOpen ? '连接恢复中' : sessions.sources[item.device_id] === 'cache' ? '状态待确认' : '进程已结束' }}</span>
      </div>
    </section>
    <div v-if="launch" ref="launchHost" class="task-launch-host"><TaskLaunch :key="launch.key" :device-id="launch.device_id" :cwd="launch.cwd" :label="launch.label" :command-id="launch.command_id" @close="closeLaunch" /></div>
    <div class="field projects-search"><label for="project-query">查找项目</label><input id="project-query" v-model="query" type="search" placeholder="搜索项目、目录或设备…" /></div>
    <h2 class="projects-section-heading">保存的项目</h2>
    <div v-if="preferences.loading && !preferences.projects.length" class="empty"><span class="spinner" />正在加载项目…</div>
    <div v-else-if="preferences.error && visible.length === 0" class="empty">暂时无法确认保存的项目，请刷新重试。</div>
    <div v-else-if="visible.length === 0" class="empty">{{ query.trim() ? '没有匹配的项目。' : '还没有保存的项目，选择“添加项目”保存已有目录。' }}</div>
    <div class="project-grid">
      <section v-for="project in visible" :key="project.key" class="card project-card">
        <div class="card__title"><div class="project-identity"><span class="project-avatar" aria-hidden="true">{{ project.label.slice(0, 1).toLocaleUpperCase() }}</span><h2>{{ project.label }}</h2></div><span class="badge" :class="devices.byId(project.device_id)?.online ? 'badge--ok' : 'badge--idle'">{{ devices.byId(project.device_id)?.online ? '设备在线' : devices.byId(project.device_id) ? '设备离线' : '设备已解绑' }}</span></div>
        <p class="dim small">{{ devices.byId(project.device_id)?.name || '原设备不可用' }} · {{ project.command_id }}</p>
        <p class="mono project-path project-path--compact" :title="project.cwd">{{ project.cwd }}</p>
        <div class="row project-launch">
          <RouterLink class="btn btn--primary" :to="{ name: 'project', params: { key: project.key } }">进入项目 <span aria-hidden="true">→</span></RouterLink>
        </div>
        <p v-if="unavailable(project.device_id)" class="small dim">{{ unavailable(project.device_id) }}</p>
      </section>
    </div>
    <section v-if="adding" id="add-project-area" class="repository-discovery add-project-area" aria-labelledby="add-project-heading">
      <div class="row row--between"><h2 id="add-project-heading" tabindex="-1">添加项目</h2><button class="btn btn--ghost btn--sm" type="button" @click="toggleAdding">收起添加项目</button></div>
      <p class="dim small">保存已有目录，不创建或改动文件。</p>
      <div class="row"><button class="btn" type="button" @click="addingProject = true">填写已有目录</button><button class="btn" type="button" :disabled="!devices.onlineCount || scanBusy && discoveryStarted" @click="discoveryStarted = true; loadRepositories()">发现 Git 仓库</button></div>
      <p v-if="!devices.onlineCount" class="small dim">设备上线后可以发现仓库，也可先查看已保存的项目。</p>
      <details v-if="discoveryStarted" class="repository-browser" open>
      <summary><span>发现的 Git 仓库 <span class="projects-count">{{ discovered.length }}</span></span><span class="small dim">{{ scanFailed ? '扫描有问题，展开查看' : scanBusy ? '正在扫描…' : '浏览并添加项目' }}</span></summary>
      <div class="row row--between repository-toolbar"><p class="small dim">按目录选择工具，或保存为常用项目。</p><button class="btn btn--sm" type="button" :disabled="Object.values(repositories.loading).some(Boolean) || devices.items.some(d => d.online && repositories.results[d.id]?.state === 'scanning') || !devices.onlineCount" @click="loadRepositories(true)">重新扫描</button></div>
      <p class="dim small">自动发现在线设备允许目录内的仓库。选择工具，在仓库目录中开启新对话。</p>
      <div v-for="device in devices.items.filter(d => d.online)" :key="device.id" class="repository-status">
        <p v-if="repositories.errors[device.id]" class="notice notice--warn" role="status">{{ device.name }}：{{ repositories.errors[device.id] }}</p>
        <p v-else-if="repositories.results[device.id]?.state === 'scanning' || !repositories.results[device.id]" class="dim small" role="status">{{ device.name }}：正在扫描允许的工作目录…</p>
        <details v-if="repositories.results[device.id]?.partial" class="notice notice--warn"><summary>{{ device.name }}：扫描未完成，共 {{ repositories.results[device.id]?.error_count }} 处问题</summary><p v-for="warning in repositories.results[device.id]?.warnings" :key="warning" class="small repository-warning">{{ warning }}</p></details>
      </div>
      <div v-if="!discovered.length && !scanBusy && !scanFailed" class="empty">{{ query.trim() ? '没有匹配的仓库。' : devices.onlineCount ? '尚未发现 Git 仓库。请确认仓库位于目标设备允许的目录内，并查看上方扫描状态。' : '连接设备后自动发现其允许目录下的 Git 仓库。' }}</div>
      <div class="project-grid">
        <section v-for="repo in discovered" :key="`${repo.device_id}:${repo.path}`" class="card project-card repository-card">
          <div class="card__title"><div class="project-identity"><span class="project-avatar" aria-hidden="true">{{ repo.name.slice(0, 1).toLocaleUpperCase() }}</span><h3>{{ repo.name }}</h3></div><span class="badge">Git</span></div>
          <p class="dim small">{{ repo.device_name }}{{ devices.byId(repo.device_id)?.online ? '' : ' · 离线' }}</p>
          <p class="mono project-path">{{ repo.path }}</p>
          <RouterLink class="btn btn--ghost btn--sm" :to="{ name: 'workspaces', params: { id: repo.device_id }, query: { path: repo.path } }">管理独立工作区</RouterLink>
          <div class="field"><label :for="`repo-cli-${repo.device_id}-${repo.path}`">编程 CLI</label><select :id="`repo-cli-${repo.device_id}-${repo.path}`" :value="commandFor(repo.device_id, repo.path)" :disabled="!devices.byId(repo.device_id)?.online" @change="repoCommands[JSON.stringify([repo.device_id, repo.path])] = ($event.target as HTMLSelectElement).value"><option v-if="!repositories.commands[repo.device_id]?.length" value="">请先在设备页授权已安装的工具</option><option v-for="tool in repositories.commands[repo.device_id]" :key="tool.id" :value="tool.id">{{ tool.label }}</option></select></div>
          <p v-if="unavailable(repo.device_id) || !commandFor(repo.device_id, repo.path)" class="small dim">{{ unavailable(repo.device_id) || '请先在设备页授权已安装的工具。' }}</p>
          <div class="row project-launch"><button class="btn btn--primary" type="button" :disabled="!conn.isOpen || !devices.byId(repo.device_id)?.online || !commandFor(repo.device_id, repo.path)" @click="create({ key: `${repo.device_id}:${repo.path}`, label: repo.name, name: '', device_id: repo.device_id, command_id: commandFor(repo.device_id, repo.path), cwd: repo.path })">新建任务</button><button class="btn btn--sm" :disabled="preferences.saving || !commandFor(repo.device_id, repo.path)" @click="saveRepository(repo)">保存项目</button><RouterLink class="btn btn--ghost" :to="{ name: 'device', params: { id: repo.device_id } }">设备与工具 / 独立工作区</RouterLink></div>
        </section>
      </div>
      </details>
    </section>
  </main>
</template>

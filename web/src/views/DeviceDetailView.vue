<script setup lang="ts">
/**
 * 设备详情：会话列表 + 新建会话 + 设备管理。
 *
 * 会话列表的两个来源与回落顺序见 `stores/sessions.ts`。
 * 这里额外做一件事：**把列表标记为「实时」还是「缓存」**。
 * Agent 离线时列表来自数据库快照，用户必须知道这一点 ——
 * 否则他会对着一个显示"运行中"的会话点进去，然后发现连不上。
 */

import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { usePreferencesStore } from '../stores/preferences'
import { api, type DeviceUpdateStatus } from '../lib/api'
import { humanizeError, platformLabel, relativeTime, statusKind, statusLabel, toMs } from '../lib/format'
import { PRESETS, type CommandPreset } from '../lib/commands'
import { estimateTerminalSize } from '../lib/viewport'
import { MessageType, isLiveStatus, type SessionSummary } from '../lib/protocol'

const props = defineProps<{ id: string }>()

const devices = useDevicesStore()
const sessions = useSessionsStore()
const conn = useConnStore()
const preferences = usePreferencesStore()
const hasLegacyPreferences = ref(false)
const importingPreferences = ref(false)
const preferenceNotice = ref('')
async function importLegacyPreferences(): Promise<void> {
  importingPreferences.value = true
  try {
    const pins: unknown = JSON.parse(localStorage.getItem(`codegate.pins.${props.id}`) ?? '[]')
    if (Array.isArray(pins)) {
      for (const id of pins) {
        if (typeof id === 'string' && list.value.some(s => s.session_id === id) && !preferences.isPinned(id)) await preferences.change(`pin.${id}`, true)
      }
    }
    const saved: unknown = JSON.parse(localStorage.getItem(`codegate.shortcuts.${props.id}`) ?? '[]')
    if (Array.isArray(saved)) {
      for (const p of saved) {
        if (typeof p?.label !== 'string' || typeof p?.commandId !== 'string' || typeof p?.cwd !== 'string') continue
        if (preferences.projects.some(project => project.device_id === props.id && project.command_id === p.commandId && project.cwd === p.cwd)) continue
        await preferences.saveProject({ label: p.label, device_id: props.id, command_id: p.commandId, cwd: p.cwd, name: typeof p.name === 'string' ? p.name : '' })
      }
    }
    localStorage.removeItem(`codegate.pins.${props.id}`)
    localStorage.removeItem(`codegate.shortcuts.${props.id}`)
    hasLegacyPreferences.value = false
    preferenceNotice.value = '旧版组合与置顶已导入当前账号。'
  } catch (e) { sessionActionError.value = humanizeError(e) }
  finally { importingPreferences.value = false }
}
const router = useRouter()

const device = computed(() => devices.byId(props.id))
const list = computed(() => sessions.forDevice(props.id))
const showArchived = ref(false)
const sessionQuery = ref('')
const pinnedIds = computed(() => list.value.filter(s => preferences.isPinned(s.session_id)).map(s => s.session_id))
const visibleSessions = computed(() => {
  const query = sessionQuery.value.trim().toLocaleLowerCase()
  return list.value.filter(s => Boolean(s.archived) === showArchived.value &&
    (!query || [s.name, s.command, s.cwd].some(v => v?.toLocaleLowerCase().includes(query))))
    .sort((a, b) => Number(pinnedIds.value.includes(b.session_id)) - Number(pinnedIds.value.includes(a.session_id)) ||
      (toMs(b.created_at) ?? 0) - (toMs(a.created_at) ?? 0))
})
function togglePin(id: string): void {
  void preferences.togglePin(id).catch(() => {})
}
const archivedCount = computed(() => list.value.filter(s => s.archived).length)
const sessionActionError = ref<string | null>(null)
const deletingSession = ref<string | null>(null)
async function toggleArchive(s: SessionSummary): Promise<void> {
  sessionActionError.value = null
  try { await sessions.archive(props.id, s.session_id, !s.archived) }
  catch (e) { sessionActionError.value = humanizeError(e) }
}
async function deleteRecord(s: SessionSummary): Promise<void> {
  if (deletingSession.value !== s.session_id) { deletingSession.value = s.session_id; return }
  sessionActionError.value = null
  try {
    await sessions.remove(props.id, s.session_id)
    if (pinnedIds.value.includes(s.session_id)) togglePin(s.session_id)
    deletingSession.value = null
  }
  catch (e) { sessionActionError.value = humanizeError(e) }
}
const updateInfo = ref<DeviceUpdateStatus | null>(null)
let updateTimer: ReturnType<typeof setInterval> | null = null
let updateLoading = false
async function loadUpdateStatus(): Promise<void> {
  if (updateLoading) return
  updateLoading = true
  try {
    const status = await api.getDeviceUpdateStatus(props.id)
    updateInfo.value = status
    devices.patch(props.id, { online: status.online, ...(status.agent_version ? { agent_version: status.agent_version } : {}) })
  } catch {
    updateInfo.value = null
  } finally {
    updateLoading = false
  }
}
const updateLabel = computed(() => {
  const info = updateInfo.value
  if (info === null) return '状态暂不可用'
  if (!info.online) return '设备离线'
  const u = info.update
  if (u === null || !u.state) return '等待 Agent 心跳'
  if (!u.enabled || u.state === 'disabled') return '自动更新未启用'
  return ({
    waiting: '等待会话空闲',
    checking: '正在检查更新',
    current: '未发现新版本',
    staged: '更新包已校验',
    switching: '正在切换版本',
    error: '检查或切换失败',
  } as Record<string, string>)[u.state] ?? '未知状态'
})

// ---- 新建会话表单 ----
const presetId = ref('claude')
const cwd = ref('')
const sessionName = ref('')
const shortcuts = computed(() => preferences.projects.filter(p => p.device_id === props.id).map(p => ({ ...p, commandId: p.command_id })))
function saveShortcut(): void {
  const command = selectedPreset.value
  const path = cwd.value.trim()
  if (!command || !path) return
  const label = `${command.label} · ${path.split(/[\\/]/).filter(Boolean).pop() || path}`
  preferenceNotice.value = ''
  void preferences.saveProject({ label, device_id: props.id, command_id: command.id, cwd: path, name: sessionName.value.trim() }).then(() => {
    preferenceNotice.value = '已保存为项目，可从“项目”页打开，并在其他设备登录后使用。'
  }).catch(() => {})
}
function selectShortcut(index: number): void {
  const shortcut = shortcuts.value[index]
  if (!shortcut) return
  presetId.value = shortcut.commandId
  cwd.value = shortcut.cwd
  sessionName.value = shortcut.name
}
function removeShortcut(index: number): void {
  const shortcut = shortcuts.value[index]
  if (shortcut) void preferences.change(shortcut.key).catch(() => {})
}
const creating = ref(false)
const createError = ref<string | null>(null)

// The Agent reports what is configured and actually available on this host.
// Older Agents do not send an inventory; keep the legacy list with a warning.
const reportedCommands = computed(() => updateInfo.value?.commands)

/**
 * Agent 上报的 allowed_roots —— 工作目录白名单。
 *
 * 用途只有两个：本地没记住值时自动带出第一个根，以及给输入框提供候选。
 * 它不是授权依据 —— 真正的校验在 Agent 侧的 Workspace.Resolve。
 * 旧版 Agent 不上报此字段，列表为空时界面回落成纯手输。
 */
const allowedRoots = computed(() => updateInfo.value?.roots ?? [])
const cwdPlaceholder = computed(() =>
  allowedRoots.value[0] ?? '请填写工作目录的绝对路径（须在 Agent 的 allowed_roots 内）',
)
const selectableCommands = computed<CommandPreset[]>(() => {
  if (reportedCommands.value === undefined) return PRESETS
  return reportedCommands.value.filter(c => c.allowed && c.installed).map(c => ({
    id: c.id, label: c.label, kind: c.kind,
    hint: PRESETS.find(p => p.id === c.id)?.hint ?? '本机允许的命令',
    ...(c.resume ? { resume: true } : {}),
  }))
})
const knownTools = computed(() => (reportedCommands.value ?? []).filter(c =>
  ['claude', 'codex', 'opencode', 'dsh'].includes(c.id),
))
const toolBusy = ref('')
const toolError = ref('')
async function setTool(id: string, enabled: boolean): Promise<void> {
  toolBusy.value = id
  toolError.value = ''
  try {
    await api.setDeviceTool(props.id, id, enabled)
    await loadUpdateStatus()
  } catch (e) { toolError.value = humanizeError(e) }
  finally { toolBusy.value = '' }
}
const selectedPreset = computed(() => selectableCommands.value.find(p => p.id === presetId.value))
const selectedWebURL = computed(() => reportedCommands.value?.find(c => c.id === presetId.value && c.allowed)?.web_url)
const dshCommand = computed(() => reportedCommands.value?.find(c => c.id === 'dsh'))
const dshInstallCommand = 'dsh plugin --profile tui add github:deepseek-harness/turtle-ui'
const dshCopyStatus = ref('')
const dshOpening = ref(false)
const dshStopping = ref(false)
const dshOpenError = ref<string | null>(null)
const dshOpenURL = ref<string | null>(null)
async function openDSHWeb(): Promise<void> {
  dshOpenError.value = null
  dshOpenURL.value = null
  // Open on the user gesture so mobile popup blockers permit the new tab.
  const tab = window.open('', '_blank')
  if (tab) tab.opener = null
  dshOpening.value = true
  try {
    const result = await api.startDSHWeb(props.id)
    if (tab) tab.location.href = result.url
    else dshOpenURL.value = result.url
  } catch (e) {
    if (tab) tab.close()
    dshOpenError.value = humanizeError(e)
  } finally {
    dshOpening.value = false
  }
}
async function stopDSHWeb(): Promise<void> {
  dshOpenError.value = null
  dshStopping.value = true
  try {
    await api.stopDSHWeb(props.id)
    dshOpenURL.value = null
  } catch (e) {
    dshOpenError.value = humanizeError(e)
  } finally {
    dshStopping.value = false
  }
}
async function copyDshInstallCommand(): Promise<void> {
  try {
    await navigator.clipboard.writeText(dshInstallCommand)
    dshCopyStatus.value = '已复制安装命令'
  } catch {
    dshCopyStatus.value = '复制失败，请手动选择命令复制'
  }
}
watch(selectableCommands, commands => {
  if (!commands.some(c => c.id === presetId.value)) presetId.value = commands[0]?.id ?? ''
})

// 设备离线时首次取不到 roots（updateInfo 为 null），等心跳或下一次轮询补上。
watch(allowedRoots, roots => {
  if (cwd.value.trim() === '' && roots.length > 0) cwd.value = roots[0] ?? ''
})

/**
 * cwd 按设备记住上次用的值。
 *
 * 用户几乎总是从同一个目录开始（项目根）。每次都要重新输入一个
 * Windows 长路径，是这个界面里最烦人的一件事。
 */
const cwdKey = computed(() => `codegate.cwd.${props.id}`)

// ---- 设备管理 ----
const renaming = ref(false)
const nameDraft = ref('')
const manageError = ref<string | null>(null)
const confirmDelete = ref(false)

let unsubscribeControl: (() => void) | null = null

onMounted(async () => {
  await preferences.load(true)
  await devices.load()
  if (device.value !== undefined) {
    nameDraft.value = device.value.name
  }
  const saved = localStorage.getItem(cwdKey.value)
  if (saved !== null && saved !== '') cwd.value = saved

  await sessions.load(props.id)
  hasLegacyPreferences.value = !!(localStorage.getItem(`codegate.pins.${props.id}`) || localStorage.getItem(`codegate.shortcuts.${props.id}`))
  await loadUpdateStatus()

  // 本地没有记住的值时，用 Agent 配置里的第一个 allowed_root 兜底 ——
  // 否则每次新建会话都要手打一个 Windows 长路径。
  if (cwd.value.trim() === '') cwd.value = allowedRoots.value[0] ?? ''
  updateTimer = setInterval(() => { void loadUpdateStatus() }, 15_000)

  // 订阅会话生命周期事件，让列表保持最新。
  // ★ 必须在 onUnmounted 里取消订阅 —— 否则来回切换页面会累积监听器。
  unsubscribeControl = conn.onControl((env) => {
    const p = env.payload as Partial<SessionSummary> & { session_id?: string } | undefined
    const sid = p?.session_id
    if (sid === undefined || sid === '') return

    switch (env.type) {
      case MessageType.SessionExit:
        sessions.patch(sid, { status: 'exited' })
        break
      case MessageType.SessionClosed:
        sessions.patch(sid, { status: 'terminated' })
        break
      case MessageType.SessionDetached:
        sessions.patch(sid, { status: 'detached' })
        break
      default:
        break
    }
  })
})

onUnmounted(() => {
  unsubscribeControl?.()
  unsubscribeControl = null
  if (updateTimer !== null) clearInterval(updateTimer)
  updateTimer = null
})

function describeSession(s: SessionSummary): string {
  const parts: string[] = []
  if (s.command !== '') parts.push(s.command)
  if (s.cwd !== '') parts.push(s.cwd)
  if (s.status === 'exited' && s.exit_code !== null && s.exit_code !== undefined) {
    parts.push(`退出码 ${s.exit_code}`)
  } else if (s.pid !== undefined && s.pid > 0) {
    parts.push(`pid ${s.pid}`)
  }
  return parts.join(' · ')
}

function createdText(s: SessionSummary): string {
  const ms = toMs(s.created_at)
  return ms === null ? '' : relativeTime(ms)
}

async function createSession(): Promise<void> {
  createError.value = null

  if (!conn.isOpen) {
    createError.value = 'WebSocket 未连接，无法新建会话。请等待连接恢复后重试。'
    return
  }
  if (cwd.value.trim() === '') {
    createError.value = '请填写工作目录（cwd）。它必须在 Agent 的 allowed_roots 内。'
    return
  }

  const preset = selectedPreset.value
  if (preset === undefined) {
    createError.value = '请选择要启动的命令'
    return
  }

  creating.value = true
  try {
    const size = estimateTerminalSize()
    const name = sessionName.value.trim()
    const s = await sessions.create(props.id, {
      commandId: preset.id,
      cwd: cwd.value.trim(),
      cols: size.cols,
      rows: size.rows,
      ...(name === '' ? {} : { name }),
      // 新建始终是全新 CLI 对话；接回原会话请使用下方的会话列表。
    })

    localStorage.setItem(cwdKey.value, cwd.value.trim())
    sessionName.value = ''
    await router.push({ name: 'session', params: { id: s.session_id } })
  } catch (e) {
    createError.value = humanizeError(e)
  } finally {
    creating.value = false
  }
}

async function doRename(): Promise<void> {
  manageError.value = null
  const name = nameDraft.value.trim()
  if (name === '') {
    manageError.value = '设备名不能为空'
    return
  }
  try {
    await devices.rename(props.id, name)
    renaming.value = false
  } catch (e) {
    manageError.value = humanizeError(e)
  }
}

async function doDelete(): Promise<void> {
  manageError.value = null
  try {
    await devices.remove(props.id)
    await router.replace({ name: 'devices' })
  } catch (e) {
    manageError.value = humanizeError(e)
    confirmDelete.value = false
  }
}

function openSession(s: SessionSummary): void {
  if (!canOpen(s)) return
  void router.push({ name: 'session', params: { id: s.session_id } })
}

/** 会话是否还活着（可进入）。 */
function canOpen(s: SessionSummary): boolean {
  return isLiveStatus(s.status) && device.value?.online === true
}
</script>

<template>
  <main class="page device-page">
    <div class="device-heading">
      <div class="row row--between">
      <div class="row">
        <RouterLink class="btn btn--ghost btn--sm" :to="{ name: 'devices' }">‹ 设备</RouterLink>
        <h1 style="margin: 0">{{ device?.name ?? '设备' }}</h1>
      </div>
      <button class="btn btn--sm" type="button" :disabled="sessions.loading" @click="sessions.load(id, true)">
        <span v-if="sessions.loading" class="spinner" />
        刷新
      </button>
      </div>
      <p class="device-heading__summary"><span class="health__dot" :style="{ background: device?.online ? 'var(--ok)' : 'var(--fg-faint)' }" />{{ device?.online ? '设备在线' : '设备离线' }}<span v-if="device">· {{ platformLabel(device.platform) }} · Agent {{ device.agent_version || '—' }}</span></p>
    </div>

    <div v-if="preferences.error" class="notice notice--err">偏好同步失败：{{ preferences.error }} <button class="btn btn--sm" type="button" @click="preferences.load(true)">重试</button></div>
    <div v-if="hasLegacyPreferences" class="notice notice--info">此浏览器有旧版置顶或启动组合。<button class="btn btn--sm" type="button" :disabled="importingPreferences || preferences.saving" @click="importLegacyPreferences">{{ importingPreferences ? '正在导入…' : '导入到当前账号' }}</button></div>
    <div v-if="preferenceNotice" class="notice notice--info" role="status">{{ preferenceNotice }}</div>
    <div class="device-workspace">
    <!-- ---- 新建会话 ---- -->
    <div class="card device-create">
      <div class="card__title">
        <h2>新建会话</h2>
        <span v-if="!conn.isOpen" class="badge badge--warn">连接未就绪</span>
      </div>

      <form class="stack" @submit.prevent="createSession">
        <div v-if="shortcuts.length" class="field">
          <label for="launch-shortcut">常用启动组合</label>
          <div class="row">
            <select id="launch-shortcut" aria-label="选择常用启动组合" @change="selectShortcut(Number(($event.target as HTMLSelectElement).value)); ($event.target as HTMLSelectElement).value = ''">
              <option value="">选择组合填入下方表单</option>
              <option v-for="(shortcut, index) in shortcuts" :key="index" :value="index">{{ shortcut.label }}</option>
            </select>
            <button v-if="shortcuts.some(s => s.commandId === presetId && s.cwd === cwd.trim())" class="btn btn--sm" type="button" @click="removeShortcut(shortcuts.findIndex(s => s.commandId === presetId && s.cwd === cwd.trim()))">移除</button>
          </div>
          <span class="field__hint">组合随账号同步，并显示在“项目”页。选择后点击“新建独立会话”。</span>
        </div>
        <div class="field">
          <label for="preset">命令</label>
          <select id="preset" v-model="presetId" :disabled="creating">
            <option v-for="p in selectableCommands" :key="p.id" :value="p.id">
              {{ p.label }} —— {{ p.hint }}
            </option>
          </select>
        </div>

        <div class="field">
          <label for="cwd">工作目录</label>
          <input
            id="cwd"
            v-model="cwd"
            class="mono"
            type="text"
            list="cwd-roots"
            autocapitalize="none"
            autocorrect="off"
            spellcheck="false"
            :placeholder="cwdPlaceholder"
            :disabled="creating"
          />
          <datalist id="cwd-roots">
            <option v-for="root in allowedRoots" :key="root" :value="root" />
          </datalist>
          <span class="field__hint">
            <template v-if="allowedRoots.length > 0">
              可在 Agent 允许的目录内选择：{{ allowedRoots.join('、') }}
            </template>
            <template v-else>
              必须是 Agent 的 allowed_roots 里的目录，否则会被拒绝（Agent 离线或版本过低，未上报允许目录）。
            </template>
          </span>
        </div>

        <div class="field">
          <label for="sname">会话名（可选）</label>
          <input
            id="sname"
            v-model="sessionName"
            type="text"
            placeholder="留空则用命令名"
            :disabled="creating"
          />
        </div>

        <div v-if="reportedCommands === undefined" class="notice notice--warn small">
          当前 Agent 尚未上报本机命令清单；预置列表仅供旧版本兼容，是否可运行仍由本机 allowed_commands 决定。
        </div>
        <div v-else-if="selectableCommands.length === 0" class="notice notice--warn small">
          还没有可启动的工具。<a href="#device-tools-heading">在“本机工具”中授权已安装的 CLI</a>。
        </div>
        <details class="device-advanced"><summary>命令与 DSH 设置</summary>
        <div v-if="selectedWebURL" class="notice small">
          外部 Web 地址：<a :href="selectedWebURL" target="_blank" rel="noopener noreferrer">打开独立 Web UI ↗</a>。此链接由你另行部署，与下方 CodeGate 内建转发互不影响。
        </div>
        <div v-if="dshCommand" class="notice small">
          <strong>DSH TUI：</strong>
          <span v-if="!dshCommand.installed">本机服务的 PATH 中未找到 dsh。</span>
          <span v-else-if="!dshCommand.allowed">已找到 dsh，可在下方“本机工具”中授权。</span>
          <span v-else>已授权 dsh；TUI 插件状态尚未自动验证。</span>
          <p>要在终端使用，先在目标机器以运行 Agent 的同一用户执行：</p>
          <div class="row"><code class="mono">{{ dshInstallCommand }}</code><button class="btn btn--ghost btn--sm" type="button" @click="copyDshInstallCommand">复制命令</button></div>
          <span v-if="dshCopyStatus" class="field__hint">{{ dshCopyStatus }}</span>
          <p>在该用户环境验证 <code class="mono">dsh --profile tui</code> 能打开 TUI，再在“本机工具”中授权 dsh。安装过程可能需要 pnpm 按提示允许插件构建；CodeGate 不会自动安装插件。</p>
        </div>
        </details>

        <!-- Windows shell 的 ConPTY 中断限制。 -->
        <div v-if="selectedPreset?.kind === 'shell' && device?.platform === 'windows'" class="notice notice--warn">
          <strong>{{ selectedPreset.label }}</strong> 属于 shell 类程序。在 Windows 上，
          系统自带的 ConPTY 无法把中断信号送达它 —— 会话里的 Ctrl+C 按钮会被禁用。
          终端内的 Ctrl+C 会被屏蔽以免关闭会话；结束整个会话需在终端页确认「关闭会话」。
        </div>

        <div v-if="createError" class="notice notice--err">{{ createError }}</div>

        <div class="row">
          <button class="btn btn--primary" type="submit" :disabled="creating || !conn.isOpen || selectableCommands.length === 0">
            <span v-if="creating" class="spinner" />
            新建独立会话 →
          </button>
          <button class="btn btn--sm" type="button" :disabled="!selectedPreset || !cwd.trim() || preferences.saving" @click="saveShortcut">{{ preferences.saving ? '保存中…' : '保存为项目' }}</button>
        </div>
      </form>
    </div>

    <!-- ---- 会话列表 ---- -->
    <div class="card device-sessions">
      <div class="card__title">
        <h2>会话</h2>
        <span v-if="sessions.source !== null" class="badge" :class="sessions.source === 'live' ? 'badge--ok' : 'badge--idle'">
          {{ sessions.source === 'live' ? '实时' : '缓存快照' }}
        </span>
      </div>



      <div v-if="sessions.error" class="notice notice--err" style="margin-bottom: 12px">
        {{ sessions.error }}
      </div>

      <div v-if="sessionActionError" class="notice notice--err" style="margin-bottom: 12px">{{ sessionActionError }}</div>

      <div class="row session-filters">
        <button class="btn btn--sm" :class="!showArchived ? 'btn--primary' : ''" type="button" @click="showArchived = false">最近会话</button>
        <button class="btn btn--sm" :class="showArchived ? 'btn--primary' : ''" type="button" @click="showArchived = true">归档 {{ archivedCount }}</button>
      </div>
      <div class="field"><label for="session-query">查找会话</label><input id="session-query" v-model="sessionQuery" type="search" placeholder="按名称、命令或工作目录搜索" /></div>

      <p class="dim small session-help">离开终端后会话继续运行。设备在线时可接回；结束进程请在终端内关闭会话。</p>

      <div v-if="visibleSessions.length === 0" class="empty">{{ sessionQuery.trim() ? '没有匹配的会话。' : showArchived ? '暂无归档会话。' : '还没有会话，创建一个新的工作空间吧。' }}</div>

      <div v-else class="list">
        <div
          v-for="s in visibleSessions"
          :key="s.session_id"
          class="item"
        >
          <span class="item__main">
            <span class="item__title">
              {{ s.name || s.command || '会话' }}
              <span class="badge" :class="`badge--${statusKind(s.status)}`">
                {{ statusLabel(s.status) }}
              </span>
            </span>
            <span class="item__meta" :title="describeSession(s)">{{ s.command || '命令' }} · {{ s.cwd || '工作目录未知' }}</span>
          </span>
          <span class="item__actions">
            <span class="faint small nowrap session-time">{{ createdText(s) }}</span>
            <button class="btn btn--ghost btn--sm" type="button" :aria-label="pinnedIds.includes(s.session_id) ? '取消置顶会话' : '置顶会话'" :aria-pressed="pinnedIds.includes(s.session_id)" @click="togglePin(s.session_id)">{{ pinnedIds.includes(s.session_id) ? '★ 已置顶' : '☆ 置顶' }}</button>
            <button v-if="isLiveStatus(s.status)" class="btn btn--primary btn--sm" type="button" :disabled="!canOpen(s)" :title="canOpen(s) ? '接回原会话' : '设备离线，重新上线后可接回'" @click="openSession(s)">{{ canOpen(s) ? '接回' : '离线，暂不可接回' }}</button>
            <button class="btn btn--sm" type="button" @click="toggleArchive(s)">{{ s.archived ? '移出归档' : '归档' }}</button>
            <button v-if="!isLiveStatus(s.status)" class="btn btn--danger btn--sm" type="button" @click="deleteRecord(s)">
              {{ deletingSession === s.session_id ? '确认删除' : '删除记录' }}
            </button>
          </span>
        </div>
      </div>
    </div>
    <section class="card device-tools" aria-labelledby="device-tools-heading">
      <div class="card__title"><h2 id="device-tools-heading">本机工具</h2><span class="faint small">由 Agent 扫描</span></div>
      <p class="dim small">Agent 扫描本机工具；授权后即可从网页新建会话，随时可关闭授权。</p>
      <div v-if="!updateInfo?.online" class="dim small">设备上线后显示扫描结果。</div>
      <div v-else-if="!reportedCommands" class="dim small">等待 Agent 上报工具清单。</div>
      <div v-else class="device-tools__list">
        <div v-for="tool in knownTools" :key="tool.id" class="device-tools__item">
          <span>{{ tool.label }}</span>
          <span class="badge" :class="tool.allowed && tool.installed ? 'badge--ok' : tool.installed ? 'badge--warn' : 'badge--idle'">{{ !tool.installed ? '未找到' : tool.allowed ? '可使用' : '待授权' }}</span>
          <button v-if="tool.installed && !tool.allowed" class="btn btn--primary btn--sm" type="button" :disabled="!!toolBusy" @click="setTool(tool.id, true)">{{ toolBusy === tool.id ? '授权中…' : '授权使用' }}</button>
          <button v-else-if="tool.installed && tool.allowed && tool.managed" class="btn btn--ghost btn--sm" type="button" :disabled="!!toolBusy" @click="setTool(tool.id, false)">{{ toolBusy === tool.id ? '处理中…' : '关闭授权' }}</button>
        </div>
      </div>
      <div v-if="toolError" class="notice notice--err small">{{ toolError }}</div>
      <p class="small dim">网页授权适用于这四个工具；自定义命令仍由目标电脑配置。旧版 Agent 需要先更新。</p>
    </section>

    <!-- DSH Web is a separate browser experience, independent of TUI sessions. -->
    <section class="card device-dsh" aria-labelledby="dsh-web-heading">
      <div class="card__title"><h2 id="dsh-web-heading">DSH Web</h2><span class="badge" :class="updateInfo?.web_proxy_enabled ? 'badge--ok' : 'badge--idle'">{{ updateInfo?.web_proxy_enabled ? '转发已配置' : '待配置' }}</span></div>
      <p class="dim small">在目标电脑启动 DSH Web，通过 CodeGate 的独立 HTTPS 地址打开。</p>
      <ol class="device-dsh__checklist">
        <li :class="updateInfo?.web_proxy_enabled ? 'device-dsh__ready' : ''">
          <span>{{ updateInfo?.web_proxy_enabled ? '✓' : '1' }}</span>
          <div><strong>Server 域名与 HTTPS</strong><small>{{ updateInfo?.web_proxy_enabled ? '转发已启用' : '设置 CODEGATE_DSH_PROXY_DOMAIN，并为通配子域名配置 DNS 和 TLS 证书' }}</small></div>
        </li>
        <li :class="dshCommand?.installed ? 'device-dsh__ready' : ''">
          <span>{{ dshCommand?.installed ? '✓' : '2' }}</span>
          <div><strong>目标电脑上的 DSH</strong><small>{{ dshCommand?.installed ? 'Agent 服务账户已找到 dsh' : updateInfo?.online ? 'Agent 服务账户未找到 dsh；安装后重启 Agent' : '设备上线后检查安装状态' }}</small></div>
        </li>
        <li :class="updateInfo?.dsh_web_enabled ? 'device-dsh__ready' : ''">
          <span>{{ updateInfo?.dsh_web_enabled ? '✓' : '3' }}</span>
          <div><strong>允许启动 Web</strong><small>{{ updateInfo?.dsh_web_enabled ? 'Agent 已允许' : '在目标电脑的 agent.json 设置 "dsh_web_enabled": true，然后重启 Agent' }}</small></div>
        </li>
      </ol>
      <a class="small" href="https://github.com/JOJO-5/codegate/blob/main/docs/DEPLOY-DOCKER.md#%E5%86%85%E5%BB%BA-dsh-web-%E8%BD%AC%E5%8F%91%E5%8F%AF%E9%80%89" target="_blank" rel="noopener noreferrer">查看 Server 域名和证书设置 ↗</a>
      <div v-if="updateInfo && !updateInfo.online" class="notice notice--warn small">设备离线，连接后才能启动 DSH Web。</div>
      <div class="row device-dsh__actions">
        <button class="btn btn--primary btn--sm" type="button" :disabled="dshOpening || !updateInfo?.online || !updateInfo?.web_proxy_enabled || !dshCommand?.installed || updateInfo?.dsh_web_enabled === false" @click="openDSHWeb">{{ dshOpening ? '正在启动…' : '启动并打开 Web ↗' }}</button>
        <button class="btn btn--ghost btn--sm" type="button" :disabled="dshStopping || !updateInfo?.online || !updateInfo?.web_proxy_enabled" @click="stopDSHWeb">{{ dshStopping ? '正在停止…' : '停止' }}</button>
      </div>
      <a v-if="dshOpenURL" :href="dshOpenURL" target="_blank" rel="noopener noreferrer">浏览器阻止了新窗口，点此打开 DSH Web ↗</a>
      <div v-if="dshOpenError" class="notice notice--err small">{{ dshOpenError }}</div>
    </section>

    <!-- ---- 设备信息 ---- -->
    <details class="card device-admin"><summary>设备设置 <span class="faint small">改名、设备信息与解绑</span></summary>
      <div class="card__title">
        <h2>设备信息</h2>
        <button class="btn btn--ghost btn--sm" type="button" @click="renaming = !renaming">
          {{ renaming ? '取消' : '改名' }}
        </button>
      </div>

      <div v-if="device === undefined" class="dim small">设备不存在或无权访问。</div>

      <template v-else>
        <table class="table">
          <tbody>
            <tr>
              <th style="width: 96px">状态</th>
              <td>
                <span class="badge" :class="device.online ? 'badge--ok' : 'badge--idle'">
                  {{ device.online ? '在线' : '离线' }}
                </span>
                <span v-if="!device.online && toMs(device.last_seen_at) !== null" class="faint small">
                  最后在线 {{ relativeTime(toMs(device.last_seen_at) as number) }}
                </span>
              </td>
            </tr>
            <tr>
              <th>平台</th>
              <td>{{ platformLabel(device.platform) }}/{{ device.arch }}</td>
            </tr>
            <tr>
              <th>Agent</th>
              <td class="mono">{{ device.agent_version || '—' }}</td>
            </tr>
            <tr>
              <th>设备 ID</th>
              <td class="mono">{{ device.id }}</td>
            </tr>
          </tbody>
        </table>

        <div v-if="renaming" class="row" style="margin-top: 12px">
          <input v-model="nameDraft" type="text" :disabled="devices.loading" />
          <button class="btn btn--primary btn--sm" type="button" @click="doRename">保存</button>
        </div>

        <div v-if="manageError" class="notice notice--err" style="margin-top: 12px">
          {{ manageError }}
        </div>

        <div class="row" style="margin-top: 14px">
          <template v-if="!confirmDelete">
            <button class="btn btn--danger btn--sm" type="button" @click="confirmDelete = true">
              解绑设备
            </button>
          </template>
          <template v-else>
            <span class="small" style="color: var(--err)">
              解绑后 Agent 需要重新配对才能连上。确定吗？
            </span>
            <button class="btn btn--danger btn--sm" type="button" @click="doDelete">确定解绑</button>
            <button class="btn btn--sm" type="button" @click="confirmDelete = false">取消</button>
          </template>
        </div>
      </template>
    </details>

    <details class="card device-update"><summary>Agent 更新 <span class="badge" :class="updateInfo?.update?.state === 'error' ? 'badge--err' : 'badge--idle'">{{ updateLabel }}</span></summary>
      <div class="card__title">
        <h2>Agent 更新</h2>
        <span class="badge" :class="updateInfo?.update?.state === 'error' ? 'badge--err' : updateInfo?.update?.state === 'waiting' || updateInfo?.update?.state === 'switching' ? 'badge--warn' : 'badge--idle'">
          {{ updateLabel }}
        </span>
      </div>
      <p v-if="updateInfo?.update?.enabled && updateInfo.update.state === 'waiting'" class="dim small">
        当前有会话正在运行或等待回看。明确关闭这些会话后，Agent 才会在下次检查时下载更新。
      </p>
      <div v-if="updateInfo?.update?.last_failure" class="notice notice--warn small" style="margin-bottom: 8px">
        上次升级未完成（{{ updateInfo.update.last_failure.version }}）：{{ updateInfo.update.last_failure.reason }}。
        {{ relativeTime(updateInfo.update.last_failure.occurred_at) }}，已继续使用旧版。
      </div>
      <p v-if="updateInfo?.update?.version" class="dim small">目标版本：{{ updateInfo.update.version }}</p>
      <p v-if="updateInfo?.update?.detail" class="dim small">{{ updateInfo.update.detail }}</p>
      <p v-if="updateInfo?.update?.checked_at" class="faint small">
        最近检查：{{ relativeTime(updateInfo.update.checked_at) }}。状态由 Agent 心跳上报，可能有短暂延迟。
      </p>
      <p v-if="updateInfo?.online && updateInfo.update?.state === 'disabled'" class="dim small">
        要启用空闲自动更新，请在本机 agent.json 设置 update_enabled，并使用 supervise 开机服务。
      </p>
    </details>

    </div>
  </main>
</template>

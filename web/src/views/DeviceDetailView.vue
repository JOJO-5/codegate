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
import { api, type DeviceUpdateStatus } from '../lib/api'
import { humanizeError, platformLabel, relativeTime, statusKind, statusLabel, toMs } from '../lib/format'
import { PRESETS, type CommandPreset } from '../lib/commands'
import { estimateTerminalSize } from '../lib/viewport'
import { MessageType, isLiveStatus, type SessionSummary } from '../lib/protocol'

const props = defineProps<{ id: string }>()

const devices = useDevicesStore()
const sessions = useSessionsStore()
const conn = useConnStore()
const router = useRouter()

const device = computed(() => devices.byId(props.id))
const list = computed(() => sessions.forDevice(props.id))
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
const detectedUnapproved = computed(() =>
  (reportedCommands.value ?? []).filter(c => c.installed && !c.allowed),
)
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
  await devices.load()
  if (device.value !== undefined) {
    nameDraft.value = device.value.name
  }
  const saved = localStorage.getItem(cwdKey.value)
  if (saved !== null && saved !== '') cwd.value = saved

  await sessions.load(props.id)
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
      ...(preset.resume === true ? { resume: true } : {}),
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
  void router.push({ name: 'session', params: { id: s.session_id } })
}

/** 会话是否还活着（可进入）。 */
function canOpen(s: SessionSummary): boolean {
  return isLiveStatus(s.status)
}
</script>

<template>
  <main class="page">
    <div class="row row--between" style="margin-bottom: 16px">
      <div class="row">
        <RouterLink class="btn btn--ghost btn--sm" :to="{ name: 'devices' }">‹ 设备</RouterLink>
        <h1 style="margin: 0">{{ device?.name ?? '设备' }}</h1>
      </div>
      <button class="btn btn--sm" type="button" :disabled="sessions.loading" @click="sessions.load(id, true)">
        <span v-if="sessions.loading" class="spinner" />
        刷新
      </button>
    </div>

    <!-- ---- 设备信息 ---- -->
    <div class="card">
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
    </div>

    <div class="card">
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
    </div>

    <!-- ---- 新建会话 ---- -->
    <div class="card">
      <div class="card__title">
        <h2>新建会话</h2>
        <span v-if="!conn.isOpen" class="badge badge--warn">连接未就绪</span>
      </div>

      <form class="stack" @submit.prevent="createSession">
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
              必须是 Agent 的 allowed_roots 里的目录，否则会被拒绝。可选：{{ allowedRoots.join('、') }}
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
          本机没有已授权且可找到的命令。请检查 Agent 的 allowed_commands 和开机服务使用的 PATH。
        </div>
        <div v-if="selectedWebURL" class="notice small">
          外部 Web 地址：<a :href="selectedWebURL" target="_blank" rel="noopener noreferrer">打开独立 Web UI ↗</a>。此链接由你另行部署，与下方 CodeGate 内建转发互不影响。
        </div>
        <div v-if="dshCommand" class="notice small">
          <strong>DSH TUI：</strong>
          <span v-if="!dshCommand.installed">本机服务的 PATH 中未找到 dsh。</span>
          <span v-else-if="!dshCommand.allowed">已找到 dsh，但尚未配置为可启动命令。</span>
          <span v-else>已找到 dsh 且入口已配置；TUI 插件状态尚未自动验证。</span>
          <p>要在终端使用，先在目标机器以运行 Agent 的同一用户执行：</p>
          <div class="row"><code class="mono">{{ dshInstallCommand }}</code><button class="btn btn--ghost btn--sm" type="button" @click="copyDshInstallCommand">复制命令</button></div>
          <span v-if="dshCopyStatus" class="field__hint">{{ dshCopyStatus }}</span>
          <p>然后在该用户环境验证 <code class="mono">dsh --profile tui</code> 能打开 TUI，再把 dsh 配置到 agent.json 的 allowed_commands（args 为 ["--profile", "tui"]）。安装过程可能需要 pnpm 按提示允许插件构建；CodeGate 不会自动安装。</p>
        </div>
        <div v-if="dshCommand?.installed && updateInfo?.web_proxy_enabled" class="notice small">
          <strong>DSH Web（内建转发）：</strong>目标机器需在 agent.json 启用 dsh_web_enabled；CodeGate 会在该机器启动仅监听本机的 DSH，并通过 Agent 出站连接打开独立 HTTPS 子域名。
          <button class="btn btn--ghost btn--sm" type="button" :disabled="dshOpening || !updateInfo?.online" @click="openDSHWeb">
            {{ dshOpening ? '正在启动…' : '启动并打开 DSH Web ↗' }}
          </button>
          <button class="btn btn--ghost btn--sm" type="button" :disabled="dshStopping || !updateInfo?.online" @click="stopDSHWeb">
            {{ dshStopping ? '正在停止…' : '停止 DSH Web' }}
          </button>
          <a v-if="dshOpenURL" :href="dshOpenURL" target="_blank" rel="noopener noreferrer">打开 DSH Web ↗</a>
          <span v-if="dshOpenError" class="notice notice--err">{{ dshOpenError }}</span>
        </div>
        <div v-if="detectedUnapproved.length" class="notice small">
          检测到但尚未授权：{{ detectedUnapproved.map(c => c.label).join('、') }}。在本机 agent.json 的 allowed_commands 中配置后才能从网页启动。
          仅找到 dsh 命令并不代表 TUI 插件已就绪。
        </div>

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
            创建并进入
          </button>
        </div>
      </form>
    </div>

    <!-- ---- 会话列表 ---- -->
    <div class="card">
      <div class="card__title">
        <h2>会话</h2>
        <span v-if="sessions.source !== null" class="badge" :class="sessions.source === 'live' ? 'badge--ok' : 'badge--idle'">
          {{ sessions.source === 'live' ? '实时' : '缓存快照' }}
        </span>
      </div>

      <div v-if="sessions.source === 'cache'" class="notice notice--info" style="margin-bottom: 12px">
        Agent 当前离线，下面是数据库里的快照 —— 状态可能已经过时。
      </div>

      <div v-if="sessions.error" class="notice notice--err" style="margin-bottom: 12px">
        {{ sessions.error }}
      </div>

      <p class="dim small">离开终端页面不会结束远端进程；点击运行中的会话可接回原会话。只有在终端页确认关闭才会结束进程。</p>

      <div v-if="list.length === 0" class="empty">这个设备上还没有会话。</div>

      <div v-else class="list">
        <button
          v-for="s in list"
          :key="s.session_id"
          class="item"
          type="button"
          style="cursor: pointer; text-align: left; font: inherit; width: 100%"
          @click="openSession(s)"
        >
          <span class="item__main">
            <span class="item__title">
              {{ s.name || s.command || '会话' }}
              <span class="badge" :class="`badge--${statusKind(s.status)}`">
                {{ statusLabel(s.status) }}
              </span>
              <span v-if="!canOpen(s)" class="faint small">已结束</span>
            </span>
            <span class="item__meta">{{ describeSession(s) }}</span>
          </span>
          <span class="faint small nowrap">{{ createdText(s) }}</span>
        </button>
      </div>
    </div>
  </main>
</template>

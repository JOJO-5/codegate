<script setup lang="ts">
/**
 * 设备详情：会话列表 + 新建会话 + 设备管理。
 *
 * 会话列表的两个来源与回落顺序见 `stores/sessions.ts`。
 * 这里额外做一件事：**把列表标记为「实时」还是「缓存」**。
 * Agent 离线时列表来自数据库快照，用户必须知道这一点 ——
 * 否则他会对着一个显示"运行中"的会话点进去，然后发现连不上。
 */

import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { humanizeError, platformLabel, relativeTime, statusKind, statusLabel, toMs } from '../lib/format'
import { PRESETS } from '../lib/commands'
import { estimateTerminalSize } from '../lib/viewport'
import { MessageType, isLiveStatus, type SessionSummary } from '../lib/protocol'

const props = defineProps<{ id: string }>()

const devices = useDevicesStore()
const sessions = useSessionsStore()
const conn = useConnStore()
const router = useRouter()

const device = computed(() => devices.byId(props.id))
const list = computed(() => sessions.forDevice(props.id))

// ---- 新建会话表单 ----
const presetId = ref('claude')
const cwd = ref('')
const sessionName = ref('')
const creating = ref(false)
const createError = ref<string | null>(null)

const selectedPreset = computed(() => PRESETS.find((p) => p.id === presetId.value) ?? PRESETS[0])

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
    createError.value = '请填写工作目录（cwd）。它必须在 Agent 的 allowed_workspaces 内。'
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
            <option v-for="p in PRESETS" :key="p.id" :value="p.id">
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
            autocapitalize="none"
            autocorrect="off"
            spellcheck="false"
            placeholder="例如 C:\Users\me\project 或 /home/me/project"
            :disabled="creating"
          />
          <span class="field__hint">必须是 Agent 的 allowed_workspaces 里的目录，否则会被拒绝。</span>
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

        <!-- Windows shell 的 ConPTY 中断限制。 -->
        <div v-if="selectedPreset?.kind === 'shell' && device?.platform === 'windows'" class="notice notice--warn">
          <strong>{{ selectedPreset.label }}</strong> 属于 shell 类程序。在 Windows 上，
          系统自带的 ConPTY 无法把中断信号送达它 —— 会话里的 Ctrl+C 按钮会被禁用。
          终端内的 Ctrl+C 会被屏蔽以免关闭会话；结束整个会话请使用「关闭会话」。
        </div>

        <div v-if="createError" class="notice notice--err">{{ createError }}</div>

        <div class="row">
          <button class="btn btn--primary" type="submit" :disabled="creating || !conn.isOpen">
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

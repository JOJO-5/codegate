/**
 * 会话列表与创建/关闭。
 *
 * # 两个数据源，按可用性回落
 *
 *   - **WS `session.list`**：从 Agent 实时取，包含 `starting` 状态的会话
 *     和最新的 cols/rows/pid。Agent 在线时这是唯一正确的来源。
 *   - **REST `/devices/{id}/sessions`**：数据库里的缓存快照。Agent 离线时
 *     WS 会返回 `device_offline`，但用户仍然应该看到「上次有哪些会话、
 *     它们的退出码是多少」。
 *
 * 先 WS 再回落 REST 是刻意的顺序：反过来会先渲染一份可能过时的列表，
 * 再被实时数据覆盖，视觉上会闪一下。
 */

import { ref } from 'vue'
import { defineStore } from 'pinia'
import { api } from '../lib/api'
import { MessageType } from '../lib/protocol'
import type {
  SessionCreatePayload,
  SessionCreatedPayload,
  SessionListedPayload,
  SessionSummary,
} from '../lib/protocol'
import { humanizeError } from '../lib/format'
import { useConnStore } from './conn'

export const useSessionsStore = defineStore('sessions', () => {
  /** deviceId → 会话列表。 */
  const byDevice = ref<Record<string, SessionSummary[]>>({})
  const loading = ref(false)
  const error = ref<string | null>(null)

  /** 本次列表来自哪里，UI 可以据此提示「这是缓存」。 */
  const source = ref<'live' | 'cache' | null>(null)

  function forDevice(deviceId: string): SessionSummary[] {
    return byDevice.value[deviceId] ?? []
  }

  function setFor(deviceId: string, list: SessionSummary[]): void {
    byDevice.value = { ...byDevice.value, [deviceId]: list }
  }

  async function load(deviceId: string, force = false): Promise<void> {
    if (loading.value && !force) return
    loading.value = true
    error.value = null
    source.value = null

    const conn = useConnStore()

    // ---- 1. 优先走 WS（实时）----
    if (conn.isOpen) {
      try {
        const env = await conn.request<SessionListedPayload>(
          MessageType.SessionList,
          { device_id: deviceId },
        )
        setFor(deviceId, env.payload?.sessions ?? [])
        source.value = 'live'
        loading.value = false
        return
      } catch {
        // device_offline / timeout 都是预期情况，静默回落到缓存。
        // 这里**不**设 error —— 能拿到缓存就不是错误状态。
      }
    }

    // ---- 2. 回落 REST 缓存 ----
    try {
      const list = await api.deviceSessions(deviceId)
      setFor(deviceId, list)
      source.value = 'cache'
    } catch (e) {
      error.value = humanizeError(e)
    } finally {
      loading.value = false
    }
  }

  /**
   * 新建会话。
   *
   * 返回新建出来的 SessionSummary —— 调用方（设备详情页）拿它去跳转终端页。
   */
  async function create(
    deviceId: string,
    opts: {
      commandId?: string
      command?: string
      args?: string[]
      cwd: string
      cols: number
      rows: number
      name?: string
      resume?: boolean
    },
  ): Promise<SessionSummary> {
    const conn = useConnStore()
    const payload: SessionCreatePayload = {
      device_id: deviceId,
      cwd: opts.cwd,
      cols: opts.cols,
      rows: opts.rows,
      ...(opts.name !== undefined && opts.name !== '' ? { name: opts.name } : {}),
      ...(opts.commandId !== undefined ? { command_id: opts.commandId } : {}),
      ...(opts.command !== undefined ? { command: opts.command } : {}),
      ...(opts.args !== undefined ? { args: opts.args } : {}),
      ...(opts.resume === true ? { resume: true } : {}),
    }

    const env = await conn.request<SessionCreatedPayload>(MessageType.SessionCreate, payload)
    const s = env.payload?.session
    if (s === undefined) {
      throw new Error('服务端未返回新建的会话')
    }

    // 乐观插入列表，避免返回详情页时列表里还没有它
    const cur = forDevice(deviceId)
    if (!cur.some((x) => x.session_id === s.session_id)) {
      setFor(deviceId, [s, ...cur])
    }
    return s
  }

  /** 终止会话（Ctrl+C 在 cooked mode 下无效时的替代动作，§6.5）。 */
  async function close(sessionId: string, force = false): Promise<void> {
    const conn = useConnStore()
    await conn.request(MessageType.SessionClose, { session_id: sessionId, force }, sessionId)
  }

  /** 用一条新快照替换列表里的某个会话（收到 session.exit / closed 时用）。 */
  function patch(sessionId: string, changes: Partial<SessionSummary>): void {
    const next: Record<string, SessionSummary[]> = {}
    for (const [dev, list] of Object.entries(byDevice.value)) {
      next[dev] = list.map((s) => (s.session_id === sessionId ? { ...s, ...changes } : s))
    }
    byDevice.value = next
  }

  function removeFromList(sessionId: string): void {
    const next: Record<string, SessionSummary[]> = {}
    for (const [dev, list] of Object.entries(byDevice.value)) {
      next[dev] = list.filter((s) => s.session_id !== sessionId)
    }
    byDevice.value = next
  }

  function reset(): void {
    byDevice.value = {}
    error.value = null
    source.value = null
  }

  return {
    byDevice,
    loading,
    error,
    source,
    forDevice,
    setFor,
    load,
    create,
    close,
    patch,
    removeFromList,
    reset,
  }
})

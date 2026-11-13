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
 * 首次显示 Server 历史，再用 WS 核对。后台刷新保留上次确认的列表；
 * 只有核对失败、断线或过期才降为缓存，避免每次轮询都移走主要操作。
 */

import { ref, watch } from 'vue'
import { useAuthStore } from './auth'
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
import { groupConversationSessions, conversationRecords, liveSession } from '../lib/conversationSessions'
import { useConnStore } from './conn'

export const useSessionsStore = defineStore('sessions', () => {
  /** deviceId → 会话列表。 */
  const byDevice = ref<Record<string, SessionSummary[]>>({})
  const loading = ref(false)
  const error = ref<string | null>(null)

  /** 本次列表来自哪里，UI 可以据此提示「这是缓存」。 */
  const source = ref<'live' | 'cache' | null>(null)
  const sources = ref<Record<string, 'live' | 'cache'>>({})
  const loadStates = ref<Record<string, { refreshing: boolean; checkedAt: number; error: string | null }>>({})
  const inflight = new Map<string, Promise<void>>()
  const revisions = new Map<string, number>()
  const drafts = new Map<string, string>()
  let generation = 0
  let transportEpoch = 0
  const auth = useAuthStore()
  watch(() => auth.user?.id, reset, { flush: 'sync' })
  const connection = useConnStore()
  watch(() => connection.isOpen, open => {
    if (!open) { transportEpoch++; for (const id of Object.keys(sources.value)) sources.value[id] = 'cache'; source.value = 'cache' }
  }, { flush: 'sync' })
  function setDraft(id: string, text: string): void { drafts.set(id, text) }
  function takeDraft(id: string): string { const text = drafts.get(id) ?? ''; drafts.delete(id); return text }


  function forDevice(deviceId: string): SessionSummary[] {
    return groupConversationSessions(byDevice.value[deviceId] ?? [])
  }

  function recordsFor(deviceId: string, sessionId: string): SessionSummary[] {
    return conversationRecords(byDevice.value[deviceId] ?? [], sessionId)
  }

  function setFor(deviceId: string, list: SessionSummary[]): void {
    revisions.set(deviceId, (revisions.get(deviceId) ?? 0) + 1)
    replaceFor(deviceId, list)
  }
  function replaceFor(deviceId: string, list: SessionSummary[]): void {
    byDevice.value = { ...byDevice.value, [deviceId]: list }
  }

  function load(deviceId: string, _force = false): Promise<void> {
    const pending = inflight.get(deviceId)
    if (pending) return pending
    const current = generation
    const request = loadDevice(deviceId, current).finally(() => {
      if (current === generation) {
        inflight.delete(deviceId); loading.value = inflight.size > 0
        const state = loadStates.value[deviceId]
        if (state) state.refreshing = false
      }
    })
    inflight.set(deviceId, request)
    return request
  }
  async function loadDevice(deviceId: string, current: number): Promise<void> {
    const revision = revisions.get(deviceId) ?? 0
    const transport = transportEpoch
    const stale = () => current !== generation || transport !== transportEpoch || revision !== (revisions.get(deviceId) ?? 0)
    loading.value = true
    error.value = null
    loadStates.value[deviceId] = { refreshing: true, checkedAt: loadStates.value[deviceId]?.checkedAt ?? 0, error: null }
    const hadLive = sources.value[deviceId] === 'live'

    const conn = useConnStore()

    // Server 保存已结束和归档的记录，Agent 只提供当前存活会话。
    let saved: SessionSummary[] = []
    try {
      saved = await api.deviceSessions(deviceId)
      if (stale()) return
      if (!hadLive) { replaceFor(deviceId, saved); source.value = 'cache'; sources.value[deviceId] = 'cache' }
    } catch (e) {
      if (!stale()) { error.value = humanizeError(e); loadStates.value[deviceId]!.error = error.value }
    }

    if (stale()) return
    if (conn.isOpen) {
      try {
        const env = await conn.request<SessionListedPayload>(
          MessageType.SessionList,
          { device_id: deviceId },
        )
        if (stale()) return
        const live = env.payload?.sessions ?? []
        const active = new Set(live.map(s => s.session_id))
        const archived = new Map(saved.map(s => [s.session_id, s.archived]))
        replaceFor(deviceId, [
          ...live.map(s => ({ ...s, archived: archived.get(s.session_id) ?? false })),
          ...saved.filter(s => !active.has(s.session_id)).map(s =>
            s.status === 'running' || s.status === 'starting' || s.status === 'detached'
              ? { ...s, status: 'exited' as const }
              : s,
          ),
        ])
        source.value = 'live'
        sources.value[deviceId] = 'live'
        error.value = null
        loadStates.value[deviceId] = { refreshing: true, checkedAt: Date.now(), error: null }
        return
      } catch {
        // device_offline / timeout 都是预期情况，静默回落到缓存。
        // 这里**不**设 error —— 能拿到缓存就不是错误状态。
      }
    }
    if (stale()) return
    source.value = 'cache'; sources.value[deviceId] = 'cache'
    // Preserve the visible records on failure, with honest stale provenance.
    if (saved.length && !byDevice.value[deviceId]?.length) replaceFor(deviceId, saved)
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
      launchMode?: string
      command?: string
      args?: string[]
      cwd: string
      cols: number
      rows: number
      name?: string
      worktree?: boolean
      resume?: boolean
      nonce?: string
    },
  ): Promise<SessionSummary> {
    const conn = useConnStore()
    const payload: SessionCreatePayload = {
      nonce: opts.nonce ?? crypto.randomUUID(),
      device_id: deviceId,
      cwd: opts.cwd,
      cols: opts.cols,
      rows: opts.rows,
      ...(opts.name !== undefined && opts.name !== '' ? { name: opts.name } : {}),
      ...(opts.commandId !== undefined ? { command_id: opts.commandId } : {}),
      ...(opts.launchMode ? { launch_mode: opts.launchMode } : {}),
      ...(opts.command !== undefined ? { command: opts.command } : {}),
      ...(opts.args !== undefined ? { args: opts.args } : {}),
      ...(opts.worktree === true ? { worktree: true } : {}),
      ...(opts.resume === true ? { resume: true } : {}),
    }

    const current = generation
    const env = await conn.request<SessionCreatedPayload>(MessageType.SessionCreate, payload)
    if (current !== generation) throw new Error('账号已切换，请在原账号核对启动结果')
    const s = env.payload?.session
    if (s === undefined) {
      throw new Error('服务端未返回新建的会话')
    }

    // 乐观插入列表，避免返回详情页时列表里还没有它
    const cur = byDevice.value[deviceId] ?? []
    if (!cur.some((x) => x.session_id === s.session_id)) {
      setFor(deviceId, [s, ...cur])
    }
    return s
  }

  /** 终止会话（Ctrl+C 在 cooked mode 下无效时的替代动作，§6.5）。 */
  async function close(sessionId: string, force = false): Promise<void> {
    const conn = useConnStore()
    await conn.request(MessageType.SessionClose, { session_id: sessionId, force }, sessionId)
	patch(sessionId, { status: 'terminated' })
  }

  async function archive(deviceId: string, sessionId: string, archived: boolean): Promise<void> {
    const current = generation
    try {
      for (const record of recordsFor(deviceId, sessionId)) {
        if (current !== generation) return
        await api.archiveSession(deviceId, record.session_id, archived)
        if (current !== generation) return
        patch(record.session_id, { archived })
      }
    } catch (e) { if (current === generation) await load(deviceId, true); throw e }
  }

  async function remove(deviceId: string, sessionId: string): Promise<void> {
    const current = generation
    const records = recordsFor(deviceId, sessionId)
    if (records.some(liveSession)) throw new Error('请先结束该对话的所有远端进程，再删除记录')
    try {
      for (const record of records) {
        if (current !== generation) return
        await api.deleteSession(deviceId, record.session_id)
        if (current !== generation) return
        removeFromList(record.session_id)
      }
    } catch (e) { if (current === generation) await load(deviceId, true); throw e }
  }

  /** 用一条新快照替换列表里的某个会话（收到 session.exit / closed 时用）。 */
  function patch(sessionId: string, changes: Partial<SessionSummary>): void {
    const next: Record<string, SessionSummary[]> = {}
    for (const [dev, list] of Object.entries(byDevice.value)) {
      if (list.some(s => s.session_id === sessionId)) revisions.set(dev, (revisions.get(dev) ?? 0) + 1)
      next[dev] = list.map((s) => (s.session_id === sessionId ? { ...s, ...changes } : s))
    }
    byDevice.value = next
  }

  function removeFromList(sessionId: string): void {
    const next: Record<string, SessionSummary[]> = {}
    for (const [dev, list] of Object.entries(byDevice.value)) {
      if (list.some(s => s.session_id === sessionId)) revisions.set(dev, (revisions.get(dev) ?? 0) + 1)
      next[dev] = list.filter((s) => s.session_id !== sessionId)
    }
    byDevice.value = next
  }

  function reset(): void {
    generation++; inflight.clear(); revisions.clear(); drafts.clear(); sources.value = {}; loadStates.value = {}; loading.value = false
    byDevice.value = {}
    error.value = null
    source.value = null
  }

  return {
    byDevice,
    loading,
    error,
    source, sources, loadStates, setDraft, takeDraft,
    forDevice, recordsFor,
    setFor,
    load,
    create,
    close,
    archive,
    remove,
    patch,
    removeFromList,
    reset,
  }
})

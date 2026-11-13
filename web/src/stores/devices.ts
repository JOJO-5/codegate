/**
 * 设备列表与配对。
 *
 * 设备信息有**两个来源**，这个 store 只负责 REST 那一路：
 *   - REST `GET /devices`：来自数据库，永远可用（哪怕 Agent 全离线）
 *   - WS 推送：Agent 上下线时服务端会通知，用于刷新在线状态
 *
 * 分开的理由：列表页在 WS 还没连上时也必须能渲染。把列表挂在 WS 上
 * 会让「连接中」变成「白屏」。
 */

import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { api, type DeviceDTO, type PairPreview } from '../lib/api'
import { humanizeError } from '../lib/format'
import { useAuthStore } from './auth'

export const useDevicesStore = defineStore('devices', () => {
  const items = ref<DeviceDTO[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  const loaded = ref(false)
  const checkedAt = ref<Record<string, number>>({})
  const confirmed = ref<Record<string, boolean>>({})
  let generation = 0, lastLoaded = 0
  let pending: Promise<void> | undefined
  const revisions = new Map<string, number>()
  const auth = useAuthStore()
  watch(() => auth.user?.id, reset, { flush: 'sync' })

  function onlineState(id: string, now = Date.now()): 'online' | 'offline' | 'unknown' {
    const device = byId(id)
    if (!device || !confirmed.value[id] || now - (checkedAt.value[id] ?? 0) > 30000) return 'unknown'
    return device.online ? 'online' : 'offline'
  }

  const onlineCount = computed(() => items.value.filter((d) => d.online).length)

  /** 在线优先、其次按名字排序 —— 列表页最想先看到能用的那台。 */
  const sorted = computed(() =>
    [...items.value].sort((a, b) => {
      if (a.online !== b.online) return a.online ? -1 : 1
      return a.name.localeCompare(b.name, 'zh-CN')
    }),
  )

  function byId(id: string): DeviceDTO | undefined {
    return items.value.find((d) => d.id === id)
  }

  function load(force = false): Promise<void> {
    if (pending) return pending
    if (loaded.value && !force && Date.now() - lastLoaded < 15000) return Promise.resolve()
    const current = generation
    const request = loadDevices(current).finally(() => { if (current === generation) { pending = undefined; loading.value = false } })
    pending = request
    return request
  }
  async function loadDevices(current: number): Promise<void> {
    const started = new Map(revisions)
    loading.value = true
    error.value = null
    try {
      const rows = await api.listDevices()
      if (current !== generation) return
      const changed = (id: string) => started.get(id) !== revisions.get(id)
      const previous = new Map(items.value.map(d => [d.id, d]))
      items.value = rows.flatMap(d => changed(d.id) ? previous.has(d.id) ? [previous.get(d.id)!] : [] : [d])
      loaded.value = true
      lastLoaded = Date.now()
      checkedAt.value = Object.fromEntries(items.value.map(d => [d.id, changed(d.id) ? checkedAt.value[d.id] ?? 0 : lastLoaded]))
      confirmed.value = Object.fromEntries(items.value.map(d => [d.id, changed(d.id) ? confirmed.value[d.id] ?? false : true]))
    } catch (e) {
      if (current !== generation) return
      error.value = humanizeError(e)
      confirmed.value = Object.fromEntries(items.value.map(d => [d.id, started.get(d.id) !== revisions.get(d.id) && !!confirmed.value[d.id]]))
    }
  }

  /** 就地更新一台设备的在线状态（WS 推送来时用，避免整表重拉）。 */
  function patch(id: string, changes: Partial<DeviceDTO>): void {
    const i = items.value.findIndex((d) => d.id === id)
    if (i >= 0) {
      revisions.set(id, (revisions.get(id) ?? 0) + 1)
      items.value[i] = { ...items.value[i], ...changes }
      if (changes.online !== undefined) { checkedAt.value[id] = Date.now(); confirmed.value[id] = true }
    }
  }

  async function rename(id: string, name: string): Promise<void> {
    const current = generation
    const updated = await api.renameDevice(id, name)
    if (current !== generation) return
    const i = items.value.findIndex((d) => d.id === id)
    if (i >= 0) patch(id, { name: updated.name })
  }

  async function remove(id: string): Promise<void> {
    const current = generation
    await api.deleteDevice(id)
    if (current !== generation) return
    revisions.set(id, (revisions.get(id) ?? 0) + 1)
    items.value = items.value.filter((d) => d.id !== id)
  }

  // ---- 配对（两阶段）----

  /** 第一步：只预览，不绑定。 */
  function previewPair(code: string): Promise<PairPreview> {
    return api.pairPreview(code)
  }

  /** 第二步：用户确认后真正绑定。 */
  async function confirmPair(code: string, name?: string): Promise<void> {
    await api.pairConfirm(code, name)
    await load(true)
  }

  function reset(): void {
    generation++; pending = undefined; lastLoaded = 0; loading.value = false
    revisions.clear()
    checkedAt.value = {}; confirmed.value = {}
    items.value = []
    loaded.value = false
    error.value = null
  }

  return {
    items,
    sorted,
    loading,
    error,
    loaded,
    onlineCount,
    byId,
    checkedAt, onlineState,
    load,
    patch,
    rename,
    remove,
    previewPair,
    confirmPair,
    reset,
  }
})

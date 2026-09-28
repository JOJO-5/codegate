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

import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { api, type DeviceDTO, type PairPreview } from '../lib/api'
import { humanizeError } from '../lib/format'

export const useDevicesStore = defineStore('devices', () => {
  const items = ref<DeviceDTO[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  const loaded = ref(false)

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

  async function load(force = false): Promise<void> {
    if (loading.value) return
    // 已有数据且不是强制刷新时直接返回：设备列表变化很慢，
    // 每次进页面都重新拉一遍只会让界面闪。
    if (loaded.value && !force) return

    loading.value = true
    error.value = null
    try {
      items.value = await api.listDevices()
      loaded.value = true
    } catch (e) {
      error.value = humanizeError(e)
    } finally {
      loading.value = false
    }
  }

  /** 就地更新一台设备的在线状态（WS 推送来时用，避免整表重拉）。 */
  function patch(id: string, changes: Partial<DeviceDTO>): void {
    const i = items.value.findIndex((d) => d.id === id)
    if (i >= 0) {
      items.value[i] = { ...items.value[i], ...changes }
    }
  }

  async function rename(id: string, name: string): Promise<void> {
    const updated = await api.renameDevice(id, name)
    const i = items.value.findIndex((d) => d.id === id)
    if (i >= 0) items.value[i] = updated
  }

  async function remove(id: string): Promise<void> {
    await api.deleteDevice(id)
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
    load,
    patch,
    rename,
    remove,
    previewPair,
    confirmPair,
    reset,
  }
})

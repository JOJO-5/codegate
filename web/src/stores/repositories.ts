import { ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { api, type RepositoryListResult, type CommandAvailability } from '../lib/api'
import { humanizeError } from '../lib/format'
import { useAuthStore } from './auth'

export const useRepositoriesStore = defineStore('repositories', () => {
  const auth = useAuthStore()
  const results = ref<Record<string, RepositoryListResult>>({})
  const commands = ref<Record<string, CommandAvailability[]>>({})
  const errors = ref<Record<string, string>>({})
  const loading = ref<Record<string, boolean>>({})
  const fetchedAt = new Map<string, number>()
  let generation = 0
  watch(() => auth.user?.id, () => { generation++; fetchedAt.clear(); results.value = {}; commands.value = {}; errors.value = {}; loading.value = {} }, { flush: 'sync' })

  async function load(id: string, refresh = false): Promise<void> {
    if (loading.value[id] || (!refresh && results.value[id]?.state === 'ready' && Date.now() - (fetchedAt.get(id) ?? 0) < 15_000)) return
    const current = generation
    loading.value[id] = true
    try {
      const [status, first] = await Promise.all([api.getDeviceUpdateStatus(id), api.getDeviceRepositories(id, 0, refresh)])
      let result = first
      let next = first.next_offset
      while (next !== undefined) {
        const page = await api.getDeviceRepositories(id, next)
        if (page.scanned_at !== first.scanned_at) throw new Error('仓库扫描刚刚更新，请刷新清单。')
        result = { ...result, repositories: [...result.repositories, ...page.repositories] }
        if (page.next_offset !== undefined && page.next_offset <= next) throw new Error('仓库分页响应无效')
        next = page.next_offset
      }
      if (current !== generation) return
      fetchedAt.set(id, Date.now())
      results.value[id] = result
      commands.value[id] = (status.commands ?? []).filter(command => command.installed && command.allowed)
      delete errors.value[id]
    } catch (e) {
      if (current === generation) errors.value[id] = humanizeError(e)
    } finally { if (current === generation) loading.value[id] = false }
  }
  return { results, commands, errors, loading, load }
})

import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { api, type ContinuationTask, type Workflow } from '../lib/api'
import { humanizeError } from '../lib/format'
import { workItems, type WorkItem } from '../lib/projectWork'
import { mergeWorkflows } from '../lib/workflows'
import { useAuthStore } from './auth'

interface Snapshot { continuations: ContinuationTask[]; workflows: Workflow[]; continuationLoaded: boolean; workflowLoaded: boolean; checkedAt: number }
export const useWorkCatalogStore = defineStore('workCatalog', () => {
  const auth = useAuthStore()
  const snapshots = ref<Record<string, Snapshot>>({}), errors = ref<Record<string, string>>({})
  const loading = ref<Record<string, boolean>>({}), cancelling = ref<Record<string, boolean>>({}), notices = ref<Record<string, string>>({})
  const epochs = new Map<string, number>()
  let generation = 0
  const items = computed(() => Object.entries(snapshots.value).flatMap(([id, s]) => workItems(id, s.continuations, s.workflows)))
  function empty(): Snapshot { return { continuations: [], workflows: [], continuationLoaded: false, workflowLoaded: false, checkedAt: 0 } }
  watch(() => auth.user?.id, () => {
    generation++; epochs.clear(); snapshots.value = {}; errors.value = {}; loading.value = {}; cancelling.value = {}; notices.value = {}
  }, { flush: 'sync' })
  async function load(deviceId: string): Promise<void> {
    if (loading.value[deviceId] || items.value.some(t => t.deviceId === deviceId && cancelling.value[t.key])) return
    const g = generation, epoch = epochs.get(deviceId) ?? 0
    loading.value = { ...loading.value, [deviceId]: true }
    const results = await Promise.allSettled([
      api.continuations(deviceId).then(result => { if (!Array.isArray(result.tasks)) throw new Error('续跑记录格式无法识别，请刷新或更新服务端'); return result }),
      api.workflows(deviceId).then(result => { if (!Array.isArray(result.workflows)) throw new Error('调度记录格式无法识别，请刷新或更新服务端'); return result }),
    ])
    if (g !== generation || epoch !== (epochs.get(deviceId) ?? 0)) return
    const previous = snapshots.value[deviceId] ?? empty()
    const [continuations, workflows] = results
    snapshots.value = { ...snapshots.value, [deviceId]: {
      continuations: continuations.status === 'fulfilled' ? continuations.value.tasks : previous.continuations,
      workflows: workflows.status === 'fulfilled' ? mergeWorkflows(previous.workflows, workflows.value.workflows) : previous.workflows,
      continuationLoaded: previous.continuationLoaded || continuations.status === 'fulfilled',
      workflowLoaded: previous.workflowLoaded || workflows.status === 'fulfilled', checkedAt: Date.now(),
    } }
    errors.value = { ...errors.value, [deviceId]: results.flatMap((r, i) => r.status === 'rejected' ? [`${i === 0 ? '预约与续跑' : '主 Agent 调度'}：${humanizeError(r.reason)}`] : []).join('；') }
    loading.value = { ...loading.value, [deviceId]: false }
  }
  async function cancel(item: WorkItem): Promise<void> {
    if (cancelling.value[item.key] || item.state === 'cancelling') return
    const g = generation, epoch = (epochs.get(item.deviceId) ?? 0) + 1
    epochs.set(item.deviceId, epoch)
    loading.value = { ...loading.value, [item.deviceId]: false }
    cancelling.value = { ...cancelling.value, [item.key]: true }
    notices.value = { ...notices.value, [item.key]: '' }
    try {
      const saved = item.kind === 'continuation' ? await api.cancelContinuation(item.deviceId, item.id) : await api.cancelWorkflow(item.deviceId, item.id)
      if (g !== generation) return
      const previous = snapshots.value[item.deviceId] ?? empty()
      snapshots.value = { ...snapshots.value, [item.deviceId]: item.kind === 'continuation'
        ? { ...previous, continuations: previous.continuations.map(t => t.id === item.id ? saved as ContinuationTask : t) }
        : { ...previous, workflows: mergeWorkflows(previous.workflows, [saved as Workflow]) } }
      notices.value = { ...notices.value, [item.key]: saved.state === 'cancelling' ? '已请求取消，等待 Agent 确认停止。' : '已收到服务端回执，请核对当前状态。' }
    } catch (e) {
      if (g === generation) notices.value = { ...notices.value, [item.key]: `取消结果尚未确认：${humanizeError(e)}。请刷新核对，不会自动重发。` }
    } finally { if (g === generation) cancelling.value = { ...cancelling.value, [item.key]: false } }
  }
  return { snapshots, errors, loading, cancelling, notices, items, load, cancel }
})

import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { api, type ContinuationTask, type Workflow } from '../lib/api'
import { humanizeError } from '../lib/format'
import { workItems, canRemoveWork, type WorkItem } from '../lib/projectWork'
import { mergeWorkflows } from '../lib/workflows'
import { useAuthStore } from './auth'
import { useDraftsStore } from './drafts'

interface Snapshot { continuations: ContinuationTask[]; workflows: Workflow[]; continuationLoaded: boolean; workflowLoaded: boolean; checkedAt: number }
export const useWorkCatalogStore = defineStore('workCatalog', () => {
  const auth = useAuthStore()
  const drafts = useDraftsStore()
  type RemovedEntry = Pick<WorkItem, 'key' | 'id' | 'deviceId' | 'kind' | 'title'>
  const undoEntries = ref<RemovedEntry[]>([])
  const historyBusy = ref(false), historyNotice = ref('')
  const mutating = ref<Record<string, boolean>>({})
  try {
    const saved = JSON.parse(drafts.get('task-history-undo') || '[]')
    if (Array.isArray(saved)) undoEntries.value = saved.filter(e => e && typeof e.key === 'string' && typeof e.id === 'string' && typeof e.deviceId === 'string' && ['continuation','workflow'].includes(e.kind))
  } catch { /* Broken undo metadata cannot affect execution or task evidence. */ }
  function saveUndo(): void { drafts.set('task-history-undo', JSON.stringify(undoEntries.value)) }
  const snapshots = ref<Record<string, Snapshot>>({}), errors = ref<Record<string, string>>({})
  const loading = ref<Record<string, boolean>>({}), cancelling = ref<Record<string, boolean>>({}), notices = ref<Record<string, string>>({})
  const epochs = new Map<string, number>()
  let generation = 0
  const items = computed(() => Object.entries(snapshots.value).flatMap(([id, s]) => workItems(id, s.continuations, s.workflows)))
  function empty(): Snapshot { return { continuations: [], workflows: [], continuationLoaded: false, workflowLoaded: false, checkedAt: 0 } }
  watch(() => auth.user?.id, () => {
    generation++; epochs.clear(); snapshots.value = {}; errors.value = {}; loading.value = {}; cancelling.value = {}; notices.value = {}; mutating.value = {}; undoEntries.value = []; historyBusy.value = false; historyNotice.value = ''
  }, { flush: 'sync' })
  async function load(deviceId: string): Promise<void> {
    if (loading.value[deviceId] || items.value.some(t => t.deviceId === deviceId && cancelling.value[t.key]) || Object.keys(mutating.value).some(key => key.startsWith(deviceId + ':') && mutating.value[key])) return
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
    if (cancelling.value[item.key] || mutating.value[item.key] || item.state === 'cancelling') return
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
  function replace(item: RemovedEntry, saved: ContinuationTask | Workflow): void {
    const previous = snapshots.value[item.deviceId] ?? empty()
    snapshots.value = { ...snapshots.value, [item.deviceId]: item.kind === 'continuation'
      ? { ...previous, continuations: [...previous.continuations.filter(t => t.id !== item.id), saved as ContinuationTask] }
      : { ...previous, workflows: mergeWorkflows(previous.workflows, [saved as Workflow]) } }
  }
  async function changeHistory(targets: RemovedEntry[], restore: boolean): Promise<void> {
    if (historyBusy.value || !targets.length) return
    const g = generation
    historyBusy.value = true; historyNotice.value = ''
    let confirmed = 0, uncertain = 0
    for (const item of targets) {
      mutating.value[item.key] = true
      epochs.set(item.deviceId, (epochs.get(item.deviceId) ?? 0) + 1)
      loading.value[item.deviceId] = false
    }
    for (const item of targets) {
      if (g !== generation) return
      // Keep an undo identity before the request, including a lost receipt.
      if (!restore && !undoEntries.value.some(e => e.key === item.key)) {
        undoEntries.value.push({ key: item.key, id: item.id, deviceId: item.deviceId, kind: item.kind, title: item.title }); saveUndo()
      }
      try {
        const saved = item.kind === 'continuation'
          ? await (restore ? api.restoreContinuation : api.deleteContinuation)(item.deviceId, item.id)
          : await (restore ? api.restoreWorkflow : api.deleteWorkflow)(item.deviceId, item.id)
        if (g !== generation) return
        replace(item, saved); confirmed++
        if (restore) { undoEntries.value = undoEntries.value.filter(e => e.key !== item.key); saveUndo() }
        notices.value[item.key] = restore ? '已恢复记录，未重新执行任务。' : ''
      } catch (e) {
        if (g !== generation) return
        uncertain++
        notices.value[item.key] = `${restore ? '恢复' : '移除'}结果待核对：${humanizeError(e)}。请刷新核对；保留撤销入口。`
      }
    }
    if (g !== generation) return
    historyNotice.value = `已确认${restore ? '恢复' : '移除'} ${confirmed} 条记录${uncertain ? `，${uncertain} 条未确认，请刷新核对` : ''}。项目文件、对话和执行证据保留；不会重新运行任务。`
    for (const item of targets) delete mutating.value[item.key]
    historyBusy.value = false
  }
  async function remove(targets: WorkItem[]): Promise<void> { await changeHistory(targets.filter(canRemoveWork), false) }
  async function undoRemoval(): Promise<void> { await changeHistory([...undoEntries.value], true) }
  return { snapshots, errors, loading, cancelling, notices, items, load, cancel, historyBusy, historyNotice, undoEntries, remove, undoRemoval }
})

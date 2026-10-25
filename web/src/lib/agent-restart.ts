import { ref } from 'vue'
import type { DeviceUpdateStatus } from './api'

type Status = Pick<DeviceUpdateStatus, 'online' | 'agent_version' | 'update'>
export interface RestartIntent { instance: number; version: string; target?: string; checkedAt: number }
interface Dependencies {
  current: () => Status | null
  submit: (instance: number) => Promise<unknown>
  read: () => RestartIntent | null
  write: (intent: RestartIntent | null) => void
  describe: (error: unknown) => string
}
export function createImmediateRestart(deps: Dependencies) {
  const dialog = ref(false), busy = ref(false), message = ref('')
  const phase = ref<'idle' | 'pending' | 'unknown' | 'error' | 'complete' | 'cancelled'>('idle')
  let intent = deps.read(), snapshot = 0, generation = 0
  if (intent) { phase.value = 'unknown'; message.value = '上次立即重启结果尚未确认，请核对设备状态；不会自动重复提交。' }
  function open() {
    const status = deps.current()
    if (!status?.online || !status.update?.managed || !status.update.can_restart_now || !status.update.instance_started_at) {
      message.value = '当前设备离线或 Agent 不支持立即重启，请先完成一次空闲升级。'; return
    }
    if (busy.value || ['draining', 'switching'].includes(status.update.state)) return
    snapshot = status.update.instance_started_at
    dialog.value = true
    message.value = ''
  }
  function close() { if (!busy.value) dialog.value = false }
  async function confirm() {
    if (!dialog.value || busy.value) return
    const status = deps.current()
    if (!status?.online || !status.update?.can_restart_now || snapshot !== status.update.instance_started_at) {
      message.value = '设备状态或 Agent 实例已变化，请取消后重新核对。'; return
    }
    const gen = ++generation
    busy.value = true
    intent = { instance: snapshot, version: status.agent_version ?? '', target: status.update.version, checkedAt: status.update.checked_at ?? 0 }
    deps.write(intent)
    try {
      await deps.submit(snapshot)
      if (gen !== generation) return
      phase.value = 'pending'
      message.value = 'Agent 已受理：先检查更新，再停止任务并重启；等待新实例上线确认。'
    } catch (error) {
      if (gen !== generation) return
      phase.value = 'unknown'
      message.value = `请求结果尚未确认：${deps.describe(error)}。请核对状态，不会自动重复提交。`
    } finally {
      if (gen === generation) { busy.value = false; dialog.value = false }
    }
  }
  function observe(status: Status | null) {
    if (!intent || !status?.online || !status.update) return
    const update = status.update
    if (update.instance_started_at && update.instance_started_at < intent.instance) return
    if (update.instance_started_at && update.instance_started_at > intent.instance) {
      const upgraded = !intent.target || status.agent_version === intent.target
      phase.value = upgraded ? 'complete' : 'error'
      message.value = `Agent 已重新上线，当前版本 ${status.agent_version ?? '待确认'}。${upgraded ? '' : '尚未运行原目标版本，请查看升级失败说明。'}`
    } else if (update.state === 'error' && (update.checked_at ?? 0) > intent.checkedAt) {
      phase.value = 'error'; message.value = update.detail || '立即重启未完成，请核对 Agent 状态。'
    } else if (!update.immediate_restart && !update.restart_scheduled && ['current', 'waiting', 'staged'].includes(update.state) && (update.checked_at ?? 0) > intent.checkedAt) {
      phase.value = 'cancelled'; message.value = '当前未安排立即重启；已下载更新仍按空闲策略处理。'
    } else {
      if (update.version) { intent.target = update.version; deps.write(intent) }
      if (update.state === 'draining') { phase.value = 'pending'; message.value = '正在停止任务并关闭终端，完成后重启；此阶段不能撤销。' }
      else if (update.state === 'switching') { phase.value = 'pending'; message.value = '任务已停止，正在等待新 Agent 上线。' }
      return
    }
    intent = null; deps.write(null)
  }
  return { dialog, busy, message, phase, open, close, confirm, observe, dispose: () => { generation++ } }
}

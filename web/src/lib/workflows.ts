import type { Workflow } from './api'

export const workflowLabels: Record<string, string> = {
  queued: '等待调度', waiting_agent: '等待设备上线', waiting_idle: '等待本机调度空闲',
  dispatching: '正在交给主 Agent', running: '主 Agent 决策中', delegating: '工作 CLI 执行中',
  checking: '实际验证中', cancelling: '等待确认停止', cancelled: '已确认取消',
  ready_for_review: '已交付 · 待你验收', needs_review: '需要你核对', failed: '执行失败',
}
export function workflowTerminal(state: string): boolean {
  return ['ready_for_review', 'cancelled', 'needs_review', 'failed'].includes(state)
}
/** A delayed list or cancellation response must never replace newer evidence. */
export function mergeWorkflows(current: Workflow[], incoming: Workflow[]): Workflow[] {
  const rows = new Map(current.map(row => [row.id, row]))
  for (const row of incoming) if (!rows.has(row.id) || row.revision > rows.get(row.id)!.revision) rows.set(row.id, row)
  return [...rows.values()].sort((a, b) => Number(workflowTerminal(a.state)) - Number(workflowTerminal(b.state)) || b.updated_at - a.updated_at)
}

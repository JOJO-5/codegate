import type { ContinuationTask, Workflow } from './api'

/** Match the saved directory, not a prefix. This only groups UI records. */
export function inProject(path: string, cwd: string, platform = ''): boolean {
  const normalize = (value: string) => {
    const clean = value.replace(/\\/g, '/').replace(/\/+$/, '')
    return platform === 'windows' ? clean.toLocaleLowerCase() : clean
  }
  return normalize(path) === normalize(cwd)
}
export type WorkGroup = 'active' | 'review' | 'planned' | 'history'
export function taskGroup(kind: string, state: string, mode?: string): WorkGroup {
  if (['failed', 'needs_review', 'ready_for_review'].includes(state)) return 'review'
  if (['completed', 'cancelled'].includes(state)) return 'history'
  if (kind === 'continuation' && mode !== 'live_quota' && ['queued', 'waiting_agent', 'waiting_idle', 'waiting_quota'].includes(state)) return 'planned'
  if (['queued', 'waiting_agent', 'waiting_idle', 'waiting_wall', 'waiting_retry', 'waiting_quota', 'dispatching', 'running', 'delegating', 'checking', 'cancelling'].includes(state)) return 'active'
  return 'review'
}
export interface WorkItem {
  key: string; id: string; deviceId: string; kind: 'continuation' | 'workflow'; group: WorkGroup
  title: string; cwd: string; state: string; detail: string; updatedAt: number
  continuation?: ContinuationTask; workflow?: Workflow
}
export function workItems(deviceId: string, continuations: ContinuationTask[], workflows: Workflow[]): WorkItem[] {
  return [
    ...continuations.filter(t => !t.deleted_at).map(t => ({
      key: `${deviceId}:continuation:${t.id}`, id: t.id, deviceId, kind: 'continuation' as const,
      group: taskGroup('continuation', t.state, t.mode), title: t.source.name || '原对话续跑', cwd: t.source.cwd,
      state: t.state, detail: t.detail || '', updatedAt: t.updated_at || t.created_at || 0, continuation: t,
    })),
    ...workflows.map(t => ({
      key: `${deviceId}:workflow:${t.id}`, id: t.id, deviceId, kind: 'workflow' as const,
      group: taskGroup('workflow', t.state), title: t.goal, cwd: t.cwd, state: t.state,
      detail: t.detail || '', updatedAt: t.updated_at, workflow: t,
    })),
  ].sort((a, b) => b.updatedAt - a.updatedAt || a.key.localeCompare(b.key))
}

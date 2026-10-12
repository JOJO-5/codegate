import type { SessionSummary } from './protocol'
import { nativeCLI } from './sessionQuota'

/** A native conversation survives multiple terminal processes. Unknown identities
 * stay separate: names, working directories and timestamps are never evidence. */
export function conversationKey(s: SessionSummary): string {
  const r = s.recovery
  if (!r?.native_id || !r.command_id) return JSON.stringify(['terminal', s.session_id])
  return JSON.stringify(['native', s.device_id.replace(/-/g, '').toLowerCase(), nativeCLI(s.command, s.args) || r.command_id,
    r.store_key ?? '', r.native_root ?? '', r.native_id])
}
export const liveSession = (s: SessionSummary): boolean => ['starting', 'running', 'detached'].includes(s.status)
export function conversationRecords(records: SessionSummary[], sessionId: string): SessionSummary[] {
  const source = records.find(s => s.session_id === sessionId)
  return source ? records.filter(s => conversationKey(s) === conversationKey(source)) : []
}
export function groupConversationSessions(records: SessionSummary[]): SessionSummary[] {
  const groups = new Map<string, SessionSummary[]>()
  for (const record of records) {
    const key = conversationKey(record)
    const group = groups.get(key) ?? []
    group.push(record); groups.set(key, group)
  }
  return Array.from(groups.values(), group => {
    const sorted = [...group].sort((a, b) => Number(liveSession(b)) - Number(liveSession(a))
      || (b.last_attached_at || b.created_at) - (a.last_attached_at || a.created_at)
      || b.created_at - a.created_at || a.session_id.localeCompare(b.session_id))
    return { ...sorted[0]!, archived: group.every(s => Boolean(s.archived)) }
  })
}

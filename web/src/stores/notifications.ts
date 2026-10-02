import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { useAuthStore } from './auth'
import { useSessionsStore } from './sessions'
import { MessageType, type Envelope, type SessionExitPayload } from '../lib/protocol'

export const useNotificationsStore = defineStore('notifications', () => {
  const auth = useAuthStore()
  const sessions = useSessionsStore()
  const enabled = ref(false)
  const permission = ref<NotificationPermission | 'unsupported'>('unsupported')
  const error = ref('')
  const events = ref<{ sessionId: string; label: string; detail: string; at: number }[]>([])
  const seen = new Set<string>()
  const key = computed(() => `codegate.process-notifications.${auth.user?.id ?? ''}`)
  function readPreference(): void {
    permission.value = typeof Notification === 'undefined' ? 'unsupported' : Notification.permission
    try { enabled.value = !!auth.user && localStorage.getItem(key.value) === 'true' && permission.value === 'granted' }
    catch { enabled.value = false }
  }
  watch(() => auth.user?.id, () => { events.value = []; seen.clear(); error.value = ''; readPreference() }, { immediate: true, flush: 'sync' })
  async function enable(): Promise<void> {
    error.value = ''
    if (typeof Notification === 'undefined' || !window.isSecureContext) { error.value = '此浏览器或连接不支持系统通知，仍可查看页面中的退出事件。'; return }
    try {
      permission.value = await Notification.requestPermission()
      enabled.value = permission.value === 'granted'
      if (!enabled.value) { error.value = '浏览器没有允许通知，可在网站权限设置中调整。'; return }
      localStorage.setItem(key.value, 'true')
    } catch { enabled.value = false; error.value = '无法启用通知，请检查浏览器权限。' }
  }
  function disable(): void { enabled.value = false; try { localStorage.removeItem(key.value) } catch { /* This browser session still disables notifications. */ } }
  function receive(env: Envelope): void {
    if (!auth.user || env.type !== MessageType.SessionExit) return
    const payload = env.payload as SessionExitPayload | undefined
    if (!payload || typeof payload.session_id !== 'string' || typeof payload.exit_code !== 'number' || payload.reason === 'terminated' || payload.reason === 'user_requested' || seen.has(payload.session_id)) return
    const summary = Object.values(sessions.byDevice).flat().find(s => s.session_id === payload.session_id)
    const label = summary?.name || `会话 ${payload.session_id.slice(0, 8)}`
    const detail = `进程已退出，${payload.exit_code >= 0 ? `退出码 ${payload.exit_code}` : '退出码未知'}。任务结果请查看输出和测试记录。`
    seen.add(payload.session_id); if (seen.size > 200) seen.delete(seen.values().next().value!)
    events.value = [{ sessionId: payload.session_id, label, detail, at: Date.now() }, ...events.value].slice(0, 20)
    sessions.patch(payload.session_id, { status: 'exited', exit_code: payload.exit_code, ended_at: Date.now() })
    // System notifications are opt-in and emitted only for a background page.
    // This is a connected-browser feature, not a push service for closed pages.
    if (enabled.value && typeof Notification !== 'undefined' && Notification.permission === 'granted' && document.hidden) {
      try { const notice = new Notification('CodeGate · 进程已退出', { body: `${payload.exit_code >= 0 ? `退出码 ${payload.exit_code}` : '退出码未知'}；请返回页面查看结果。`, tag: payload.session_id }); setTimeout(() => notice.close(), 10000) }
      catch { error.value = '系统通知不可用，退出事件已保留在页面中。' }
    }
  }
  function clear(): void { events.value = [] }
  return { enabled, permission, error, events, enable, disable, receive, clear }
})

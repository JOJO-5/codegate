<script setup lang="ts">
import { ref, watch, onMounted, onBeforeUnmount } from 'vue'
import { useConnStore } from '../stores/conn'
import { MessageType } from '../lib/protocol'
import { humanizeError } from '../lib/format'
interface Window { label: string; used_percent: number; resets_at?: number }
interface Provider { provider: string; source: string; status: string; message?: string; checked_at: number; windows: Window[] }
const props = defineProps<{ sessionId: string; provider: string }>()
const conn = useConnStore()
const providers = ref<Provider[]>([])
const busy = ref(false)
const error = ref('')
const panel = ref<HTMLElement | null>(null)
const visible = ref(false)
const cached = ref(false)
const nextRefreshAt = ref<number | undefined>()
const elapsed = ref(0)
let started = 0, lastAttempt = 0, epoch = 0
let timer: ReturnType<typeof setInterval> | undefined
let observer: IntersectionObserver | undefined
function autoRefresh(): void {
  elapsed.value = busy.value ? Math.floor((Date.now() - started) / 1000) : 0
  if (visible.value && document.visibilityState === 'visible' && conn.isOpen && !busy.value && Date.now() - lastAttempt >= 30_000) void refresh()
}
onMounted(() => {
  observer = new IntersectionObserver(entries => { visible.value = entries.some(e => e.isIntersecting); autoRefresh() })
  if (panel.value) observer.observe(panel.value)
  timer = setInterval(autoRefresh, 1000)
  document.addEventListener('visibilitychange', autoRefresh)
})
onBeforeUnmount(() => { epoch++; observer?.disconnect(); clearInterval(timer); document.removeEventListener('visibilitychange', autoRefresh) })
const names: Record<string,string> = { codex: 'Codex', claude: 'Claude', opencode: 'OpenCode Go' }
function date(value?: number): string { return value && value > 0 ? new Date(value * 1000).toLocaleString() : '未知' }
async function refresh(): Promise<void> {
  if (busy.value || !conn.isOpen) return
  busy.value = true
  started = lastAttempt = Date.now()
  elapsed.value = 0
  const current = ++epoch
  error.value = ''
  const id = props.sessionId
  try {
    const env = await conn.request<{ providers: Provider[]; cached?: boolean; next_refresh_at?: number }>(MessageType.QuotaRead, { session_id: id }, id, 15_000)
    if (current !== epoch || id !== props.sessionId) return
    const results = (env.payload?.providers ?? []).filter(p => p.provider === props.provider)
    if (!results.length) throw new Error('未返回当前 CLI 的额度，请更新 Agent 后重试')
    providers.value = results
    cached.value = Boolean(env.payload?.cached)
    nextRefreshAt.value = env.payload?.next_refresh_at
  } catch (e) { if (current === epoch) error.value = humanizeError(e) }
  finally { if (current === epoch) busy.value = false }
}
watch(() => [props.sessionId, props.provider, conn.isOpen], () => { epoch++; busy.value = false; if (conn.isOpen) void refresh() }, { immediate: true })
</script>
<template>
  <section ref="panel" class="quota" aria-label="账号额度">
    <div class="quota__heading"><strong>{{ names[props.provider] || props.provider }} 额度</strong><button class="btn btn--ghost btn--sm" :disabled="busy || !conn.isOpen" @click="refresh">{{ busy ? `查询中 ${elapsed}s…` : '刷新' }}</button></div>
    <p class="small dim">设备本机登录账号，与终端当前模型可能不同。</p>
    <p v-if="error" class="small" role="alert">{{ error }}{{ providers.length ? '（下方为上次结果）' : '' }}</p>
    <article v-for="p in providers" :key="p.provider" class="quota__provider">
      <strong :title="p.source">{{ names[p.provider] || p.provider }}</strong>
      <template v-if="p.status === 'ok'">
        <div v-for="w in p.windows" :key="w.label" class="quota__window">
          <div class="quota__heading"><span>{{ w.label }}</span><span>剩余 {{ Math.round((100 - w.used_percent)*10)/10 }}%</span></div>
          <meter :value="100-w.used_percent" min="0" max="100" :aria-label="`${p.provider} ${w.label} 剩余额度`" />
          <small class="dim">重置：{{ date(w.resets_at) }}</small>
        </div>
      </template>
      <details v-else class="small dim"><summary>暂不可用 · 查看原因</summary><p>{{ p.message || '额度暂不可用' }}</p></details>
      <small class="dim">{{ p.source }}</small><br />
      <small class="dim">查询：{{ date(p.checked_at) }}</small>
    </article>
    <p v-if="cached" class="small dim">本次为缓存结果<span v-if="nextRefreshAt">，{{ date(nextRefreshAt) }} 后可重新查询</span>。</p>
    <p class="small dim">页面可见时每 30 秒自动更新；Agent 缓存 15 秒。查询最多等待 15 秒；失败时保留上次结果。</p>
  </section>
</template>
<style scoped>
.quota { margin-top: auto; padding: 10px 6px; font-size: 12px; overflow-wrap: anywhere; flex-shrink: 0; max-height: 55%; overflow-y: auto; }
.quota__heading { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.quota__provider { border-top: 1px solid var(--border); padding: 10px 0; }
.quota__window { margin: 8px 0; }
meter { display: block; width: 100%; height: 8px; margin: 5px 0; }
p { margin: 6px 0; }
</style>

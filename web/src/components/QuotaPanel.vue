<script setup lang="ts">
import { ref, watch } from 'vue'
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
const names: Record<string,string> = { codex: 'Codex', claude: 'Claude', opencode: 'OpenCode Go' }
function date(value?: number): string { return value && value > 0 ? new Date(value * 1000).toLocaleString() : '未知' }
async function refresh(): Promise<void> {
  if (busy.value || !conn.isOpen) return
  busy.value = true
  error.value = ''
  const id = props.sessionId
  try {
    const env = await conn.request<{ providers: Provider[] }>(MessageType.QuotaRead, { session_id: id }, id)
    if (id === props.sessionId) providers.value = (env.payload?.providers ?? []).filter(p => p.provider === props.provider)
  } catch (e) { error.value = humanizeError(e) }
  finally { busy.value = false }
}
watch(() => [props.sessionId, props.provider, conn.isOpen], () => { if (conn.isOpen) void refresh() }, { immediate: true })
</script>
<template>
  <section class="quota" aria-label="账号额度">
    <div class="quota__heading"><strong>{{ names[props.provider] || props.provider }} 额度</strong><button class="btn btn--ghost btn--sm" :disabled="busy || !conn.isOpen" @click="refresh">{{ busy ? '刷新中…' : '刷新' }}</button></div>
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
    <p class="small dim">连续刷新间隔 15 秒；仅查询，不产生模型调用。</p>
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

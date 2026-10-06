<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { useRouter } from 'vue-router'
import { useConnStore } from '../stores/conn'
import { useSessionsStore } from '../stores/sessions'
import { MessageType, type SessionSummary, type SessionCreatedPayload } from '../lib/protocol'
import { estimateTerminalSize } from '../lib/viewport'
import { absoluteTime, humanizeError } from '../lib/format'

const props = defineProps<{ source: SessionSummary }>()
const emit = defineEmits<{ close: [] }>()
const router = useRouter(), conn = useConnStore(), sessions = useSessionsStore()
const busy = ref(true), error = ref('')
const conversations = ref<{id: string; title: string; created_at: number; updated_at: number}[]>([])
const selected = ref('')
let cancelled = false
onBeforeUnmount(() => { cancelled = true })
async function restore(id = ''): Promise<void> {
  busy.value = true; error.value = ''
  try {
    const size = estimateTerminalSize()
    const env = await conn.request<SessionCreatedPayload>(MessageType.ConversationRestore,
      { session_id: props.source.session_id, native_id: id, ...size }, props.source.session_id)
    if (cancelled) return
    const session = env.payload?.session
    if (!session) throw new Error('未收到恢复后的会话')
    await sessions.load(props.source.device_id, true)
    emit('close')
    await router.push({ name: 'session', params: { id: session.session_id } })
  } catch (e) { if (!cancelled) error.value = humanizeError(e) }
  finally { busy.value = false }
}
onMounted(async () => {
  if (props.source.recovery?.native_id) { await restore(); return }
  try {
    const env = await conn.request<{conversations: typeof conversations.value}>(MessageType.ConversationList,
      { session_id: props.source.session_id }, props.source.session_id)
    if (!cancelled) conversations.value = env.payload?.conversations ?? []
  } catch (e) { if (!cancelled) error.value = humanizeError(e) }
  finally { busy.value = false }
})
</script>

<template>
  <div class="recovery-backdrop">
    <section class="recovery-panel card" role="dialog" aria-modal="true" aria-labelledby="recovery-title">
      <h2 id="recovery-title">恢复原对话</h2>
      <p class="dim small">{{ source.name }} · {{ source.cwd }}</p>
      <p v-if="busy" role="status">正在读取或恢复原生对话…</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <template v-if="!source.recovery?.native_id && !busy && !error">
        <p class="small">这条记录尚未绑定原生对话，请核对标题、时间和 ID 后选择。选定后会记住关联。</p>
        <p v-if="!conversations.length" class="dim">该目录没有可恢复的原生历史。CLI 必须已保存对话；被删除的历史无法恢复。</p>
        <div class="recovery-options">
          <label v-for="item in conversations" :key="item.id" class="recovery-option">
            <input v-model="selected" type="radio" name="native-conversation" :value="item.id">
            <span>{{ item.title }}<small class="dim">更新于 {{ absoluteTime(item.updated_at) }}<br>{{ item.id }}</small></span>
          </label>
        </div>
      </template>
      <div class="recovery-actions">
        <button class="btn" type="button" :disabled="busy" @click="emit('close')">返回会话列表</button>
        <button v-if="!source.recovery?.native_id" class="btn btn--primary" type="button" :disabled="busy || !selected" @click="restore(selected)">恢复选定对话</button>
        <button v-else-if="error" class="btn btn--primary" type="button" :disabled="busy" @click="restore()">重试</button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.recovery-backdrop{position:fixed;inset:0;z-index:80;background:#0008;display:flex;align-items:center;justify-content:center;padding:12px}
.recovery-panel{max-width:600px;width:100%;max-height:calc(100dvh - 24px);overflow:auto;overflow-wrap:anywhere}
.recovery-options{display:grid;gap:8px;margin:12px 0}.recovery-option{display:flex;gap:10px;align-items:flex-start;padding:12px;border:1px solid var(--border);border-radius:8px;cursor:pointer}.recovery-option small{display:block;margin-top:5px}.recovery-actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:16px}
</style>

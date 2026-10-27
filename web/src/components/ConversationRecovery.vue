<script setup lang="ts">
import ModalDialog from './ModalDialog.vue'
import { computed, ref, watch, onMounted, onBeforeUnmount } from 'vue'
import { useRouter } from 'vue-router'
import { useConnStore } from '../stores/conn'
import { useSessionsStore } from '../stores/sessions'
import { MessageType, type SessionSummary, type SessionCreatedPayload } from '../lib/protocol'
import { estimateTerminalSize } from '../lib/viewport'
import { absoluteTime, humanizeError } from '../lib/format'

const props = defineProps<{ source: SessionSummary; projectKey?: string }>()
const emit = defineEmits<{ close: [] }>()
const router = useRouter(), conn = useConnStore(), sessions = useSessionsStore()
const busy = ref(false), error = ref('')
const phase = ref<'list' | 'restore'>('list')
const conversations = ref<{id: string; title: string; created_at: number; updated_at: number}[]>([])
const selected = ref('')
let cancelled = false
let generation = 0
const bound = computed(() => !!props.source.recovery?.native_id)
onBeforeUnmount(() => { cancelled = true; generation++ })
async function restore(id = ''): Promise<void> {
  if (busy.value || cancelled) return
  const current = generation, source = props.source
  busy.value = true; phase.value = 'restore'; error.value = ''
  try {
    const size = estimateTerminalSize()
    const env = await conn.request<SessionCreatedPayload>(MessageType.ConversationRestore,
      { session_id: source.session_id, native_id: id, ...size }, source.session_id)
    if (cancelled || current !== generation) return
    const session = env.payload?.session
    if (!session) throw new Error('未收到恢复后的会话')
    const device = source.device_id.replace(/-/g, '')
    sessions.setFor(device, [session, ...(sessions.byDevice[device] ?? []).filter(s => s.session_id !== session.session_id)])
    void sessions.load(device, true)
    emit('close')
    await router.push({ name: 'session', params: { id: session.session_id }, query: props.projectKey ? { project: props.projectKey } : {} })
  } catch (e) { if (!cancelled && current === generation) error.value = humanizeError(e) }
  finally { if (!cancelled && current === generation) busy.value = false }
}
async function loadConversations(): Promise<void> {
  if (busy.value || cancelled) return
  const current = generation, source = props.source
  busy.value = true; phase.value = 'list'; error.value = ''
  try {
    const env = await conn.request<{conversations: typeof conversations.value}>(MessageType.ConversationList,
      { session_id: source.session_id }, source.session_id)
    if (!cancelled && current === generation) conversations.value = env.payload?.conversations ?? []
  } catch (e) { if (!cancelled && current === generation) error.value = humanizeError(e) }
  finally { if (!cancelled && current === generation) busy.value = false }
}
function initialize(): void {
  if (bound.value) void restore()
  else void loadConversations()
}
watch([() => props.source.session_id, () => props.source.recovery?.native_id], () => {
  generation++; busy.value = false; error.value = ''; conversations.value = []; selected.value = ''
  initialize()
})
onMounted(initialize)
</script>

<template>
  <ModalDialog title-id="recovery-title" :busy="busy" @close="emit('close')">
    <section class="recovery-panel card" >
      <h2 id="recovery-title">恢复原对话</h2>
      <p class="dim small">{{ source.name }} · {{ source.cwd }}</p>
      <p v-if="busy" role="status">{{ phase === 'list' ? '正在读取原生对话历史…' : '正在恢复原对话，请等待设备回执…' }}</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <p v-if="error && phase === 'restore'" class="small dim">尚未确认恢复成功。请检查设备连接后重试；原生对话历史保留。</p>
      <template v-if="!bound && !busy">
        <p class="small">这条记录尚未绑定原生对话，请核对标题、时间和 ID 后选择。选定后会记住关联。</p>
        <p v-if="!conversations.length && !error" class="dim">该目录没有可恢复的原生历史。CLI 必须已保存对话；被删除的历史无法恢复。</p>
        <div class="recovery-options">
          <label v-for="item in conversations" :key="item.id" class="recovery-option">
            <input v-model="selected" type="radio" name="native-conversation" :value="item.id">
            <span>{{ item.title }}<small class="dim">更新于 {{ absoluteTime(item.updated_at) }}<br>{{ item.id }}</small></span>
          </label>
        </div>
      </template>
      <div class="recovery-actions">
        <button class="btn" type="button" :disabled="busy" @click="emit('close')">返回会话</button>
        <button v-if="!bound && error && phase === 'list'" class="btn" type="button" :disabled="busy" @click="loadConversations">重试读取历史</button>
        <button v-if="!bound" class="btn btn--primary" type="button" :disabled="busy || !selected" @click="restore(selected)">恢复选定对话</button>
        <button v-else-if="error" class="btn btn--primary" type="button" :disabled="busy" @click="restore()">重试</button>
      </div>
    </section>
  </ModalDialog>
</template>

<style scoped>
.recovery-backdrop{position:fixed;inset:0;z-index:80;background:#0008;display:flex;align-items:center;justify-content:center;padding:12px}
.recovery-panel{max-width:600px;width:100%;max-height:calc(100dvh - 24px);overflow:auto;overflow-wrap:anywhere}
.recovery-options{display:grid;gap:8px;margin:12px 0}.recovery-option{display:flex;gap:10px;align-items:flex-start;padding:12px;border:1px solid var(--border);border-radius:8px;cursor:pointer}.recovery-option small{display:block;margin-top:5px}.recovery-actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:16px}
</style>

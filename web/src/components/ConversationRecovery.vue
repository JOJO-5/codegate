<script setup lang="ts">
import ModalDialog from './ModalDialog.vue'
import { computed, ref, watch, onMounted, onBeforeUnmount } from 'vue'
import { useRouter } from 'vue-router'
import { useConnStore } from '../stores/conn'
import { useSessionsStore } from '../stores/sessions'
import { useDraftsStore } from '../stores/drafts'
import { MessageType, type SessionSummary, type SessionCreatedPayload } from '../lib/protocol'
import { estimateTerminalSize } from '../lib/viewport'
import { absoluteTime, humanizeError } from '../lib/format'

const props = defineProps<{ source: SessionSummary; projectKey?: string }>()
const emit = defineEmits<{ close: [] }>()
const router = useRouter(), conn = useConnStore(), sessions = useSessionsStore()
const drafts = useDraftsStore()
const busy = ref(false), error = ref('')
const uncertain = ref(false)
const verifiedMissing = ref(false), retryConfirmation = ref(false)
const phase = ref<'list' | 'restore' | 'check'>('list')
const conversations = ref<{id: string; title: string; created_at: number; updated_at: number}[]>([])
const selected = ref('')
let cancelled = false
let generation = 0
const bound = computed(() => !!props.source.recovery?.native_id)
const pendingKey = () => `recovery-pending:${props.source.device_id}:${props.source.session_id}`
let pendingNativeID = ''
onBeforeUnmount(() => { cancelled = true; generation++ })
async function restore(id = ''): Promise<void> {
  if (busy.value || cancelled || uncertain.value) return
  const current = generation, source = props.source
  verifiedMissing.value = false; retryConfirmation.value = false
  pendingNativeID = id || source.recovery?.native_id || ''
  // Store intent before dispatch so a refresh cannot silently start it again.
  drafts.set(pendingKey(), JSON.stringify({ native_id: pendingNativeID }))
  busy.value = true; phase.value = 'restore'; error.value = ''
  try {
    const size = estimateTerminalSize()
    const env = await conn.request<SessionCreatedPayload>(MessageType.ConversationRestore,
      { session_id: source.session_id, native_id: id, ...size }, source.session_id, 55_000)
    if (cancelled || current !== generation) return
    const session = env.payload?.session
    if (!session) throw new Error('未收到恢复后的会话')
    await openSession(session)
  } catch (e) {
    if (!cancelled && current === generation) {
      const code = e && typeof e === 'object' && 'code' in e ? e.code : ''
      uncertain.value = code === 'timeout' || code === 'request_outcome_unknown' ||
        (e instanceof Error && /^连接已(断开|关闭)|未收到恢复后的会话/.test(e.message))
      if (!uncertain.value) drafts.set(pendingKey(), '')
      error.value = uncertain.value ? '恢复结果尚未确认，请先检查已恢复的终端，不要重复恢复。' : humanizeError(e)
    }
  }
  finally { if (!cancelled && current === generation) busy.value = false }
}
async function openSession(session: SessionSummary): Promise<void> {
  drafts.set(pendingKey(), '')
  const device = props.source.device_id.replace(/-/g, '')
  sessions.setFor(device, [session, ...(sessions.byDevice[device] ?? []).filter(s => s.session_id !== session.session_id)])
  void sessions.load(device, true)
  emit('close')
  await router.push({ name: 'session', params: { id: session.session_id }, query: props.projectKey ? { project: props.projectKey } : {} })
}
async function checkResult(): Promise<void> {
  if (busy.value || cancelled || !uncertain.value) return
  const current = generation, source = props.source
  busy.value = true; phase.value = 'check'; error.value = ''
  verifiedMissing.value = false; retryConfirmation.value = false
  try {
    const device = source.device_id.replace(/-/g, '')
    await sessions.load(device, true)
    if (cancelled || current !== generation) return
    if (sessions.sources[device] !== 'live') throw new Error('无法确认设备的实时会话，请连接设备后再次检查；不会重复恢复。')
    const binding = source.recovery
    const target = binding?.native_id || pendingNativeID || selected.value
    const matches = (sessions.byDevice[device] ?? []).filter(s =>
      ['starting', 'running', 'detached'].includes(s.status) && s.cwd === source.cwd &&
      (s.session_id === source.session_id || !!target && s.recovery?.native_id === target &&
        (binding?.command_id ? s.recovery?.command_id === binding.command_id : s.command === source.command) &&
        s.recovery?.store_key === binding?.store_key && s.recovery?.native_root === binding?.native_root))
    if (matches.length !== 1) {
      verifiedMissing.value = matches.length === 0 && !!target
      throw new Error(matches.length ? '找到多个可能的终端，请返回会话列表核对。' : '暂未发现已恢复的终端，原请求可能仍在处理；可稍后检查，或核对后确认重新恢复。')
    }
    await openSession(matches[0]!)
  } catch (e) { if (!cancelled && current === generation) error.value = humanizeError(e) }
  finally { if (!cancelled && current === generation) busy.value = false }
}
async function confirmRetry(): Promise<void> {
  if (busy.value || cancelled || !uncertain.value || !verifiedMissing.value || !retryConfirmation.value || !conn.isOpen) return
  uncertain.value = false
  await restore(pendingNativeID || selected.value)
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
  const pending = drafts.get(pendingKey())
  if (pending) {
    try { const saved = JSON.parse(pending); pendingNativeID = typeof saved.native_id === 'string' ? saved.native_id : '' } catch { pendingNativeID = '' }
    uncertain.value = true
    error.value = '上次恢复结果尚未确认，请检查恢复结果；不会重复启动 CLI。'
    return
  }
  if (bound.value) void restore()
  else void loadConversations()
}
watch([() => props.source.session_id, () => props.source.recovery?.native_id], () => {
  generation++; busy.value = false; uncertain.value = false; verifiedMissing.value = false; retryConfirmation.value = false; error.value = ''; conversations.value = []; selected.value = ''
  initialize()
})
watch(() => drafts.owner, () => {
  cancelled = true; generation++; busy.value = false
  error.value = '账号已变化，请关闭面板后重新选择会话。'
})
onMounted(initialize)
</script>

<template>
  <ModalDialog title-id="recovery-title" :busy="busy" @close="emit('close')">
    <section class="recovery-panel card" >
      <h2 id="recovery-title">恢复原对话</h2>
      <p class="dim small">{{ source.name }} · {{ source.cwd }}</p>
      <p v-if="busy" role="status">{{ phase === 'list' ? '正在读取原生对话历史…' : phase === 'check' ? '正在核对设备的实时会话…' : '正在恢复原对话，请等待设备回执…' }}</p>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <p v-if="drafts.storageUnavailable" class="notice notice--warn small">浏览器无法保存恢复状态，核对结果前请勿刷新或重复恢复。</p>
      <p v-if="error && phase === 'restore'" class="small dim">{{ uncertain ? '连接中断或回执超时不代表恢复失败。检查结果只读取实时状态，不会重新启动 CLI。' : '恢复未成功，请检查设备连接后重试；原生对话历史保留。' }}</p>
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
      <section v-if="retryConfirmation" class="notice notice--warn" aria-label="确认重新恢复">
        <p>实时列表尚未找到终端，但上次请求仍可能在处理中。确认后会再次请求恢复同一原生对话，可能重新启动 CLI；已保存的历史保留。</p>
        <button class="btn" type="button" @click="retryConfirmation = false">继续等待</button>
        <button class="btn" type="button" :disabled="busy || !conn.isOpen" @click="confirmRetry">确认重新恢复</button>
      </section>
      <div class="recovery-actions">
        <button class="btn" type="button" :disabled="busy" @click="emit('close')">返回会话</button>
        <button v-if="!bound && error && phase === 'list'" class="btn" type="button" :disabled="busy" @click="loadConversations">重试读取历史</button>
        <template v-if="uncertain">
          <button class="btn btn--primary" type="button" :disabled="busy || !conn.isOpen" @click="checkResult">检查恢复结果</button>
          <button v-if="verifiedMissing && !retryConfirmation" class="btn" type="button" :disabled="busy || !conn.isOpen" @click="retryConfirmation = true">核对后重新恢复…</button>
        </template>
        <button v-else-if="!bound" class="btn btn--primary" type="button" :disabled="busy || !selected" @click="restore(selected)">恢复选定对话</button>
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

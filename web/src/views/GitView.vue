<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, type GitResult } from '../lib/api'
import { MessageType, type SessionCreatedPayload } from '../lib/protocol'
import { useConnStore } from '../stores/conn'
import { humanizeError } from '../lib/format'
const props = defineProps<{ id: string }>()
const router = useRouter()
const conn = useConnStore()
const result = ref<GitResult | null>(null)
const error = ref('')
const busy = ref(false)
const file = ref('')
const staged = ref(false)
let deviceId = ''
let cwd = ''
let alive = true
async function load(action: 'status' | 'diff' = 'status'): Promise<void> {
  if (busy.value || !deviceId) return
  busy.value = true; error.value = ''
  try {
    const data = await api.deviceGit(deviceId, cwd, action, file.value, staged.value)
    if (alive) { result.value = data; if (action === 'status') file.value = '' }
  } catch (e) { if (alive) error.value = humanizeError(e) }
  finally { if (alive) busy.value = false }
}
function select(path: string): void { file.value = path; staged.value = false; void load('diff') }
onMounted(async () => {
  busy.value = true
  try {
    const env = await conn.request<SessionCreatedPayload>(MessageType.SessionGet, { session_id: props.id }, props.id)
    if (!alive) return
    if (!env.payload?.session) throw new Error('无法读取会话工作目录')
    deviceId = env.payload.session.device_id.replace(/-/g, '')
    cwd = env.payload.session.cwd
  } catch (e) { if (alive) error.value = humanizeError(e) }
  finally { busy.value = false }
  if (alive) await load()
})
onUnmounted(() => { alive = false })
</script>

<template>
  <div class="stack git-view">
    <div class="row row--between"><h1>Git 改动</h1><button class="btn" @click="router.push({ name: 'session', params: { id } })">返回终端</button></div>
    <div v-if="error" class="notice notice--err" role="alert">{{ error }}</div>
    <section class="card stack">
      <div class="row row--between"><strong>{{ result?.branch || '读取仓库…' }}</strong><button class="btn btn--sm" :disabled="busy || !deviceId" @click="load()">{{ busy ? '读取中…' : '刷新改动' }}</button></div>
      <p class="small dim git-path">{{ result?.root || cwd }}</p>
      <p v-if="result && result.files.length === 0" class="dim">工作区干净，没有待提交改动。</p>
      <p v-if="result?.truncated" class="notice notice--warn small">清单或预览过大，已截断。完整内容请在终端查看。</p>
      <div class="git-files">
        <button v-for="item in result?.files" :key="item.path" class="git-file" :class="{ 'git-file--active': file === item.path }" :disabled="busy" @click="select(item.path)"><code>{{ item.status }}</code><span>{{ item.path }}</span></button>
      </div>
    </section>
    <section v-if="file" class="card stack">
      <strong class="git-path">{{ file }}</strong>
      <div class="row"><button class="btn btn--sm" :class="!staged ? 'btn--primary' : ''" :disabled="busy" @click="staged = false; load('diff')">未暂存</button><button class="btn btn--sm" :class="staged ? 'btn--primary' : ''" :disabled="busy" @click="staged = true; load('diff')">已暂存</button></div>
      <pre class="git-diff" aria-label="文件差异">{{ busy ? '正在读取…' : result?.diff || '此侧没有文本改动（可能是二进制文件或改动已变化）' }}</pre>
    </section>
  </div>
</template>

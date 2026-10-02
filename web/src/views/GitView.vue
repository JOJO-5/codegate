<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, type GitResult } from '../lib/api'
import { MessageType, type SessionCreatedPayload } from '../lib/protocol'
import { useConnStore } from '../stores/conn'
import { absoluteTime, humanizeError } from '../lib/format'
const props = defineProps<{ id: string }>()
const router = useRouter()
const conn = useConnStore()
const result = ref<GitResult | null>(null)
const error = ref('')
const busy = ref(false)
const file = ref('')
const staged = ref(false)
const commitMessage = ref('')
const confirmAction = ref<'commit' | 'push' | null>(null)
const notice = ref('')
const delivery = ref<GitResult | null>(null)
// Independently restrict external links even if a remote Agent returns bad data.
const repositoryURL = computed(() => {
  const raw = delivery.value?.repository_url ?? ''
  return /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(raw) ? raw : ''
})
const comparisonURL = computed(() => repositoryURL.value && delivery.value?.branch && delivery.value.branch !== '分离 HEAD' ? `${repositoryURL.value}/compare/${encodeURIComponent(delivery.value.branch)}?expand=1` : '')
const pullRequestsURL = computed(() => repositoryURL.value ? `${repositoryURL.value}/pulls?q=${encodeURIComponent(`is:pr is:open head:${delivery.value?.branch ?? ''}`)}` : '')

let deviceId = ''
let cwd = ''
let alive = true
async function mutate(): Promise<void> {
  if (!confirmAction.value || !result.value?.head || busy.value || !deviceId) return
  const action = confirmAction.value
  busy.value = true; error.value = ''; notice.value = ''
  try {
    const data = await api.deviceGit(deviceId, cwd, action, '', false, {
      message: commitMessage.value, expected_head: result.value.head, confirm: true,
    })
    if (alive) { result.value = data; delivery.value = data; notice.value = data.message ?? '操作完成'; file.value = ''; confirmAction.value = null; if (action === 'commit') commitMessage.value = '' }
  } catch (e) { if (alive) error.value = humanizeError(e) }
  finally { if (alive) busy.value = false }
}
async function load(action: 'status' | 'diff' = 'status'): Promise<void> {
  if (busy.value || !deviceId) return
  busy.value = true; error.value = ''; confirmAction.value = null
  try {
    const data = await api.deviceGit(deviceId, cwd, action, file.value, staged.value)
    if (alive) { result.value = data; if (action === 'status') { file.value = ''; delivery.value = data } }
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
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>
    <section class="card stack">
      <div class="row row--between"><strong>{{ result?.branch || '读取仓库…' }}</strong><button class="btn btn--sm" :disabled="busy || !deviceId" @click="load()">{{ busy ? '读取中…' : '刷新改动' }}</button></div>
      <p class="small dim git-path">{{ result?.root || cwd }}</p>
      <p v-if="result && result.files.length === 0" class="dim">工作区干净，没有待提交改动。</p>
      <p v-if="result?.truncated" class="notice notice--warn small">清单或预览过大，已截断。完整内容请在终端查看。</p>
      <div class="git-files">
        <button v-for="item in result?.files" :key="item.path" class="git-file" :class="{ 'git-file--active': file === item.path }" :disabled="busy" @click="select(item.path)"><code>{{ item.status }}</code><span>{{ item.path }}</span></button>
      </div>
    </section>
    <section v-if="delivery" class="card stack git-delivery">
      <h2>提交与交付状态</h2>
      <p v-if="typeof delivery.sync_known !== 'boolean'" class="dim">此 Agent 未上报交付信息，请升级到 v0.1.13。</p>
      <p v-else-if="delivery.sync_known">上游 {{ delivery.upstream }}：{{ delivery.ahead }} 个提交待推送，{{ delivery.behind }} 个上游提交尚未纳入。</p>
      <p v-else class="dim">{{ delivery.upstream ? '上游引用暂不可比较，请在终端检查或 fetch。' : '当前分支未建立可比较的上游，请在终端配置后刷新。' }}</p>
      <p class="small dim">以上基于本机已有的远端引用，未联网刷新；工作区干净只说明没有文件改动，任务是否完成还需检查输出和测试结果。</p>
      <div v-if="repositoryURL" class="row"><a v-if="comparisonURL" class="btn" :href="comparisonURL" target="_blank" rel="noopener noreferrer">在 GitHub 比较 / 新建 PR</a><a class="btn btn--ghost" :href="pullRequestsURL" target="_blank" rel="noopener noreferrer">查找此分支 PR</a></div>
      <p v-if="repositoryURL" class="small dim">先推送当前分支，再打开 GitHub 页面登录、选择基础分支并提交。此入口不会自动发布 PR。</p>
      <h3>最近提交</h3><p v-if="!delivery.commits?.length" class="small dim">暂无可显示的提交记录。</p>
      <div v-for="commit in delivery.commits" :key="commit.hash" class="git-commit"><div><a v-if="repositoryURL" :href="`${repositoryURL}/commit/${encodeURIComponent(commit.hash)}`" target="_blank" rel="noopener noreferrer" class="mono">{{ commit.hash.slice(0, 8) }}</a><code v-else>{{ commit.hash.slice(0, 8) }}</code> {{ commit.subject }}</div><time class="small dim">{{ absoluteTime(commit.committed_at) }}</time></div>
    </section>
    <section v-if="result" class="card stack">
      <h2>提交与推送</h2>
      <p class="small dim">先停止 CLI 修改文件并刷新改动。提交会暂存并提交此仓库的全部改动；推送仅发送当前分支到已配置上游，使用目标电脑的 Git 身份与凭据。</p>
      <label for="commit-message">提交说明</label><textarea id="commit-message" v-model="commitMessage" rows="2" maxlength="4096" :disabled="busy" placeholder="描述这次修改" />
      <div class="row"><button class="btn btn--primary" :disabled="busy || !commitMessage.trim() || !result.files.length || result.truncated || !result.head" @click="confirmAction = 'commit'">提交全部改动</button><button class="btn" :disabled="busy || !result.head" @click="confirmAction = 'push'">推送当前分支</button></div>
      <div v-if="confirmAction" class="notice notice--warn stack" role="alert">
        <span>{{ confirmAction === 'commit' ? `将提交“${result.branch}”的全部改动（${result.files.length} 个文件）。确认已审查并停止其他修改？` : `将“${result.branch}”推送到它配置的上游。确认发布这些提交？` }}</span>
        <div class="row"><button class="btn btn--primary" :disabled="busy" @click="mutate">{{ busy ? '执行中…' : confirmAction === 'commit' ? '确认提交' : '确认推送' }}</button><button class="btn" :disabled="busy" @click="confirmAction = null">取消</button></div>
      </div>
    </section>
    <section v-if="file" class="card stack">
      <strong class="git-path">{{ file }}</strong>
      <div class="row"><button class="btn btn--sm" :class="!staged ? 'btn--primary' : ''" :disabled="busy" @click="staged = false; load('diff')">未暂存</button><button class="btn btn--sm" :class="staged ? 'btn--primary' : ''" :disabled="busy" @click="staged = true; load('diff')">已暂存</button></div>
      <pre class="git-diff" aria-label="文件差异">{{ busy ? '正在读取…' : result?.diff || '此侧没有文本改动（可能是二进制文件或改动已变化）' }}</pre>
    </section>
  </div>
</template>

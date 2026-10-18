<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api, ApiError, type Workflow, type WorkflowProfile, type WorkflowInput } from '../lib/api'
import { humanizeError, absoluteTime } from '../lib/format'
import { mergeWorkflows, workflowLabels, workflowTerminal } from '../lib/workflows'
import { useDraftsStore } from '../stores/drafts'

const props = defineProps<{ deviceId: string; cwd?: string; label?: string }>()
const drafts = useDraftsStore()
const key = computed(() => `workflow:${props.deviceId}:${props.cwd ?? ''}`)
const goal = ref(''), master = ref(''), freeWorkers = ref(true), maxTurns = ref(12), maxMinutes = ref(30)
const profiles = ref<WorkflowProfile[]>([]), rows = ref<Workflow[]>([])
const loading = ref(false), loaded = ref(false), catalogLoading = ref(false), submitting = ref(false)
const error = ref(''), catalogError = ref(''), submitError = ref(''), notice = ref(''), cancelling = ref('')
const pending = ref<WorkflowInput | null>(null)
const selected = computed(() => profiles.value.find(p => p.id === master.value))
const shown = computed(() => props.cwd ? rows.value.filter(r => r.cwd === props.cwd) : rows.value)
let hydrating = false
let disposed = false, generation = 0, timer: ReturnType<typeof setInterval> | undefined

watch([() => drafts.owner, key], () => {
  hydrating = true
  generation++; profiles.value = []; rows.value = []; loaded.value = false; error.value = ''; notice.value = ''
  goal.value = ''; master.value = ''; freeWorkers.value = true; maxTurns.value = 12; maxMinutes.value = 30; pending.value = null
  try {
    const saved = JSON.parse(drafts.get(key.value) || '{}')
    if (typeof saved.goal === 'string') goal.value = saved.goal
    if (typeof saved.master === 'string') master.value = saved.master
    if (typeof saved.freeWorkers === 'boolean') freeWorkers.value = saved.freeWorkers
    if (Number.isInteger(saved.maxTurns)) maxTurns.value = saved.maxTurns
    if (Number.isInteger(saved.maxMinutes)) maxMinutes.value = saved.maxMinutes
    if (saved.pending?.nonce && saved.pending.cwd === props.cwd) pending.value = saved.pending
  } catch { /* A corrupt local draft must not prevent reading durable records. */ }
  hydrating = false
}, { immediate: true })
watch([goal, master, freeWorkers, maxTurns, maxMinutes, pending], () => {
  if (hydrating) return
  drafts.set(key.value, JSON.stringify({ goal: goal.value, master: master.value, freeWorkers: freeWorkers.value, maxTurns: maxTurns.value, maxMinutes: maxMinutes.value, pending: pending.value }))
}, { flush: 'sync' })

async function load(): Promise<void> {
  if (loading.value || disposed || document.hidden) return
  loading.value = true; const g = generation
  try {
    const result = await api.workflows(props.deviceId)
    if (!disposed && g === generation) { rows.value = mergeWorkflows(rows.value, result.workflows ?? []); loaded.value = true; error.value = '' }
  } catch (e) { if (!disposed && g === generation) error.value = humanizeError(e) }
  finally { loading.value = false }
}
async function loadProfiles(): Promise<void> {
  if (catalogLoading.value || disposed) return
  catalogLoading.value = true; catalogError.value = ''; const g = generation
  try {
    const result = await api.workflowProfiles(props.deviceId)
    if (!disposed && g === generation) profiles.value = result.profiles ?? []
  } catch (e) { if (!disposed && g === generation) catalogError.value = humanizeError(e) }
  finally { catalogLoading.value = false }
}
async function create(): Promise<void> {
  if (submitting.value || !props.cwd || (!pending.value && (!selected.value || !goal.value.trim()))) return
  submitting.value = true; submitError.value = ''; const g = generation
  // Keep the exact request and nonce across ambiguous failures and page refreshes.
  if (!pending.value) pending.value = { nonce: crypto.randomUUID(), goal: goal.value.trim(), cwd: props.cwd, master_id: master.value, free_workers: freeWorkers.value, max_turns: maxTurns.value, max_minutes: maxMinutes.value }
  try {
    const saved = await api.createWorkflow(props.deviceId, pending.value)
    if (disposed || g !== generation) return
    rows.value = mergeWorkflows(rows.value, [saved]); pending.value = null; goal.value = ''
    notice.value = '调度目标已保存，关闭网页后仍可执行。可在设备页查看进度和取消。'
  } catch (e) {
    if (!disposed && g === generation) {
      submitError.value = humanizeError(e)
      if (e instanceof ApiError && [400, 403, 404, 409, 422].includes(e.status)) pending.value = null
    }
  } finally { submitting.value = false }
}
async function cancel(row: Workflow): Promise<void> {
  if (cancelling.value) return
  cancelling.value = row.id; notice.value = ''; const g = generation
  try {
    const saved = await api.cancelWorkflow(props.deviceId, row.id)
    if (!disposed && g === generation) { rows.value = mergeWorkflows(rows.value, [saved]); notice.value = saved.detail ?? '取消请求已保存' }
  } catch (e) { if (!disposed && g === generation) error.value = humanizeError(e) }
  finally { cancelling.value = '' }
}
const stepLabels: Record<string, string> = { delegate: '分配工作', inspect: '检查改动', check: '实际验证', finish: '请求交付', need_help: '请求协助' }
onMounted(() => { void load(); if (props.cwd) void loadProfiles(); timer = setInterval(() => void load(), 5000) })
onBeforeUnmount(() => { disposed = true; generation++; if (timer) clearInterval(timer) })
</script>

<template>
  <section class="card stack workflow-panel" aria-label="主 Agent 自主调度">
    <div class="row row--between"><h2>主 Agent 调度</h2><button class="btn btn--sm" type="button" :disabled="loading" @click="load">{{ loading ? '读取中…' : '刷新进度' }}</button></div>
    <p class="small dim">选择一个主 Agent，交给它目标。它自行选择已授权 CLI 和模型、分配工作、验证和返工。当前在这台电脑串行执行。</p>
    <form v-if="cwd" class="stack" @submit.prevent="create">
      <label class="field">主 Agent · CLI / 模型
        <select v-model="master" :disabled="submitting || !!pending || catalogLoading" required aria-label="主 Agent 模型">
          <option value="">{{ catalogLoading ? '读取本机可用配置…' : '选择一个主 Agent' }}</option>
          <option v-for="profile in profiles" :key="profile.id" :value="profile.id">{{ profile.label }}</option>
        </select>
      </label>
      <p v-if="catalogError" class="notice notice--err" role="alert">{{ catalogError }}</p>
      <p v-else-if="!catalogLoading && !profiles.length" class="notice notice--warn" role="status">未发现可用主 Agent。请在设备页检查 CLI 安装与授权，确认 Agent 已更新，再重新读取配置。</p>
      <button class="btn btn--sm" type="button" :disabled="catalogLoading || submitting" @click="loadProfiles">重新读取 CLI / 模型</button>
      <label class="field">要完成的目标<textarea v-model="goal" rows="4" required maxlength="2000" :disabled="submitting || !!pending" placeholder="描述期望行为、改动范围和验收条件；主 Agent 自己决定如何执行。" aria-label="调度目标" /></label>
      <details><summary>运行上限和工作模型费用</summary><div class="stack workflow-options">
        <label><input v-model="freeWorkers" type="checkbox" :disabled="submitting || !!pending" /> 工作模型仅使用当前确认免费的配置</label>
        <label class="field">最多调度决策次数<input v-model.number="maxTurns" type="number" min="2" max="32" required :disabled="submitting || !!pending" /></label>
        <label class="field">运行上限（分钟）<input v-model.number="maxMinutes" type="number" min="1" max="180" required :disabled="submitting || !!pending" /></label>
      </div></details>
      <p class="small workflow-summary">{{ label || deviceId }} · {{ cwd }}<br />从当前 Git 提交创建独立分支，原目录未提交的改动不会带入。保留产物供你验收。<br />主 Agent：{{ selected?.label || '尚未选择' }}。{{ selected?.free ? '当前模型价格为免费。' : '使用本机登录和额度，费用以所选 CLI / 模型为准。' }} {{ freeWorkers ? '工作模型不转用付费配置。' : '允许主 Agent 选择本机授权的非免费工作配置。' }}</p>
      <p v-if="pending" class="notice notice--warn" role="status">正在核对这次提交是否保存。重试沿用同一标识，避免重复启动；核对完成前保留当前目标。</p>
      <p v-if="submitError" class="notice notice--err" role="alert">{{ submitError }}</p>
      <button class="btn btn--primary" :disabled="submitting || (!pending && (!selected || !goal.trim()))" type="submit">{{ submitting ? '正在保存目标…' : pending ? '核对并重试本次提交' : '交给主 Agent 调度' }}</button>
    </form>
    <p v-else class="small dim">从项目的“新建任务”选择“主 Agent 自主调度”开始。电脑需保持开机；关闭网页不停止调度。</p>
    <p v-if="notice" class="notice small" role="status">{{ notice }}</p>
    <p v-if="error" class="notice notice--err" role="alert">{{ error }}<span v-if="shown.length"> 下方保留上次读取结果。</span></p>
    <p v-else-if="loading && !loaded" class="dim small" role="status">正在读取调度记录…</p>
    <p v-else-if="loaded && !shown.length" class="dim small">暂无调度记录。</p>
    <article v-for="row in shown" :key="row.id" class="workflow-record">
      <div class="row row--between"><strong>{{ workflowLabels[row.state] ?? row.state }}</strong><button v-if="!workflowTerminal(row.state)" class="btn btn--sm" type="button" :disabled="!!cancelling || row.state === 'cancelling'" @click="cancel(row)">{{ row.state === 'cancelling' ? '等待 Agent 确认停止…' : cancelling === row.id ? '正在请求取消…' : '取消调度' }}</button></div>
      <p>{{ row.goal }}</p><p class="small">{{ row.detail }}</p>
      <p class="small dim">主 Agent：{{row.master_label || profiles.find(p=>p.id===row.master_id)?.label || '本机配置 '+row.master_id}} · {{row.free_workers?'工作模型仅用免费配置':'允许已授权的非免费工作模型'}}</p>
      <p class="small dim">{{ row.cwd }} · 更新于 {{ absoluteTime(row.updated_at) }}</p>
      <p v-if="row.state === 'needs_review'" class="notice notice--warn small">自动调度已停止派发。请核对原生对话及工作区；结果不明时不会自动重放。</p>
      <p v-if="row.master_native_id" class="small dim">主 Agent 原生对话：{{ row.master_native_id }}</p>
      <details v-if="row.steps?.length"><summary>调度过程和验证证据（{{ row.steps.length }} 步）</summary>
        <ol><li v-for="step in row.steps" :key="step.id"><strong>{{ stepLabels[step.kind] ?? step.kind }} · {{ step.state === 'completed' ? '执行成功' : step.state === 'failed' ? '失败' : step.state === 'blocked' ? '已阻止' : step.state==='needs_review'?'需要你核对':'执行中' }}</strong><p v-if="step.profile_id" class="small dim">{{ step.profile_label || profiles.find(p => p.id === step.profile_id)?.label || step.profile_id }}</p><p>{{ step.instruction }}</p><pre v-if="step.output">{{ step.output }}</pre><p v-if="step.native_id" class="small dim">原生对话：{{ step.native_id }}</p></li></ol>
      </details>
      <details v-if="row.worktree"><summary>工作区与实际改动</summary><p class="small">{{ row.worktree }}<br />分支：{{ row.branch }} · 起点：{{ row.base_commit }}</p><pre v-if="row.diff">{{ row.diff }}</pre><p v-else class="small dim">暂无可展示的改动，请在工作区核对文件。</p><p v-if="row.diff_truncated" class="notice notice--warn small">改动预览已截断，需要在实际工作区查看完整结果。</p><p class="small dim">代码保留在这个工作区。请核对文件、测试和目标后再决定如何合入。</p></details>
    </article>
  </section>
</template>

<style scoped>
.workflow-panel{margin-top:16px;min-width:0;overflow-wrap:anywhere}.workflow-panel h2{margin:0}.workflow-panel select,.workflow-panel textarea,.workflow-panel input[type=number]{width:100%;min-width:0}.workflow-summary{border-left:3px solid var(--border);padding-left:12px}.workflow-options{padding-top:12px}.workflow-record{border-top:1px solid var(--border);padding-top:16px}.workflow-panel pre{white-space:pre-wrap;overflow-wrap:anywhere;max-height:400px;overflow:auto;padding:10px;background:var(--bg)}.workflow-record li{margin-bottom:14px}.workflow-panel summary{cursor:pointer;min-height:32px}.workflow-record .row{flex-wrap:wrap}.workflow-panel ol{padding-left:24px}
</style>

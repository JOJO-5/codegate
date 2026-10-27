<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { continuationLabels } from '../lib/continuationLabels'
import { RouterLink } from 'vue-router'
import { api, type GitResult, type ContinuationTask } from '../lib/api'
import { useSessionsStore } from '../stores/sessions'
import { absoluteTime, humanizeError, statusLabel } from '../lib/format'
const props = defineProps<{ deviceId: string; cwd: string; online: boolean; projectKey?: string }>()
const sessions = useSessionsStore()
const records = computed(() => sessions.forDevice(props.deviceId).filter(s => s.cwd === props.cwd && !s.archived))
const latest = computed(() => [...records.value].sort((a,b) => b.created_at - a.created_at)[0])
const git = ref<GitResult | null>(null), tasks = ref<ContinuationTask[]>([])
const busy = ref(false), error = ref(''), checked = ref(0)
let alive = true
onBeforeUnmount(() => { alive = false })
async function refresh(): Promise<void> {
  if (busy.value || !props.online) return
  busy.value = true; error.value = ''
  const result = await Promise.allSettled([api.deviceGit(props.deviceId, props.cwd, 'status'), api.continuations(props.deviceId), sessions.load(props.deviceId, true)])
  if (!alive) return
  const [g,t] = result
  if (g.status === 'fulfilled') git.value = g.value
  if (t.status === 'fulfilled') tasks.value = t.value.tasks.filter(task => task.source.cwd === props.cwd)
  error.value = [g,t].flatMap(r => r.status === 'rejected' ? [humanizeError(r.reason)] : []).join('；')
  if (sessions.error) error.value += `${error.value ? '；' : ''}${sessions.error}`
  checked.value = Date.now(); busy.value = false
}
</script>
<template>
  <details class="project-summary" @toggle="($event.target as HTMLDetailsElement).open && !checked && refresh()">
    <summary>工作摘要与人工核对</summary>
    <div class="stack" style="margin-top:12px">
      <p class="small dim">进程退出和后台执行结束仅代表本次执行停止，任务结果需人工核对。</p>
      <p class="small">会话：{{ records.length }} 条 · 最近进程 {{ latest ? statusLabel(latest.status) : '暂无记录' }}{{ sessions.sources[deviceId] === 'cache' ? '（缓存快照）' : '' }}</p>
      <p v-if="!online" class="small dim">设备离线，摘要无法刷新。</p>
      <button class="btn btn--sm" type="button" :disabled="busy || !online" @click="refresh">{{ busy ? '正在读取…' : '刷新工作摘要' }}</button>
      <p v-if="error" class="notice notice--err small" role="alert">{{ error }}。已有结果保留，可能已过期。</p>
      <template v-if="git">
        <p class="small">Git：{{ git.branch }} · {{ git.files.length }} 个改动路径{{ git.truncated ? '（结果截断）' : '' }}</p>
        <p class="small">交付：{{ git.sync_known ? `相对上游领先 ${git.ahead ?? 0}、落后 ${git.behind ?? 0} 个提交` : '上游同步状态未知' }}。提交或推送不代表构建、部署通过。</p>
      </template>
      <p v-for="task in tasks" :key="task.id" class="small">后台续跑：{{ continuationLabels[task.state] ?? '状态待确认' }}{{ task.detail ? ` · ${task.detail}` : '' }}</p>
      <p v-if="checked && !tasks.length && !error" class="small dim">此目录暂无后台续跑任务。</p>
      <p v-if="checked" class="faint small">{{ error ? '最近读取尝试' : '读取时间' }}：{{ absoluteTime(checked) }}</p>
      <div v-if="latest" class="row">
        <RouterLink class="btn btn--sm" :to="{ name: 'session', params: { id: latest.session_id }, query: projectKey ? { project: projectKey } : {} }">核对原对话</RouterLink>
        <RouterLink class="btn btn--sm" :to="{ name: 'session-files', params: { id: latest.session_id }, query: projectKey ? { project: projectKey } : {} }">核对文件</RouterLink>
        <RouterLink class="btn btn--sm" :to="{ name: 'session-git', params: { id: latest.session_id }, query: projectKey ? { project: projectKey } : {} }">核对 Git 与交付</RouterLink>
      </div>
      <p class="small dim">人工核对：需求是否满足、测试结果是否可信、文件改动是否符合预期，以及需要的发布是否完成。</p>
    </div>
  </details>
</template>

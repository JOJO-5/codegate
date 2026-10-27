<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { useDevicesStore } from '../stores/devices'
import { usePreferencesStore } from '../stores/preferences'
import { useWorkCatalogStore } from '../stores/workCatalog'
import { useDraftsStore } from '../stores/drafts'
import { continuationLabels } from '../lib/continuationLabels'
import { workflowLabels, workflowTerminal } from '../lib/workflows'
import { absoluteTime } from '../lib/format'
import { inProject, type WorkItem, type WorkGroup } from '../lib/projectWork'
const route = useRoute(), devices = useDevicesStore(), preferences = usePreferencesStore(), catalog = useWorkCatalogStore(), drafts = useDraftsStore()
const initialDevice = typeof route.query.device === 'string' ? route.query.device : ''
const initialCwd = typeof route.query.cwd === 'string' ? route.query.cwd : ''
const filterKey = `task-filter:${initialDevice}:${initialCwd}`
let saved: { device?: string; cwd?: string; group?: string; query?: string } = {}
try { saved = JSON.parse(drafts.get(filterKey) || '{}') ?? {} } catch { /* A broken view preference must not block durable task records. */ }
const deviceId = ref(typeof saved.device === 'string' ? saved.device : initialDevice)
const cwd = ref(typeof saved.cwd === 'string' ? saved.cwd : initialCwd)
const group = ref<WorkGroup>(['active','review','planned','history'].includes(saved.group || '') ? saved.group as WorkGroup : 'active')
const query = ref(typeof saved.query === 'string' ? saved.query : ''), page = ref(1)
watch([deviceId, cwd, group, query], () => drafts.set(filterKey, JSON.stringify({ device: deviceId.value, cwd: cwd.value, group: group.value, query: query.value })), { flush: 'post' })
const opened = ref(typeof route.query.task === 'string' ? `${route.query.device}:${route.query.kind}:${route.query.task}` : '')
const groups: { id: WorkGroup; label: string }[] = [{ id: 'active', label: '进行中' }, { id: 'review', label: '需要处理' }, { id: 'planned', label: '已预约' }, { id: 'history', label: '历史' }]
const scopedDevices = computed(() => devices.items.filter(d => !deviceId.value || d.id === deviceId.value))
const scoped = computed(() => catalog.items.filter(t => scopedDevices.value.some(d => d.id === t.deviceId) && (!cwd.value || inProject(t.cwd, cwd.value, devices.byId(t.deviceId)?.platform))))
const counts = computed(() => Object.fromEntries(groups.map(g => [g.id, scoped.value.filter(t => t.group === g.id).length])))
const filtered = computed(() => scoped.value.filter(t => t.group === group.value && `${t.title} ${t.cwd} ${t.detail}`.toLocaleLowerCase().includes(query.value.trim().toLocaleLowerCase())).sort((a,b) => b.updatedAt - a.updatedAt))
const shown = computed(() => filtered.value.slice((page.value - 1) * 20, page.value * 20))
const loading = computed(() => scopedDevices.value.some(d => catalog.loading[d.id]))
const incomplete = computed(() => scopedDevices.value.some(d => !!catalog.errors[d.id] || !catalog.snapshots[d.id]?.continuationLoaded || !catalog.snapshots[d.id]?.workflowLoaded))
const missingDevice = computed(() => !!deviceId.value && !devices.byId(deviceId.value))
watch([deviceId, cwd, group, query], () => { page.value = 1 })
watch(() => filtered.value.length, length => { page.value = Math.min(page.value, Math.max(1, Math.ceil(length / 20))) })
watch(deviceId, () => { cwd.value = ''; void refresh() })
async function refresh(): Promise<void> { await devices.load(); await Promise.all(scopedDevices.value.map(d => catalog.load(d.id))) }
function label(t: WorkItem): string { return (t.kind === 'continuation' ? continuationLabels : workflowLabels)[t.state] || '状态待核对' }
function canCancel(t: WorkItem): boolean { return t.kind === 'workflow' ? !workflowTerminal(t.state) : !['completed', 'cancelled', 'failed'].includes(t.state) }
function projectFor(t: WorkItem) { return preferences.projects.find(p => p.device_id === t.deviceId && inProject(p.cwd, t.cwd, devices.byId(t.deviceId)?.platform)) }
let timer: ReturnType<typeof setInterval> | undefined
let disposed = false
onMounted(async () => { await Promise.all([refresh(), preferences.load()]); if (disposed) return; const selected = scoped.value.find(t => t.key === opened.value); if (selected) group.value = selected.group; timer = setInterval(() => { if (!document.hidden) void refresh() }, 15000) })
onBeforeUnmount(() => { disposed = true; clearInterval(timer) })
const stepLabels: Record<string, string> = { delegate: '分配工作', inspect: '检查改动', check: '实际验证', finish: '请求交付', need_help: '请求协助' }
const stepStates: Record<string, string> = { completed: '执行成功', failed: '执行失败', blocked: '已阻止', needs_review: '需要核对', running: '执行中' }
</script>
<template>
  <main class="page page--wide tasks-page">
    <header class="workspace-heading"><div><p class="projects-eyebrow">保持进展，看清阻塞</p><h1>任务</h1><p class="dim">预约、续跑与主 Agent 调度，在这里查看和处理。</p></div><button class="btn" :disabled="loading || devices.loading" @click="refresh">{{ loading ? '读取中…' : '刷新任务' }}</button></header>
    <p v-if="devices.error" class="notice notice--err" role="alert">{{ devices.error }} <button class="btn btn--sm" @click="devices.load(true).then(refresh)">重试读取设备</button></p>
    <div class="task-toolbar"><label class="field">设备<select v-model="deviceId"><option value="">全部设备</option><option v-for="d in devices.items" :key="d.id" :value="d.id">{{ d.name }}{{ d.online ? '' : '（离线）' }}</option></select></label><label class="field">查找任务<input v-model="query" type="search" placeholder="名称、目录或等待原因…" /></label></div>
    <p v-if="cwd" class="notice small">仅显示项目：<span class="mono">{{ cwd }}</span> <button class="btn btn--ghost btn--sm" @click="cwd = ''">查看此设备全部任务</button></p>
    <p v-if="missingDevice" class="notice notice--warn" role="status">所选设备不可用，可能已解绑；请选择其他设备。</p>
    <div class="workspace-sections" role="group" aria-label="任务状态"><button v-for="g in groups" :key="g.id" class="btn btn--ghost" :aria-pressed="group === g.id" @click="group = g.id">{{ g.label }} <span class="projects-count">{{ counts[g.id] }}</span></button></div>
    <p v-if="(loading || devices.loading) && !scoped.length" class="empty" role="status">正在读取任务…</p>
    <p v-if="incomplete && !loading" class="notice notice--warn" role="status">部分记录尚未确认，下方数量仅包含已读取结果。请刷新重试。</p>
    <template v-for="d in scopedDevices" :key="d.id"><p v-if="catalog.errors[d.id]" class="notice notice--err small" role="alert">{{ d.name }} · {{ catalog.errors[d.id] }}。已有记录保留。</p><p v-else-if="!d.online" class="small dim">{{ d.name }} 离线：显示服务端最后记录，执行与停止状态等待设备确认。</p><p v-if="(catalog.snapshots[d.id]?.continuations.length || 0) >= 200 || (catalog.snapshots[d.id]?.workflows.length || 0) >= 200" class="small dim">{{ d.name }} 已达到单次列表上限；这里不是全部历史。</p></template>
    <p v-if="!devices.items.length && !devices.loading && !devices.error" class="empty">连接设备后可在项目内开始工作。<RouterLink :to="{ name: 'devices' }">管理设备</RouterLink></p>
    <p v-else-if="!shown.length && !loading && !devices.loading && !incomplete && !devices.error && !missingDevice" class="empty">{{ query.trim() ? '没有匹配的任务。' : `暂无${groups.find(g => g.id === group)?.label}任务。` }}</p>
    <article v-for="t in shown" :key="t.key" class="task-row">
      <div class="task-row__heading"><div><p class="small dim">{{ devices.byId(t.deviceId)?.name }} · {{ t.kind === 'workflow' ? '主 Agent 调度' : t.continuation?.mode === 'live_quota' ? '原终端自动续跑' : '后台预约与续跑' }}</p><h2>{{ t.title }}</h2></div><span class="badge" :class="t.group === 'review' ? 'badge--warn' : 'badge--idle'">{{ label(t) }}</span></div>
      <p v-if="t.detail" class="task-reason">{{ t.detail }}</p>
      <p class="small dim mono project-path--compact" :title="t.cwd">{{ t.cwd }}</p>
      <p v-if="t.state === 'ready_for_review'" class="notice small">自动执行已交付，仍需你核对改动和验证结果。</p>
      <p v-if="t.state === 'needs_review'" class="notice small">派发已暂停，请核对原因与原对话；不会自动重放。</p>
      <div class="row task-actions"><button class="btn btn--sm" :aria-expanded="opened === t.key" @click="opened = opened === t.key ? '' : t.key">{{ opened === t.key ? '收起过程' : '查看过程与结果' }}</button><RouterLink v-if="projectFor(t)" class="btn btn--ghost btn--sm" :to="{ name: 'project', params: { key: projectFor(t)!.key } }">进入项目</RouterLink><button v-if="canCancel(t)" class="btn btn--sm" :disabled="catalog.cancelling[t.key] || t.state === 'cancelling'" @click="catalog.cancel(t)">{{ t.state === 'cancelling' ? '等待停止确认' : catalog.cancelling[t.key] ? '正在请求取消…' : '取消任务' }}</button></div>
      <p v-if="catalog.notices[t.key]" class="notice small" role="status">{{ catalog.notices[t.key] }}</p>
      <div v-if="opened === t.key" class="task-evidence stack">
        <p class="small dim">更新：{{ absoluteTime(t.updatedAt) }} · {{ devices.byId(t.deviceId)?.online ? '服务端任务记录' : '设备离线，等待同步' }}</p>
        <template v-if="t.continuation"><p>{{ t.continuation.prompt }}</p><p v-if="t.continuation.progress" class="small">{{ t.continuation.progress.activity }} · 最近同步 {{ absoluteTime(t.continuation.progress.observed_at) }}</p><p v-if="t.continuation.progress && t.group !== 'history' && Date.now() - t.continuation.progress.observed_at > 15000" class="notice small">活动更新已延迟，以下为最后确认结果；暂无新输出不代表停止。</p><pre v-if="t.continuation.output || t.continuation.progress?.text">{{ t.continuation.output || t.continuation.progress?.text }}</pre><RouterLink v-if="t.continuation.session_id" :to="{ name: 'session', params: { id: t.continuation.session_id } }">查看原会话</RouterLink><RouterLink :to="{ name: 'device', params: { id: t.deviceId }, hash: '#device-tasks' }">管理续跑历史与撤销删除</RouterLink></template>
        <template v-if="t.workflow"><p class="small">主 Agent：{{ t.workflow.master_label || '本机配置' }} · {{ t.workflow.free_workers ? '工作模型仅用免费配置' : '允许本机授权的非免费工作模型' }}</p><ol v-if="t.workflow.steps?.length"><li v-for="step in t.workflow.steps" :key="step.id"><strong>{{ stepLabels[step.kind] || "原生步骤" }} · {{ stepStates[step.state] || "状态待核对" }}</strong><p>{{ step.instruction }}</p><pre v-if="step.output">{{ step.output }}</pre></li></ol><p v-if="t.workflow.worktree" class="mono small">产物目录：{{ t.workflow.worktree }}<br />分支：{{ t.workflow.branch }}</p><pre v-if="t.workflow.diff">{{ t.workflow.diff }}</pre><p v-if="t.workflow.diff_truncated" class="notice small">改动预览已截断，请在产物目录核对完整结果。</p></template>
        <p class="small dim">执行状态、检查证据与人工验收分别核对；取消保留已经生成的代码。</p>
      </div>
    </article>
    <nav v-if="filtered.length > 20" class="row task-pagination" aria-label="任务翻页"><button class="btn" :disabled="page === 1" @click="page--">上一页</button><span>{{ page }} / {{ Math.ceil(filtered.length / 20) }}</span><button class="btn" :disabled="page * 20 >= filtered.length" @click="page++">下一页</button></nav>
  </main>
</template>

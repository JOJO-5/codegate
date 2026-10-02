<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { api, type WorktreeInfo } from '../lib/api'
import { humanizeError } from '../lib/format'
import { useDevicesStore } from '../stores/devices'
import { useConnStore } from '../stores/conn'
import TaskLaunch from '../components/TaskLaunch.vue'
const props = defineProps<{ id: string }>()
const route = useRoute()
const devices = useDevicesStore()
const conn = useConnStore()
const path = ref(typeof route.query.path === 'string' ? route.query.path : '')
const primary = ref('')
const items = ref<WorktreeInfo[]>([])
const busy = ref(false)
const loaded = ref(false)
const truncated = ref(false)
const error = ref('')
const notice = ref('')
const confirm = ref<WorktreeInfo | null>(null)
const launch = ref<WorktreeInfo | null>(null)
watch(path, () => { loaded.value = false; items.value = []; confirm.value = null; launch.value = null; primary.value = '' })
const unavailable = computed(() => !devices.byId(props.id)?.online ? '设备离线，上线后可读取和管理工作区。' : !conn.isOpen ? '连接恢复中，请稍候。' : '')
async function load(): Promise<void> {
  if (busy.value || unavailable.value || !path.value.trim()) return
  busy.value = true; error.value = ''; confirm.value = null
  try {
    const data = await api.deviceGit(props.id, path.value.trim(), 'worktrees')
    primary.value = data.root; items.value = data.worktrees ?? []; truncated.value = data.truncated; loaded.value = true
  } catch (e) { error.value = humanizeError(e); loaded.value = false; items.value = [] }
  finally { busy.value = false }
}
async function remove(): Promise<void> {
  const item = confirm.value
  if (!item || busy.value || unavailable.value) return
  busy.value = true; error.value = ''; notice.value = ''
  try {
    const result = await api.deviceGit(props.id, primary.value, 'worktree_remove', '', false, { target: item.path, expected_head: item.head, confirm: true })
    notice.value = result.message ?? '工作区已清理'; confirm.value = null; launch.value = null
    items.value = items.value.filter(w => w.path !== item.path)
  } catch (e) { error.value = humanizeError(e); confirm.value = null }
  finally { busy.value = false }
}
onMounted(async () => { await devices.load(); await load() })
</script>
<template>
  <main class="page page--wide stack">
    <div class="row row--between"><h1>独立工作区</h1><RouterLink class="btn" to="/projects">返回项目</RouterLink></div>
    <p class="dim">{{ devices.byId(id)?.name }} · 关闭会话后目录仍保留，可继续使用或安全清理。</p>
    <form class="card stack" @submit.prevent="load"><label for="workspace-repository">所属仓库或工作区目录</label><input id="workspace-repository" v-model="path" :disabled="busy" autocapitalize="none" spellcheck="false" /><button class="btn" :disabled="busy || !!unavailable || !path.trim()">{{ busy ? '读取中…' : '刷新工作区' }}</button></form>
    <p v-if="unavailable" class="notice notice--warn" role="status">{{ unavailable }}</p>
    <p v-if="error" class="notice notice--err" role="alert">{{ error }} <button class="btn btn--sm" :disabled="busy || !!unavailable" @click="load">重试读取</button></p>
    <p v-if="notice" class="notice" role="status">{{ notice }}</p>
    <p v-if="primary" class="small dim project-path">主工作区：{{ primary }}</p>
    <p class="small dim">仅管理 CodeGate 创建的工作区。先停止其他编辑器和后台服务。清理要求无存活会话、无本地文件改动，并且提交已合入主工作区；分支始终保留。</p>
    <p v-if="loaded && !items.length" class="empty">此仓库没有可管理的独立工作区。</p>
    <p v-if="truncated" class="notice notice--warn">工作区列表较长，已截断。其余工作区请在终端管理。</p>
    <TaskLaunch v-if="launch" :key="launch.path" :device-id="id" :cwd="launch.path" :label="launch.branch" command-id="" @close="launch = null" />
    <div class="project-grid">
      <section v-for="item in items" :key="item.path" class="card stack managed-workspace">
        <h2 class="project-path">{{ item.branch }}</h2><p class="mono project-path">{{ item.path }}</p>
        <p class="small">{{ item.occupied ? '会话使用中' : '无存活会话' }} · {{ item.changed ? '有本地文件' : '没有本地文件改动' }} · {{ item.unique_commits }} 个提交未合入主工作区</p>
        <p v-if="item.blocked_reason" class="notice notice--warn small">暂不能清理：{{ item.blocked_reason }}</p>
        <div class="row"><button class="btn btn--primary" :disabled="busy || !!unavailable" @click="launch = item">在此目录新建任务</button><button class="btn" :disabled="busy || !!unavailable || !!item.blocked_reason" @click="confirm = item">清理目录</button></div>
        <div v-if="confirm?.path === item.path" class="notice notice--warn stack" role="alert"><p>将移除这个工作区目录。Git 分支保留；这项删除不能撤销。确认已查看最新状态？</p><div class="row"><button class="btn btn--danger" :disabled="busy" @click="remove">确认清理目录</button><button class="btn" :disabled="busy" @click="confirm = null">取消清理</button></div></div>
      </section>
    </div>
  </main>
</template>

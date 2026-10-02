<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useRepositoriesStore } from '../stores/repositories'
import { useDevicesStore } from '../stores/devices'
import { useSessionsStore } from '../stores/sessions'
import { useConnStore } from '../stores/conn'
import { TASK_TEMPLATES } from '../lib/tasks'
import { estimateTerminalSize } from '../lib/viewport'
import { humanizeError } from '../lib/format'
const props = defineProps<{ deviceId: string; cwd: string; label: string; commandId: string }>()
const emit = defineEmits<{ close: [] }>()
const repositories = useRepositoriesStore()
const devices = useDevicesStore()
const sessions = useSessionsStore()
const conn = useConnStore()
const router = useRouter()
const command = ref(props.commandId)
const cwd = ref(props.cwd)
const name = ref(props.label)
const isolated = ref(false)
const task = ref('')
const busy = ref(false)
const error = ref('')
const tools = computed(() => repositories.commands[props.deviceId] ?? [])
const selected = computed(() => tools.value.find(t => t.id === command.value))
const reason = computed(() => !devices.byId(props.deviceId)?.online ? '设备离线，上线后才能新建任务。' : !conn.isOpen ? '连接正在恢复，请稍候。' : !selected.value ? '请选择已安装并授权的工具；可在设备页管理授权。' : !cwd.value.trim() ? '请选择工作目录。' : '')
onMounted(async () => {
  await repositories.load(props.deviceId)
  if (!selected.value) command.value = tools.value[0]?.id ?? ''
})
async function launch(): Promise<void> {
  if (reason.value || busy.value) return
  busy.value = true; error.value = ''
  try {
    const size = estimateTerminalSize()
    const session = await sessions.create(props.deviceId, { commandId: command.value, cwd: cwd.value.trim(), name: name.value.trim(), worktree: isolated.value, ...size })
    const template = TASK_TEMPLATES.find(t => t.id === task.value)
    if (template && selected.value?.kind === 'tui') sessions.setDraft(session.session_id, template.prompt)
    await router.push({ name: 'session', params: { id: session.session_id } })
  } catch (e) { error.value = humanizeError(e) }
  finally { busy.value = false }
}
</script>
<template>
  <section class="card stack task-launch" aria-labelledby="launch-title">
    <div class="row row--between"><h2 id="launch-title">新建任务 · {{ label }}</h2><button class="btn btn--ghost" :disabled="busy" @click="emit('close')">取消新建</button></div>
    <form class="stack" @submit.prevent="launch">
      <div class="field"><label for="launch-cli">编程工具</label><select id="launch-cli" v-model="command" :disabled="busy"><option value="">选择已授权工具</option><option v-for="tool in tools" :key="tool.id" :value="tool.id">{{ tool.label }}</option></select></div>
      <div class="field"><label for="launch-cwd">任务工作目录</label><input id="launch-cwd" v-model="cwd" :disabled="busy" autocapitalize="none" spellcheck="false" /></div>
      <div class="field"><label for="launch-name">任务名称</label><input id="launch-name" v-model="name" :disabled="busy" maxlength="160" /></div>
      <div class="field"><label for="launch-mode">工作方式</label><select id="launch-mode" v-model="isolated" :disabled="busy"><option :value="false">使用原目录</option><option :value="true">创建独立 Git 工作区</option></select><span class="field__hint">{{ isolated ? '从当前提交创建新分支；原目录的未提交改动不会带入。启动后显示实际分支和目录。' : 'CLI 将直接修改所选目录中的文件。' }}</span></div>
      <div v-if="selected?.kind === 'tui'" class="field"><label for="launch-template">任务模板</label><select id="launch-template" v-model="task" :disabled="busy"><option value="">自由对话</option><option v-for="item in TASK_TEMPLATES" :key="item.id" :value="item.id">{{ item.label }}</option></select><span class="field__hint">只预填终端输入框，由你编辑并发送。</span></div>
      <p class="small dim launch-summary">{{ devices.byId(deviceId)?.name }} · {{ selected?.label || '未选择工具' }} · {{ isolated ? '新建独立分支' : '原目录' }}<br />{{ cwd }}</p>
      <p v-if="reason" class="notice notice--warn" role="status">{{ reason }}</p>
      <p v-if="error" class="notice notice--err" role="alert">{{ error }}</p>
      <button class="btn btn--primary" :disabled="busy || !!reason">{{ busy ? '正在启动任务…' : '启动任务' }}</button>
    </form>
  </section>
</template>

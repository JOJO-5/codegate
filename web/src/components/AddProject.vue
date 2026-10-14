<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import ModalDialog from './ModalDialog.vue'
import { useDevicesStore } from '../stores/devices'
import { useRepositoriesStore } from '../stores/repositories'
import { usePreferencesStore } from '../stores/preferences'
import { humanizeError } from '../lib/format'
const emit = defineEmits<{ close: [] }>()
const devices = useDevicesStore(), repositories = useRepositoriesStore(), preferences = usePreferencesStore()
const device = ref(devices.items.find(d => d.online)?.id ?? '')
const command = ref(''), cwd = ref(''), label = ref(''), error = ref(''), busy = ref(false), loading = ref(false)
const tools = computed(() => repositories.commands[device.value] ?? [])
let generation = 0
watch(device, async id => {
  const g = ++generation
  command.value = ''; cwd.value = ''; loading.value = true
  try { if (id) await repositories.load(id) }
  finally { if (g === generation) { command.value = tools.value[0]?.id ?? ''; loading.value = false } }
}, { immediate: true })
async function save(): Promise<void> {
  if (busy.value || loading.value || !tools.value.some(t => t.id === command.value) || !cwd.value.trim() || !label.value.trim()) return
  busy.value = true; error.value = ''
  try {
    await preferences.saveProject({ device_id: device.value, command_id: command.value, cwd: cwd.value.trim(), label: label.value.trim(), name: '' })
    emit('close')
  } catch (e) { error.value = humanizeError(e) }
  finally { busy.value = false }
}
</script>
<template>
  <ModalDialog title-id="add-project-title" :busy="busy" @close="emit('close')">
    <h2 id="add-project-title">添加常用项目</h2>
    <p class="small dim">保存已有目录的入口，之后可从项目页启动任务。需要创建目录或初始文件时，使用设备页的“新建项目 / 文件”。</p>
    <form class="stack" @submit.prevent="save">
      <label class="field">设备<select v-model="device" :disabled="busy"><option value="">选择设备</option><option v-for="d in devices.items" :key="d.id" :value="d.id" :disabled="!d.online">{{ d.name }}{{ d.online ? '' : '（离线）' }}</option></select></label>
      <label class="field">编程工具<select v-model="command" :disabled="busy || loading"><option value="">{{ loading ? '正在读取工具…' : '选择已授权工具' }}</option><option v-for="t in tools" :key="t.id" :value="t.id">{{ t.label }}</option></select></label>
      <label class="field">项目名称<input v-model="label" type="text" required maxlength="160" :disabled="busy" /></label>
      <label class="field">已有目录<input v-model="cwd" type="text" required :disabled="busy" autocapitalize="none" spellcheck="false" /></label>
      <p v-if="!tools.length && !loading" class="small dim">请先连接设备，并在设备页授权已安装工具。</p>
      <p v-if="repositories.errors[device]" class="notice notice--err small" role="alert">{{ repositories.errors[device] }}</p>
      <p v-if="error" role="alert" class="notice notice--err">{{ error }}</p>
      <div class="row"><button class="btn" type="button" :disabled="busy" @click="emit('close')">取消</button><button class="btn btn--primary" :disabled="busy || loading || !command || !label.trim() || !cwd.trim()">保存项目入口</button></div>
    </form>
  </ModalDialog>
</template>

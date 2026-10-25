<script setup lang="ts">
import { computed, onBeforeUnmount, watch } from 'vue'
import ModalDialog from './ModalDialog.vue'
import { api, type DeviceUpdateStatus } from '../lib/api'
import { humanizeError } from '../lib/format'
import { createImmediateRestart, type RestartIntent } from '../lib/agent-restart'

const props = defineProps<{ deviceId: string; ownerId: string; deviceName: string; status: DeviceUpdateStatus | null }>()
const emit = defineEmits<{ refresh: [] }>()
const id = props.deviceId
const key = `codegate.agent-restart.${props.ownerId}.${id}`
const controller = createImmediateRestart({
  current: () => props.status,
  submit: async instance => { await api.controlAgentUpdate(id, 'restart_now', { confirmed: true, instance_started_at: instance }); emit('refresh') },
  read: () => { try { const value = JSON.parse(sessionStorage.getItem(key) ?? 'null'); return typeof value?.instance === 'number' && value.instance > 0 ? value as RestartIntent : null } catch { return null } },
  write: value => { try { if (value) sessionStorage.setItem(key, JSON.stringify(value)); else sessionStorage.removeItem(key) } catch { /* Current tab still retains observable state. */ } },
  describe: humanizeError,
})
const { dialog, busy, message, phase } = controller
const unavailable = computed(() => !props.status?.online || !props.status.update?.can_restart_now || ['draining', 'switching'].includes(props.status.update.state) || busy.value || phase.value === 'pending')
watch(() => props.status, value => controller.observe(value), { immediate: true })
onBeforeUnmount(controller.dispose)
</script>
<template>
  <div class="immediate-restart">
    <button class="btn btn--danger btn--sm" type="button" :disabled="unavailable" @click="controller.open">立即关闭任务并重启升级</button>
    <p v-if="!status?.update?.can_restart_now" class="small dim">立即重启需要支持此功能的新版 Agent；请先完成一次空闲升级。</p>
    <p v-if="message" class="notice small" :class="phase === 'error' ? 'notice--err' : ''" role="status">{{ message }}</p>
    <button v-if="phase === 'pending' || phase === 'unknown'" class="btn btn--sm" type="button" @click="emit('refresh')">核对重启状态</button>
    <ModalDialog v-if="dialog" title-id="immediate-agent-restart-title" :busy="busy" @close="controller.close">
      <h2 id="immediate-agent-restart-title">确认关闭任务并立即重启？</h2>
      <p>目标设备：<strong>{{ deviceName || deviceId }}</strong></p>
      <p>当前 Agent：{{ status?.agent_version || '待确认' }}；目标版本：{{ status?.update?.version || '检查更新后确定' }}。没有新版本时将重启当前版本。</p>
      <p>将关闭 {{ status?.update?.blocking_sessions ?? 0 }} 个终端会话（含保留输出）、{{ status?.update?.blocking_operations ?? 0 }} 项后台或 Git 等操作{{ status?.update?.blocking_web ? '，并停止 DSH Web' : '' }}。</p>
      <p class="notice notice--warn">会取消正在执行或监测中的后台任务，关闭本设备由 CodeGate 管理的终端和 Web 服务。尚未开始的未来预约保留。未保存的输入、终端回看输出和进程内存会丢失；已保存的代码、原生对话历史与执行回执保留。</p>
      <p class="small dim">先下载并校验更新包；检查失败时不关闭任务。开始停止后不能撤销；无法确认任务停止时会暂停重启并显示原因。</p>
      <p v-if="message" role="alert">{{ message }}</p>
      <div class="row">
        <button class="btn btn--sm" type="button" autofocus :disabled="busy" @click="controller.close">取消，继续工作</button>
        <button class="btn btn--danger" type="button" :disabled="busy || unavailable" @click="controller.confirm">{{ busy ? '正在请求…' : '确认关闭任务并重启' }}</button>
      </div>
    </ModalDialog>
  </div>
</template>
<style scoped>
.immediate-restart{margin-top:12px;overflow-wrap:anywhere}.immediate-restart p{margin:10px 0}.immediate-restart .row{gap:8px;flex-wrap:wrap}
</style>

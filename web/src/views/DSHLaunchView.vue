<script setup lang="ts">
import {computed,onMounted,onUnmounted,watch} from 'vue'
import {api} from '../lib/api'
import {humanizeError} from '../lib/format'
import {createDSHLauncher} from '../lib/dsh-launch'
import {useDevicesStore} from '../stores/devices'
import {useAuthStore} from '../stores/auth'

const props=defineProps<{id:string}>()
const devices=useDevicesStore(),auth=useAuthStore()
const launcher=createDSHLauncher({status:api.getDSHWebStatus,start:api.startDSHWeb,navigate:url=>window.location.replace(url),describe:humanizeError})
const {busy,error,observed,phase,compatibility}=launcher
const target=computed(()=>devices.byId(props.id)?.name || props.id)
const phaseText=computed(()=>({checking:'正在核对设备和转发状态…',starting:'电脑正在启动 DSH Web，等待真实回执…',connecting:'正在请求打开或复用 DSH Web，等待电脑回执…',opening:'已取得打开链接，正在前往 DSH Web…',idle:'可核对状态后重新连接。',error:'打开未完成，请按原因处理后重新连接。'}[phase.value]))
const statusText=computed(()=>({offline:'设备离线',not_open:'尚无已确认的转发',starting:'启动中',stopping:'停止中',forwarding:'已建立转发，网页加载仍需确认',unknown:'上次操作结果未确认'}[observed.value || 'unknown']))
watch(()=>props.id,id=>{void launcher.open(id)},{immediate:true})
onMounted(()=>{void devices.load()})
watch(()=>auth.user?.id,(user,previous)=>{if(user!==previous){launcher.invalidate();error.value='账号已变化，请返回设备页重新核对访问权限。'}})
onUnmounted(launcher.invalidate)
</script>

<template>
  <section class="card dsh-launch" aria-labelledby="dsh-launch-title" :aria-busy="busy">
    <p class="dim small">CodeGate · DSH Web</p>
    <h1 id="dsh-launch-title">打开 DSH Web</h1>
    <p class="dsh-launch__target">目标设备：<strong>{{ target }}</strong></p>
    <p role="status" aria-live="polite">{{ phaseText }}</p>
    <p v-if="observed" class="dim small">最近核对：{{ statusText }}</p>
    <p v-if="compatibility.startsWith('unsupported')" class="notice small">当前 DSH 页面未通过手机适配兼容检查，将保留原版布局。</p>
    <p v-if="error" class="notice notice--err" role="alert">{{ error }}</p>
    <p class="dim small">重新连接优先复用现有 DSH。离开本页只取消等待，不会停止已启动的服务或运行任务；回来后可核对状态。</p>
    <div class="row dsh-launch__actions">
      <button v-if="!busy" class="btn btn--primary" type="button" @click="launcher.open(id)">重新连接</button>
      <button v-if="!busy" class="btn" type="button" @click="launcher.inspect(id)">核对状态</button>
      <RouterLink class="btn btn--ghost" :to="{name:'device',params:{id},query:{dsh:'reconnect'},hash:'#dsh-web-heading'}">{{ busy ? '取消等待并返回设备' : '返回设备' }}</RouterLink>
    </div>
  </section>
</template>

<style scoped>
.dsh-launch{max-width:40rem;margin:2rem auto;overflow-wrap:anywhere}
.dsh-launch h1{font-size:1.35rem}
.dsh-launch__target{margin:1rem 0}
.dsh-launch__actions{flex-wrap:wrap;gap:.65rem;margin-top:1rem}
.dsh-launch__actions .btn{min-height:44px}
@media(max-width:480px){.dsh-launch{margin:1rem 0}.dsh-launch__actions{align-items:stretch;flex-direction:column}}
</style>

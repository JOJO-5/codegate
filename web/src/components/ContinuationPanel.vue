<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, watch } from 'vue'
import { api, type ContinuationTask } from '../lib/api'
import { useConnStore } from '../stores/conn'
import { MessageType, type SessionSummary } from '../lib/protocol'
import { humanizeError, absoluteTime } from '../lib/format'
const props = defineProps<{ deviceId: string; source?: SessionSummary | null }>()
const emit = defineEmits<{ close: [] }>()
const conn = useConnStore()
const rows = ref<ContinuationTask[]>([]), error = ref(''), busy = ref(false)
const prompt = ref('请继续之前尚未完成的任务，先检查当前进度，避免重复已完成的操作。')
const mode = ref('time'), selected = ref(''), minutes = ref(120)
const choices = ref<{ id: string; title: string }[]>([])
function localDate(ms: number): string { const d=new Date(ms); return new Date(ms-d.getTimezoneOffset()*60000).toISOString().slice(0,16) }
const due = ref(localDate(Date.now()+5*3600000))
let disposed=false, loading=false, generation=0, nonce=crypto.randomUUID()
async function load(): Promise<void> {
 if (loading || disposed || document.hidden) return
 loading=true;const g=generation
 try { const result=await api.continuations(props.deviceId);if (!disposed && g===generation) rows.value=result.tasks }
 catch(e) {if(!disposed && g===generation) error.value=humanizeError(e)} finally {loading=false}
}
watch(()=>props.source, async source=>{
 const g=++generation;nonce=crypto.randomUUID();error.value='';choices.value=[];selected.value=source?.recovery?.native_id??''
 if(source && !selected.value) {busy.value=true;try {const env=await conn.request<{conversations:{id:string;title:string}[]}>(MessageType.ConversationList,{session_id:source.session_id},source.session_id);if(!disposed && g===generation) choices.value=env.payload?.conversations??[]} catch(e){if(!disposed && g===generation) error.value=humanizeError(e)} finally{if(g===generation) busy.value=false}}
},{immediate:true})
async function create():Promise<void>{
 if(!props.source)return;busy.value=true;error.value=''
 try {const saved=await api.createContinuation(props.deviceId,{session_id:props.source.session_id,native_id:selected.value,prompt:prompt.value,mode:mode.value,due_at:new Date(due.value).getTime(),max_run_minutes:minutes.value,nonce});if(disposed)return;rows.value=[saved,...rows.value.filter(t=>t.id!==saved.id)];emit('close');await load()}
 catch(e){if(!disposed)error.value=humanizeError(e)} finally{busy.value=false}
}
async function cancel(t:ContinuationTask):Promise<void>{busy.value=true;try{await api.cancelContinuation(props.deviceId,t.id);await load()}catch(e){error.value=humanizeError(e)}finally{busy.value=false}}
const labels:Record<string,string>={queued:'等待时间',waiting_agent:'等待设备',waiting_idle:'等待原终端结束',waiting_quota:'等待额度',dispatching:'正在派发',running:'后台执行中',cancelling:'正在停止',completed:'本次执行结束',cancelled:'已取消',failed:'执行失败',needs_review:'需要确认'}
function terminal(state:string):boolean{return ['completed','cancelled','failed','needs_review'].includes(state)}
let timer:ReturnType<typeof setInterval>|undefined
onMounted(()=>{void load();timer=setInterval(()=>void load(),15000)})
onBeforeUnmount(()=>{disposed=true;generation++;if(timer)clearInterval(timer)})
</script>
<template>
 <div v-if="source" class="continuation-backdrop">
  <form class="card continuation-dialog" role="dialog" aria-modal="true" aria-labelledby="continue-title" @submit.prevent="create">
   <h2 id="continue-title">安排原对话继续</h2>
   <p class="dim small">{{source.name}} · {{source.cwd}}</p>
   <p class="small">到时在该电脑后台恢复原生对话，关闭网页也能等待。请先结束原终端进程；电脑需保持开机、Agent 在线。</p>
   <label v-if="!source.recovery?.native_id">原生对话<select v-model="selected" required aria-label="原生对话"><option value="">核对历史后选择</option><option v-for="item in choices" :key="item.id" :value="item.id">{{item.title}} · {{item.id}}</option></select></label>
   <label>继续方式<select v-model="mode" aria-label="继续方式"><option value="time">按时间继续</option><option value="quota">到时检查额度，耗尽则等待重置</option></select></label>
   <p v-if="mode==='quota'" class="dim small">仅支持可可靠查询额度的默认 CLI 配置；查询失败不会启动模型，请改用按时间继续。</p>
   <label>最早继续时间<input v-model="due" type="datetime-local" required aria-label="最早继续时间"></label>
   <label>继续执行的任务<textarea v-model="prompt" rows="4" required aria-label="继续执行的任务"></textarea></label>
   <label>本次运行上限（分钟）<input v-model.number="minutes" type="number" min="1" max="360" required aria-label="本次运行上限"></label>
   <p v-if="error" class="error" role="alert">{{error}}</p>
   <div class="row"><button class="btn" type="button" :disabled="busy" @click="emit('close')">返回</button><button class="btn btn--primary" :disabled="busy || !selected" type="submit">保存后台任务</button></div>
  </form>
 </div>
 <section v-if="rows.length" class="card continuation-list">
  <div class="row row--between"><h2>后台续跑</h2><button class="btn btn--sm" type="button" :disabled="busy" @click="load">刷新</button></div>
  <p class="dim small">列表每15秒刷新。执行结束不代表任务已全部完成，请核对原对话和文件改动。</p>
  <article v-for="task in rows" :key="task.id">
   <strong>{{task.source.name || '原对话'}} · {{labels[task.state]??task.state}}</strong>
   <p class="dim small">{{absoluteTime(task.due_at)}} · {{task.source.cwd}}</p><p class="small">{{task.detail}}</p>
   <details><summary>任务与执行结果</summary><p>{{task.prompt}}</p><pre v-if="task.output">{{task.output}}</pre><p v-else class="dim">暂无最终回复；可恢复原对话查看历史。</p></details>
   <button v-if="!terminal(task.state)" class="btn btn--sm" type="button" :disabled="busy" @click="cancel(task)">取消续跑</button>
  </article>
  <p v-if="error && !source" class="error" role="alert">{{error}}</p>
 </section>
</template>
<style scoped>
.continuation-backdrop{position:fixed;inset:0;z-index:80;background:#0008;display:flex;align-items:center;justify-content:center;padding:12px}.continuation-dialog{width:100%;max-width:580px;max-height:calc(100dvh - 24px);overflow:auto;overflow-wrap:anywhere}.continuation-dialog label{display:grid;gap:6px;margin:12px 0}.continuation-dialog input,.continuation-dialog select,.continuation-dialog textarea{width:100%;min-width:0}.continuation-list{margin-top:16px}.continuation-list article{border-top:1px solid var(--border);padding:12px 0;overflow-wrap:anywhere}.continuation-list pre{white-space:pre-wrap;overflow-wrap:anywhere}
</style>

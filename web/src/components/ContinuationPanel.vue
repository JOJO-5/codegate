<script setup lang="ts">
import ModalDialog from './ModalDialog.vue'
import { continuationLabels as labels } from '../lib/continuationLabels'

import { computed, ref, onMounted, onBeforeUnmount, watch, nextTick } from 'vue'
import { api, type ContinuationTask } from '../lib/api'
import { useConnStore } from '../stores/conn'
import { MessageType, type SessionSummary } from '../lib/protocol'
import { nativeCLI } from '../lib/sessionQuota'
import { humanizeError, absoluteTime } from '../lib/format'
const props = defineProps<{ deviceId: string; source?: SessionSummary | null; compact?: boolean; sessionId?: string }>()
const emit = defineEmits<{ close: [] }>()
const conn = useConnStore()
const rows = ref<ContinuationTask[]>([]), error = ref(''), busy = ref(false)
const listError = ref(''), loading = ref(false), loaded = ref(false)
const prompt = ref('请继续之前尚未完成的任务，先检查当前进度，避免重复已完成的操作。')
const canLive = computed(() => !!props.source?.recovery?.native_id && ['codex','claude','opencode','dsh'].includes(nativeCLI(props.source.command, props.source.args)) && ['running','starting','detached'].includes(props.source.status))
const notice = ref('')
const filter = ref('active')
const removed = ref<ContinuationTask[]>([])
const undoButton=ref<HTMLButtonElement|null>(null),historyButton=ref<HTMLButtonElement|null>(null)
const now = ref(Date.now())
function history(t:ContinuationTask):boolean {return ['completed','cancelled','failed'].includes(t.state)}
function group(t:ContinuationTask):string {return t.state==='needs_review'?'review':history(t)?'history':'active'}
const counts = computed(()=>({active:rows.value.filter(t=>group(t)==='active').length,review:rows.value.filter(t=>group(t)==='review').length,history:rows.value.filter(history).length}))
const visibleRows = computed(() => {
 const shown = rows.value.filter(t => !t.deleted_at)
 return props.compact ? shown.filter(t => t.mode === 'live_quota' && t.session_id === props.sessionId).slice(0,1) : shown.filter(t=>group(t)===filter.value)
})
async function remove(t:ContinuationTask):Promise<void> {
 if(busy.value || !history(t))return
 busy.value=true;generation++;listError.value=''
 try {await api.deleteContinuation(props.deviceId,t.id);if(disposed)return;rows.value=rows.value.filter(row=>row.id!==t.id);removed.value=[t];notice.value='记录已删除，原生对话和项目文件保留。'}
 catch(e){if(!disposed)listError.value=humanizeError(e)}finally{busy.value=false;await nextTick();if(!disposed)undoButton.value?.focus()}
}
async function undoRemove():Promise<void> {
 if(busy.value || !removed.value.length)return
 busy.value=true;generation++;listError.value=''
 try {for(const t of [...removed.value]) {const saved=await api.restoreContinuation(props.deviceId,t.id);if(disposed)return;rows.value=[saved,...rows.value.filter(row=>row.id!==saved.id)];removed.value=removed.value.filter(row=>row.id!==saved.id)}notice.value='已恢复记录。';await nextTick();historyButton.value?.focus()}
 catch(e){if(!disposed)listError.value=humanizeError(e)}finally{busy.value=false}
}
async function clearHistory():Promise<void> {
 if(busy.value)return
 const targets=rows.value.filter(history);if(!targets.length)return
 busy.value=true;generation++;listError.value='';removed.value=[]
 try {for(const t of targets) {await api.deleteContinuation(props.deviceId,t.id);if(disposed)return;rows.value=rows.value.filter(row=>row.id!==t.id);removed.value.push(t)}}
 catch(e){if(!disposed)listError.value=humanizeError(e)}finally{busy.value=false;notice.value=`已删除 ${removed.value.length} 条历史记录，可撤销；项目文件保留。`;await nextTick();undoButton.value?.focus()}
}
function elapsed(start:number):string {const seconds=Math.max(0,Math.floor((now.value-start)/1000));return `${Math.floor(seconds/60)} 分 ${seconds%60} 秒`}
const mode = ref('time'), selected = ref(''), minutes = ref(120)
const maxRetries = ref(3), retryErrors = ref(true), quotaWaitMinutes = ref(300)
const choices = ref<{ id: string; title: string }[]>([])
function localDate(ms: number): string { const d=new Date(ms); return new Date(ms-d.getTimezoneOffset()*60000).toISOString().slice(0,16) }
const due = ref(localDate(Date.now()+5*3600000))
let disposed=false, generation=0, nonce=crypto.randomUUID(), lastPollAt=0
async function load(): Promise<void> {
 if (loading.value || disposed || document.hidden) return
 loading.value=true;lastPollAt=Date.now();const g=generation
 try { const result=await api.continuations(props.deviceId);if (!disposed && g===generation) { rows.value=result.tasks; loaded.value=true; listError.value='' } }
 catch(e) {if(!disposed && g===generation) listError.value=humanizeError(e)} finally {loading.value=false}
}
watch(()=>props.source, async source=>{
 mode.value = canLive.value ? 'live_quota' : 'time';due.value = localDate(Date.now() + (canLive.value ? 0 : 5*3600000))
 const g=++generation;nonce=crypto.randomUUID();error.value='';choices.value=[];selected.value=source?.recovery?.native_id??''
 if(source && !selected.value) {busy.value=true;try {const env=await conn.request<{conversations:{id:string;title:string}[]}>(MessageType.ConversationList,{session_id:source.session_id},source.session_id);if(!disposed && g===generation) choices.value=env.payload?.conversations??[]} catch(e){if(!disposed && g===generation) error.value=humanizeError(e)} finally{if(g===generation) busy.value=false}}
},{immediate:true})
async function create():Promise<void>{
 if(!props.source)return;busy.value=true;error.value=''
 try {const saved=await api.createContinuation(props.deviceId,{session_id:props.source.session_id,native_id:selected.value,prompt:prompt.value,mode:mode.value,due_at:mode.value === 'live_quota' ? Date.now() : new Date(due.value).getTime(),max_run_minutes:minutes.value,nonce,max_retries:maxRetries.value,retry_errors:mode.value==='live_quota' && retryErrors.value,quota_wait_minutes:quotaWaitMinutes.value});if(disposed)return;rows.value=[saved,...rows.value.filter(t=>t.id!==saved.id)];emit('close');await load()}
 catch(e){if(!disposed)error.value=humanizeError(e)} finally{busy.value=false}
}
async function cancel(t:ContinuationTask):Promise<void>{
 if (busy.value) return
 busy.value=true;notice.value='';generation++
 try {
  const saved=await api.cancelContinuation(props.deviceId,t.id)
  if(disposed)return
  rows.value=rows.value.map(row=>row.id===saved.id?saved:row)
  listError.value='';notice.value=saved.state==='cancelling'?'已请求停止自动执行，等待 Agent 确认。':'已取消续跑安排，记录已移至历史。';if(!props.compact && history(saved))filter.value='history'
 }catch(e){if(!disposed)listError.value=humanizeError(e)}finally{busy.value=false}
}

function terminal(state:string):boolean{return ['completed','cancelled','failed','needs_review'].includes(state)}
let timer:ReturnType<typeof setInterval>|undefined
onMounted(()=>{void load();timer=setInterval(()=>{now.value=Date.now();if(rows.value.some(t=>['running','dispatching','cancelling'].includes(t.state)) || now.value-lastPollAt>=15000)void load()},3000)})
onBeforeUnmount(()=>{disposed=true;generation++;if(timer)clearInterval(timer)})
</script>
<template>
 <ModalDialog v-if="source" title-id="continue-title" :busy="busy" @close="emit('close')">
  <form class="card continuation-dialog"  @submit.prevent="create">
   <h2 id="continue-title">安排原对话继续</h2>
   <p class="dim small">{{source.name}} · {{source.cwd}}</p>
   <p v-if="mode !== 'live_quota'" class="small">到时在该电脑后台恢复原生对话，关闭网页也能等待。可以提前安排；原终端仍运行时会等待其结束，不会并行执行。电脑需保持开机、Agent 在线。</p>
   <label v-if="!source.recovery?.native_id">原生对话<select v-model="selected" required aria-label="原生对话"><option value="">核对历史后选择</option><option v-for="item in choices" :key="item.id" :value="item.id">{{item.title}} · {{item.id}}</option></select></label>
   <label>继续方式<select v-model="mode" aria-label="继续方式"><option v-if="canLive" value="live_quota">撞额度墙后在原终端自动继续</option><option v-if="source && nativeCLI(source.command,source.args)!=='dsh'" value="time">按时间继续</option><option v-if="source && nativeCLI(source.command,source.args)!=='dsh'" value="quota">到时检查额度，耗尽则等待重置</option></select></label>
   <p v-if="mode==='quota'" class="dim small">仅支持可可靠查询额度的默认 CLI 配置；查询失败不会启动模型，请改用按时间继续。</p>
   <p v-if="mode==='live_quota'" class="small">立即监测当前编程回合；订阅额度耗尽后等待恢复，再在同一原生对话继续。不需关闭终端。Codex 使用本机守护进程，其他CLI使用原生事件桥；旧终端需要恢复原对话启动一次新版终端。手动输入、新回合或原终端结束会关闭自动续跑。按设置的次数限制自动继续，不购买额度或切换账号、模型。</p>
   <label v-if="mode!=='live_quota'">最早继续时间<input v-model="due" type="datetime-local" required aria-label="最早继续时间"></label>
   <label>继续执行的任务<textarea v-model="prompt" rows="4" required aria-label="继续执行的任务"></textarea></label>
   <label v-if="mode==='live_quota' && source && nativeCLI(source.command,source.args)!=='codex'">无查询接口时的额度等待（分钟）<input v-model.number="quotaWaitMinutes" type="number" min="1" max="10080" required aria-label="额度等待分钟"></label>
   <p v-if="mode==='live_quota' && source && nativeCLI(source.command,source.args)!=='codex'" class="dim small">优先查询可用的订阅额度；服务商没有查询接口时，按返回的重试时间或这里设置的时间有限重试，不代表已确认额度恢复。</p>
   <label v-if="mode!=='time'">最大自动重试次数<input v-model.number="maxRetries" type="number" min="1" max="10" required aria-label="最大自动重试次数"></label>
   <label v-if="mode==='live_quota'"><span><input v-model="retryErrors" type="checkbox"> 同时重试临时网络、限流和服务端故障</span></label>
   <label>本次运行上限（分钟）<input v-model.number="minutes" type="number" min="1" max="360" required aria-label="本次运行上限"></label>
   <p v-if="error" class="error" role="alert">{{error}}</p>
   <div class="row"><button class="btn" type="button" :disabled="busy" @click="emit('close')">返回</button><button class="btn btn--primary" :disabled="busy || !selected" type="submit">{{ mode==='live_quota' ? '开启自动续跑' : '保存后台任务' }}</button></div>
  </form>
 </ModalDialog>
 <section v-if="!compact || visibleRows.length || listError || removed.length" class="card continuation-list" :class="{ 'continuation-list--compact': compact }">
  <div v-if="!compact" class="row row--between"><h2>预约与续跑</h2><button class="btn btn--sm" type="button" :disabled="busy || loading" @click="load">{{ loading ? '读取中…' : '刷新' }}</button></div>
  <div v-if="!compact" class="row continuation-filters" role="group" aria-label="任务分类">
   <button class="btn btn--sm" :aria-pressed="filter==='active'" @click="filter='active'">进行中 {{counts.active}}</button>
   <button class="btn btn--sm" :aria-pressed="filter==='review'" @click="filter='review'">需要处理 {{counts.review}}</button>
   <button ref="historyButton" class="btn btn--sm" :aria-pressed="filter==='history'" @click="filter='history'">历史记录 {{counts.history}}</button>
   <button v-if="filter==='history' && counts.history" class="btn btn--sm" :disabled="busy" @click="clearHistory">清理当前列表历史</button>
  </div>
  <p v-if="listError" class="error" role="alert">{{listError}} <button class="btn btn--sm" type="button" :disabled="busy || loading" @click="load">重试读取</button><span v-if="visibleRows.length" class="small"> 下方保留上次读取结果。</span></p>
  <p v-else-if="loading && !visibleRows.length && !compact" class="dim small" role="status">正在读取后台任务…</p>
  <p v-else-if="loaded && !visibleRows.length && !compact" class="dim small">{{filter==='history'?'暂无历史记录。':filter==='review'?'暂无需要处理的任务。':'暂无进行中的任务，可在会话中选择“安排继续”。'}}</p>
  <p v-if="notice" class="notice small" role="status">{{ notice }}</p>
  <button v-if="removed.length" ref="undoButton" class="btn btn--sm" type="button" :disabled="busy" @click="undoRemove">撤销删除（{{removed.length}}）</button>
  <p v-if="!compact" class="dim small">运行中可查看只读执行过程；历史可删除并撤销，保留原生对话与文件。</p>
  <p v-if="!compact && rows.length>=200" class="notice small">仅显示最近200条记录；删除历史后可刷新读取更早记录。</p>
  <article v-for="task in visibleRows" :key="task.id">
   <strong>{{task.source.name || '原对话'}} · {{task.mode === 'live_quota' && task.state === 'queued' ? '等待开启监测' : labels[task.state]??task.state}}</strong>
   <p v-if="!compact" class="dim small">{{task.source.cwd}} · {{task.source.command}}</p>
   <p v-if="!compact && history(task) && task.updated_at" class="dim small">结束时间：{{absoluteTime(task.updated_at)}}</p>
   <p v-else-if="!compact && task.state!=='needs_review' && task.state!=='running' && task.state!=='cancelling'" class="dim small">下次检查：{{absoluteTime(task.due_at)}}（实际启动取决于设备、原终端和额度状态）</p>
   <p v-if="task.state === 'needs_review'" class="small notice notice--warn">自动执行已暂停，需要处理下方原因；到时间不会自动重试。可取消安排，核对原对话后重新开启。</p>
   <p class="small" :title="task.detail">{{task.detail}}</p>
   <details v-if="!compact"><summary>{{history(task)?'查看结果':'查看执行'}}</summary>
    <p class="dim small">第 {{task.attempt || 0}} 次执行 · {{task.mode==='live_quota'?'原终端自动继续':'独立后台执行 · 只读记录，不接受终端输入'}}</p>
    <p v-if="task.source.recovery?.native_id" class="dim small">原生对话：{{task.source.recovery.native_id}}</p>
    <RouterLink v-if="task.session_id" :to="{name:'session',params:{id:task.session_id}}">打开原会话记录</RouterLink>
    <p v-if="task.progress" class="small">{{task.progress.activity}}<br>开始：{{absoluteTime(task.progress.started_at)}}<span v-if="task.state==='running'"> · 已运行 {{elapsed(task.progress.started_at)}}</span><br>最近同步：{{absoluteTime(task.progress.observed_at)}}<span v-if="task.progress.activity_at"> · 最近活动：{{absoluteTime(task.progress.activity_at)}}</span></p>
    <p v-if="task.progress && !history(task) && now-task.progress.observed_at>15000" class="notice notice--warn small">状态更新已延迟，以下为最后确认数据；正在等待设备同步，请勿重复派发。</p>
    <p v-else-if="!task.progress && task.state==='running'" class="dim small">当前 Agent 尚未提供执行活动，请更新 Agent 后查看；暂无输出不代表执行停止。</p>
    <p>{{task.prompt}}</p>
    <pre v-if="task.output || task.progress?.text">{{task.output || task.progress?.text}}</pre>
    <p v-else class="dim">暂未收到模型回复；等待状态和工具活动不等于任务已完成。</p>
   </details>
   <button v-if="history(task)" class="btn btn--sm" type="button" :disabled="busy" @click="remove(task)">删除记录</button>
   <button v-if="!terminal(task.state) || task.state === 'needs_review'" class="btn btn--sm" type="button" :disabled="busy || task.state==='cancelling'" @click="cancel(task)">{{task.state==='cancelling'?'等待停止确认':task.state === 'needs_review' ? '取消安排' : task.mode === 'live_quota' ? '关闭自动续跑' : task.state==='running'||task.state==='dispatching'?'停止本次执行':'取消预约'}}</button>
  </article>
 </section>
</template>
<style scoped>
.continuation-backdrop{position:fixed;inset:0;z-index:80;background:#0008;display:flex;align-items:center;justify-content:center;padding:12px}.continuation-dialog{width:100%;max-width:580px;max-height:calc(100dvh - 24px);overflow:auto;overflow-wrap:anywhere}.continuation-dialog label{display:grid;gap:6px;margin:12px 0}.continuation-dialog input,.continuation-dialog select,.continuation-dialog textarea{width:100%;min-width:0}.continuation-list{margin-top:16px}.continuation-list article{border-top:1px solid var(--border);padding:12px 0;overflow-wrap:anywhere}.continuation-list pre{white-space:pre-wrap;overflow-wrap:anywhere}
.continuation-list--compact{margin:0;flex:none;padding:4px 10px}.continuation-list--compact article{border:0;padding:0;display:flex;align-items:center;gap:8px;flex-wrap:wrap;font-size:12px}.continuation-list--compact p{margin:0;flex:1;min-width:80px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.continuation-dialog input[type=checkbox]{width:auto;margin-right:6px}
.continuation-filters{flex-wrap:wrap;margin:8px 0}.continuation-filters [aria-pressed=true]{border-color:var(--accent);font-weight:600}.continuation-list summary{cursor:pointer;padding:8px 0}.continuation-list pre{max-height:360px;overflow:auto}
</style>

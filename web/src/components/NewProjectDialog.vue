<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { useConnStore } from '../stores/conn'
import { MessageType } from '../lib/protocol'
import { validProjectName } from '../lib/projectNames'
import { humanizeError } from '../lib/format'
const props = defineProps<{ deviceId: string; roots: string[]; commands: { id: string; label: string }[]; defaultCommand: string; defaultParent: string }>()
const emit = defineEmits<{ close: []; ready: [result: { path: string; filePath?: string; commandId: string; warning?: string; createdProject: boolean }] }>()
const conn = useConnStore()
const dialog = ref<HTMLDialogElement | null>(null)
const parent = ref(props.defaultParent || props.roots[0] || '')
const kind = ref('project')
const name = ref('')
const fileName = ref('README.md')
const content = ref('')
const initGit = ref(true)
const command = ref(props.commands.some(c => c.id === props.defaultCommand) ? props.defaultCommand : props.commands[0]?.id ?? '')
const busy = ref(false), error = ref('')
const created = ref<{path:string;file_path?:string;warning?:string} | null>(null)
function finish():void {
 if(created.value) emit('ready',{path:created.value.path,filePath:created.value.file_path,commandId:command.value,warning:created.value.warning,createdProject:kind.value==='project'})
}
let alive = true
onMounted(() => dialog.value?.showModal())
onBeforeUnmount(() => { alive = false; dialog.value?.close() })
async function create(): Promise<void> {
  if (busy.value) return
  if (created.value) {finish();return}
  error.value = ''
  if ((kind.value === 'project' && !validProjectName(name.value)) || (fileName.value && !validProjectName(fileName.value))) { error.value = '请使用有效的项目和文件名称，不要包含路径分隔符或系统保留名称'; return }
  if (kind.value === 'file' && !fileName.value) {error.value='请填写文件名称';return}
  if (new TextEncoder().encode(content.value).length > 64 * 1024) { error.value = '初始文本上限为 64 KB'; return }
  busy.value = true
  try {
    const env = await conn.request<{ path: string; file_path?: string; warning?: string }>(MessageType.ProjectCreate, {
      kind: kind.value, device_id: props.deviceId, parent: parent.value, name: name.value, file_name: fileName.value, content: fileName.value ? content.value : '', init_git: kind.value === 'project' && initGit.value,
    })
    if (!alive) return
    if (!env.payload?.path) throw new Error('未返回项目目录')
    created.value=env.payload
    if(env.payload.warning){error.value=env.payload.warning;return}
    finish()
  } catch (e) { if (alive) error.value = humanizeError(e) }
  finally { busy.value = false }
}
</script>
<template>
  <dialog ref="dialog" class="new-project" aria-labelledby="new-project-title" @cancel.prevent="!busy && emit('close')">
    <div class="row row--between"><h2 id="new-project-title">新建项目或文件</h2><button class="btn btn--ghost btn--sm" type="button" :disabled="busy" @click="emit('close')">关闭</button></div>
    <form @submit.prevent="create">
      <label class="field">创建类型<select aria-label="创建类型" v-model="kind" :disabled="busy || !!created"><option value="project">项目 / 文件夹</option><option value="file">现有目录中的文件</option></select></label>
      <label class="field">父目录<input v-model="parent" list="project-parent-roots" :disabled="busy || !!created" required /><datalist id="project-parent-roots"><option v-for="root in roots" :key="root" :value="root" /></datalist></label>
      <label v-if="kind === 'project'" class="field">项目名称<input v-model="name" :disabled="busy || !!created" required placeholder="例如 my-app" /></label>
      <label class="field">{{ kind === 'file' ? '文件名称' : '初始文件名称' }}<input v-model="fileName" :disabled="busy || !!created" :placeholder="kind === 'file' ? '例如 main.py' : '留空则只创建目录'" /></label>
      <label v-if="fileName" class="field">初始文件内容<textarea v-model="content" :disabled="busy || !!created" rows="5" placeholder="项目说明或初始代码，可留空" /></label>
      <label v-if="kind === 'project'" class="row small"><input v-model="initGit" type="checkbox" :disabled="busy || !!created" /> 初始化 Git 仓库</label>
      <label class="field">使用编程助手<select aria-label="使用编程助手" v-model="command" :disabled="busy"><option value="">{{ kind === 'file' ? '仅创建文件' : '仅创建项目' }}</option><option v-for="cli in commands" :key="cli.id" :value="cli.id">{{ cli.label }}</option></select></label>
      <p class="small dim">创建在设备授权目录内，同名目录和文件不会覆盖。初始文件会加入助手输入草稿，确认后再发送。</p>
      <p v-if="error" class="notice notice--err" role="alert">{{ error }}</p>
      <button class="btn btn--primary" type="submit" :disabled="busy || !conn.isOpen || !roots.length">{{ busy ? '正在创建…' : created ? '继续' : command ? '创建并打开 CLI' : '创建' }}</button>
    </form>
  </dialog>
</template>
<style scoped>
.new-project { width: min(540px, calc(100vw - 24px)); max-height: calc(100dvh - 24px); overflow: auto; border: 1px solid var(--border); border-radius: 12px; padding: 18px; background: var(--bg-elev); color: var(--fg); }
.new-project::backdrop { background: #0008; }
.new-project .field { display: flex; flex-direction: column; gap: 6px; margin: 12px 0; }
.new-project input:not([type=checkbox]), .new-project select, .new-project textarea { width: 100%; min-width: 0; }
</style>

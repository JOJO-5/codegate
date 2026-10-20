<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useConnStore } from '../stores/conn'
import { humanizeError } from '../lib/format'
import { attachmentFilename, MAX_UPLOAD_SIZE, uploadFile, type UploadedAttachment } from '../lib/uploads'
import { readClipboardImages } from '../lib/clipboardImages'

interface Item extends UploadedAttachment {
  id: string
  file: File
  preview: string
  state: 'selected' | 'uploading' | 'ready' | 'failed' | 'cancelled'
  percent: number
  error: string
}
const props = defineProps<{ sessionId: string; enabled: boolean; nativeImages: boolean }>()
const emit = defineEmits<{
  close: []
  reference: [files: UploadedAttachment[]]
  native: [file: UploadedAttachment]
}>()
const conn = useConnStore()
const items = ref<Item[]>([])
const imagePicker = ref<HTMLInputElement | null>(null)
const filePicker = ref<HTMLInputElement | null>(null)
const busy = ref(false)
const cancelling = ref(false)
const error = ref('')
const status = ref('')
const reading = ref(false)
let clipboardEpoch = 0
let disposed = false
let abort: AbortController | null = null
const pending = computed(() => items.value.filter(x => x.state !== 'ready'))
const ready = computed(() => items.value.filter(x => x.state === 'ready'))
function nativeImage(item: Item): boolean { return props.nativeImages && /\.(png|jpe?g|webp|gif|avif|pdf)$/i.test(item.path) }
function formatSize(size: number): string { return size < 1024 * 1024 ? `${Math.ceil(size / 1024)} KB` : `${(size / 1024 / 1024).toFixed(1)} MB` }

function select(event: Event): void {
  const input = event.target as HTMLInputElement
  addFiles(Array.from(input.files ?? []))
  input.value = '' // Allow selecting the same local file again.
}
function addFiles(files: File[]): void {
  // A direct paste supersedes a pending permission prompt/read.
  clipboardEpoch++
  reading.value = false
  if (disposed) return
  if (!props.enabled || !conn.isOpen) { error.value = '连接恢复且拥有运行中会话的控制权后才能添加图片或文件。'; return }
  if (busy.value) { error.value = '正在上传，请完成或取消后再次粘贴或选择文件。'; return }
  error.value = ''
  status.value = ''
  let count = 0
  for (const file of files) {
    if (items.value.length >= 20) { error.value = '一次最多保留 20 个附件，请先移除部分文件'; break }
    if (file.size > MAX_UPLOAD_SIZE) { error.value = `${file.name} 超过单个文件 100 MB 的上限`; continue }
    const preview = /^image\/(png|jpeg|webp|gif|avif|bmp)$/.test(file.type) && file.size <= 10 * 1024 * 1024 ? URL.createObjectURL(file) : ''
    items.value.push({ id: crypto.randomUUID(), name: file.name, file, path: attachmentFilename(file.name), size: file.size, preview, state: 'selected', percent: 0, error: '' })
    count++
  }
  if (count) status.value = `已添加 ${count} 个附件，尚未上传。确认后点击“上传所选文件”。`
}
async function pasteImages(): Promise<void> {
  if (!props.enabled || !conn.isOpen || busy.value || reading.value) return
  const epoch = ++clipboardEpoch
  reading.value = true
  error.value = ''; status.value = ''
  try {
    const files = await readClipboardImages(navigator.clipboard)
    if (disposed || epoch !== clipboardEpoch) return
    if (!files.length) { error.value = '剪贴板中没有图片，请先复制图片或使用“选择图片”。'; return }
    addFiles(files)
  } catch {
    if (!disposed && epoch === clipboardEpoch) error.value = '未能读取剪贴板图片，请在终端或输入框用 Ctrl+V / ⌘V 粘贴，或使用“选择图片”。'
  } finally { if (epoch === clipboardEpoch) reading.value = false }
}
function close(): void { clipboardEpoch++; reading.value = false; emit('close') }
defineExpose({ addFiles })
function remove(item: Item): void {
  if (busy.value) return
  if (item.preview) URL.revokeObjectURL(item.preview)
  items.value = items.value.filter(x => x.id !== item.id)
  status.value = ''
}
function cancel(): void { cancelling.value = true; abort?.abort() }
async function upload(): Promise<void> {
  if (!props.enabled || !conn.isOpen || busy.value || !pending.value.length) return
  busy.value = true
  cancelling.value = false
  error.value = ''
  status.value = ''
  const controller = new AbortController()
  abort = controller
  try {
    for (const item of [...pending.value]) {
      if (controller.signal.aborted || !props.enabled) break
      item.state = 'uploading'; item.percent = 0; item.error = ''
      try {
        await uploadFile(conn, props.sessionId, item.file, item.path, { signal: controller.signal, progress: value => { item.percent = value } })
        item.state = 'ready'
      } catch (e) {
        item.state = controller.signal.aborted ? 'cancelled' : 'failed'
        item.error = controller.signal.aborted ? '已取消，可重新上传' : humanizeError(e)
        if (!conn.isOpen || controller.signal.aborted) break
      }
    }
  } finally { abort = null; busy.value = false; cancelling.value = false }
}
watch(() => props.enabled && conn.isOpen, enabled => {
  if (!enabled) { clipboardEpoch++; reading.value = false; if (busy.value) cancel() }
}, { flush: 'sync' })
onUnmounted(() => {
  disposed = true; clipboardEpoch++
  abort?.abort()
  for (const item of items.value) if (item.preview) URL.revokeObjectURL(item.preview)
})
</script>

<template>
  <section class="term__attachments" aria-label="会话附件">
    <div class="row row--between">
      <strong>图片和文件</strong>
      <button class="btn btn--ghost btn--sm" type="button" @click="close">收起附件</button>
    </div>
    <p class="small dim">上传到当前会话工作目录，再加入输入，由你确认发送。单个文件上限 100 MB。</p>
    <p class="small dim">可在终端或输入框用 Ctrl+V / ⌘V 粘贴图片。此处仅保留本地预览，刷新或离开会话会清除尚未上传的附件。</p>
    <input ref="imagePicker" type="file" accept="image/*" multiple hidden aria-label="选择图片附件" @change="select" />
    <input ref="filePicker" type="file" multiple hidden aria-label="选择文件附件" @change="select" />
    <div class="row term__attachment-actions">
      <button class="btn btn--sm" type="button" :disabled="!enabled || busy" @click="imagePicker?.click()">选择图片</button>
      <button class="btn btn--sm" type="button" :disabled="!enabled || !conn.isOpen || busy || reading" @click="pasteImages">{{ reading ? '正在读取图片…' : '粘贴图片' }}</button>
      <button class="btn btn--sm" type="button" :disabled="!enabled || busy" @click="filePicker?.click()">选择文件</button>
      <button class="btn btn--primary btn--sm" type="button" :disabled="!enabled || !conn.isOpen || busy || !pending.length" @click="upload">上传所选文件</button>
      <button v-if="busy" class="btn btn--sm" type="button" :disabled="cancelling" @click="cancel">{{ cancelling ? '正在取消…' : '取消上传' }}</button>
    </div>
    <p v-if="error" class="notice notice--err small" role="alert">{{ error }}</p>
    <p v-if="status" class="small" role="status">{{ status }}</p>
    <p v-if="reading" class="small" role="status">正在读取剪贴板；可收起附件以取消本次读取。</p>
    <div v-for="item in items" :key="item.id" class="term__attachment">
      <img v-if="item.preview" :src="item.preview" :alt="item.name" />
      <div class="term__attachment-info">
        <strong>{{ item.name }}</strong><span class="small dim">{{ formatSize(item.size) }}</span>
        <span v-if="item.state === 'selected'" class="small">待上传</span>
        <span v-if="item.state === 'uploading'" class="small" role="status">正在上传 · {{ item.percent }}%</span>
        <progress v-if="item.state === 'uploading'" :value="item.percent" max="100" :aria-label="`${item.name} 上传进度`" />
        <span v-if="item.state === 'ready'" class="small" role="status">已上传到工作区</span>
        <span v-if="item.error" class="small" role="alert">{{ item.error }}</span>
        <code v-if="item.state === 'ready'" class="small">{{ item.path }}</code>
      </div>
      <div class="term__attachment-buttons">
        <button v-if="item.state === 'ready' && nativeImage(item)" class="btn btn--sm" type="button" :disabled="!enabled || !conn.isOpen" @click="emit('native', item)">作为附件加入 OpenCode</button>
        <button v-if="item.state === 'ready'" class="btn btn--sm" type="button" :disabled="!enabled || !conn.isOpen" @click="emit('reference', [item])">路径加入输入</button>
        <button class="btn btn--ghost btn--sm" type="button" :disabled="busy" @click="remove(item)">移除</button>
      </div>
    </div>
    <button v-if="ready.length > 1" class="btn btn--sm" type="button" :disabled="!enabled || !conn.isOpen" @click="emit('reference', ready)">全部路径加入输入</button>
    <p v-if="ready.length" class="small dim">移除列表项不会删除已上传文件。图片识别取决于 CLI 和模型支持；其他 CLI 可通过文件路径读取。</p>
    <p v-if="!enabled" class="small dim">连接恢复且拥有运行中会话的控制权后才能上传或加入输入。</p>
  </section>
</template>

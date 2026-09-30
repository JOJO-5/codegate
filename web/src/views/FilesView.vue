<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useConnStore } from '../stores/conn'
import { MessageType, type SessionCreatedPayload } from '../lib/protocol'
import { humanizeError } from '../lib/format'

interface Entry { name: string; path: string; is_dir: boolean; size: number; modified: number; symlink?: boolean }
interface Listed { path: string; entries: Entry[] }
interface Stat { path: string; size: number; is_dir: boolean; is_binary: boolean; encoding: string }
interface Chunk { path: string; size: number; offset: number; data: string }
const props = defineProps<{ id: string }>()
const conn = useConnStore()
const router = useRouter()
const cwd = ref('')
const folder = ref('')
const entries = ref<Entry[]>([])
const busy = ref(false)
const error = ref('')
const previewName = ref('')
const previewText = ref('')
const previewURL = ref('')
const previewEntry = ref<Entry | null>(null)
const transferLabel = ref('')
const transferPercent = ref(0)
const transferDone = ref('')
const uploading = ref(false)
const overwrite = ref(false)
const selected = ref<File | null>(null)
const picker = ref<HTMLInputElement | null>(null)
const folderLabel = computed(() => folder.value || '工作区根目录')

function clearPreview(): void {
  if (previewURL.value) URL.revokeObjectURL(previewURL.value)
  previewURL.value = ''
  previewText.value = ''
  previewName.value = ''
  previewEntry.value = null
}

async function list(path = folder.value): Promise<void> {
  busy.value = true
  error.value = ''
  clearPreview()
  try {
    const env = await conn.request<Listed>(MessageType.FileList, { session_id: props.id, path }, props.id)
    entries.value = env.payload?.entries ?? []
    folder.value = path
  } catch (e) { error.value = humanizeError(e) }
  finally { busy.value = false }
}

function up(): void {
  const parts = folder.value.split('/').filter(Boolean)
  parts.pop()
  void list(parts.join('/'))
}

async function stat(path: string): Promise<Stat> {
  const env = await conn.request<Stat>(MessageType.FileStat, { session_id: props.id, path }, props.id)
  if (!env.payload) throw new Error('没有文件信息')
  return env.payload
}

async function read(path: string, size: number, track = false): Promise<Blob> {
  if (size > 100 * 1024 * 1024) throw new Error('浏览器单次下载上限为 100 MB')
  const chunks: BlobPart[] = []
  for (let offset = 0; offset < size;) {
    const env = await conn.request<Chunk>(MessageType.FileRead, {
      session_id: props.id, path, offset, length: Math.min(192 * 1024, size - offset),
    }, props.id)
    const p = env.payload
    if (!p || p.offset !== offset || p.size !== size) throw new Error('文件在传输期间发生变化，请重试')
    const raw = atob(p.data)
    if (raw.length === 0) throw new Error('文件读取提前结束')
    const bytes = new Uint8Array(raw.length)
    for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i)
    chunks.push(bytes)
    offset += bytes.length
    if (track) transferPercent.value = Math.round(offset / size * 100)
  }
  return new Blob(chunks)
}

async function preview(e: Entry): Promise<void> {
  busy.value = true
  error.value = ''
  clearPreview()
  try {
    const info = await stat(e.path)
    const image = /\.(png|jpe?g|gif|webp)$/i.test(e.name)
    if (image && info.size <= 5 * 1024 * 1024) {
      const mime = /\.png$/i.test(e.name) ? 'image/png' : /\.webp$/i.test(e.name) ? 'image/webp' : /\.gif$/i.test(e.name) ? 'image/gif' : 'image/jpeg'
      previewURL.value = URL.createObjectURL(new Blob([await read(e.path, info.size)], { type: mime }))
    } else if (!info.is_binary && info.size <= 1024 * 1024) {
      previewText.value = await (await read(e.path, info.size)).text()
    } else {
      throw new Error('仅预览 1 MB 内的 UTF-8 文本或 5 MB 内的常见图片；可使用下载')
    }
    previewName.value = e.name
    previewEntry.value = e
  } catch (err) { error.value = humanizeError(err) }
  finally { busy.value = false }
}

async function download(e: Entry): Promise<void> {
  busy.value = true
  error.value = ''
  transferDone.value = ''
  transferLabel.value = `下载 ${e.name}`
  transferPercent.value = 0
  try {
    const info = await stat(e.path)
    const url = URL.createObjectURL(await read(e.path, info.size, true))
    const link = document.createElement('a')
    link.href = url
    link.download = e.name
    link.click()
    setTimeout(() => URL.revokeObjectURL(url), 60000)
    transferPercent.value = 100
    transferDone.value = `已下载 ${e.name}`
  } catch (err) { error.value = humanizeError(err) }
  finally { busy.value = false; transferLabel.value = '' }
}

async function upload(): Promise<void> {
  const file = selected.value
  if (!file) return
  if (file.size > 100 * 1024 * 1024) { error.value = '上传上限为 100 MB'; return }
  uploading.value = true
  error.value = ''
  transferDone.value = ''
  transferLabel.value = `上传 ${file.name}`
  transferPercent.value = 0
  const uploadId = crypto.randomUUID()
  const path = [folder.value, file.name].filter(Boolean).join('/')
  try {
    for (let offset = 0; offset < file.size || (file.size === 0 && offset === 0);) {
      const bytes = new Uint8Array(await file.slice(offset, offset + 192 * 1024).arrayBuffer())
      let binary = ''
      for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]!)
      const next = offset + bytes.length
      await conn.request(MessageType.FileWrite, {
        session_id: props.id, upload_id: uploadId, path, size: file.size,
        offset, data: btoa(binary), final: next === file.size, overwrite: overwrite.value,
      }, props.id)
      if (file.size === 0) break
      offset = next
      transferPercent.value = Math.round(offset / file.size * 100)
    }
    selected.value = null
    if (picker.value) picker.value.value = ''
    await list()
    transferPercent.value = 100
    transferDone.value = `已上传 ${file.name}`
  } catch (e) {
    error.value = humanizeError(e)
    if (conn.isOpen) void conn.request(MessageType.FileCancel, { session_id: props.id, transfer_id: uploadId }, props.id).catch(() => {})
  } finally { uploading.value = false; transferLabel.value = '' }
}

onMounted(async () => {
  try {
    const env = await conn.request<SessionCreatedPayload>(MessageType.SessionGet, { session_id: props.id }, props.id)
    cwd.value = env.payload?.session.cwd ?? ''
    await list('')
  } catch (e) { error.value = humanizeError(e) }
})
onUnmounted(clearPreview)
</script>

<template>
  <main class="page">
    <div class="row row--between" style="margin-bottom: 16px">
      <div class="row">
        <button class="btn btn--sm" type="button" @click="router.push({ name: 'session', params: { id } })">‹ 返回终端</button>
        <h1 style="margin: 0">工作区文件</h1>
      </div>
      <button class="btn btn--sm" type="button" :disabled="busy" @click="list()">刷新</button>
    </div>
    <div class="card">
      <div v-if="transferLabel" class="notice notice--info" role="status">{{ transferLabel }} · {{ transferPercent }}%<progress :value="transferPercent" max="100" style="display: block; width: 100%; margin-top: 6px" /></div>
      <div v-if="transferDone" class="notice notice--info" role="status">{{ transferDone }}</div>
      <div class="small dim mono">{{ cwd }} / {{ folderLabel }}</div>
      <div class="row" style="margin: 12px 0">
        <button class="btn btn--sm" type="button" :disabled="!folder || busy" @click="up">上一级</button>
        <span v-if="busy" class="spinner" />
      </div>
      <div v-if="error" class="notice notice--err">{{ error }}</div>
      <div v-if="!busy && entries.length === 0" class="empty">目录为空</div>
      <div v-for="entry in entries" :key="entry.path" class="row row--between" style="padding: 8px 0; border-bottom: 1px solid var(--border)">
        <button class="btn btn--ghost btn--sm mono" type="button" :disabled="busy" @click="entry.is_dir ? list(entry.path) : preview(entry)">
          {{ entry.is_dir ? '📁' : '📄' }} {{ entry.name }}
        </button>
        <div v-if="!entry.is_dir" class="row">
          <span class="faint small">{{ entry.size }} B</span>
          <button class="btn btn--sm" type="button" :disabled="busy" @click="download(entry)">下载</button>
        </div>
      </div>
    </div>
    <div v-if="previewName" class="card" style="margin-top: 14px">
      <div class="card__title"><h2>{{ previewName }}</h2><div class="row"><button v-if="previewEntry" class="btn btn--sm" type="button" :disabled="busy" @click="download(previewEntry)">下载</button><button class="btn btn--sm" type="button" @click="clearPreview">关闭预览</button></div></div>
      <img v-if="previewURL" :src="previewURL" alt="文件预览" style="max-width: 100%; max-height: 70vh" />
      <pre v-else style="overflow: auto; white-space: pre-wrap; max-height: 70vh">{{ previewText }}</pre>
    </div>
    <div class="card" style="margin-top: 14px">
      <h2>上传到当前目录</h2>
      <input ref="picker" type="file" @change="selected = ($event.target as HTMLInputElement).files?.[0] ?? null" />
      <label class="row small"><input v-model="overwrite" type="checkbox" /> 覆盖同名文件</label>
      <button class="btn btn--primary" type="button" :disabled="!selected || uploading || !conn.isOpen" @click="upload">
        {{ uploading ? '正在上传…' : '上传' }}
      </button>
      <p class="small dim">上传及下载单个文件上限 100 MB，文件只在当前会话工作目录内访问。</p>
    </div>
  </main>
</template>

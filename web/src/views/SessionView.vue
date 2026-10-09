<script setup lang="ts">
/**
 * 终端页（xterm.js 全屏）。
 *
 * # 三条纪律（架构文档 §5.3，每条都有具体的失败症状）
 *
 * 1. **`term.write()` 只收 `Uint8Array`，不做 UTF-8 解码。**
 *    xterm.js 自己做**增量**解码，能正确处理跨帧的多字节字符。
 *    在这里先 `TextDecoder` 解一遍，会把一个中文字符的两个字节
 *    分到两帧里分别解码 —— 屏幕上出现两个乱码方块，而且只在
 *    输出被切在字符中间时偶发。这是最难查的一类 bug。
 *
 * 2. **不覆盖 `attachCustomKeyEventHandler`。**
 *    xterm.js 对 Ctrl+C / Tab / 方向键的默认处理已经是正确的，
 *    自己拦一遍只会拦错。只处理浏览器级快捷键（粘贴/搜索/复制）。
 *
 * 3. **卸载必须 `terminal.dispose()` + `ResizeObserver.disconnect()`。**
 *    少任何一个，路由来回切几次就会泄漏 —— xterm 持有 canvas 和
 *    一大堆 DOM 监听器，而 ResizeObserver 会一直抓着已卸载的 DOM 不放。
 *
 * # seq 的语义（这一条不看代码很容易写错）
 *
 * 二进制帧头里**没有**序号字段。序号是**字节偏移**，来自 Agent 的 ring buffer：
 *   `seqTo = ring.Total()`，`seqFrom = seqTo - len(重放数据)`。
 *
 * 而 attach 的重放是**六步**（见 `internal/session/session.go` 的 `Attach`）：
 *   reset → preamble → clear → **真实数据** → syncOff
 * 其中只有第 4 步的字节计入 seq，前后几帧是合成的控制序列。
 *
 * 所以客户端**不能**按所有 Buffer 帧的长度累加。正确做法是：
 *   - attach 响应回来时 `lastSeq = seq_to`（Agent 侧也是这么记的）
 *   - 之后只对 **Stdout**（实时输出）帧按 payload 长度累加
 *   - `FlagBufferEnd` 只用来判断「重放结束」，不参与计数
 */

import { api } from '../lib/api'
import { TASK_TEMPLATES } from '../lib/tasks'
import QuotaPanel from '../components/QuotaPanel.vue'
import FilesView from './FilesView.vue'
import SessionAttachments from '../components/SessionAttachments.vue'
import TerminalTextReader from '../components/TerminalTextReader.vue'
import { terminalTextSnapshot, type TerminalTextSnapshot } from '../lib/terminalText'
import { workspaceFilePath, type UploadedAttachment } from '../lib/uploads'
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { SearchAddon } from '@xterm/addon-search'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { WebglAddon } from '@xterm/addon-webgl'

import { useConnStore } from '../stores/conn'
import { useSessionsStore } from '../stores/sessions'
import { useDevicesStore } from '../stores/devices'
import { usePreferencesStore } from '../stores/preferences'
import {
  FrameFlag,
  FrameType,
  MessageType,
  type DecodedFrame,
  type Envelope,
  type SessionAttachedPayload,
  type SessionExitPayload,
  type SessionSummary,
} from '../lib/protocol'
import { classifyCommand } from '../lib/commands'
import { humanizeError, statusKind, statusLabel } from '../lib/format'

const props = defineProps<{ id: string }>()

const conn = useConnStore()
const sessions = useSessionsStore()
const devices = useDevicesStore()
const preferences = usePreferencesStore()
const router = useRouter()

const hostEl = ref<HTMLDivElement | null>(null)

const sessionId = computed(() => props.id)

const summary = ref<SessionSummary | null>(null)
const workspaceBranch = ref('')
const workspaceError = ref('')
const workspaceOpen = ref(false)
const filesOpen = ref(false)
function openFiles(): void {
 if (window.matchMedia('(min-width: 769px)').matches) filesOpen.value = !filesOpen.value
 else void router.push({ name: 'session-files', params: { id: sessionId.value } })
}
const sessionTool = computed(() => {
  const s = summary.value
  if (!s) return '终端'
  const command = /(?:^|[\\/])cmd\.exe$/i.test(s.command) && s.args?.[1] ? s.args[1] : s.command
  return command.split(/[\\/]/).pop() || '终端'
})
const sessionDevice = computed(() => summary.value?.device_id.replace(/-/g, '') ?? '')
const isolatedWorkspace = computed(() => /[\\/]\.codegate-worktrees[\\/]/.test(summary.value?.cwd ?? ''))
async function refreshIdentity(): Promise<void> {
  const s = summary.value
  if (!s) return
  const id = s.session_id
  try {
    const result = await api.deviceGit(sessionDevice.value, s.cwd, 'status')
    if (summary.value?.session_id === id) { workspaceBranch.value = result.branch; workspaceError.value = '' }
  } catch { if (summary.value?.session_id === id) { workspaceBranch.value = ''; workspaceError.value = '分支信息暂不可用，可能不是 Git 目录。' } }
}

const role = ref<'controller' | 'viewer'>('viewer')
const remoteCols = ref(80)
const remoteRows = ref(24)
const claiming = ref(false)
function applyTerminalSize(): void {
 if (!term || !fit) return
 if (role.value === 'viewer' || !attachmentReady.value) term.resize(remoteCols.value, remoteRows.value)
 else fit.fit()
}
async function claimControl(): Promise<void> {
 if (claiming.value || !attachmentReady.value || !conn.isOpen) return
 claiming.value = true
 try {
  await conn.request(MessageType.SessionClaimControl, { session_id: sessionId.value }, sessionId.value)
 } catch (e) { errorText.value = humanizeError(e) }
 finally { claiming.value = false }
}
const terminalReady = ref(false)
const attachmentReady = ref(false)
const attaching = ref(false)
let attachmentEpoch = 0
let fullReplayQueued = false
const replaying = ref(false)
const exiting = ref(false)
const confirmTerminate = ref(false)
const replayWarning = ref(false)
const exitInfo = ref<SessionExitPayload | null>(null)
const errorText = ref<string | null>(null)
const mobileDraft = ref('')
const composer = ref<HTMLTextAreaElement | null>(null)
const shortcutInput = ref(false)
const attachmentsOpen = ref(false)
const nativeImages = computed(() => !isShell.value && /(?:^|[\\/\s])opencode(?:\.exe)?(?:\s|$)/i.test(summary.value?.command ?? ''))
function referenceAttachments(files: UploadedAttachment[]): void {
  if (!summary.value || role.value !== 'controller' || !conn.isOpen || !isLive.value) return
  const paths = files.map(file => workspaceFilePath(summary.value!.cwd, file.path))
  const references = paths.map(path => {
    if (!isShell.value) return JSON.stringify(path)
    if (/\b(?:powershell|pwsh)(?:\.exe)?\b/i.test(summary.value!.command)) return `'${path.replace(/'/g, "''")}'`
    if (windowsShell.value) return JSON.stringify(path)
    return `'${path.replace(/'/g, "'\\''")}'`
  }).join(' ')
  mobileDraft.value = mobileDraft.value
    ? `${mobileDraft.value} ${references}`
    : `${isShell.value ? '' : '请查看这些文件：'}${references}`
  attachmentsOpen.value = false
  clipboardStatus.value = '文件路径已加入输入框，请补充说明后确认发送。'
}
function pasteNativeAttachment(file: UploadedAttachment): void {
  if (!summary.value || !nativeImages.value || !term || role.value !== 'controller' || !conn.isOpen || !isLive.value) return
  const path = workspaceFilePath(summary.value.cwd, file.path)
  if (/[\x00-\x1f\x7f]/.test(path)) { errorText.value = '目录包含控制字符，请通过文件路径引用附件。'; return }
  if (!term.modes.bracketedPasteMode) { referenceAttachments([file]); return }
  // OpenCode recognizes a bracketed paste of a local image/PDF path as an
  // attachment. Keep its own attachment parsing; never send a submit key.
  term.paste(path)
  attachmentsOpen.value = false
  clipboardStatus.value = '附件已粘贴到 OpenCode，请检查 CLI 中的预览后确认发送。'
}
const showSessionPanel = ref(false)
const showExtraKeys = ref(false)
const pasteOpen = ref(false)
const pasteDraft = ref('')
const clipboardStatus = ref('')
const copyFallback = ref('')
const textSnapshot = ref<TerminalTextSnapshot | null>(null)
function openTextReader(): void {
  if (!term || textSnapshot.value) return
  cancelTerminalTouch()
  shortcutInput.value = true
  if (term.textarea) term.textarea.inputMode = 'none'
  composer.value?.blur()
  term.blur()
  textSnapshot.value = terminalTextSnapshot(term.buffer.active)
}
async function readClipboard(): Promise<void> {
  pasteOpen.value = true
  pasteDraft.value = ''
  clipboardStatus.value = ''
  try { pasteDraft.value = await navigator.clipboard.readText() }
  catch { clipboardStatus.value = '浏览器未允许读取剪贴板，请在下方输入框手动粘贴。' }
}
function pasteToTerminal(): void {
  if (!term || role.value !== 'controller' || !isLive.value || !conn.isOpen || !pasteDraft.value) return
  term.paste(pasteDraft.value)
  pasteOpen.value = false
  pasteDraft.value = ''
  clipboardStatus.value = '已粘贴到终端。'
}
async function copyOutput(screen = false): Promise<void> {
  if (!term) return
  let text = term.getSelection()
  if (screen) {
    const buffer = term.buffer.active
    text = Array.from({ length: term.rows }, (_, row) => buffer.getLine(buffer.viewportY + row)?.translateToString(true) ?? '').join('\n')
  }
  if (!text.trim()) { clipboardStatus.value = '请先选择输出文字，或使用“复制画面”。'; return }
  try { await navigator.clipboard.writeText(text); clipboardStatus.value = '已复制'; copyFallback.value = '' }
  catch { copyFallback.value = text; clipboardStatus.value = '浏览器未允许复制，请在下方选中文字手动复制。' }
}
watch(() => preferences.fontSize, size => {
  if (term) { term.options.fontSize = size; scheduleFit() }
})
function setFontSize(event: Event): void {
  void preferences.change('font_size', Number((event.target as HTMLSelectElement).value)).catch(() => {})
}
const siblingSessions = computed(() => sessions.forDevice(summary.value?.device_id.replace(/-/g, '') ?? '').filter(s => !s.archived))
const termHeight = ref('100dvh')
function syncViewport(): void {
  termHeight.value = `${window.visualViewport?.height ?? window.innerHeight}px`
  scheduleFit()
}
function submitDraft(event?: KeyboardEvent): void {
  if (event?.isComposing || !mobileDraft.value || role.value !== 'controller' || !isLive.value || !conn.isOpen) return
  sendText(mobileDraft.value + '\r')
  mobileDraft.value = ''
  composer.value?.focus()
}
function onComposerKeydown(event: KeyboardEvent): void {
  if (event.key !== 'Enter' || event.shiftKey || event.isComposing || event.keyCode === 229) return
  event.preventDefault()
  submitDraft()
}
function enableSoftKeyboard(): void {
  shortcutInput.value = false
  if (term?.textarea) term.textarea.inputMode = 'text'
}
function prepareShortcutInput(event: PointerEvent | MouseEvent): void {
  const touch = 'pointerType' in event
    ? event.pointerType === 'touch' || event.pointerType === 'pen'
    : event.detail > 0 && window.matchMedia('(pointer: coarse)').matches
  if (!touch) return
  shortcutInput.value = true
  // Keeping an editable field focused can reactivate the mobile IME even
  // when pointerdown's default focus change is prevented. Explicit keyboard
  // intent (the keyboard button, composer, or terminal) re-enables it.
  if (term?.textarea) term.textarea.inputMode = 'none'
  composer.value?.blur()
  term?.blur()
}
function focusTerminal(): void { enableSoftKeyboard(); term?.focus() }
function scrollHistory(lines: number): void {
  if (term?.buffer.active.type === 'alternate') {
    // Full-screen TUIs own their history. Their alternate buffer has no
    // xterm scrollback; send the navigation key to the running program.
    if (role.value === 'controller' && isLive.value && conn.isOpen) {
      sendText(lines < 0 ? '\x1b[5~' : '\x1b[6~')
    }
  } else {
    term?.scrollLines(lines)
  }
}
let terminalTouch: { id: number; x: number; y: number; lastY: number; pending: number; started: number; dragging: boolean } | null = null
let suppressTerminalClickUntil = 0
let terminalLongPress: ReturnType<typeof setTimeout> | null = null
function cancelTerminalTouch(): void {
  terminalTouch = null
  if (terminalLongPress !== null) clearTimeout(terminalLongPress)
  terminalLongPress = null
}
function onTerminalPointerDown(e: PointerEvent): void {
  if (e.pointerType !== 'touch') enableSoftKeyboard()
}
function onTerminalTouchStart(e: TouchEvent): void {
  cancelTerminalTouch()
  if (role.value === 'viewer') { e.stopPropagation(); return }
  if (e.touches.length !== 1 || !term || (e.target as Element).closest('.term__overlay')) return
  suppressTerminalClickUntil = 0
  const touch = e.touches[0]!
  terminalTouch = { id: touch.identifier, x: touch.clientX, y: touch.clientY, lastY: touch.clientY, pending: 0, started: Date.now(), dragging: false }
  terminalLongPress = setTimeout(() => {
    suppressTerminalClickUntil = Date.now() + 700
    openTextReader()
  }, 550)
  // Own vertical touch scrolling before xterm's viewport handler. Its handler
  // ignores touches when a TUI enables mouse tracking, and alt buffers have
  // no local scrollback. A tap still enables typing when the finger lifts.
  if (term.textarea) term.textarea.inputMode = 'none'
  e.stopPropagation()
}
function onTerminalTouchMove(e: TouchEvent): void {
  const gesture = terminalTouch
  if (!gesture || !term) return
  if (e.touches.length !== 1) { cancelTerminalTouch(); return }
  const touch = Array.from(e.touches).find(t => t.identifier === gesture.id)
  if (!touch) return
  e.stopPropagation()
  if (!gesture.dragging) {
    const dx = touch.clientX - gesture.x
    const dy = touch.clientY - gesture.y
    if (Math.max(Math.abs(dx), Math.abs(dy)) >= 8 && terminalLongPress !== null) {
      clearTimeout(terminalLongPress)
      terminalLongPress = null
    }
    // Leave horizontal gestures available to the browser.
    if (Math.abs(dx) > 8 && Math.abs(dx) > Math.abs(dy)) {
      cancelTerminalTouch()
      return
    }
    if (Math.abs(dy) < 8) return
    gesture.dragging = true
    shortcutInput.value = true
    composer.value?.blur()
    term.blur()
  }
  if (e.cancelable) e.preventDefault()
  suppressTerminalClickUntil = Date.now() + 700
  gesture.pending += gesture.lastY - touch.clientY
  gesture.lastY = touch.clientY
  const screen = term.element?.querySelector('.xterm-screen')
  const bounds = screen?.getBoundingClientRect()
  if (!screen || !bounds || bounds.height <= 0) return
  const alternate = term.buffer.active.type === 'alternate'
  const mouse = alternate && term.modes.mouseTrackingMode !== 'none' && term.modes.mouseTrackingMode !== 'x10'
  const step = alternate && !mouse ? Math.max(48, Math.min(96, bounds.height / 3)) : bounds.height / term.rows
  const lines = Math.trunc(gesture.pending / step)
  if (!lines) return
  gesture.pending -= lines * step
  if (!alternate) { term.scrollLines(lines); return }
  if (role.value !== 'controller' || !isLive.value || !conn.isOpen) return
  if (!mouse) { scrollHistory(lines); return }
  // Let xterm encode the CLI's negotiated mouse protocol and coordinates.
  // Use one wheel event per row: xterm mouse reports encode direction only.
  for (let i = 0; i < Math.min(Math.abs(lines), 32); i++) {
    screen.dispatchEvent(new WheelEvent('wheel', {
      bubbles: true, cancelable: true, deltaMode: WheelEvent.DOM_DELTA_LINE,
      deltaY: Math.sign(lines),
      clientX: Math.max(bounds.left + 1, Math.min(gesture.x, bounds.right - 1)),
      clientY: Math.max(bounds.top + 1, Math.min(touch.clientY, bounds.bottom - 1)),
    }))
  }
}
function onTerminalTouchEnd(e: TouchEvent): void {
  const gesture = terminalTouch
  if (!gesture || !Array.from(e.changedTouches).some(t => t.identifier === gesture.id)) return
  cancelTerminalTouch()
  if (gesture.dragging) {
    if (e.cancelable) e.preventDefault()
    e.stopPropagation()
    suppressTerminalClickUntil = Date.now() + 700
  } else {
    enableSoftKeyboard()
  }
}
function onTerminalClick(e: MouseEvent): void {
  if (Date.now() < suppressTerminalClickUntil) { e.preventDefault(); e.stopPropagation() }
}
function onTerminalContextMenu(e: MouseEvent): void {
  if (textSnapshot.value || (terminalTouch && Date.now() - terminalTouch.started >= 450)) {
    e.preventDefault()
    openTextReader()
  }
}
function redraw(): void {
  if (role.value !== 'controller' || !isLive.value) return
  sendText('\x0c') // Most interactive CLIs repaint on Ctrl+L.
}

// ---- 搜索（Ctrl+Shift+F，规格 §5.3 要求拦这个组合键）----
const searchOpen = ref(false)
const searchTerm = ref('')
const searchInput = ref<HTMLInputElement | null>(null)

/** 当前会话是否属于 shell 类（Ctrl+C 无效，§6.5）。 */
const kind = computed(() => {
  const s = summary.value
  if (s === null) return 'tui' as const
  // 用命令行 + 名字一起判断：Agent 可能给的是 `cmd.exe /k`，
  // 也可能只回一个名字。任一命中 shell 就按 shell 处理。
  const byCmd = classifyCommand(s.command ?? '')
  if (byCmd === 'shell') return 'shell' as const
  return classifyCommand(s.name ?? '')
})
const isShell = computed(() => kind.value === 'shell')
const windowsShell = computed(() => {
  const platform = devices.byId(summary.value?.device_id.replace(/-/g, '') ?? '')?.platform
  return isShell.value && platform !== 'linux' && platform !== 'darwin'
})
const isLive = computed(() => {
  const st = summary.value?.status
  return st === 'starting' || st === 'running' || st === 'detached'
})

// ---------------------------------------------------------------------------
// xterm 实例（非响应式 —— 它是一堆 DOM 和 canvas，不需要被 Vue 代理）
// ---------------------------------------------------------------------------

let term: Terminal | null = null
let fit: FitAddon | null = null
let search: SearchAddon | null = null
let ro: ResizeObserver | null = null

/** 已确认收到的字节序号。重连时作为 `since` 发回去。 */
let lastSeq = 0

/** 上一次真正发给服务端的尺寸，用于「值未变则不发」。 */
let sentCols = 0
let sentRows = 0

let resizeTimer: ReturnType<typeof setTimeout> | null = null
function scheduleFit(): void {
  if (resizeTimer !== null) clearTimeout(resizeTimer)
  resizeTimer = setTimeout(() => {
    resizeTimer = null
    if (!term || !fit) return
    try { applyTerminalSize() } catch { return }
    sendResize()
  }, RESIZE_DEBOUNCE_MS)
}
let unsubFrame: (() => void) | null = null
let unsubControl: (() => void) | null = null

/** 单一编码器复用。每帧 new 一个 TextEncoder 在按键高频路径上是纯浪费。 */
const encoder = new TextEncoder()

const RESIZE_DEBOUNCE_MS = 120

// ---------------------------------------------------------------------------
// 收发
// ---------------------------------------------------------------------------

function sendBytes(bytes: Uint8Array): void {
  if (bytes.length === 0) return
  conn.sendFrame(FrameType.Stdin, 0, sessionId.value, bytes)
}

function sendText(text: string): void {
  sendBytes(encoder.encode(text))
}

function sendResize(): void {
  const t = term
  if (t === null || role.value !== 'controller' || !conn.isOpen) return
  // ★ 「值未变则不发」不是优化，是正确性：
  //   拖窗口时每个像素都会触发 ResizeObserver，不判断的话会产生
  //   resize 风暴 —— 每次 resize 在 ConPTY 里都是一次完整重排，
  //   而 Windows 上 resize 不是事件、要靠轮询发现，中间尺寸会被整个跳过。
  if (t.cols === sentCols && t.rows === sentRows) return
  sentCols = t.cols
  sentRows = t.rows
  void conn.request(
    MessageType.SessionResize,
    { session_id: sessionId.value, cols: t.cols, rows: t.rows },
    sessionId.value,
  ).catch(() => {})
}

function onFrame(frame: DecodedFrame): void {
  if (frame.streamId !== sessionId.value || !terminalReady.value || !conn.isOpen) return
  const t = term
  if (t === null) return

  switch (frame.type) {
    case FrameType.Buffer:
    case FrameType.Stdout:
      // ★ 直接写字节，不解码、不转字符串。见文件头第 1 条纪律。
      if (frame.type === FrameType.Buffer && (frame.flags & FrameFlag.BufferEnd) !== 0) {
        const epoch = attachmentEpoch
        t.write(frame.payload, () => { if (epoch === attachmentEpoch) replaying.value = false })
      } else {
        t.write(frame.payload)
      }
      break
    default:
      // Stdin / FileData 不该出现在这条路径上。忽略而不是断连 ——
      // 服务端已经在协议层挡住了，这里再断一次只会把服务端 bug
      // 表现成"用户莫名其妙掉线"。
      return
  }

  if (frame.type === FrameType.Stdout) {
    // 实时输出：每个字节都会追加到 Agent 的 ring buffer，所以 seq 按长度前进
    lastSeq += frame.payload.length
  }

  if ((frame.flags & FrameFlag.Dropped) !== 0) {
    // 服务端明确告知有字节被丢弃 —— 只能全量重放，增量补不回来。
    replaying.value = true
    lastSeq = 0
    void attach(0, true)
    return
  }

  // BufferEnd clears the overlay in xterm's write callback, after rendering.
}

function onControl(env: Envelope): void {
  const p = env.payload as Record<string, unknown> | undefined
  const sid = p?.['session_id']
  if (typeof sid !== 'string' || sid !== sessionId.value) return

  switch (env.type) {
    case MessageType.SessionExit: {
      const e = env.payload as SessionExitPayload
      exitInfo.value = e
      if (summary.value) summary.value = { ...summary.value, status: 'exited' }
      sessions.patch(sessionId.value, {
        status: 'exited',
        ...(typeof e.exit_code === 'number' ? { exit_code: e.exit_code } : {}),
      })
      break
    }
    case MessageType.SessionClosed:
      exitInfo.value = { session_id: sessionId.value, exit_code: -1, reason: 'terminated' }
      if (summary.value) summary.value = { ...summary.value, status: 'terminated' }
      sessions.patch(sessionId.value, { status: 'terminated' })
      break
    case MessageType.SessionDetached:
      if (summary.value) summary.value = { ...summary.value, status: 'detached' }
      sessions.patch(sessionId.value, { status: 'detached' })
      break
    case MessageType.SessionRoleChanged: {
      const nextRole = p?.['role']
      const cols = p?.['cols'], rows = p?.['rows']
      if (nextRole === 'controller' || nextRole === 'viewer') {
        const changed = role.value !== nextRole
        role.value = nextRole
        if (typeof cols === 'number' && cols > 0) remoteCols.value = cols
        if (typeof rows === 'number' && rows > 0) remoteRows.value = rows
        if (changed) { sentCols = 0; sentRows = 0 }
        const epoch = attachmentEpoch
        // Drain earlier bytes before changing their coordinate grid; later writes
        // stay behind this callback in xterm's parser queue.
        term?.write(new Uint8Array(), () => {
          if (epoch !== attachmentEpoch || !term) return
          try {
            if (nextRole === 'viewer' || !attachmentReady.value) term.resize(typeof cols === 'number' && cols > 0 ? cols : remoteCols.value, typeof rows === 'number' && rows > 0 ? rows : remoteRows.value)
            else { fit?.fit(); sendResize() }
          } catch { /* renderer was disposed during reconnect */ }
        })
      }
      break
    }
    default:
      break
  }
}

// ---------------------------------------------------------------------------
// attach
// ---------------------------------------------------------------------------

async function attach(since: number, queueFullReplay = false): Promise<void> {
  if (!terminalReady.value || !conn.isOpen) return
  if (attaching.value) {
    if (queueFullReplay) fullReplayQueued = true
    return
  }
  const epoch = attachmentEpoch
  attaching.value = true
  attachmentReady.value = false
  role.value = 'viewer'
  replaying.value = true
  errorText.value = null
  try {
    const env = await conn.request<SessionAttachedPayload>(
      MessageType.SessionAttach,
      {
        session_id: sessionId.value,
        since,
        cols: term?.cols ?? 80,
        rows: term?.rows ?? 24,
      },
      sessionId.value,
    )
    // A closed socket or an unmounted page must not finish an old recovery
    // attempt over a newer connection's state.
    if (epoch !== attachmentEpoch || !terminalReady.value || !conn.isOpen) return
    const p = env.payload
    if (p === undefined) {
      throw new Error('服务端未返回会话信息')
    }

    summary.value = p.session
    const draft = sessions.takeDraft(p.session.session_id)
    if (draft && !mobileDraft.value) mobileDraft.value = draft
    void refreshIdentity()
    role.value = p.role
    remoteCols.value = p.session.cols || 80
    remoteRows.value = p.session.rows || 24
    lastSeq = p.seq_to
    // xterm parses writes asynchronously. Flush replay at the native grid before fitting.
    await new Promise<void>(resolve => { if (term) term.write(new Uint8Array(), resolve); else resolve() })
    if (epoch !== attachmentEpoch || !terminalReady.value || !conn.isOpen) return
    attachmentReady.value = true
    applyTerminalSize()
    void sessions.load(p.session.device_id.replace(/-/g, ''))

    // 落后太多、中间有丢帧：必须清屏后按 seq_from 重放，否则屏幕上会
    // 拼出错误的画面（新旧内容交错，光标位置也不对）。
    // 清屏本身由 Agent 的六步重放里的 reset/clear 完成，这里只需要记账。
    replayWarning.value = p.seq_from > since && since > 0
    // Reconcile the fitted terminal size on every successful attachment.
    sentCols = 0
    sentRows = 0
    sendResize()
  } catch (e) {
    if (epoch === attachmentEpoch && terminalReady.value && conn.isOpen) {
      replaying.value = false
      errorText.value = humanizeError(e)
    }
  } finally {
    if (epoch === attachmentEpoch) {
      attaching.value = false
      if (fullReplayQueued && terminalReady.value && conn.isOpen) {
        fullReplayQueued = false
        void attach(0)
      }
    }
  }
}

// ---------------------------------------------------------------------------
// 动作
// ---------------------------------------------------------------------------

/** Ctrl+C：只有 tui 类命令可用（§6.5）。 */
function fillTaskTemplate(event: Event): void {
  const select = event.target as HTMLSelectElement
  const task = TASK_TEMPLATES.find(item => item.id === select.value)
  if (task) {
    if (mobileDraft.value.trim()) { errorText.value = '输入框已有文字，请先发送或清空后选择模板。' }
    else { mobileDraft.value = task.prompt; void nextTick(() => composer.value?.focus()) }
  }
  select.value = ''
}

function sendInterrupt(): void {
  if (windowsShell.value || role.value !== 'controller' || !isLive.value) return
  void conn.request(
    MessageType.SessionSignal,
    { session_id: sessionId.value, signal: 'int' },
    sessionId.value,
  ).catch((e) => { errorText.value = humanizeError(e) })
}

/** 「终止会话」：shell 类命令的唯一可靠停止方式。 */
async function terminate(): Promise<void> {
  exiting.value = true
  errorText.value = null
  try {
    await sessions.close(sessionId.value)
	// Successful request replies are consumed by the request promise, not by
	// the unsolicited-event listener. Reflect that acknowledgement explicitly.
	exitInfo.value = { session_id: sessionId.value, exit_code: -1, reason: 'terminated' }
	if (summary.value) summary.value = { ...summary.value, status: 'terminated' }
    confirmTerminate.value = false
  } catch (e) {
    errorText.value = humanizeError(e)
  } finally {
    exiting.value = false
  }
}

function goBack(): void {
  const dev = summary.value?.device_id.replace(/-/g, '')
  if (dev !== undefined && dev !== '') {
    void router.push({ name: 'device', params: { id: dev } })
  } else {
    void router.push({ name: 'devices' })
  }
}

// Cold refresh has no reconnectCount increment. Wait for BOTH the first
// socket and the terminal subscriptions, regardless of which is ready first.
// The same path restores this exact session on subsequent connections.
watch([() => conn.isOpen, terminalReady], ([open, ready]) => {
  attachmentEpoch++
  fullReplayQueued = false
  attaching.value = false
  attachmentReady.value = false
  role.value = 'viewer'
  replaying.value = false
  if (open && ready) void attach(lastSeq)
}, { flush: 'sync' })

// ---------------------------------------------------------------------------
// 搜索
// ---------------------------------------------------------------------------

function openSearch(): void {
  searchOpen.value = true
  void nextTick(() => searchInput.value?.focus())
}

function closeSearch(): void {
  searchOpen.value = false
  searchTerm.value = ''
  search?.clearDecorations()
  // 把焦点还给终端，否则用户关掉搜索框之后按键没有反应
  term?.focus()
}

function findNext(): void {
  if (searchTerm.value !== '') search?.findNext(searchTerm.value)
}

function findPrev(): void {
  if (searchTerm.value !== '') search?.findPrevious(searchTerm.value)
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

onMounted(async () => {
  syncViewport()
  window.visualViewport?.addEventListener('resize', syncViewport)
  window.addEventListener('resize', syncViewport)
  await nextTick()
  await devices.load()
  await preferences.load(true)

  const host = hostEl.value
  if (host === null) return

  const t = new Terminal({
    // 规格要求的极简 dark theme，与 styles.css 的变量保持同一族色
    theme: {
      background: '#000000',
      foreground: '#e6e9ef',
      cursor: '#4c8dff',
      cursorAccent: '#000000',
      selectionBackground: '#2b3b57',
    },
    fontFamily:
      'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
    fontSize: preferences.fontSize,
    lineHeight: 1,
    cursorBlink: true,
    // 手机上软键盘需要它才能在输入时弹出
    allowTransparency: false,
    scrollback: 5000,
    // OpenCode/OpenTUI queries CSI 14 t for pixel dimensions before its first frame.
    // xterm disables window reports by default; without this reply OpenCode stays blank.
    windowOptions: { getWinSizePixels: true },
    // ★ Unicode11Addon 需要这个开关（它用的是 proposed API）
    allowProposedApi: true,
    // 关掉 xterm 自己的右键菜单，远程场景里它只会碍事
    rightClickSelectsWord: false,
  })
  term = t

  const f = new FitAddon()
  fit = f
  t.loadAddon(f)
  t.loadAddon(new WebLinksAddon())
  const s = new SearchAddon()
  search = s
  t.loadAddon(s)

  const u11 = new Unicode11Addon()
  t.loadAddon(u11)
  t.unicode.activeVersion = '11'

  t.open(host)
  // Draw TUI box and block characters as continuous cell-sized glyphs.
  // Keep the DOM renderer when WebGL is unavailable or its context is lost.
  const webgl = new WebglAddon()
  webgl.onContextLoss(() => webgl.dispose())
  try {
    t.loadAddon(webgl)
  } catch {
    webgl.dispose()
  }

  // 先 fit 一次再 attach：这样 attach 时带上的 cols/rows 就是真实尺寸，
  // 而不是 80×24 —— 否则 TUI 会先按 80 列画一遍再被 resize 打断重画。
  try {
    f.fit()
  } catch {
    // 容器尺寸为 0（比如刚挂载还没布局）时会抛，忽略即可，
    // ResizeObserver 随后会补一次。
  }
  sentCols = t.cols
  sentRows = t.rows

  // ---- 输入 ----
  t.onData((data) => {
    // Windows ConPTY treats Ctrl+C in a cooked shell as EOF, which can close
    // the shell. Suppress it there; on Unix and in TUIs, keep native input.
    if (windowsShell.value && data.includes('\x03')) {
      errorText.value = 'Windows shell 无法安全接收 Ctrl+C；请在 CLI 内使用自己的停止命令。会话仍在运行。'
      return
    }
    if (role.value === 'controller' && isLive.value) sendText(data)
  })
  // 8-bit 输入（鼠标协议、某些终端的粘贴）走这条
  t.onBinary((data) => {
    if (role.value !== 'controller' || !isLive.value) return
    const bytes = new Uint8Array(data.length)
    for (let i = 0; i < data.length; i++) {
      bytes[i] = data.charCodeAt(i) & 0xff
    }
    sendBytes(bytes)
  })

  // ---- 浏览器级快捷键 ----
  //
  // ★ 只拦这三个，其余一律放行给 xterm.js 的默认处理。
  //   默认处理已经是正确的（Ctrl+C 发 0x03、Tab 发 0x09、方向键发 CSI 序列），
  //   自己重写一遍只会写错。
  t.attachCustomKeyEventHandler((ev: KeyboardEvent) => {
    if (ev.type !== 'keydown') return true
    if (!ev.ctrlKey || !ev.shiftKey) return true

    switch (ev.key.toLowerCase()) {
      case 'v':
        // 粘贴：读剪贴板后按普通输入送进去，让 PTY 看到的就是一次粘贴
        void readClipboard()
        return false
      case 'c':
        void copyOutput()
        return false
      case 'f':
        // 搜索走 SearchAddon（规格 §5.3 指定的三个浏览器级快捷键之一）。
        // 不用浏览器原生查找：它找不到 xterm 画在 canvas 里的文字。
        if (searchOpen.value) closeSearch()
        else openSearch()
        return false
      default:
        return true
    }
  })

  // ---- 订阅 ----
  unsubFrame = conn.onFrame(onFrame)
  unsubControl = conn.onControl(onControl)

  // ---- resize ----
  ro = new ResizeObserver(scheduleFit)
  ro.observe(host)
  terminalReady.value = true
})

onUnmounted(() => {
  cancelTerminalTouch()
  terminalReady.value = false
  attachmentEpoch++
  window.visualViewport?.removeEventListener('resize', syncViewport)
  window.removeEventListener('resize', syncViewport)
  // 先退订，再销毁 —— 反过来的话，dispose 过程中触发的帧会打到已销毁的实例上
  unsubFrame?.()
  unsubControl?.()
  unsubFrame = null
  unsubControl = null

  if (resizeTimer !== null) {
    clearTimeout(resizeTimer)
    resizeTimer = null
  }

  // ★ 第 3 条纪律：两个都要放掉。少一个就会泄漏。
  ro?.disconnect()
  ro = null

  // 主动 detach：让服务端把会话标成 detached（PTY 继续跑，I2 不变量）
  if (conn.isOpen) {
    void conn.request(MessageType.SessionDetach, { session_id: sessionId.value, reason: 'client_close' }, sessionId.value).catch(() => {})
  }

  term?.dispose()
  term = null
  fit = null
  search = null
})
</script>

<template>
  <div class="term" :style="{ height: termHeight }">
    <!-- ---- 顶栏 ---- -->
    <div class="term__bar">
      <button class="btn btn--ghost btn--sm" type="button" @click="goBack">‹ 会话</button>
      <button class="btn btn--ghost btn--sm term__panel-button" type="button" @click="showSessionPanel = !showSessionPanel">☰ 切换</button>
      <button class="btn btn--sm" type="button" @click="openFiles">文件</button>

      <button class="btn btn--sm" type="button" @click="router.push({ name: 'session-git', params: { id: sessionId } })">Git</button>
      <span class="term__title" :title="summary?.name ?? sessionId">
        {{ summary?.name || summary?.command || sessionId }}
      </span>

      <span v-if="summary !== null" class="badge" :class="`badge--${statusKind(summary.status)}`">
        {{ statusLabel(summary.status) }}
      </span>
      <button v-if="attachmentReady && role === 'viewer'" class="btn btn--sm" type="button" :disabled="claiming || !conn.isOpen" @click="claimControl">{{ claiming ? '接管中…' : '只读 · 接管' }}</button>
      <span v-if="!conn.isOpen" class="badge badge--warn">连接中断 · 会话保留</span>
    </div>

    <details class="term__identity" :open="workspaceOpen" @toggle="workspaceOpen = ($event.target as HTMLDetailsElement).open">
      <summary aria-label="会话信息">
        <span class="term__identity-arrow" aria-hidden="true">▸</span>
        <span class="term__identity-label">会话信息 · {{ sessionTool }}</span>
        <span class="dim">{{ workspaceOpen ? '收起' : '展开' }}</span>
      </summary>
      <div class="term__identity-content">
        <dl>
          <dt>设备</dt><dd>{{ devices.byId(sessionDevice)?.name || '设备' }}</dd>
          <dt>命令</dt><dd class="mono">{{ summary?.command || '工具' }}</dd>
          <dt>分支</dt><dd>{{ workspaceBranch || '分支待确认' }} · {{ isolatedWorkspace ? '独立工作区' : '原目录' }}</dd>
          <dt>目录</dt><dd class="mono">{{ summary?.cwd }}</dd>
        </dl>
        <div class="row">
          <button class="btn btn--ghost btn--sm" type="button" @click="refreshIdentity">刷新分支</button>
          <RouterLink v-if="summary" class="btn btn--ghost btn--sm" :to="{ name: 'workspaces', params: { id: sessionDevice }, query: { path: summary.cwd } }">管理独立工作区</RouterLink>
        </div>
        <p v-if="workspaceError" class="small dim">{{ workspaceError }}</p>
      </div>
    </details>
    <!-- ---- 终端 ---- -->
    <div class="term__workspace">
      <aside class="term__sidebar" :class="{ 'term__sidebar--open': showSessionPanel }" aria-label="会话工作区">
        <div class="term__sidebar-heading">工作区</div>
        <button class="btn btn--primary" type="button" @click="goBack">＋ 新会话</button>
        <div class="term__recent">
        <div class="term__sidebar-heading">最近会话</div>
        <button v-for="item in siblingSessions" :key="item.session_id" class="term__session-link" :class="{ 'term__session-link--active': item.session_id === sessionId }" type="button" @click="router.push({ name: 'session', params: { id: item.session_id } })">
          <span>{{ item.name || item.command || '会话' }}</span>
          <small>{{ statusLabel(item.status) }}</small>
        </button>
        <button class="term__session-link" type="button" @click="openFiles">▣ 工作区文件</button>
        </div>
        <QuotaPanel v-if="summary" :session-id="sessionId" />
      </aside>
      <div ref="hostEl" class="term__host" :class="{ 'term__host--viewer': role === 'viewer' }" @pointerdown="onTerminalPointerDown" @touchstart.capture.passive="onTerminalTouchStart" @touchmove.capture="onTerminalTouchMove" @touchend.capture="onTerminalTouchEnd" @touchcancel="cancelTerminalTouch" @click.capture="onTerminalClick" @contextmenu.capture="onTerminalContextMenu">
      <div v-if="attaching" class="term__overlay" role="status">
        <span class="spinner" />
        <span class="small dim">正在接回原会话…</span>
      </div>

      <div v-else-if="replaying" class="term__overlay">
        <span class="spinner" />
        <span class="small dim">正在重放历史输出…</span>
      </div>

      <div v-else-if="!conn.isOpen" class="term__overlay">
        <span class="spinner" />
        <div class="small dim">{{ conn.healthText }}</div>
        <div class="small faint">恢复后会自动接着上次的位置继续，不需要刷新</div>
      </div>

      <div v-else-if="terminalReady && !attachmentReady" class="term__overlay">
        <span class="small dim">{{ errorText ? '暂未接回会话，请重试或返回设备查看状态' : '正在接回原会话…' }}</span>
        <button v-if="errorText" class="btn btn--sm" type="button" @click="attach(lastSeq)">重试连接</button>
        <button v-if="errorText" class="btn btn--ghost btn--sm" type="button" @click="goBack">返回设备</button>
      </div>

      <div v-else-if="exitInfo !== null" class="term__overlay">
        <div>
          {{ exitInfo.reason === 'terminated' ? '会话已终止' : '进程已退出' }}
          <template v-if="exitInfo.exit_code >= 0">（退出码 {{ exitInfo.exit_code }}）</template>
        </div>
        <div class="small faint">上面的输出是它留下的最后内容</div>
        <button class="btn btn--sm" type="button" @click="goBack">返回设备</button>
      </div>
      </div>
      <aside v-if="filesOpen" class="term__files" aria-label="工作区文件面板">
        <button class="btn btn--sm term__files-close" type="button" @click="filesOpen = false">关闭文件面板</button>
        <FilesView :id="sessionId" embedded />
      </aside>
    </div>

    <!-- ---- 底部工具条 ---- -->
    <div class="term__footer">
      <!-- 搜索栏（Ctrl+Shift+F） -->
      <div v-if="searchOpen" class="row" style="gap: 6px; margin-bottom: 6px">
        <input
          ref="searchInput"
          v-model="searchTerm"
          class="mono"
          type="text"
          placeholder="在输出中查找…"
          autocapitalize="none"
          autocorrect="off"
          spellcheck="false"
          @keydown.enter.exact.prevent="findNext"
          @keydown.shift.enter.prevent="findPrev"
          @keydown.esc.prevent="closeSearch"
        />
        <button class="btn btn--sm" type="button" :disabled="searchTerm === ''" @click="findPrev">↑</button>
        <button class="btn btn--sm" type="button" :disabled="searchTerm === ''" @click="findNext">↓</button>
        <button class="btn btn--ghost btn--sm" type="button" @click="closeSearch">关闭</button>
      </div>

      <div v-if="replayWarning" class="notice notice--warn small" style="margin-bottom: 6px">
        已接回原会话，但离线期间部分较早的输出已超过缓存范围；当前终端显示的是仍可恢复的内容。
      </div>
      <div v-if="errorText !== null" class="notice notice--err small" style="margin-bottom: 6px">
        {{ errorText }}
      </div>

      <!--
        Ctrl+C 的前端兜底（架构文档 §6.5）。

        Windows 自带的 ConPTY **无法**把中断信号送达 cooked mode 的子进程
        （cmd / PowerShell）—— 写 0x03 会被 conhost 拦成 stdin EOF，
        命令不会中断。三条后端方案都实测失败，唯一可靠的路径是随包分发
        conpty.dll，那不在 MVP 范围内。

        所以这里的责任是**如实告知 + 给替代动作**：
        绝不呈现一个点了没反应的按钮 —— 那比没有按钮更糟。
      -->
      <div v-if="windowsShell" class="notice notice--warn small" style="margin-bottom: 6px">
        Windows shell 的 Ctrl+C 会被 ConPTY 当成 EOF，因此已屏蔽该按键以保护会话。需要结束整个会话时，请点击「关闭会话」。
      </div>

      <div v-if="confirmTerminate" class="notice notice--warn small" role="alert" style="margin-bottom: 6px">
        关闭「{{ summary?.name || summary?.command || sessionId }}」会结束远端进程；返回设备或关闭网页只会断开显示，会话会继续运行。
        <div class="row" style="margin-top: 8px">
          <button class="btn btn--danger btn--sm" type="button" :disabled="exiting || !conn.isOpen" @click="terminate">
            <span v-if="exiting" class="spinner" />确定结束进程
          </button>
          <button class="btn btn--sm" type="button" :disabled="exiting" @click="confirmTerminate = false">取消</button>
        </div>
      </div>
      <div v-if="role === 'controller' && isLive && !isShell" class="row task-templates">
        <select aria-label="任务模板" :disabled="!conn.isOpen" value="" @change="fillTaskTemplate">
          <option value="">任务模板…</option><option v-for="task in TASK_TEMPLATES" :key="task.id" :value="task.id">{{ task.label }}</option>
        </select><span class="small dim">仅填入输入框，确认后发送</span>
      </div>
      <SessionAttachments v-if="summary" v-show="attachmentsOpen" :key="sessionId" :session-id="sessionId" :enabled="role === 'controller' && isLive && conn.isOpen" :native-images="nativeImages" @close="attachmentsOpen = false" @reference="referenceAttachments" @native="pasteNativeAttachment" />
      <div class="term__composer" v-if="role === 'controller' && isLive">
        <button class="btn btn--sm term__attach-button" type="button" aria-label="添加图片或文件" :aria-expanded="attachmentsOpen" :disabled="!conn.isOpen" @click="attachmentsOpen = !attachmentsOpen">附件</button>
        <textarea ref="composer" v-model="mobileDraft" rows="1" aria-label="输入终端文字" placeholder="输入命令或文字，点发送…" :inputmode="shortcutInput ? 'none' : 'text'" autocapitalize="none" autocorrect="off" spellcheck="false" @pointerdown="enableSoftKeyboard" @keydown="onComposerKeydown" />
        <button class="btn btn--primary" type="button" :disabled="!mobileDraft || !conn.isOpen" @click="submitDraft()">发送 ↵</button>
      </div>
      <div v-if="clipboardStatus" class="small" role="status" style="margin-bottom: 6px">{{ clipboardStatus }}</div>
      <div v-if="preferences.error" class="notice notice--err small">偏好同步失败：{{ preferences.error }}</div>
      <div v-if="copyFallback" class="stack clipboard-panel"><textarea :value="copyFallback" readonly rows="4" aria-label="手动复制终端输出" /><button class="btn btn--sm" type="button" @click="copyFallback = ''">关闭</button></div>
      <div v-if="pasteOpen" class="stack clipboard-panel">
        <label for="paste-preview">确认要粘贴的文字</label>
        <textarea id="paste-preview" v-model="pasteDraft" rows="4" autocapitalize="none" autocorrect="off" spellcheck="false" />
        <p class="small dim">粘贴后不会额外发送回车；文本中的换行可能让 Shell 执行多条命令。</p>
        <div class="row"><button class="btn btn--primary btn--sm" type="button" :disabled="!pasteDraft || role !== 'controller' || !conn.isOpen || !isLive" @click="pasteToTerminal">粘贴到终端</button><button class="btn btn--sm" type="button" @click="pasteOpen = false">取消</button></div>
      </div>
      <div class="row term__quick-keys" role="group" aria-label="终端常用按键" @pointerdown="prepareShortcutInput" @click.capture="prepareShortcutInput">
        <button class="btn btn--sm" type="button" :disabled="role !== 'controller' || !conn.isOpen || !isLive" title="发送 Esc（返回或取消）" @pointerdown.prevent @click="sendText('\x1b')">Esc</button>
        <button class="btn btn--sm" type="button" :disabled="role !== 'controller' || !conn.isOpen || !isLive" title="发送 Tab（切换或补全）" @pointerdown.prevent @click="sendText('\t')">Tab</button>
        <button class="btn btn--sm" type="button" :disabled="role !== 'controller' || !conn.isOpen || !isLive" aria-label="方向键向上" title="发送方向键 ↑" @pointerdown.prevent @click="sendText('\x1b[A')">↑</button>
        <button class="btn btn--sm" type="button" :disabled="role !== 'controller' || !conn.isOpen || !isLive" aria-label="方向键向下" title="发送方向键 ↓" @pointerdown.prevent @click="sendText('\x1b[B')">↓</button>
        <button class="btn btn--sm" type="button" :disabled="role !== 'controller' || !conn.isOpen || !isLive" title="发送回车（确认选择）" @pointerdown.prevent @click="sendText('\r')">Enter</button>
      </div>
      <div class="row term__tools term__actions" style="gap: 6px; flex-wrap: wrap">
        <button class="btn btn--sm" type="button" @click="focusTerminal">⌨ 键盘</button>
        <button class="btn btn--sm" type="button" :disabled="!terminalReady" @click="openTextReader">选字</button>
        <button class="btn btn--sm" type="button" :disabled="role !== 'controller' || !conn.isOpen || !isLive" @click="readClipboard">粘贴</button>
        <button class="btn btn--sm" type="button" :disabled="windowsShell || role !== 'controller' || !conn.isOpen || !isLive" :title="windowsShell ? 'Windows shell 不支持安全中断' : '发送 Ctrl+C，不关闭会话'" @pointerdown.prevent="prepareShortcutInput" @click="prepareShortcutInput($event); sendInterrupt()">Ctrl+C</button>
        <button class="btn btn--sm" type="button" :aria-expanded="showExtraKeys" aria-controls="terminal-extra-keys" @click="showExtraKeys = !showExtraKeys">{{ showExtraKeys ? '收起按键' : '更多按键' }}</button>
      </div>
      <div v-if="showExtraKeys" id="terminal-extra-keys" class="row term__tools term__extra-keys" role="group" aria-label="终端辅助按键" style="gap: 6px; flex-wrap: wrap">
        <button class="btn btn--sm" type="button" @click="scrollHistory(-12)">向上翻</button>
        <button class="btn btn--sm" type="button" @click="scrollHistory(12)">向下翻</button>
        <button class="btn btn--sm" type="button" @click="term?.scrollToBottom()">回到底部</button>
        <button class="btn btn--sm" type="button" @click="copyOutput()">复制选中</button>
        <button class="btn btn--sm" type="button" @click="copyOutput(true)">复制画面</button>
        <label class="row small">字号<select :value="preferences.fontSize" :disabled="preferences.saving" aria-label="终端字号（随账号同步）" @change="setFontSize"><option v-for="size in [10, 12, 14, 16, 18, 20, 22, 24]" :key="size" :value="size">{{ size }}</option></select></label>
        <button class="btn btn--sm" type="button" :disabled="role !== 'controller' || !conn.isOpen || !isLive" @click="redraw">重绘</button>
        <!-- 手机上没有 Ctrl+Shift+F，所以搜索也必须有个可点的入口 -->
        <button class="btn btn--sm" type="button" title="查找" @click="openSearch">查找</button>
        <button class="btn btn--sm btn--danger" type="button" :disabled="exiting || !conn.isOpen || !isLive" title="关闭这个会话并结束进程" @click="confirmTerminate = true">关闭会话</button>
      </div>
    </div>
    <TerminalTextReader v-if="textSnapshot" :snapshot="textSnapshot" @close="textSnapshot = null" />
  </div>
</template>

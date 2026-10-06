<script setup lang="ts">
import { nextTick, onMounted, onUnmounted, ref } from 'vue'
import type { TerminalTextSnapshot } from '../lib/terminalText'

const props = defineProps<{ snapshot: TerminalTextSnapshot }>()
const emit = defineEmits<{ close: [] }>()
const dialog = ref<HTMLElement | null>(null)
const body = ref<HTMLElement | null>(null)
const textEl = ref<HTMLElement | null>(null)
const selectedText = ref('')
const status = ref('')
const manualText = ref('')
let previousFocus: HTMLElement | null = null
let holdTimer: ReturnType<typeof setTimeout> | null = null
let touchSelection: { id: number; x: number; y: number; start: number; end: number; selecting: boolean; original: string } | null = null

function cancelTouchSelection(): void {
  if (holdTimer !== null) clearTimeout(holdTimer)
  holdTimer = null
  touchSelection = null
}
function textOffset(x: number, y: number): number | null {
  const position = document.caretPositionFromPoint?.(x, y)
  const range = position ? null : document.caretRangeFromPoint?.(x, y)
  const node = position?.offsetNode ?? range?.startContainer
  return node === textEl.value?.firstChild ? position?.offset ?? range?.startOffset ?? null : null
}
function selectRange(start: number, end: number): void {
  const node = textEl.value?.firstChild
  if (!node) return
  const range = document.createRange()
  range.setStart(node, Math.min(start, end))
  range.setEnd(node, Math.max(start, end))
  const selection = window.getSelection()
  selection?.removeAllRanges()
  selection?.addRange(range)
  selectionChanged()
}
function touchStart(event: TouchEvent): void {
  cancelTouchSelection()
  if (event.touches.length !== 1) return
  const touch = event.touches[0]!
  const offset = textOffset(touch.clientX, touch.clientY)
  if (offset === null) return
  const gesture = { id: touch.identifier, x: touch.clientX, y: touch.clientY, start: offset, end: offset, selecting: false, original: window.getSelection()?.toString() ?? '' }
  touchSelection = gesture
  holdTimer = setTimeout(() => {
    holdTimer = null
    // Keep native mobile selection when the browser has already made it.
    // Otherwise supply long-press/drag selection using actual DOM text ranges.
    if (selectedText.value && selectedText.value !== gesture.original) return
    const segment = typeof Intl.Segmenter === 'function'
      ? new Intl.Segmenter(undefined, { granularity: 'word' }).segment(props.snapshot.text).containing(offset)
      : undefined
    gesture.start = segment?.index ?? offset
    gesture.end = Math.min(props.snapshot.text.length, segment ? segment.index + segment.segment.length : offset + (props.snapshot.text.codePointAt(offset)! > 0xffff ? 2 : 1))
    gesture.selecting = true
    selectRange(gesture.start, gesture.end)
  }, 550)
}
function touchMove(event: TouchEvent): void {
  const gesture = touchSelection
  if (!gesture) return
  if (event.touches.length !== 1) { cancelTouchSelection(); return }
  const touch = Array.from(event.touches).find(t => t.identifier === gesture.id)
  if (!touch) return
  if (!gesture.selecting) {
    if (Math.hypot(touch.clientX - gesture.x, touch.clientY - gesture.y) >= 8) cancelTouchSelection()
    return
  }
  if (event.cancelable) event.preventDefault()
  const offset = textOffset(touch.clientX, touch.clientY)
  if (offset !== null) selectRange(offset < gesture.start ? gesture.end : gesture.start, offset)
}
function touchEnd(event: TouchEvent): void {
  if (touchSelection?.selecting && event.cancelable) event.preventDefault()
  cancelTouchSelection()
}

function selectionChanged(): void {
  const selection = window.getSelection()
  const text = textEl.value
  selectedText.value = selection && text && !selection.isCollapsed
    && text.contains(selection.anchorNode) && text.contains(selection.focusNode)
    ? selection.toString() : ''
}
async function copySelected(): Promise<void> {
  const text = selectedText.value
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    status.value = '已复制选中的文字'
    manualText.value = ''
  } catch {
    manualText.value = text
    status.value = '浏览器未允许自动复制，请在下方长按选字，使用系统“复制”。'
  }
}
function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); emit('close'); return }
  if (event.key !== 'Tab' || !dialog.value) return
  const controls = Array.from(dialog.value.querySelectorAll<HTMLElement>('button:not(:disabled), textarea'))
  const first = controls[0]
  const last = controls.at(-1)
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus() }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus() }
}
onMounted(async () => {
  previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
  document.addEventListener('selectionchange', selectionChanged)
  await nextTick()
  dialog.value?.querySelector<HTMLButtonElement>('[data-close]')?.focus({ preventScroll: true })
  const node = textEl.value?.firstChild
  if (node && body.value && props.snapshot.text.length) {
    const range = document.createRange()
    const offset = Math.min(props.snapshot.viewportOffset, props.snapshot.text.length - 1)
    range.setStart(node, offset)
    range.setEnd(node, offset + 1)
    body.value.scrollTop += range.getBoundingClientRect().top - body.value.getBoundingClientRect().top
  }
})
onUnmounted(() => {
  cancelTouchSelection()
  document.removeEventListener('selectionchange', selectionChanged)
  // Never restore focus to an editable terminal/composer and reopen the IME.
  if (previousFocus?.isConnected && !previousFocus.matches('input, textarea, [contenteditable="true"]')) previousFocus.focus({ preventScroll: true })
})
</script>

<template>
  <Teleport to="body">
    <section ref="dialog" class="terminal-reader" role="dialog" aria-modal="true" aria-labelledby="terminal-reader-title" @keydown="onKeydown">
      <div class="terminal-reader__header">
        <strong id="terminal-reader-title">选字复制</strong>
        <button class="btn btn--primary btn--sm" type="button" :disabled="!selectedText" @pointerdown.prevent @click="copySelected">复制选中</button>
        <button class="btn btn--sm" type="button" data-close @click="emit('close')">返回终端</button>
      </div>
      <p class="terminal-reader__hint">长按选字，按住后拖动可选择一段文字。这里保留打开时的文字，终端仍在运行。</p>
      <p v-if="status" class="terminal-reader__hint" role="status">{{ status }}</p>
      <textarea v-if="manualText" class="terminal-reader__manual" :value="manualText" readonly inputmode="none" rows="3" aria-label="手动复制选中文字" />
      <div ref="body" class="terminal-reader__body">
        <pre ref="textEl" class="terminal-reader__text" aria-label="可选取的终端文字" @touchstart.passive="touchStart" @touchmove="touchMove" @touchend="touchEnd" @touchcancel="cancelTouchSelection">{{ snapshot.text }}</pre>
      </div>
    </section>
  </Teleport>
</template>

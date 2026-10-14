<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref } from 'vue'
defineProps<{ titleId: string; busy?: boolean }>()
const emit = defineEmits<{ close: [] }>()
const dialog = ref<HTMLDialogElement | null>(null)
const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null
function keepFocus(event: KeyboardEvent): void {
  if (event.key !== 'Tab' || !dialog.value) return
  const items = [...dialog.value.querySelectorAll<HTMLElement>('a[href], button, input, select, textarea, [tabindex]')]
    .filter(el => el.tabIndex >= 0 && !el.matches(':disabled, [hidden], input[type="hidden"]') && el.getClientRects().length > 0)
  const first = items[0], last = items.at(-1)
  if (!first || !last) { event.preventDefault(); dialog.value.focus(); return }
  if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus() }
  else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus() }
}
onMounted(() => dialog.value?.showModal())
onBeforeUnmount(() => {
  dialog.value?.close()
  if (opener?.isConnected) opener.focus()
})
</script>
<template>
  <dialog ref="dialog" class="modal-dialog" tabindex="-1" :aria-labelledby="titleId" @keydown="keepFocus" @cancel.prevent="!busy && emit('close')"><slot /></dialog>
</template>
<style scoped>
.modal-dialog{color:var(--fg);background:var(--bg-elev);border:1px solid var(--border);border-radius:12px;padding:16px;width:min(600px,calc(100vw - 24px));max-height:calc(100dvh - 24px);overflow:auto;box-sizing:border-box}
.modal-dialog::backdrop{background:#0008}
</style>

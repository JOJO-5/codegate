<script setup lang="ts">
import { computed } from 'vue'
import type { NativeLaunchMode } from '../lib/protocol'
import { launchModeIssue } from '../lib/nativeLaunchModes'
const props = defineProps<{ id: string; modes?: NativeLaunchMode[]; note?: string; modelValue: string; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
const selected = computed(() => props.modes?.find(mode => mode.id === props.modelValue))
const issue = computed(() => launchModeIssue(props.modes, props.modelValue))
</script>
<template>
  <div class="field">
    <label :for="id">CLI 原生权限模式</label>
    <select :id="id" :value="issue ? 'unavailable' : modelValue" :disabled="disabled" :aria-describedby="`${id}-description`" :aria-invalid="!!issue" @change="emit('update:modelValue', ($event.target as HTMLSelectElement).value)">
      <option value="">继承 CLI 本机设置</option>
      <option v-if="issue" value="unavailable" disabled>原选择不可用，请重新选择</option>
      <option v-for="mode in modes" :key="mode.id" :value="mode.id">{{ mode.label }}</option>
    </select>
    <p :id="`${id}-description`" :class="selected?.full_access ? 'notice notice--warn' : 'field__hint'">{{ issue || selected?.description || '仅在本次新建终端时传入原生启动参数；之后仍可在 CLI 自己的 TUI 中切换。' }}</p>
    <details v-if="selected" class="small"><summary>对应的原生启动参数</summary><code class="mono native-launch-params">{{ selected.command_hint }}</code></details>
    <span v-if="note" class="field__hint">{{ note }}</span>
    <span v-else-if="!modes?.length" class="field__hint">Agent 未上报可预选的原生模式；可在 CLI 自身的 TUI 中选择。</span>
  </div>
</template>
<style scoped>
.native-launch-params{white-space:pre-wrap;overflow-wrap:anywhere}
</style>

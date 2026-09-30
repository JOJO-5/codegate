import { computed, ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { api, type ProjectPreference } from '../lib/api'
import { humanizeError } from '../lib/format'
import { useAuthStore } from './auth'

export const usePreferencesStore = defineStore('preferences', () => {
  const auth = useAuthStore()
  const values = ref<Record<string, unknown>>({})
  const loading = ref(false)
  const saving = ref(false)
  const error = ref('')
  let loadedUser = ''
  let generation = 0
  let inflight: Promise<void> | null = null
  let writes: Promise<void> = Promise.resolve()
  const projects = computed(() => Object.entries(values.value).flatMap(([key, value]) => {
    if (!key.startsWith('project.') || !value || typeof value !== 'object') return []
    const p = value as ProjectPreference
    return typeof p.label === 'string' && typeof p.device_id === 'string' && typeof p.cwd === 'string' && typeof p.command_id === 'string'
      ? [{ ...p, key }] : []
  }))
  const fontSize = computed(() => typeof values.value.font_size === 'number' ? values.value.font_size : 14)
  function isPinned(id: string): boolean { return values.value[`pin.${id}`] === true }

  watch(() => auth.user?.id, () => {
    generation++
    values.value = {}
    loadedUser = ''
    inflight = null
    loading.value = false
    saving.value = false
    error.value = ''
  }, { flush: 'sync' })

  function load(force = false): Promise<void> {
    const user = auth.user?.id
    if (!user || saving.value || (!force && loadedUser === user)) return Promise.resolve()
    if (inflight) return inflight
    const current = generation
    loading.value = true
    error.value = ''
    const request = api.preferences().then(result => {
      if (current === generation) { values.value = result.values; loadedUser = user }
    }).catch(e => { if (current === generation) error.value = humanizeError(e) }).finally(() => {
      if (current === generation) { loading.value = false; inflight = null }
    })
    inflight = request
    return request
  }

  function change(key: string, value?: unknown): Promise<void> {
    const user = auth.user?.id
    const current = generation
    const action = writes.catch(() => {}).then(async () => {
      if (!user || auth.user?.id !== user || current !== generation) return
      if (inflight) await inflight
      if (current !== generation) return
      saving.value = true
      error.value = ''
      try {
        if (value === undefined) await api.deletePreference(key)
        else {
          const result = await api.setPreference(key, value)
          value = result.value
        }
        if (current !== generation) return
        const next = { ...values.value }
        if (value === undefined) delete next[key]
        else next[key] = value
        values.value = next
      } catch (e) {
        if (current === generation) error.value = humanizeError(e)
        throw e
      } finally { if (current === generation) saving.value = false }
    })
    writes = action
    return action
  }
  async function togglePin(id: string): Promise<void> {
    await change(`pin.${id}`, isPinned(id) ? undefined : true)
  }
  async function saveProject(project: ProjectPreference): Promise<void> {
    const existing = projects.value.find(p => p.device_id === project.device_id && p.command_id === project.command_id && p.cwd === project.cwd)
    await change(existing?.key ?? `project.${crypto.randomUUID()}`, project)
  }
  return { values, projects, fontSize, loading, saving, error, load, change, isPinned, togglePin, saveProject }
})

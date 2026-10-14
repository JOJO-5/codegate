import { ref, watch } from 'vue'
import { defineStore } from 'pinia'
import { useAuthStore } from './auth'

const prefix = 'codegate.drafts.v1.'

/** Unsent text stays in this tab; never transmit it or share it across accounts. */
export const useDraftsStore = defineStore('drafts', () => {
  const auth = useAuthStore()
  const owner = ref('')
  const items = ref<Record<string, string>>(Object.create(null))
  const storageUnavailable = ref(false)
  function storage(): Storage | null {
    try { return window.sessionStorage }
    catch { storageUnavailable.value = true; return null }
  }
  watch(() => auth.user?.id ?? '', id => {
    const previous = owner.value
    const target = storage()
    if (previous) {
      try { target?.removeItem(prefix + previous) }
      catch { storageUnavailable.value = true }
    }
    const loaded: Record<string, string> = Object.create(null)
    if (id && target) {
      try {
        const saved: unknown = JSON.parse(target.getItem(prefix + id) ?? '{}')
        if (saved && typeof saved === 'object' && !Array.isArray(saved)) {
          for (const [key, value] of Object.entries(saved)) if (typeof value === 'string') loaded[key] = value
        }
      } catch { storageUnavailable.value = true }
    }
    items.value = loaded
    owner.value = id
  }, { immediate: true, flush: 'sync' })
  function get(key: string): string { return items.value[key] ?? '' }
  function set(key: string, text: string): void {
    if (!owner.value) return
    if (text) items.value[key] = text
    else delete items.value[key]
    try {
      const target = storage()
      if (!target) return
      if (Object.keys(items.value).length) target.setItem(prefix + owner.value, JSON.stringify(items.value))
      else target.removeItem(prefix + owner.value)
    } catch { storageUnavailable.value = true }
  }
  return { owner, storageUnavailable, get, set }
})

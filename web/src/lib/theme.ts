import { computed, ref } from 'vue'

export type ThemePreference = 'system' | 'light' | 'dark'
const storageKey = 'codegate.theme'
const system = window.matchMedia('(prefers-color-scheme: dark)')
function readPreference(): ThemePreference {
  try {
    const saved = localStorage.getItem(storageKey)
    if (saved === 'light' || saved === 'dark') return saved
  } catch { /* Use the system theme when storage is unavailable. */ }
  return 'system'
}
const preference = ref<ThemePreference>(readPreference())
const systemDark = ref(system.matches)
const dark = computed(() => preference.value === 'dark' || (preference.value === 'system' && systemDark.value))
function apply(): void {
  document.documentElement.dataset.theme = dark.value ? 'dark' : 'light'
  document.documentElement.style.colorScheme = dark.value ? 'dark' : 'light'
}
export function setTheme(value: ThemePreference): void {
  preference.value = value
  try { localStorage.setItem(storageKey, value) } catch { /* Keep the choice for this tab. */ }
  apply()
}
system.addEventListener('change', event => { systemDark.value = event.matches; apply() })
window.addEventListener('storage', event => {
  if (event.key === storageKey || event.key === null) { preference.value = readPreference(); apply() }
})
apply()
export function useTheme() { return { preference, dark, setTheme, toggleTheme: () => setTheme(dark.value ? 'light' : 'dark') } }

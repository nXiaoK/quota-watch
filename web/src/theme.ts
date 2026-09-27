import { ref } from 'vue'

type Theme = 'light' | 'dark'
const storageKey = 'quota-watch-theme'
const systemTheme = window.matchMedia('(prefers-color-scheme: dark)')
let preference: Theme | null = null
try {
  const stored = localStorage.getItem(storageKey)
  if (stored === 'light' || stored === 'dark') preference = stored
} catch {}

export const theme = ref<Theme>(preference ?? (systemTheme.matches ? 'dark' : 'light'))

function applyTheme() {
  document.documentElement.dataset.theme = theme.value
  document.documentElement.style.colorScheme = theme.value
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', theme.value === 'light' ? '#f7f9f8' : '#101010')
}

export function setTheme(value: Theme) {
  preference = value
  theme.value = value
  try { localStorage.setItem(storageKey, value) } catch {}
  applyTheme()
}

systemTheme.addEventListener('change', (event) => {
  if (preference !== null) return
  theme.value = event.matches ? 'dark' : 'light'
  applyTheme()
})
applyTheme()

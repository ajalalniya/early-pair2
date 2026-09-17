import { defineStore } from 'pinia'
import { ref } from 'vue'

export interface Toast {
  id: number
  message: string
  tone: 'info' | 'warn' | 'danger' | 'accent'
}

export const useToasts = defineStore('toasts', () => {
  const items = ref<Toast[]>([])
  const seen = new Set<string>()
  let next = 1

  function push(message: string, tone: Toast['tone'] = 'info', ms = 5000) {
    const id = next++
    items.value = [...items.value, { id, message, tone }].slice(-4)
    window.setTimeout(() => dismiss(id), ms)
  }

  /** Show a message at most once per key for the life of the tab. */
  function once(key: string, message: string, tone: Toast['tone'] = 'info') {
    if (seen.has(key)) return
    seen.add(key)
    push(message, tone)
  }

  function dismiss(id: number) {
    items.value = items.value.filter((t) => t.id !== id)
  }

  return { items, push, once, dismiss }
})

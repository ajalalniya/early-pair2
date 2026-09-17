<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

const props = withDefaults(defineProps<{ title: string; subtitle?: string; width?: number; labelledBy?: string }>(), {
  width: 410,
})
const emit = defineEmits<{ close: [] }>()

const panel = ref<HTMLElement | null>(null)
let restoreFocus: HTMLElement | null = null

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') {
    e.stopPropagation()
    emit('close')
    return
  }
  if (e.key !== 'Tab' || !panel.value) return
  // Keep focus inside the dialog.
  const focusable = panel.value.querySelectorAll<HTMLElement>(
    'button:not([disabled]), [href], input:not([disabled]), select, [tabindex]:not([tabindex="-1"])',
  )
  if (!focusable.length) return
  const first = focusable[0]!
  const last = focusable[focusable.length - 1]!
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault()
    last.focus()
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault()
    first.focus()
  }
}

onMounted(async () => {
  restoreFocus = document.activeElement as HTMLElement | null
  document.body.style.overflow = 'hidden'
  await nextTick()
  panel.value?.querySelector<HTMLElement>('[data-autofocus], button:not(.close)')?.focus()
})
onBeforeUnmount(() => {
  document.body.style.overflow = ''
  restoreFocus?.focus?.()
})
void props
</script>

<template>
  <div class="overlay" @click.self="emit('close')" @keydown="onKey">
    <div ref="panel" class="panel" role="dialog" aria-modal="true" :aria-label="title" :style="{ width: width + 'px' }">
      <div class="head">
        <span>
          <span class="title">{{ title }}</span>
          <span v-if="subtitle" class="subtitle">{{ subtitle }}</span>
        </span>
        <button class="close" type="button" aria-label="Close" @click="emit('close')">✕</button>
      </div>
      <slot />
    </div>
  </div>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 90;
  background: oklch(0.1 0.01 265 / 0.72);
  backdrop-filter: blur(3px);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px 16px;
  overflow-y: auto;
}
.panel {
  max-width: 100%;
  max-height: calc(100vh - 48px);
  overflow-y: auto;
  border: 1px solid var(--border-strong);
  border-radius: 18px;
  background: var(--surface-raised);
}
.head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
  padding: 15px 18px;
  border-bottom: 1px solid var(--chip-2);
}
.title {
  display: block;
  font-size: 15px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.subtitle {
  display: block;
  font-size: 11.5px;
  font-family: var(--font-mono);
  color: var(--fg-muted);
  margin-top: 3px;
}
.close {
  border: none;
  background: transparent;
  color: oklch(0.68 0.01 265);
  font-size: 16px;
  line-height: 1;
  padding: 4px;
  border-radius: 6px;
}
.close:hover {
  color: var(--fg);
}
</style>

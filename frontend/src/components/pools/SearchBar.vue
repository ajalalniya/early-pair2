<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'

const model = defineModel<string>({ required: true })
defineProps<{ hint?: string | null }>()

const input = ref<HTMLInputElement | null>(null)

function clear() {
  model.value = ''
  input.value?.focus()
}

// "/" focuses the search from anywhere that is not already taking text.
function onGlobalKey(e: KeyboardEvent) {
  if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return
  const t = e.target as HTMLElement | null
  if (t && (t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName))) return
  e.preventDefault()
  input.value?.focus()
}
onMounted(() => window.addEventListener('keydown', onGlobalKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onGlobalKey))
</script>

<template>
  <div class="search-wrap">
    <div class="search" role="search">
      <svg class="icon" viewBox="0 0 16 16" width="14" height="14" aria-hidden="true">
        <circle cx="7" cy="7" r="4.75" fill="none" stroke="currentColor" stroke-width="1.5" />
        <path d="M10.5 10.5 14 14" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      </svg>
      <input
        ref="input"
        v-model="model"
        type="text"
        autocomplete="off"
        autocapitalize="off"
        spellcheck="false"
        placeholder="Search by pair address (0x…)"
        aria-label="Search pairs by address"
        data-testid="pair-search"
        @keydown.esc="clear"
      />
      <kbd v-if="!model" class="key" aria-hidden="true">/</kbd>
      <button v-else type="button" class="clear" aria-label="Clear search" @click="clear">×</button>
    </div>
    <p v-if="hint" class="hint" role="status">{{ hint }}</p>
  </div>
</template>

<style scoped>
.search-wrap {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.search {
  display: flex;
  align-items: center;
  gap: 10px;
  border: 1px solid var(--chip-2);
  border-radius: 12px;
  padding: 0 12px;
  background: var(--surface);
  transition: border-color 0.15s;
}
.search:focus-within {
  border-color: color-mix(in oklch, var(--accent) 60%, var(--chip-2));
}
.icon {
  flex: none;
  color: var(--fg-muted);
}
input {
  flex: 1;
  min-width: 0;
  height: 42px;
  border: none;
  outline: none;
  background: transparent;
  color: var(--fg);
  font-family: var(--font-mono);
  font-size: 13px;
}
input::placeholder {
  color: var(--fg-muted);
}
.key {
  flex: none;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--fg-muted);
  border: 1px solid var(--chip-2);
  border-radius: 5px;
  padding: 1px 6px;
}
.clear {
  flex: none;
  width: 24px;
  height: 24px;
  display: grid;
  place-items: center;
  padding: 0;
  border: none;
  border-radius: 6px;
  background: transparent;
  color: var(--fg-muted);
  font-size: 17px;
  line-height: 1;
  cursor: pointer;
}
.clear:hover {
  color: var(--fg);
  background: var(--chip);
}
.hint {
  margin: 0 4px;
  font-family: var(--font-mono);
  font-size: 11.5px;
  color: var(--fg-muted);
}
</style>

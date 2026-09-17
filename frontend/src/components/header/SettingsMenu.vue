<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { ACCENTS, useSettings } from '@/stores/settings'

const settings = useSettings()
const open = ref(false)
const root = ref<HTMLElement | null>(null)
const accentNames = ['Mint', 'Violet', 'Amber', 'Orchid']

function onDoc(e: MouseEvent) {
  if (root.value && !root.value.contains(e.target as Node)) open.value = false
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') open.value = false
}
watch(open, (v) => {
  const fn = v ? document.addEventListener : document.removeEventListener
  fn.call(document, 'mousedown', onDoc as EventListener)
  fn.call(document, 'keydown', onKey as EventListener)
})
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDoc)
  document.removeEventListener('keydown', onKey)
})
</script>

<template>
  <div ref="root" class="wrap">
    <button class="gear" type="button" aria-label="Settings" :aria-expanded="open" @click="open = !open">
      <svg width="15" height="15" viewBox="0 0 24 24" aria-hidden="true">
        <path
          fill="currentColor"
          d="M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7Zm8.9 4.9-.02-2.8-2.3-.6a7 7 0 0 0-.7-1.7l1.2-2.05-2-1.97-2.04 1.2a7 7 0 0 0-1.7-.7L12.7 2.5H9.9l-.6 2.28a7 7 0 0 0-1.7.7L5.55 4.3l-1.97 2 1.2 2.04a7 7 0 0 0-.7 1.7l-2.28.64v2.8l2.3.6c.16.6.4 1.17.7 1.7l-1.2 2.05 2 1.97 2.04-1.2c.53.3 1.1.54 1.7.7l.62 2.3h2.8l.6-2.3a7 7 0 0 0 1.7-.7l2.05 1.2 1.97-2-1.2-2.04c.3-.53.54-1.1.7-1.7l2.3-.62Z"
        />
      </svg>
    </button>
    <div v-if="open" class="menu" role="dialog" aria-label="Settings">
      <div class="section">
        <div class="caps">RISK GATE</div>
        <label class="toggle">
          <input v-model="settings.gateRiskyTrades" type="checkbox" />
          <span>Block buys and zaps into risky pools</span>
        </label>
        <div class="range-row" :class="{ dim: !settings.gateRiskyTrades }">
          <label for="gate-threshold" class="mono">Threshold</label>
          <span class="mono val">{{ settings.riskGateThreshold }}</span>
        </div>
        <input
          id="gate-threshold"
          type="range"
          min="10"
          max="100"
          step="5"
          :value="settings.riskGateThreshold"
          :disabled="!settings.gateRiskyTrades"
          @input="settings.setThreshold(Number(($event.target as HTMLInputElement).value))"
        />
        <p class="note">Selling and removing liquidity are never gated.</p>
      </div>
      <div class="section">
        <div class="caps">ACCENT</div>
        <div class="swatches" role="radiogroup" aria-label="Accent colour">
          <button
            v-for="(a, i) in ACCENTS"
            :key="a"
            type="button"
            role="radio"
            class="swatch"
            :aria-checked="settings.accent === a"
            :aria-label="accentNames[i]"
            :style="{ background: a }"
            @click="settings.accent = a"
          />
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.wrap {
  position: relative;
}
.gear {
  width: 32px;
  height: 32px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 8px;
  border: 1px solid var(--chip-2);
  background: transparent;
  color: var(--fg-3);
}
.gear:hover {
  border-color: oklch(0.42 0.02 265);
  color: var(--fg-2);
}
.menu {
  position: absolute;
  top: calc(100% + 8px);
  right: 0;
  width: 270px;
  z-index: 60;
  border: 1px solid var(--border-strong);
  border-radius: 14px;
  background: var(--surface-raised);
  padding: 4px 0;
}
.section {
  padding: 12px 14px;
  display: flex;
  flex-direction: column;
  gap: 9px;
}
.section + .section {
  border-top: 1px solid var(--chip-2);
}
.toggle {
  display: flex;
  align-items: center;
  gap: 9px;
  font-size: 12.5px;
  color: var(--fg-2);
}
.toggle input {
  accent-color: var(--accent);
  width: 15px;
  height: 15px;
}
.range-row {
  display: flex;
  justify-content: space-between;
  font-size: 11.5px;
  color: var(--fg-3);
}
.range-row .val {
  color: var(--fg);
}
.dim {
  opacity: 0.5;
}
.note {
  margin: 0;
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--fg-muted);
}
.swatches {
  display: flex;
  gap: 10px;
}
.swatch {
  width: 26px;
  height: 26px;
  border-radius: 8px;
  border: 2px solid transparent;
}
.swatch[aria-checked='true'] {
  border-color: var(--fg);
}
</style>

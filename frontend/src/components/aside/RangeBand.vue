<script setup lang="ts">
import { computed, ref } from 'vue'
import { bandGeometry, dragValue, type RangeState, type RangeView } from '@/lib/range'
import { price as fmtPrice } from '@/lib/format'

const props = defineProps<{ price: number; range: RangeState; view: RangeView; disabled?: boolean }>()
const emit = defineEmits<{
  min: [text: string]
  max: [text: string]
  /** The axis viewport is frozen for the duration of a drag so the handle does not chase the cursor. */
  freeze: [view: RangeView | null]
  step: [which: 'min' | 'max', dir: 1 | -1]
}>()

const track = ref<HTMLElement | null>(null)
const geo = computed(() => bandGeometry(props.price, props.range, props.view))
const color = computed(() => (props.range.inRange || props.range.status === 'Always in range' ? 'var(--accent)' : 'var(--warn)'))

let dragging: { which: 'min' | 'max'; rect: DOMRect; view: RangeView; pointerId: number } | null = null

function onDown(which: 'min' | 'max', e: PointerEvent) {
  if (props.disabled || !track.value) return
  e.preventDefault()
  e.stopPropagation()
  const handle = e.currentTarget as HTMLElement
  handle.setPointerCapture(e.pointerId)
  dragging = { which, rect: track.value.getBoundingClientRect(), view: { ...props.view }, pointerId: e.pointerId }
  emit('freeze', dragging.view)
}

function onMove(e: PointerEvent) {
  if (!dragging || e.pointerId !== dragging.pointerId) return
  const { which, rect, view } = dragging
  const frac = (e.clientX - rect.left) / rect.width
  const other = which === 'min' ? props.range.max : props.range.min
  const v = dragValue(which, frac, view, Number.isNaN(other) ? (which === 'min' ? Infinity : 0) : other, props.price)
  if (which === 'min') emit('min', fmtPrice(v))
  else emit('max', fmtPrice(v))
}

function onUp(e: PointerEvent) {
  if (!dragging || e.pointerId !== dragging.pointerId) return
  ;(e.currentTarget as HTMLElement).releasePointerCapture?.(e.pointerId)
  dragging = null
  emit('freeze', null)
}

function onKey(which: 'min' | 'max', e: KeyboardEvent) {
  if (props.disabled) return
  if (e.key === 'ArrowRight' || e.key === 'ArrowUp') {
    e.preventDefault()
    emit('step', which, 1)
  } else if (e.key === 'ArrowLeft' || e.key === 'ArrowDown') {
    e.preventDefault()
    emit('step', which, -1)
  }
}
</script>

<template>
  <div ref="track" class="track" :class="{ disabled }" data-testid="range-band">
    <div class="band" :style="{ left: geo.left + '%', width: geo.width + '%', background: color }" />
    <div class="price" :style="{ left: geo.priceLeft + '%' }" title="Current price" />
    <template v-if="range.valid">
      <div
        class="handle"
        role="slider"
        tabindex="0"
        aria-label="Min price"
        :aria-valuetext="range.minText"
        :style="{ left: geo.left + '%' }"
        title="Drag to set min price"
        @pointerdown="onDown('min', $event)"
        @pointermove="onMove"
        @pointerup="onUp"
        @pointercancel="onUp"
        @keydown="onKey('min', $event)"
      >
        <span :style="{ background: color }" />
      </div>
      <div
        class="handle"
        role="slider"
        tabindex="0"
        aria-label="Max price"
        :aria-valuetext="range.maxText"
        :style="{ left: geo.left + geo.width + '%' }"
        title="Drag to set max price"
        @pointerdown="onDown('max', $event)"
        @pointermove="onMove"
        @pointerup="onUp"
        @pointercancel="onUp"
        @keydown="onKey('max', $event)"
      >
        <span :style="{ background: color }" />
      </div>
    </template>
  </div>
</template>

<style scoped>
.track {
  position: relative;
  height: 44px;
  border-radius: 8px;
  background: oklch(0.22 0.012 265);
  touch-action: none;
  user-select: none;
}
.track.disabled {
  opacity: 0.45;
}
.band {
  position: absolute;
  top: 0;
  bottom: 0;
  opacity: 0.26;
}
.price {
  position: absolute;
  top: 0;
  bottom: 0;
  width: 2px;
  background: var(--fg);
}
.handle {
  position: absolute;
  top: -3px;
  bottom: -3px;
  width: 18px;
  margin-left: -9px;
  cursor: ew-resize;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: 4px;
  touch-action: none;
}
.handle span {
  width: 5px;
  height: 100%;
  border-radius: 4px;
  box-shadow: 0 0 0 1px var(--bg);
}
</style>

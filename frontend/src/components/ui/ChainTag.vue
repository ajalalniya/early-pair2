<script setup lang="ts">
import { computed } from 'vue'
import { chainMeta } from '@/lib/chains'

const props = withDefaults(defineProps<{ chainId: string; size?: 'sm' | 'md' | 'wide' }>(), { size: 'md' })
const meta = computed(() => chainMeta(props.chainId))
</script>

<template>
  <span class="tag" :class="size" :style="{ background: meta.dot }" :title="meta.name">{{ meta.tag }}</span>
</template>

<style scoped>
.tag {
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 6px;
  color: oklch(0.18 0.02 265);
  font-family: var(--font-mono);
  font-weight: 700;
  letter-spacing: -0.06em;
}
.md {
  width: 22px;
  height: 22px;
  font-size: 9.5px;
}
.sm {
  width: 20px;
  height: 20px;
  font-size: 9px;
}
.wide {
  min-width: 26px;
  height: 20px;
  padding: 0 5px;
  font-size: 9px;
}
</style>

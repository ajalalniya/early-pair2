<script setup lang="ts">
defineProps<{ title: string; body: string; action: string; busy?: boolean }>()
const emit = defineEmits<{ action: [] }>()
</script>

<template>
  <div class="wrap">
    <div class="locked">
      <span class="icon"><span /></span>
      <span class="title">{{ title }}</span>
      <span class="body">{{ body }}</span>
      <button class="btn btn-accent full" type="button" :disabled="busy" @click="emit('action')">{{ action }}</button>
    </div>
  </div>
</template>

<style scoped>
.wrap {
  padding: 54px 0;
  display: flex;
  align-items: center;
  justify-content: center;
}
.locked {
  width: 400px;
  max-width: 100%;
  border: 1px solid var(--chip-2);
  border-radius: 16px;
  background: var(--surface-2);
  padding: 26px 24px;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 13px;
  text-align: center;
}
.icon {
  width: 40px;
  height: 40px;
  border-radius: 12px;
  background: oklch(0.24 0.02 265);
  display: flex;
  align-items: center;
  justify-content: center;
}
.icon span {
  width: 12px;
  height: 12px;
  border-radius: 50%;
  background: var(--accent);
}
.title {
  font-size: 16px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.body {
  font-size: 12.5px;
  line-height: 1.55;
  color: oklch(0.68 0.01 265);
  text-wrap: pretty;
}
.full {
  width: 100%;
  font-size: 14px;
  padding: 12px;
  border-radius: 11px;
  margin-top: 3px;
}
</style>

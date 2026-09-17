<script setup lang="ts">
withDefaults(defineProps<{ vertical?: boolean }>(), { vertical: false })

const items = [
  { to: '/pools', name: 'New Pools' },
  { to: '/portfolio', name: 'Portfolio' },
  { to: '/watchlist', name: 'Watchlist' },
]
</script>

<template>
  <nav class="nav" :class="{ vertical }" aria-label="Primary">
    <RouterLink v-for="n in items" :key="n.to" v-slot="{ href, navigate, isActive }" :to="n.to" custom>
      <a :href="href" class="tab" :aria-current="isActive ? 'page' : undefined" @click="navigate">{{ n.name }}</a>
    </RouterLink>
  </nav>
</template>

<style scoped>
.nav {
  display: flex;
  gap: 4px;
  font-size: 13px;
}
.vertical {
  flex-direction: column;
}
.tab {
  padding: 6px 12px;
  border-radius: 8px;
  color: oklch(0.68 0.01 265);
  font-weight: 400;
}
.vertical .tab {
  padding: 10px 12px;
  font-size: 14px;
}
.tab:hover {
  background: oklch(0.22 0.015 265);
  color: var(--fg-2);
  filter: none;
}
.tab[aria-current='page'] {
  background: var(--surface-2);
  color: var(--fg);
  font-weight: 500;
}
</style>

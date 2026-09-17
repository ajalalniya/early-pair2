<script setup lang="ts">
import { ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import NavTabs from './NavTabs.vue'
import BlockPulse from './BlockPulse.vue'
import RpcHealthPill from './RpcHealthPill.vue'
import SettingsMenu from './SettingsMenu.vue'
import WalletButton from './WalletButton.vue'

const sheetOpen = ref(false)
const route = useRoute()
watch(
  () => route.path,
  () => (sheetOpen.value = false),
)
</script>

<template>
  <header class="header">
    <RouterLink to="/pools" class="brand" aria-label="EarlyPair home">
      <span class="mark">
        <svg width="17" height="17" viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="10" cy="11" r="6.5" fill="oklch(0.18 0.03 168)" />
          <polygon points="16,9.5 23,11.5 16,13.5" fill="oklch(0.18 0.03 168)" />
          <circle cx="12.4" cy="9" r="1.25" fill="var(--accent)" />
        </svg>
      </span>
      <span class="wordmark">EarlyPair</span>
    </RouterLink>

    <NavTabs class="nav-inline" />

    <button
      class="menu-btn"
      type="button"
      :aria-expanded="sheetOpen"
      aria-controls="nav-sheet"
      aria-label="Menu"
      @click="sheetOpen = !sheetOpen"
    >
      <span /><span /><span />
    </button>

    <div class="spacer" />

    <div class="tools">
      <BlockPulse />
      <RpcHealthPill />
      <SettingsMenu />
      <WalletButton />
    </div>

    <div v-if="sheetOpen" id="nav-sheet" class="sheet">
      <NavTabs vertical />
    </div>
  </header>
</template>

<style scoped>
.header {
  position: sticky;
  top: 0;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 24px;
  min-height: var(--header-h);
  padding: 14px 24px;
  border-bottom: 1px solid var(--chip-2);
  background: var(--bg);
  flex-wrap: wrap;
}
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  color: var(--fg);
  border-radius: 8px;
}
.mark {
  width: 26px;
  height: 26px;
  border-radius: 7px;
  background: var(--accent);
  display: flex;
  align-items: center;
  justify-content: center;
}
.wordmark {
  font-size: 16px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.spacer {
  flex: 1;
  min-width: 0;
}
.tools {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  justify-content: flex-end;
}
.menu-btn {
  display: none;
  width: 34px;
  height: 32px;
  border-radius: 8px;
  border: 1px solid var(--chip-2);
  background: transparent;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 3px;
}
.menu-btn span {
  width: 14px;
  height: 1.5px;
  background: var(--fg-3);
  border-radius: 1px;
}
.sheet {
  flex-basis: 100%;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-raised);
  padding: 6px;
}

@media (max-width: 819px) {
  .header {
    gap: 10px;
    padding: 10px 16px;
  }
  .nav-inline {
    display: none;
  }
  .menu-btn {
    display: flex;
    order: -1;
  }
  .spacer {
    display: none;
  }
  .tools {
    flex: 1;
    gap: 8px;
  }
}
</style>

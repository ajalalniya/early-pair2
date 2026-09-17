<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useQueryClient } from '@tanstack/vue-query'
import Chip from '@/components/ui/Chip.vue'
import ChainTag from '@/components/ui/ChainTag.vue'
import { useNow } from '@/composables/useNow'
import { api, errorMessage } from '@/lib/api'
import { CHAIN_META, chainMeta } from '@/lib/chains'
import { duration, num } from '@/lib/format'
import { useChainsQuery } from '@/queries'
import { qk } from '@/queries/keys'
import { useStreamStore } from '@/stores/stream'
import { useToasts } from '@/stores/toasts'
import type { EndpointStatus, RpcAction, RpcHealth } from '@/types/api'

const props = defineProps<{ health: RpcHealth | undefined; error: Error | null }>()

const qc = useQueryClient()
const toasts = useToasts()
const stream = useStreamStore()
const now = useNow()
const chains = useChainsQuery()

const blockAge = computed(() => {
  const c = stream.lastHeadChain
  const mark = c ? stream.heads[c] : undefined
  return mark ? `BLOCK +${Math.max(0, Math.floor((now.value - mark.at) / 1000))}s` : 'BLOCK —'
})

const busy = reactive<Record<string, boolean>>({})

const tones = {
  HEALTHY: { tone: 'accent', bar: 'var(--risk-safe-fg)' },
  DEGRADED: { tone: 'warn', bar: 'var(--risk-watch-fg)' },
  UNREACHABLE: { tone: 'danger', bar: 'oklch(0.78 0.16 25)' },
} as const

function row(e: EndpointStatus) {
  const ws = e.transport === 'ws'
  const down = e.state === 'UNREACHABLE'
  const ms = down ? null : (e.latencyMs ?? e.p50Ms)
  const action: RpcAction = down ? 'failover' : ws ? 'resubscribe' : 'ping'
  return {
    e,
    name: chains.data.value?.find((c) => c.id === e.chainId)?.name ?? chainMeta(e.chainId).name,
    ws,
    ms,
    tone: tones[e.state],
    barWidth: ms === null ? '0%' : `${Math.min(100, ms / 10)}%`,
    latencyWarn: ms !== null && ms > 500,
    subLabel: ws
      ? down
        ? '0 subs · socket closed'
        : `${e.subscriptions} subs · up ${duration(e.uptimeSec)}`
      : `polling ${e.pollIntervalMs ? num(e.pollIntervalMs) : '—'} ms`,
    dropLabel: e.reconnects24h === 0 ? 'no reconnects' : `${e.reconnects24h} reconnects 24h`,
    dropWarn: e.reconnects24h > 3,
    block: e.block === null ? 'no response' : num(e.block),
    lag: down || e.block === null ? '—' : e.lagBlocks === 0 ? 'tip' : e.lagBlocks === 1 ? '1 block behind' : `${e.lagBlocks} blocks behind`,
    lagWarn: e.lagBlocks > 0 && !down,
    action,
    actionLabel: action === 'failover' ? 'Failover' : action === 'resubscribe' ? 'Resubscribe' : 'Ping',
  }
}

const rows = computed(() => (props.health?.endpoints ?? []).map(row))
const footer = computed(() => {
  const s = props.health?.summary
  if (!s) return ''
  return `${s.socketsLive}/${s.socketsTotal} sockets live · ${s.subscriptions} subscriptions · ${s.failoverPolicy}`
})

async function act(e: EndpointStatus, action: RpcAction) {
  busy[e.id] = true
  try {
    const res = await api.endpointAction(e.id, action)
    if (action === 'ping') toasts.push(`${e.id}: head ${typeof res.head === 'number' ? num(res.head) : 'returned'}`, 'accent')
    else toasts.push(res.note ? `${e.id}: ${res.note}` : `${e.id}: ${action} requested`, 'info')
    void qc.invalidateQueries({ queryKey: qk.rpcHealth })
  } catch (err) {
    toasts.push(`${e.id}: ${errorMessage(err)}`, 'danger')
  } finally {
    busy[e.id] = false
  }
}

// ---------------------------------------------------------------- add endpoint
const adding = ref(false)
const form = reactive({ chainId: 'eth', transport: 'ws' as 'ws' | 'http', url: '' })
const formError = ref<string | null>(null)
const saving = ref(false)
const chainOptions = computed(() => (chains.data.value?.length ? chains.data.value.map((c) => ({ id: c.id, name: c.name })) : CHAIN_META))
// Keep the selection on a chain the backend serves (eth may be disabled).
watch(chainOptions, (opts) => {
  const first = opts[0]
  if (first && !opts.some((o) => o.id === form.chainId)) form.chainId = first.id
}, { immediate: true })

async function addEndpoint() {
  formError.value = null
  const url = form.url.trim()
  const ok = form.transport === 'ws' ? /^wss?:\/\//.test(url) : /^https?:\/\//.test(url)
  if (!ok) {
    formError.value = form.transport === 'ws' ? 'A socket endpoint needs a ws:// or wss:// URL.' : 'An HTTP endpoint needs an http:// or https:// URL.'
    return
  }
  saving.value = true
  try {
    const res = await api.addEndpoint({ chainId: form.chainId, transport: form.transport, url })
    toasts.push(res.note ? `Added ${res.url}. ${res.note}` : `Added ${res.url}`, 'accent')
    form.url = ''
    adding.value = false
    void qc.invalidateQueries({ queryKey: qk.rpcHealth })
  } catch (err) {
    formError.value = errorMessage(err)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="panel" role="dialog" aria-label="RPC health">
    <div class="head">
      <span class="caps">RPC HEALTH</span>
      <span class="mono age">{{ blockAge }}</span>
    </div>

    <div class="list">
      <div v-if="error && !health" class="empty">{{ error.message }}</div>
      <div v-else-if="!health" class="empty">Loading endpoint health…</div>
      <div v-else-if="rows.length === 0" class="empty">No RPC endpoints configured.</div>

      <div v-for="r in rows" :key="r.e.id" class="ep">
        <div class="line1">
          <ChainTag :chain-id="r.e.chainId" />
          <div class="who">
            <div class="name-row">
              <span class="name">{{ r.name }}</span>
              <Chip :tone="r.ws ? 'ws' : 'muted'">{{ r.ws ? 'WSS' : 'HTTP' }}</Chip>
            </div>
            <div class="url" :title="r.e.url">{{ r.e.url }}</div>
          </div>
          <Chip :tone="r.tone.tone" :title="r.e.lastError || undefined">{{ r.e.state }}</Chip>
        </div>

        <div class="line2">
          <div class="bar"><div :style="{ width: r.barWidth, background: r.tone.bar }" /></div>
          <span class="latency" :class="{ warn: r.latencyWarn, off: r.ms === null }">
            {{ r.ms === null ? '—' : `${r.ms} ms` }} <small>{{ r.ws ? 'push' : 'rtt' }}</small>
          </span>
        </div>

        <div class="line3">
          <span>{{ r.subLabel }}</span>
          <span :class="{ warn: r.dropWarn }">{{ r.dropLabel }}</span>
        </div>

        <div class="line4">
          <span>Head {{ r.block }}</span>
          <span :class="{ warn: r.lagWarn }">{{ r.lag }}</span>
          <button class="btn btn-sm" type="button" :disabled="busy[r.e.id]" @click="act(r.e, r.action)">
            {{ busy[r.e.id] ? '…' : r.actionLabel }}
          </button>
        </div>
      </div>
    </div>

    <div class="foot">
      <span class="summary">{{ footer }}</span>
      <button class="link" type="button" :aria-expanded="adding" @click="adding = !adding">
        {{ adding ? 'Cancel' : 'Add endpoint' }}
      </button>
    </div>

    <form v-if="adding" class="add" @submit.prevent="addEndpoint">
      <div class="add-row">
        <label class="visually-hidden" for="ep-chain">Chain</label>
        <select id="ep-chain" v-model="form.chainId" class="input">
          <option v-for="c in chainOptions" :key="c.id" :value="c.id">{{ c.name }}</option>
        </select>
        <div class="segmented seg" role="group" aria-label="Transport">
          <button type="button" :aria-pressed="form.transport === 'ws'" @click="form.transport = 'ws'">WSS</button>
          <button type="button" :aria-pressed="form.transport === 'http'" @click="form.transport = 'http'">HTTP</button>
        </div>
      </div>
      <label class="visually-hidden" for="ep-url">Endpoint URL</label>
      <input
        id="ep-url"
        v-model="form.url"
        class="input mono"
        :placeholder="form.transport === 'ws' ? 'wss://…' : 'https://…'"
        autocomplete="off"
        spellcheck="false"
      />
      <div v-if="formError" class="form-error">{{ formError }}</div>
      <button class="btn btn-accent" type="submit" :disabled="saving">{{ saving ? 'Adding…' : 'Add endpoint' }}</button>
    </form>
  </div>
</template>

<style scoped>
.panel {
  position: absolute;
  top: calc(100% + 8px);
  right: 0;
  width: 460px;
  max-width: calc(100vw - 32px);
  z-index: 60;
  border: 1px solid var(--border-strong);
  border-radius: 14px;
  background: var(--surface-raised);
  overflow: hidden;
}
@media (max-width: 819px) {
  .panel {
    position: fixed;
    top: 64px;
    left: 16px;
    right: 16px;
    width: auto;
    max-width: none;
  }
}
.head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 15px;
  border-bottom: 1px solid var(--chip-2);
}
.head .caps {
  letter-spacing: 0.07em;
  color: oklch(0.66 0.01 265);
}
.age {
  font-size: 11px;
  color: oklch(0.66 0.01 265);
}
.list {
  max-height: 60vh;
  overflow-y: auto;
}
.empty {
  padding: 18px 15px;
  font-family: var(--font-mono);
  font-size: 11.5px;
  color: var(--fg-muted);
}
.ep {
  padding: 12px 15px;
  border-bottom: 1px solid oklch(0.23 0.012 265);
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.line1 {
  display: flex;
  align-items: center;
  gap: 10px;
}
.who {
  flex: 1;
  min-width: 0;
}
.name-row {
  display: flex;
  align-items: center;
  gap: 7px;
}
.name-row :deep(.chip) {
  font-size: 9px;
  font-weight: 700;
  letter-spacing: 0.06em;
  padding: 2px 5px;
  border-radius: 4px;
}
.name {
  font-size: 13px;
  font-weight: 600;
}
.url {
  font-size: 10.5px;
  font-family: var(--font-mono);
  color: oklch(0.62 0.01 265);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  margin-top: 2px;
}
.line2 {
  display: flex;
  align-items: center;
  gap: 12px;
}
.bar {
  flex: 1;
  min-width: 0;
  height: 4px;
  border-radius: 999px;
  background: var(--border);
  overflow: hidden;
}
.bar > div {
  height: 100%;
  border-radius: 999px;
  transition: width 0.4s ease;
}
.latency {
  font-family: var(--font-mono);
  font-size: 11.5px;
  color: oklch(0.9 0.01 265);
  min-width: 76px;
  text-align: right;
}
.latency small {
  color: oklch(0.58 0.01 265);
  font-size: 9.5px;
}
.latency.off {
  color: oklch(0.6 0.01 265);
}
.line3,
.line4 {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: oklch(0.62 0.01 265);
}
.line4 {
  font-size: 11px;
  color: oklch(0.66 0.01 265);
}
.warn,
.latency.warn {
  color: var(--warn);
}
.foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 11px 15px;
}
.summary {
  font-size: 11.5px;
  font-family: var(--font-mono);
  color: oklch(0.62 0.01 265);
}
.link {
  border: none;
  background: transparent;
  font-size: 11.5px;
  font-weight: 600;
  color: var(--accent);
  white-space: nowrap;
  padding: 2px 0;
}
.add {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 0 15px 14px;
}
.add-row {
  display: flex;
  gap: 8px;
}
.add-row select {
  flex: 1;
}
.seg {
  flex: none;
  padding: 2px;
}
.seg > button {
  padding: 5px 10px;
  font-size: 11px;
  font-family: var(--font-mono);
}
.input {
  background: var(--surface-inset);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 8px 10px;
  font-size: 12px;
  color: var(--fg);
  min-width: 0;
}
.form-error {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--danger-text);
}
</style>

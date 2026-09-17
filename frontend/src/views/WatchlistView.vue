<script setup lang="ts">
import { computed, ref } from 'vue'
import { useMutation, useQueryClient } from '@tanstack/vue-query'
import ChainTag from '@/components/ui/ChainTag.vue'
import Chip from '@/components/ui/Chip.vue'
import LockedCard from '@/components/ui/LockedCard.vue'
import { useNow } from '@/composables/useNow'
import { api, errorMessage } from '@/lib/api'
import { explorerTx, poolKey } from '@/lib/chains'
import { age, hash } from '@/lib/format'
import { parseTopicInput } from '@/lib/topic'
import { useChainsQuery, useTokenWatchesQuery, useTopicWatchesQuery } from '@/queries'
import { qk } from '@/queries/keys'
import { useToasts } from '@/stores/toasts'
import { useWallet } from '@/stores/wallet'
import type { TokenWatch, TopicWatch } from '@/types/api'

const wallet = useWallet()
const qc = useQueryClient()
const toasts = useToasts()
const now = useNow()
const chains = useChainsQuery()
const tokens = useTokenWatchesQuery()
const topics = useTopicWatchesQuery()

const ingestingCount = computed(() => chains.data.value?.filter((c) => c.ingesting).length ?? 0)
const scope = (w: { chains: string[] }) => {
  const n = w.chains.length || ingestingCount.value
  return `Listening on ${n} ${n === 1 ? 'chain' : 'chains'}`
}

// ------------------------------------------------------------ token watches
const tokenInput = ref('')
const tokenKey = computed(() => qk.tokenWatches(wallet.address))

const addToken = useMutation({
  mutationFn: (name: string) => api.addTokenWatch(name),
  onMutate: async (name: string) => {
    await qc.cancelQueries({ queryKey: tokenKey.value })
    const tempId = -Date.now()
    const temp: TokenWatch = {
      id: tempId,
      name: name.toUpperCase(),
      address: '',
      chains: [],
      resolved: false,
      createdAt: new Date().toISOString(),
      resolvedHits: [],
    }
    qc.setQueryData<TokenWatch[]>(tokenKey.value, (old) => [temp, ...(old ?? [])])
    return { tempId }
  },
  onSuccess: (res, _name, context) => {
    qc.setQueryData<TokenWatch[]>(tokenKey.value, (old) =>
      (old ?? []).map((w) => (w.id === context?.tempId ? { ...res.watch, resolvedHits: res.watch.resolvedHits ?? [] } : w)),
    )
    if (res.message) toasts.push(res.message, 'info')
  },
  onError: (err, _name, context) => {
    qc.setQueryData<TokenWatch[]>(tokenKey.value, (old) => (old ?? []).filter((w) => w.id !== context?.tempId))
    toasts.push(`Could not add the watch: ${errorMessage(err)}`, 'danger')
  },
})

const removeToken = useMutation({
  mutationFn: (id: number) => api.deleteTokenWatch(id),
  onMutate: async (id: number) => {
    const prev = qc.getQueryData<TokenWatch[]>(tokenKey.value)
    qc.setQueryData<TokenWatch[]>(tokenKey.value, (old) => (old ?? []).filter((w) => w.id !== id))
    return { prev }
  },
  onError: (err, _id, context) => {
    qc.setQueryData(tokenKey.value, context?.prev)
    toasts.push(`Could not remove the watch: ${errorMessage(err)}`, 'danger')
  },
})

function submitToken() {
  const v = tokenInput.value.trim()
  if (!v) return
  tokenInput.value = ''
  addToken.mutate(v)
}

// ------------------------------------------------------------ topic watches
const topicInput = ref('')
const topicError = ref<string | null>(null)
const topicKey = computed(() => qk.topicWatches(wallet.address))
const topicPreview = computed(() => {
  const v = topicInput.value.trim()
  if (!v) return null
  const p = parseTopicInput(v)
  return p.kind === 'invalid' ? null : p.topic0
})

type TopicList = { data: TopicWatch[]; limit?: number }

const addTopic = useMutation({
  mutationFn: (sig: string) => api.addTopicWatch(sig),
  onMutate: async (sig: string) => {
    const parsed = parseTopicInput(sig)
    if (parsed.kind === 'invalid') return { tempId: 0 }
    await qc.cancelQueries({ queryKey: topicKey.value })
    const tempId = -Date.now()
    const temp: TopicWatch = {
      id: tempId,
      name: parsed.name,
      signature: parsed.signature,
      topic0: parsed.topic0,
      chains: [],
      alerts: false,
      suspended: false,
      createdAt: new Date().toISOString(),
      matches24h: 0,
      lastMatchAt: null,
      lastTx: '',
    }
    qc.setQueryData<TopicList>(topicKey.value, (old) => ({ ...(old ?? {}), data: [temp, ...(old?.data ?? [])] }))
    return { tempId }
  },
  onSuccess: (res, _sig, context) => {
    qc.setQueryData<TopicList>(topicKey.value, (old) => ({
      ...(old ?? {}),
      data: (old?.data ?? []).map((w) => (w.id === context?.tempId ? res.watch : w)),
    }))
  },
  onError: (err, _sig, context) => {
    qc.setQueryData<TopicList>(topicKey.value, (old) => ({ ...(old ?? {}), data: (old?.data ?? []).filter((w) => w.id !== context?.tempId) }))
    toasts.push(`Could not add the topic: ${errorMessage(err)}`, 'danger')
  },
})

const removeTopic = useMutation({
  mutationFn: (id: number) => api.deleteTopicWatch(id),
  onMutate: async (id: number) => {
    const prev = qc.getQueryData<TopicList>(topicKey.value)
    qc.setQueryData<TopicList>(topicKey.value, (old) => (old ? { ...old, data: old.data.filter((w) => w.id !== id) } : old))
    return { prev }
  },
  onError: (err, _id, context) => {
    qc.setQueryData(topicKey.value, context?.prev)
    toasts.push(`Could not remove the topic: ${errorMessage(err)}`, 'danger')
  },
})

const topicCount = computed(() => topics.data.value?.data.filter((w) => w.id > 0).length ?? 0)
const topicLimit = computed(() => topics.data.value?.limit)

function submitTopic() {
  topicError.value = null
  const v = topicInput.value.trim()
  if (!v) return
  const parsed = parseTopicInput(v)
  if (parsed.kind === 'invalid') {
    topicError.value = parsed.reason
    return
  }
  if (topicLimit.value !== undefined && topicCount.value >= topicLimit.value) {
    topicError.value = `You already watch ${topicLimit.value} topics; remove one first.`
    return
  }
  topicInput.value = ''
  addTopic.mutate(v)
}

function topicStatus(w: TopicWatch): { label: string; tone: 'danger' | 'warn' | 'accent' | 'muted' } {
  if (w.suspended) return { label: 'SUSPENDED', tone: 'danger' }
  if (w.alerts) return { label: 'ALERTS ON', tone: 'warn' }
  if (w.matches24h > 0) return { label: 'LIVE', tone: 'accent' }
  return { label: 'LISTENING', tone: 'muted' }
}

function lastTxUrl(w: TopicWatch): string | null {
  const chain = w.lastChainId ?? (w.chains.length === 1 ? w.chains[0] : undefined)
  return chain && w.lastTx ? explorerTx(chain, w.lastTx) : null
}
</script>

<template>
  <div class="page">
    <LockedCard
      v-if="!wallet.connected"
      title="Connect your wallet"
      body="Watches are saved to your address, so a wallet is needed to create and list them. Nothing is signed except a sign-in message."
      action="Connect Wallet"
      @action="wallet.openModal()"
    />
    <LockedCard
      v-else-if="!wallet.signedIn"
      title="Sign in to manage watches"
      :body="wallet.signInError ?? 'Sign a one-time message to prove this address is yours. It authorises nothing else and costs no gas.'"
      :action="wallet.signingIn ? 'Waiting for signature…' : 'Sign in'"
      :busy="wallet.signingIn"
      @action="wallet.signIn()"
    />

    <div v-else class="grid">
      <section class="col" aria-labelledby="tw-title">
        <div>
          <h2 id="tw-title">Token watch</h2>
          <p class="lede">Name a token and EarlyPair alerts you the moment a new pair for it is deployed on any indexed chain.</p>
        </div>
        <form class="entry" @submit.prevent="submitToken">
          <label class="visually-hidden" for="token-input">Token name, symbol or address</label>
          <input id="token-input" v-model="tokenInput" class="mono" placeholder="Token name, symbol or address" autocomplete="off" spellcheck="false" />
          <button class="btn btn-accent" type="submit">Watch</button>
        </form>

        <div v-if="tokens.isLoading.value" class="card state">Loading watches…</div>
        <div v-else-if="tokens.isError.value" class="card state">{{ errorMessage(tokens.error.value) }}</div>
        <div v-else-if="!tokens.data.value?.length" class="card state">No token watches yet.</div>

        <article v-for="w in tokens.data.value" :key="w.id" class="card item" :data-pending="w.id < 0">
          <div class="item-top">
            <div class="min0">
              <div class="item-name mono">{{ w.name }}</div>
              <div class="item-sub mono" :title="w.address || undefined">{{ w.address || 'resolving symbol…' }}</div>
            </div>
            <Chip :tone="w.resolvedHits.length ? 'accent' : 'muted'">
              {{ w.resolvedHits.length ? `${w.resolvedHits.length} new ${w.resolvedHits.length === 1 ? 'pair' : 'pairs'}` : 'LISTENING' }}
            </Chip>
            <button class="x" type="button" title="Stop watching" :aria-label="`Stop watching ${w.name}`" :disabled="w.id < 0" @click="removeToken.mutate(w.id)">×</button>
          </div>
          <div class="item-meta mono">
            <span>{{ scope(w) }}</span><span>added {{ age(w.createdAt, now) }} ago</span>
          </div>
          <div v-if="w.resolvedHits.length" class="hits">
            <div v-for="h in w.resolvedHits" :key="`${h.chainId}:${h.pool}`" class="hit">
              <ChainTag :chain-id="h.chainId" size="sm" />
              <div class="min0 grow">
                <div class="hit-pair mono">{{ h.pair || hash(h.pool) }}</div>
                <div class="hit-meta mono">{{ h.dex || 'unknown dex' }} · {{ age(h.seenAt, now) }} ago</div>
              </div>
              <RouterLink class="btn btn-sm" :to="{ path: '/pools', query: { sel: poolKey(h.chainId, h.pool) } }">Trade</RouterLink>
            </div>
          </div>
          <div v-else class="waiting">
            <span class="pulse" />
            <span>No pair deployed yet — waiting.</span>
          </div>
        </article>
      </section>

      <section class="col" aria-labelledby="ew-title">
        <div>
          <h2 id="ew-title">Event topic watch</h2>
          <p class="lede">Add an event signature or topic hash and get every transaction that emits it across the chains you index.</p>
        </div>
        <form class="entry" @submit.prevent="submitTopic">
          <label class="visually-hidden" for="topic-input">Event signature or topic hash</label>
          <input
            id="topic-input"
            v-model="topicInput"
            class="mono"
            placeholder="Transfer(address,address,uint256) or 0x topic"
            autocomplete="off"
            spellcheck="false"
            :aria-invalid="!!topicError"
            @input="topicError = null"
          />
          <button class="btn btn-accent" type="submit">Listen</button>
        </form>
        <div v-if="topicError" class="form-error mono" role="alert">{{ topicError }}</div>
        <div v-else-if="topicPreview" class="preview mono">topic0 {{ topicPreview }}</div>
        <div v-if="topicLimit" class="limit mono">{{ topicCount }} / {{ topicLimit }} topics</div>

        <div v-if="topics.isLoading.value" class="card state">Loading topics…</div>
        <div v-else-if="topics.isError.value" class="card state">{{ errorMessage(topics.error.value) }}</div>
        <div v-else-if="!topics.data.value?.data.length" class="card state">No topic watches yet.</div>

        <article v-for="w in topics.data.value?.data" :key="w.id" class="card item" :data-pending="w.id < 0">
          <div class="item-top">
            <div class="min0">
              <div class="item-name mono">{{ w.name }}</div>
              <div class="item-sig mono" :title="w.signature || undefined">{{ w.signature || 'raw topic' }}</div>
            </div>
            <Chip :tone="topicStatus(w).tone">{{ topicStatus(w).label }}</Chip>
            <button class="x" type="button" title="Stop listening" :aria-label="`Stop listening for ${w.name}`" :disabled="w.id < 0" @click="removeTopic.mutate(w.id)">×</button>
          </div>
          <div class="topic mono" :title="w.topic0">topic0 {{ w.topic0 }}</div>
          <div class="topic-foot mono">
            <span :class="{ has: w.matches24h > 0 }">{{ w.matches24h ? `${w.matches24h.toLocaleString('en-US')} matches / 24h` : 'No matches yet' }}</span>
            <span class="last">
              last {{ w.lastMatchAt ? `${age(w.lastMatchAt, now)} ago` : '—' }} ·
              <a v-if="lastTxUrl(w)" :href="lastTxUrl(w)!" target="_blank" rel="noopener noreferrer">{{ hash(w.lastTx) }}</a>
              <template v-else>{{ w.lastTx ? hash(w.lastTx) : '—' }}</template>
            </span>
          </div>
        </article>
      </section>
    </div>
  </div>
</template>

<style scoped>
.page {
  width: 100%;
  max-width: 1280px;
  margin: 0 auto;
  padding: 18px 24px 40px;
}
.grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(400px, 100%), 1fr));
  gap: 18px;
  align-items: start;
}
.col {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-width: 0;
}
h2 {
  margin: 0;
  font-size: 17px;
  font-weight: 600;
  letter-spacing: -0.01em;
}
.lede {
  margin: 5px 0 0;
  font-size: 12.5px;
  line-height: 1.5;
  color: var(--fg-3);
  text-wrap: pretty;
}
.entry {
  display: flex;
  gap: 8px;
  border: 1px solid var(--chip-2);
  border-radius: 12px;
  background: var(--surface);
  padding: 8px;
}
.entry:focus-within {
  border-color: var(--border-active);
}
.entry input {
  flex: 1;
  min-width: 0;
  background: transparent;
  border: none;
  outline: none;
  color: var(--fg);
  font-size: 13px;
  padding: 7px 8px;
}
.form-error {
  font-size: 11.5px;
  color: var(--danger-text);
}
.preview,
.limit {
  font-size: 10.5px;
  color: var(--fg-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.state {
  padding: 22px 16px;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--fg-muted);
  text-align: center;
}
.item {
  padding: 14px 15px;
  display: flex;
  flex-direction: column;
  gap: 11px;
}
.item[data-pending='true'] {
  opacity: 0.75;
}
.item-top {
  display: flex;
  align-items: flex-start;
  gap: 10px;
}
.min0 {
  min-width: 0;
  flex: 1;
}
.grow {
  flex: 1;
}
.item-name {
  font-size: 14px;
  font-weight: 600;
}
.item-sub,
.item-sig,
.topic {
  font-size: 10.5px;
  color: oklch(0.62 0.01 265);
  margin-top: 3px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.item-sig {
  font-size: 11px;
  color: var(--fg-3);
}
.topic {
  margin-top: 0;
  color: var(--fg-muted);
}
.x {
  flex: none;
  width: 24px;
  height: 24px;
  border-radius: 7px;
  border: 1px solid var(--border-strong);
  background: transparent;
  color: var(--fg-3);
  font-size: 13px;
  line-height: 1;
  display: flex;
  align-items: center;
  justify-content: center;
}
.x:hover:not(:disabled) {
  border-color: oklch(0.5 0.09 25);
  color: var(--danger-text);
}
.item-meta {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  font-size: 11px;
  color: var(--fg-muted);
}
.hits {
  display: flex;
  flex-direction: column;
  gap: 1px;
}
.hit {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 0;
  border-top: 1px solid oklch(0.235 0.012 265);
}
.hit-pair {
  font-size: 12.5px;
  font-weight: 600;
}
.hit-meta {
  font-size: 10.5px;
  color: oklch(0.62 0.01 265);
  margin-top: 2px;
}
.waiting {
  display: flex;
  align-items: center;
  gap: 9px;
  padding-top: 10px;
  border-top: 1px solid oklch(0.235 0.012 265);
  font-size: 12px;
  color: oklch(0.66 0.01 265);
}
.pulse {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: oklch(0.62 0.02 265);
  animation: pulseDot 1.8s infinite;
}
.topic-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
  padding-top: 10px;
  border-top: 1px solid oklch(0.235 0.012 265);
  font-size: 11.5px;
  color: var(--fg-muted);
}
.topic-foot .has {
  color: oklch(0.92 0.01 265);
  font-weight: 500;
}
@media (max-width: 819px) {
  .page {
    padding: 12px 16px 32px;
  }
}
</style>

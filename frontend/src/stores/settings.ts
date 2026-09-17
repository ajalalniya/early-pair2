import { defineStore } from 'pinia'
import { ref, watch } from 'vue'

export const ACCENTS = [
  'oklch(0.8 0.13 168)',
  'oklch(0.78 0.15 265)',
  'oklch(0.82 0.15 85)',
  'oklch(0.75 0.16 320)',
] as const

const KEY = 'earlypair.settings.v1'

interface Persisted {
  gateRiskyTrades: boolean
  riskGateThreshold: number
  accent: string
  slippageBps: number
  /** On pairs priced in WETH (or WBNB), pay with ETH (BNB) itself. */
  payWithNative: boolean
}

function load(): Partial<Persisted> {
  try {
    return JSON.parse(localStorage.getItem(KEY) || '{}') as Partial<Persisted>
  } catch {
    return {}
  }
}

const clampThreshold = (n: unknown): number => {
  const v = typeof n === 'number' && Number.isFinite(n) ? n : 60
  return Math.min(100, Math.max(10, Math.round(v / 5) * 5))
}

export const useSettings = defineStore('settings', () => {
  const saved = load()
  const gateRiskyTrades = ref(saved.gateRiskyTrades ?? true)
  const riskGateThreshold = ref(clampThreshold(saved.riskGateThreshold))
  const accent = ref<string>(ACCENTS.includes(saved.accent as (typeof ACCENTS)[number]) ? saved.accent! : ACCENTS[0])
  const slippageBps = ref(typeof saved.slippageBps === 'number' && saved.slippageBps > 0 ? saved.slippageBps : 100)
  const payWithNative = ref(saved.payWithNative ?? true)

  watch(
    accent,
    (v) => {
      if (typeof document !== 'undefined') document.documentElement.style.setProperty('--accent', v)
    },
    { immediate: true },
  )

  watch([gateRiskyTrades, riskGateThreshold, accent, slippageBps, payWithNative], () => {
    try {
      localStorage.setItem(
        KEY,
        JSON.stringify({
          gateRiskyTrades: gateRiskyTrades.value,
          riskGateThreshold: riskGateThreshold.value,
          accent: accent.value,
          slippageBps: slippageBps.value,
          payWithNative: payWithNative.value,
        } satisfies Persisted),
      )
    } catch {
      /* private mode: settings just do not persist */
    }
  })

  function setThreshold(n: number) {
    riskGateThreshold.value = clampThreshold(n)
  }

  return { gateRiskyTrades, riskGateThreshold, accent, slippageBps, payWithNative, setThreshold }
})

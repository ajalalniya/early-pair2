import { defineStore } from 'pinia'
import { ref } from 'vue'

export type StreamStatus = 'live' | 'polling' | 'down'

export interface HeadMark {
  block: number
  /** Client receive time; the pulse counts from when we heard it. */
  at: number
}

export const useStreamStore = defineStore('stream', () => {
  const status = ref<StreamStatus>('down')
  const connecting = ref(true)
  const lastSeq = ref<number | null>(null)
  const heads = ref<Record<string, HeadMark>>({})
  const lastHeadChain = ref<string | null>(null)

  function recordHead(chainId: string, block: number) {
    heads.value = { ...heads.value, [chainId]: { block, at: Date.now() } }
    lastHeadChain.value = chainId
  }

  return { status, connecting, lastSeq, heads, lastHeadChain, recordHead }
})

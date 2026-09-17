import { computed, onScopeDispose, ref, shallowRef, watch, type Ref } from 'vue'
import { ApiError, errorMessage } from '@/lib/api'
import type { Quote } from '@/types/api'
import { useNow } from './useNow'

/** Pause after a quote settles before the next background refresh starts. */
export const REQUOTE_MS = 10_000

/**
 * Keeps a quote fresh while a trade panel is open.
 *
 * - A new quote is fetched when the trade's inputs change (debounced while
 *   typing). Only a real change counts: the request is compared by value, so a
 *   stream update that re-creates an identical request — which arrives every
 *   few seconds while a pair is tracked — neither cancels the request in flight
 *   nor clears the quote.
 * - While the panel stays open the quote is refreshed in the background, one
 *   request at a time, each starting REQUOTE_MS after the previous one settled.
 *   Requests never run back to back, so there is always time to act on a quote.
 * - `ready` is true whenever a quote for the current inputs is held and has not
 *   expired. A background refresh does not take it away; the button stays
 *   usable, and the refreshed quote simply replaces the held one. A refresh that
 *   fails leaves a still-valid quote in place.
 *
 * A quote carries its own expiry and minimum received, both enforced when the
 * transaction is built, so acting on a quote seconds old is safe.
 */
export function useQuote<Req>(options: {
  request: Ref<Req | null>
  active: Ref<boolean>
  fetch: (req: Req, signal: AbortSignal) => Promise<Quote>
  debounceMs?: number
}) {
  const quote = shallowRef<Quote | null>(null)
  const error = shallowRef<ApiError | Error | null>(null)
  const inFlight = ref(false)
  const now = useNow()

  let controller: AbortController | null = null
  let timer: number | undefined
  let requestKey = ''
  let running = false

  const keyOf = (req: Req | null) => (req ? JSON.stringify(req) : '')
  const stillValid = (q: Quote | null) => !!q && Date.parse(q.expiresAt) > Date.now()

  function stop() {
    window.clearTimeout(timer)
    timer = undefined
    controller?.abort()
    controller = null
    inFlight.value = false
  }

  async function run() {
    window.clearTimeout(timer)
    const req = options.request.value
    if (!req || !options.active.value) return
    const key = keyOf(req)
    controller?.abort()
    const mine = new AbortController()
    controller = mine
    inFlight.value = true
    try {
      const q = await options.fetch(req, mine.signal)
      if (controller !== mine) return
      quote.value = q
      error.value = null
    } catch (err) {
      if ((err as Error).name === 'AbortError' || controller !== mine) return
      // Keep a quote that can still be traded on; only an unusable one goes.
      if (!stillValid(quote.value)) {
        quote.value = null
        error.value = err instanceof Error ? err : new Error(errorMessage(err))
      }
    } finally {
      if (controller === mine) {
        controller = null
        inFlight.value = false
        if (options.active.value && key === requestKey) {
          timer = window.setTimeout(() => void run(), REQUOTE_MS)
        }
      }
    }
  }

  function onChange() {
    const req = options.request.value
    const key = keyOf(req)
    const shouldRun = !!req && options.active.value
    const inputsChanged = key !== requestKey
    // The same trade, in the same state: an equal request object changes nothing.
    if (!inputsChanged && shouldRun === running) return
    running = shouldRun

    if (inputsChanged) {
      // A quote for different inputs must not linger next to the new ones.
      requestKey = key
      quote.value = null
      error.value = null
    }
    stop()
    if (!shouldRun) return

    if (!inputsChanged && stillValid(quote.value)) {
      // Resuming the same trade (a transaction finished, the tab came back):
      // the held quote is still good, so refresh after the usual pause.
      timer = window.setTimeout(() => void run(), REQUOTE_MS)
      return
    }
    inFlight.value = true // pending from the first keystroke, not only once sent
    timer = window.setTimeout(() => void run(), options.debounceMs ?? 350)
  }

  watch([options.request, options.active], onChange, { immediate: true, deep: true })

  onScopeDispose(stop)

  const expired = computed(() => !!quote.value && Date.parse(quote.value.expiresAt) <= now.value)
  const ready = computed(() => !!quote.value && !expired.value)
  /** A background refresh is running while a usable quote is held. */
  const refreshing = computed(() => inFlight.value && ready.value)

  return { quote, error, inFlight, expired, ready, refreshing, refresh: run }
}

import { onScopeDispose, readonly, ref, type Ref } from 'vue'

/**
 * One shared 1-second clock. Ages, block pulses and quote expiry all read from
 * it, so a table of hundreds of rows costs one timer, not hundreds.
 */
const now = ref(Date.now())
let subscribers = 0
let timer: number | undefined

export function useNow(): Readonly<Ref<number>> {
  if (subscribers++ === 0) {
    now.value = Date.now()
    timer = window.setInterval(() => (now.value = Date.now()), 1000)
  }
  onScopeDispose(() => {
    if (--subscribers === 0) window.clearInterval(timer)
  })
  return readonly(now)
}

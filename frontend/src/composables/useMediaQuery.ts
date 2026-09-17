import { onScopeDispose, ref, type Ref } from 'vue'

export function useMediaQuery(query: string): Ref<boolean> {
  const mql = typeof window !== 'undefined' && window.matchMedia ? window.matchMedia(query) : null
  const matches = ref(mql?.matches ?? false)
  const onChange = (e: MediaQueryListEvent) => (matches.value = e.matches)
  mql?.addEventListener('change', onChange)
  onScopeDispose(() => mql?.removeEventListener('change', onChange))
  return matches
}

export function usePrefersReducedMotion(): Ref<boolean> {
  return useMediaQuery('(prefers-reduced-motion: reduce)')
}

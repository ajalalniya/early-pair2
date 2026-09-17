import { computed, type MaybeRefOrGetter, toValue } from 'vue'
import { keepPreviousData, useQuery } from '@tanstack/vue-query'
import { api } from '@/lib/api'
import { qk } from './keys'

/** Pools on any chain whose address starts with the term; disabled without one. */
export function useSearchPoolsQuery(term: MaybeRefOrGetter<string | null>) {
  return useQuery({
    queryKey: computed(() => qk.search(toValue(term) ?? '')),
    queryFn: ({ signal }) => api.searchPools(toValue(term)!, signal),
    enabled: computed(() => !!toValue(term)),
    placeholderData: keepPreviousData,
    staleTime: 10_000,
  })
}

export const qk = {
  chains: ['chains'] as const,
  feed: ['pools', 'feed'] as const,
  favorites: ['pools', 'favorites'] as const,
  search: (term: string) => ['pools', 'search', term] as const,
  pool: (chainId: string, address: string) => ['pool', chainId, address.toLowerCase()] as const,
  rpcHealth: ['rpc', 'health'] as const,
  funds: (address: string | null) => ['portfolio', 'funds', address] as const,
  holdings: (address: string | null) => ['portfolio', 'holdings', address] as const,
  positions: (address: string | null) => ['portfolio', 'positions', address] as const,
  tokenWatches: (address: string | null) => ['watch', 'tokens', address] as const,
  topicWatches: (address: string | null) => ['watch', 'topics', address] as const,
}

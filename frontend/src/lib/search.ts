/**
 * Pair search takes a pool address or its start. The first `0x…` run in the
 * input is used, so a pasted DexScreener or explorer link works too.
 */
const ADDRESS_RUN = /0x[0-9a-f]{4,64}/i

/** The address or prefix to search for, or null when the input holds none. */
export function searchTerm(input: string): string | null {
  const m = input.trim().match(ADDRESS_RUN)
  return m ? m[0].toLowerCase() : null
}

import { describe, expect, it } from 'vitest'
import { searchTerm } from '@/lib/search'

describe('searchTerm', () => {
  it('takes a full address or a prefix, lowercased and trimmed', () => {
    expect(searchTerm('  0xABCDEF1234 ')).toBe('0xabcdef1234')
    expect(searchTerm('0x42b9')).toBe('0x42b9')
  })

  it('finds the address inside a pasted link', () => {
    const id = '0x42b961e421c08741fe9b25dda1d1090d42c7f782a0d73e099b4248d67357a612'
    expect(searchTerm(`https://dexscreener.com/robinhood/${id}`)).toBe(id)
  })

  it('refuses input too short or with no address in it', () => {
    expect(searchTerm('0x1')).toBeNull()
    expect(searchTerm('PEPE / WETH')).toBeNull()
    expect(searchTerm('')).toBeNull()
  })
})

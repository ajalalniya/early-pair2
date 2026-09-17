import { beforeEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import RiskChip from '@/components/ui/RiskChip.vue'
import FraudCard from '@/components/aside/FraudCard.vue'
import { makePool } from './fixtures'

describe('RiskChip', () => {
  it('renders label and score in band colours', () => {
    const w = mount(RiskChip, { props: { risk: { score: 74, band: 'DANGER', verdict: '', checks: [], complete: true } } })
    expect(w.text()).toBe('DANGER 74')
    expect(w.attributes('data-band')).toBe('danger')
  })

  it('says unscored rather than implying a clean score', () => {
    const w = mount(RiskChip, { props: { risk: null } })
    expect(w.text()).toBe('UNSCORED')
  })
})

describe('FraudCard', () => {
  beforeEach(() => setActivePinia(createPinia()))

  it('shows score, verdict and all eight rows', () => {
    const pool = makePool({
      buyers1h: 1214,
      sellers1h: 3,
      risk: { score: 91, band: 'DANGER', verdict: '', checks: [], complete: true },
    })
    const w = mount(FraudCard, { props: { pool }, global: { plugins: [createPinia()] } })
    expect(w.text()).toContain('91 / 100')
    expect(w.text()).toContain('honeypot pattern')
    expect(w.findAll('li')).toHaveLength(8)
  })
})

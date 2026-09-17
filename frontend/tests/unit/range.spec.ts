import { describe, expect, it } from 'vitest'
import { axisView, bandGeometry, computeRange, dragValue, step } from '@/lib/range'

const price = 0.00418

describe('computeRange', () => {
  it('derives bounds from a preset', () => {
    const r = computeRange({ price, preset: 'mid', minText: null, maxText: null })
    expect(r.valid).toBe(true)
    expect(r.inRange).toBe(true)
    expect(r.widthLabel).toBe('±5.0%')
    expect(r.efficiency).toBe(20)
    expect(r.status).toBe('In range now')
  })

  it('treats full range as always in range', () => {
    const r = computeRange({ price, preset: 'full', minText: null, maxText: null })
    expect(r.minText).toBe('0')
    expect(r.maxText).toBe('∞')
    expect(r.status).toBe('Always in range')
    expect(r.efficiency).toBe(1)
  })

  it('flags an inverted range', () => {
    const r = computeRange({ price, preset: 'mid', minText: '0.005', maxText: '0.004' })
    expect(r.valid).toBe(false)
    expect(r.status).toBe('Invalid range')
    expect(r.efficiency).toBeNull()
  })

  it('detects out of range', () => {
    const r = computeRange({ price, preset: 'mid', minText: '0.005', maxText: '0.006' })
    expect(r.valid).toBe(true)
    expect(r.inRange).toBe(false)
    expect(r.status).toBe('Out of range')
  })

  it('an edited bound keeps the other one derived from the preset', () => {
    const r = computeRange({ price, preset: 'tight', minText: '0.004', maxText: null })
    expect(r.max).toBeCloseTo(price * 1.02)
  })
})

describe('drag', () => {
  it('clamps min strictly below max', () => {
    const r = computeRange({ price, preset: 'mid', minText: null, maxText: null })
    const view = axisView(price, r)
    const v = dragValue('min', 1, view, r.max, price)
    expect(v).toBeLessThan(r.max)
  })

  it('clamps max strictly above min', () => {
    const r = computeRange({ price, preset: 'mid', minText: null, maxText: null })
    const view = axisView(price, r)
    const v = dragValue('max', 0, view, r.min, price)
    expect(v).toBeGreaterThan(r.min)
  })

  it('band geometry stays inside the axis', () => {
    const r = computeRange({ price, preset: 'wide', minText: null, maxText: null })
    const g = bandGeometry(price, r, axisView(price, r))
    expect(g.left).toBeGreaterThanOrEqual(0)
    expect(g.left + g.width).toBeLessThanOrEqual(100.0001)
    expect(g.priceLeft).toBeGreaterThan(g.left)
    expect(g.priceLeft).toBeLessThan(g.left + g.width)
  })

  it('steppers move by one percent', () => {
    expect(Number(step('1.0000', 1, 1))).toBeCloseTo(1.01)
    expect(Number(step('1.0000', -1, 1))).toBeCloseTo(0.99)
  })
})

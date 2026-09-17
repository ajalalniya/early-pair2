import { POOL_AERO, POOLS } from './fixtures'
import { connectWallet, expect, test } from './harness'

test('live updates to the pool do not cancel the quote or disable the button', async ({ page, harness }) => {
  await page.goto(`/pools?sel=base:${POOL_AERO}`)
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()

  await page.getByTestId('swap-amount').fill('10')
  const action = page.getByTestId('swap-action')
  await expect(action).toHaveText('Buy AERO')
  await expect(action).toBeEnabled()
  const quotesBefore = harness.bodies['/quote/swap']?.length ?? 0

  // The tracker pushes this pool's figures every few seconds while it is watched.
  const aero = POOLS.find((p) => p.address === POOL_AERO)!
  for (let i = 0; i < 4; i++) {
    await harness.push({ t: 'pools.update', seq: 400 + i, ts: Date.now(), d: { ...aero, volume1hUsd: 512_000 + i * 1000 } })
    await page.waitForTimeout(250)
    await expect(action).toBeEnabled()
  }
  expect(harness.bodies['/quote/swap']?.length ?? 0).toBe(quotesBefore)
})

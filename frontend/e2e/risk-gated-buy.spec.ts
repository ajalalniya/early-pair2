import { POOL_AERO, POOL_PEPE } from './fixtures'
import { connectWallet, expect, test } from './harness'

test('buys into a high-risk pool are blocked, selling never is', async ({ page, harness }) => {
  await page.goto(`/pools?sel=bsc:${POOL_PEPE}`)
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()

  await expect(page.getByTestId('trade-summary')).toContainText('PEPE2 / WBNB')
  await expect(page.getByTestId('trade-summary')).toContainText('DANGER 91')
  await expect(page.getByTestId('fraud-card')).toContainText('1,214 addresses bought, only 3 ever sold — honeypot pattern. Trading is gated.')

  const action = page.getByTestId('swap-action')
  await page.getByTestId('swap-amount').fill('0.5')
  await expect(action).toHaveText('High risk — accept below to trade')
  await expect(action).toBeDisabled()
  // A gated buy never asks for a quote, so it cannot reach the wallet.
  await page.waitForTimeout(600)
  expect(harness.bodies['/quote/swap'] ?? []).toHaveLength(0)

  // The gate is a guard, not a verdict: accepting the risk unlocks this pool.
  await expect(page.getByTestId('risk-gate')).toContainText('honeypot pattern')
  await page.getByTestId('risk-accept').check()
  await expect(action).toHaveText('Buy PEPE2 anyway')
  await expect(action).toBeEnabled()
  // The quote now goes out, carrying a threshold above every score.
  await expect.poll(() => harness.bodies['/quote/swap']?.length ?? 0).toBeGreaterThan(0)
  expect((harness.bodies['/quote/swap']![0] as { riskThreshold?: number }).riskThreshold).toBe(101)

  // Unchecking puts the guard back.
  await page.getByTestId('risk-accept').uncheck()
  await expect(action).toBeDisabled()

  // The zap is gated the same way.
  await page.getByRole('tab', { name: 'Buy + LP' }).click()
  await expect(page.getByTestId('zap-action')).toHaveText('Blocked — high risk pool')
  await expect(page.getByTestId('zap-action')).toBeDisabled()

  // Exiting a position is always allowed.
  await page.getByRole('tab', { name: 'Buy / Sell' }).click()
  await page.getByRole('button', { name: 'Sell', exact: true }).click()
  await expect(action).toHaveText('Sell PEPE2')
  await expect(action).toBeEnabled()

  // Turning the gate off in settings unblocks buys.
  await page.getByRole('button', { name: 'Buy', exact: true }).click()
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByLabel('Block buys and zaps into risky pools').uncheck()
  await page.keyboard.press('Escape')
  await expect(action).toHaveText('Buy PEPE2')
})

test('a confirmed buy is reported so it shows in the portfolio', async ({ page, harness }) => {
  await page.goto(`/pools?sel=base:${POOL_AERO}`)
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()

  await page.getByTestId('swap-amount').fill('10')
  const action = page.getByTestId('swap-action')
  await expect(action).toHaveText('Buy AERO')
  await expect(action).toBeEnabled()
  await action.click()

  await expect.poll(() => harness.bodies['/portfolio/trades']?.length ?? 0).toBe(1)
  expect(harness.bodies['/portfolio/trades']![0]).toEqual({ chainId: 'base', pool: POOL_AERO, txHash: '0x' + 'ab'.repeat(32) })
})

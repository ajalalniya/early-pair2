import { POOL_AERO } from './fixtures'
import { connectWallet, expect, test } from './harness'

test('zaps into a pool in one simulated, signed transaction', async ({ page, harness }) => {
  await page.goto('/pools')
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()

  await page.getByTestId(`pool-row-base:${POOL_AERO}`).click()
  await expect(page).toHaveURL(new RegExp(`sel=base%3A${POOL_AERO}|sel=base:${POOL_AERO}`))
  await expect(page.getByTestId('trade-summary')).toContainText('AERO / USDC')

  await page.getByRole('tab', { name: 'Buy + LP' }).click()
  const panel = page.getByRole('tabpanel', { name: 'Buy + LP' })
  await expect(panel).toContainText('One transaction: swap half, then deposit both sides.')
  await expect(panel).toContainText('50% swap / 50% paired')

  await page.getByTestId('zap-amount').fill('1000')
  // Legs come from the backend route plan.
  await expect(panel.getByTestId('legs')).toContainText('Swap USDC → AERO')
  await expect(panel.getByTestId('legs')).toContainText('500 USDC')
  await expect(panel).toContainText('0.25 % · 24.8 %')
  await expect(panel).toContainText('~$2')

  const action = page.getByTestId('zap-action')
  await expect(action).toHaveText('Buy + add liquidity')
  await expect(action).toBeEnabled()
  await action.click()

  await expect(page.getByTestId('tx-receipt')).toHaveText('Bought 412.5 AERO and minted LP · 1 tx')
  await expect(action).toHaveText('Zapped')

  const build = harness.bodies['/tx/build']?.[0]
  expect(build).toMatchObject({ kind: 'zap', chainId: 'base', pool: POOL_AERO, amountIn: '1000000000', splitBps: 5000, minReceived: '408375000000000000000' })
  const sent = await harness.sentTxs()
  expect(sent).toHaveLength(1)
  expect(sent[0]).toMatchObject({ to: '0x' + 'd'.repeat(40), data: '0x12345678' })
})

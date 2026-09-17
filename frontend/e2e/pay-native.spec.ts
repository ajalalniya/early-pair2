import { connectWallet, expect, test } from './harness'

const DEGEN9 = '0x' + '5'.repeat(40) // DEGEN9 / WETH on Base

test('a pair priced in WETH can be bought with ETH, or with WETH', async ({ page, harness }) => {
  await page.goto(`/pools?sel=base:${DEGEN9}`)
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()
  // DEGEN9 scores as risky; lift the gate so the buy path is reachable.
  await page.getByRole('button', { name: 'Settings' }).click()
  await page.getByLabel('Block buys and zaps into risky pools').uncheck()
  await page.keyboard.press('Escape')

  // ETH is the default way to pay.
  await expect(page.getByTestId('pay-native')).toHaveText('ETH')
  await expect(page.getByTestId('pay-native')).toHaveAttribute('aria-pressed', 'true')

  await page.getByTestId('swap-amount').fill('0.1')
  const action = page.getByTestId('swap-action')
  await expect(action).toBeEnabled()
  const quotes = () => harness.bodies['/quote/swap'] ?? []
  expect(quotes().at(-1)).toMatchObject({ side: 'buy', payNative: true, amountIn: '100000000000000000' })

  await action.click()
  await expect.poll(() => harness.bodies['/tx/build']?.length ?? 0).toBe(1)
  expect(harness.bodies['/tx/build']![0]).toMatchObject({ side: 'buy', payNative: true })

  // Switching to WETH quotes without it.
  await page.getByTestId('pay-wrapped').click()
  await expect(page.getByTestId('pay-wrapped')).toHaveAttribute('aria-pressed', 'true')
  await page.getByTestId('swap-amount').fill('0.2')
  await expect.poll(() => (quotes().at(-1) as { amountIn?: string })?.amountIn).toBe('200000000000000000')
  expect(quotes().at(-1)).not.toHaveProperty('payNative')

  // Selling pays out the pair's own quote token: no choice to make.
  await page.getByRole('button', { name: 'Sell', exact: true }).click()
  await expect(page.getByTestId('pay-native')).toHaveCount(0)
})

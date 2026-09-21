import { POOL_GROK } from './fixtures'
import { expect, test } from './harness'

// What a deposit between two prices would earn. GROK / WETH traded $188K in the
// last hour at a 0.30 % fee; the distribution below gives it liquidity of the
// order its $412K of TVL implies, so a dollar is a plausibly small share of it.
const distribution = {
  tick: 100,
  price: 0.00418,
  tickSpacing: 60,
  liquidity: 4.28e22,
  truncated: false,
  quoteUsd: 3000, // WETH
  feeBps: 30,
  bins: [
    { tickLower: -600, tickUpper: 0, priceLower: 0.0031, priceUpper: 0.0038, liquidity: 1.1e22, amount0: 120000, amount1: 0, active: false },
    { tickLower: 0, tickUpper: 600, priceLower: 0.0038, priceUpper: 0.0045, liquidity: 4.28e22, amount0: 60000, amount1: 210, active: true },
  ],
}

const dollars = async (el: { textContent(): Promise<string | null> }): Promise<number> =>
  parseFloat(((await el.textContent()) ?? '').replace(/[$,]/g, ''))

test('the liquidity tab estimates an hour of fees on a dollar between two prices', async ({ page, harness }) => {
  void harness
  await page.route('**/api/v1/pools/*/*/liquidity', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(distribution) }),
  )

  await page.goto(`/pools?sel=eth:${POOL_GROK}&tab=lp`)
  const card = page.getByTestId('lp-fee-estimate')
  await expect(card).toBeVisible()

  // A dollar, the default, over the ±5 % preset the tab opens on.
  await expect(page.getByTestId('lp-deposit')).toHaveValue('1')
  const fee = page.getByTestId('lp-fee-1h')
  await expect(fee).toContainText('$')
  const mid = await dollars(fee)
  expect(mid).toBeGreaterThan(0)
  await expect(card).toContainText('of the liquidity in range')
  await expect(card).toContainText('0.30 %')

  // The same money in a narrower range buys more liquidity, so it earns more.
  await page.getByRole('button', { name: '±2%' }).click()
  await expect.poll(() => dollars(fee)).toBeGreaterThan(mid)

  // …and less in a wider one.
  await page.getByRole('button', { name: '±15%' }).click()
  await expect.poll(() => dollars(fee)).toBeLessThan(mid)

  // Ten dollars earn about ten times as much: the deposit is a small share.
  await page.getByTestId('lp-deposit').fill('10')
  const wide = await dollars(fee)
  await page.getByTestId('lp-deposit').fill('1')
  await expect.poll(() => dollars(fee)).toBeCloseTo(wide / 10, 4)

  if (process.env.EP_SCREENSHOTS) await card.screenshot({ path: `${process.env.EP_SCREENSHOTS}/lp-fees.png` })
})

test('a range the price has left earns nothing, and says so', async ({ page, harness }) => {
  void harness
  await page.route('**/api/v1/pools/*/*/liquidity', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(distribution) }),
  )

  await page.goto(`/pools?sel=eth:${POOL_GROK}&tab=lp`)
  // Put both bounds above the current price of 0.00418.
  await page.getByTestId('lp-min').fill('0.005')
  await page.getByTestId('lp-max').fill('0.006')

  const card = page.getByTestId('lp-fee-estimate')
  await expect(card).toContainText('outside this range')
  await expect(page.getByTestId('lp-fee-1h')).toHaveText('$0')
})

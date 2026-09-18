import { POOL_AERO, POOL_GROK } from './fixtures'
import { expect, test } from './harness'

// The depth chart: where a concentrated pool's liquidity sits across price.
const distribution = {
  tick: 100,
  price: 0.00418,
  tickSpacing: 60,
  liquidity: 1500,
  truncated: false,
  bins: [
    { tickLower: -600, tickUpper: 0, priceLower: 0.0031, priceUpper: 0.0038, liquidity: 400, amount0: 120000, amount1: 0, active: false },
    { tickLower: 0, tickUpper: 600, priceLower: 0.0038, priceUpper: 0.0045, liquidity: 1500, amount0: 60000, amount1: 210, active: true },
    { tickLower: 600, tickUpper: 1800, priceLower: 0.0045, priceUpper: 0.0062, liquidity: 300, amount0: 0, amount1: 95, active: false },
  ],
}

test('a concentrated pool shows where its liquidity sits', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.route('**/api/v1/pools/*/*/liquidity', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(distribution) }),
  )

  await page.goto(`/pools?sel=eth:${POOL_GROK}`) // Uniswap v3, a clmm pool
  const chart = page.getByTestId('liquidity-depth')
  await expect(chart).toBeVisible()

  // One bar per range, with the range holding the current price marked.
  await expect(chart.getByTestId('depth-bar')).toHaveCount(2)
  await expect(chart.getByTestId('depth-bar-active')).toHaveCount(1)
  await expect(chart).toContainText('Active range')
  if (process.env.EP_SCREENSHOTS) await chart.screenshot({ path: `${process.env.EP_SCREENSHOTS}/depth.png` })
})

test('a pool whose price left every range says so, and a v2 pair has no chart', async ({ page, harness }) => {
  void harness
  await page.route('**/api/v1/pools/*/*/liquidity', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ ...distribution, liquidity: 0, bins: distribution.bins.map((b) => ({ ...b, active: false })) }),
    }),
  )
  await page.goto(`/pools?sel=eth:${POOL_GROK}`)
  await expect(page.getByTestId('liquidity-depth')).toContainText('moved past every range')

  // A constant-product pair spreads liquidity along the whole curve: no chart.
  await page.goto(`/pools?sel=base:${POOL_AERO}`)
  await expect(page.getByTestId('trade-summary')).toContainText('AERO / USDC')
  await expect(page.getByTestId('liquidity-depth')).toHaveCount(0)
})

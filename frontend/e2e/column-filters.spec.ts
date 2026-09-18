import { POOL_AERO, POOL_GROK, POOL_PEPE } from './fixtures'
import { expect, test } from './harness'

test('filtering liquidity above a value narrows the feed, and clearing restores it', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.goto('/pools')
  const table = page.getByTestId('pools-table')
  await expect(table.getByTestId(`pool-row-eth:${POOL_GROK}`)).toBeVisible()

  await page.getByTestId('filter-liq').click()
  await page.getByTestId('filter-liq-min').fill('500k')
  if (process.env.EP_SCREENSHOTS) await page.screenshot({ path: `${process.env.EP_SCREENSHOTS}/07-filter-open.png`, clip: { x: 0, y: 60, width: 960, height: 520 } })
  await page.getByTestId('filter-liq-apply').click()

  // AERO has $1.24M; GROK ($412K) and PEPE2 ($41.2K) fall below.
  await expect(page.getByTestId('filter-liq')).toContainText('≥ $500K')
  await expect(table.getByTestId(`pool-row-base:${POOL_AERO}`)).toBeVisible()
  await expect(table.getByTestId(`pool-row-eth:${POOL_GROK}`)).toHaveCount(0)
  await expect(table.getByTestId(`pool-row-bsc:${POOL_PEPE}`)).toHaveCount(0)
  await expect(page).toHaveURL(/liq=500000~/)

  // A price-change filter combines with it.
  await page.getByTestId('filter-chg').click()
  await page.getByTestId('filter-chg-min').fill('10')
  await page.getByTestId('filter-chg-apply').click()
  await expect(table.getByTestId(`pool-row-base:${POOL_AERO}`)).toHaveCount(0) // +8.4%
  await expect(page.getByTestId('filter-clear-all')).toBeVisible()
  if (process.env.EP_SCREENSHOTS) await page.screenshot({ path: `${process.env.EP_SCREENSHOTS}/08-filters-active.png`, clip: { x: 0, y: 60, width: 960, height: 420 } })

  await page.getByTestId('filter-clear-all').click()
  await expect(table.getByTestId(`pool-row-eth:${POOL_GROK}`)).toBeVisible()
  await expect(page).not.toHaveURL(/liq=/)
})

test('an unreadable bound is refused with a hint', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.goto('/pools')
  await page.getByTestId('filter-age').click()
  await page.getByTestId('filter-age-max').fill('soon')
  await expect(page.getByTestId('filter-age-apply')).toBeDisabled()
  await expect(page.getByTestId('filter-age-panel')).toContainText('Use a number of minutes, or 30m, 2h, 1d.')
})

test('the DEX filter narrows the feed to one DEX', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.goto('/pools')
  const table = page.getByTestId('pools-table')
  await expect(table.getByTestId(`pool-row-eth:${POOL_GROK}`)).toBeVisible() // Uniswap v3
  await expect(table.getByTestId(`pool-row-base:${POOL_AERO}`)).toBeVisible() // Aerodrome

  // The chips come from the backend's chain list, not a hardcoded one.
  await expect(page.getByTestId('dex-uniswap-v3')).toBeVisible()
  await expect(page.getByTestId('dex-aerodrome')).toBeVisible()

  await page.getByTestId('dex-uniswap-v3').click()
  await expect(page).toHaveURL(/dex=uniswap-v3/)
  await expect(table.getByTestId(`pool-row-eth:${POOL_GROK}`)).toBeVisible()
  await expect(table.getByTestId(`pool-row-base:${POOL_AERO}`)).toHaveCount(0)
  await expect(table.getByTestId(`pool-row-bsc:${POOL_PEPE}`)).toHaveCount(0)

  // Back to everything: the other DEXes return.
  await page.getByTestId('dex-all').click()
  await expect(page).not.toHaveURL(/dex=/)
  await expect(table.getByTestId(`pool-row-base:${POOL_AERO}`)).toBeVisible()
})

// Volume acceleration sits beside VOL 1H: the last minute against the pair's
// own average minute, so a burst is visible without reading the volume itself.
test('the feed shows how fast volume is accelerating', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.goto('/pools')
  const table = page.getByTestId('pools-table')

  const hot = table.getByTestId(`pool-row-eth:${POOL_GROK}`)
  await expect(hot).toContainText('4.2×')
  await expect(hot.locator('.accel')).toHaveAttribute('data-tone', 'hot')

  const cold = table.getByTestId('pool-row-base:0x' + '5'.repeat(40))
  await expect(cold).toContainText('0.3×')
  await expect(cold.locator('.accel')).toHaveAttribute('data-tone', 'cold')

  // A pair with nothing to compare yet says so rather than showing a number.
  await expect(table.getByTestId(`pool-row-base:${POOL_AERO}`).locator('.accel')).toHaveText('—')
})

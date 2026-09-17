import { POOL_AERO } from './fixtures'
import { expect, test } from './harness'

const key = `base:${POOL_AERO}`

test('starring a pair moves it to favorite pairs, and unstarring moves it back', async ({ page, harness }) => {
  await page.goto('/pools')
  const feed = page.getByTestId('pools-table')
  await expect(feed.getByTestId(`pool-row-${key}`)).toBeVisible()
  await expect(page.getByTestId('favorites-table')).toHaveCount(0)

  await feed.getByTestId(`star-${key}`).click()

  const favorites = page.getByTestId('favorites-table')
  await expect(favorites.getByTestId(`pool-row-${key}`)).toBeVisible()
  await expect(favorites.getByText('FAVORITE PAIRS')).toBeVisible()
  await expect(feed.getByTestId(`pool-row-${key}`)).toHaveCount(0)
  await expect(favorites.getByTestId(`star-${key}`)).toHaveAttribute('aria-pressed', 'true')
  // The star moves the row at once (optimistic); the request lands a moment later.
  await expect.poll(() => harness.bodies[`/favorites/base/${POOL_AERO}`]?.length ?? 0).toBe(1)

  // Live figures keep flowing into the favorite.
  await harness.push({
    t: 'pools.update',
    seq: 201,
    ts: Date.now(),
    d: { chainId: 'base', address: POOL_AERO, pair: 'AERO / USDC', dex: 'Aerodrome', model: 'v2', state: 'confirmed',
      createdAt: new Date().toISOString(), createdBlock: 1, createdTx: '0x' + '2'.repeat(64),
      token0: null, token1: null, feeTierBps: 30, liquidityUsd: 2_500_000, volume1hUsd: 1, priceChange1h: 1,
      buyers1h: 27, sellers1h: 6, tradersPending: 12, risk: null },
  })
  await expect(favorites.getByText('$2.50M')).toBeVisible()
  await expect(favorites.getByTestId(`pool-row-${key}`)).toContainText('27+')

  await favorites.getByTestId(`star-${key}`).click()
  await expect(page.getByTestId('favorites-table')).toHaveCount(0)
  await expect(feed.getByTestId(`pool-row-${key}`)).toBeVisible()
})

test('the star on the selected pair summary stars it and stays in sync with the row', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.goto(`/pools?sel=${key}`)
  const star = page.getByTestId('trade-summary').getByTestId('summary-star')
  await expect(star).toHaveAttribute('aria-pressed', 'false')
  // The summary links straight to the pair on DexScreener, in a new tab.
  const dex = page.getByTestId('trade-summary').getByTestId('summary-dexscreener')
  await expect(dex).toHaveAttribute('href', `https://dexscreener.com/base/${POOL_AERO}`)
  await expect(dex).toHaveAttribute('target', '_blank')

  await star.click()
  await expect(star).toHaveAttribute('aria-pressed', 'true')
  const favorites = page.getByTestId('favorites-table')
  await expect(favorites.getByTestId(`star-${key}`)).toHaveAttribute('aria-pressed', 'true')

  // Unstarring from the row clears the summary's star too.
  await favorites.getByTestId(`star-${key}`).click()
  await expect(page.getByTestId('favorites-table')).toHaveCount(0)
  await expect(star).toHaveAttribute('aria-pressed', 'false')
})

test('a pair removed by the screen leaves the feed', async ({ page, harness }) => {
  await page.goto('/pools')
  const row = page.getByTestId('pools-table').getByTestId(`pool-row-${key}`)
  await expect(row).toBeVisible()
  await harness.push({ t: 'pools.update', seq: 301, ts: Date.now(), d: { chainId: 'base', address: POOL_AERO, removed: true, removedAt: new Date().toISOString() } })
  await expect(row).toHaveCount(0)
})

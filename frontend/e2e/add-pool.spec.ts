import { POOLS } from './fixtures'
import { expect, test } from './harness'

// A pool created before the backend started watching is not in the feed at all,
// so searching its address finds nothing. Adding it by address is the way to
// trade it, and it lands in Favorite pairs tagged ADDED.
const UNLISTED = '0x' + 'd'.repeat(40)

test('a pool that is not listed can be added by address', async ({ page, harness }) => {
  void harness // installs the mocked backend
  const added = {
    ...POOLS[0]!,
    chainId: 'base',
    address: UNLISTED,
    pair: 'NEWP / USDC',
    dex: 'Uniswap v3',
    importedAt: new Date().toISOString(),
    favoritedAt: new Date().toISOString(),
  }
  let importBody: unknown = null
  let listed = false

  // Registered after the harness, so these win.
  await page.route('**/api/v1/pools/import', async (route) => {
    importBody = route.request().postDataJSON()
    listed = true
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ pool: added, added: true }) })
  })
  await page.route('**/api/v1/favorites', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: listed ? [added] : [], limit: 50 }) }),
  )

  await page.goto('/pools')
  await page.getByTestId('pair-search').fill(UNLISTED)

  // Nothing found, so the offer to add it appears.
  const panel = page.getByTestId('add-pool')
  await expect(panel).toBeVisible()
  await page.getByTestId('add-pool-chain').selectOption('base')
  await page.getByTestId('add-pool-submit').click()

  await expect(page.getByTestId('add-pool-done')).toContainText('NEWP / USDC')
  expect(importBody).toEqual({ chainId: 'base', address: UNLISTED })

  // It is now in Favorite pairs, tagged ADDED, and selected for trading.
  const favorites = page.getByTestId('favorites-table')
  await expect(favorites.getByTestId(`pool-row-base:${UNLISTED}`)).toBeVisible()
  await expect(favorites.getByTestId('added-tag')).toHaveText('ADDED')
  await expect(page).toHaveURL(new RegExp(`sel=base(%3A|:)${UNLISTED}`))
})

test('the offer to add appears only for a full address that is not listed', async ({ page, harness }) => {
  void harness
  await page.goto('/pools')

  // A partial address is a search, not something to add.
  await page.getByTestId('pair-search').fill('0xaaaa')
  await expect(page.getByTestId('add-pool')).toHaveCount(0)

  // An address that is listed needs no adding either.
  await page.getByTestId('pair-search').fill(POOLS[0]!.address)
  await expect(page.getByTestId('add-pool')).toHaveCount(0)
})

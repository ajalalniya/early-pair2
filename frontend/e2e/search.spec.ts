import { POOL_AERO, POOL_GROK } from './fixtures'
import { expect, test } from './harness'

test('searching a pair address shows the matching pair in place of the feed', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.goto('/pools')
  await expect(page.getByTestId('pools-table')).toBeVisible()

  const box = page.getByTestId('pair-search')
  await box.fill(POOL_AERO.slice(0, 10).toUpperCase().replace('0X', '0x'))

  const results = page.getByTestId('search-table')
  await expect(results.getByTestId(`pool-row-base:${POOL_AERO}`)).toBeVisible()
  await expect(results.getByTestId(`pool-row-eth:${POOL_GROK}`)).toHaveCount(0)
  await expect(page.getByTestId('pools-table')).toHaveCount(0)
  await expect(page).toHaveURL(/q=0x/)

  // Picking a result opens it in the trade panel.
  await results.getByTestId(`pool-row-base:${POOL_AERO}`).click()
  await expect(page.getByTestId('trade-summary')).toBeVisible()

  // Escape clears the search and brings the feed back.
  await box.press('Escape')
  await expect(page.getByTestId('pools-table')).toBeVisible()
  await expect(page.getByTestId('search-table')).toHaveCount(0)
})

test('a search with no match says so, and a too-short one explains what to type', async ({ page, harness }) => {
  void harness // installs the mocked backend
  await page.goto('/pools')
  const box = page.getByTestId('pair-search')

  await box.fill('0xffffffff')
  await expect(page.getByTestId('search-table')).toContainText('No pair found with an address starting 0xffffffff')

  await box.fill('0x1')
  await expect(page.getByText('Enter a pair address: 0x followed by at least 4 hex characters.')).toBeVisible()
})

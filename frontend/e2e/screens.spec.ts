/**
 * Visual snapshots for manual review, not assertions. Run with
 * `EP_SCREENSHOTS=<dir> pnpm test:e2e screens`.
 */
import path from 'node:path'
import { POOL_GROK } from './fixtures'
import { connectWallet, expect, test } from './harness'

const dir = process.env.EP_SCREENSHOTS
test.skip(!dir, 'set EP_SCREENSHOTS to a directory to capture screens')

const shot = (name: string) => path.join(dir ?? '.', `${name}.png`)

test('desktop screens', async ({ page, harness }) => {
  await page.goto('/pools')
  await expect(page.getByTestId('trade-summary')).toBeVisible()
  await page.screenshot({ path: shot('01-pools-desktop') })

  await page.getByRole('button', { name: /RPCs HEALTHY/ }).click()
  await page.screenshot({ path: shot('02-rpc-panel') })
  await page.keyboard.press('Escape')

  await page.getByTestId(`pool-row-eth:${POOL_GROK}`).click()
  await page.getByRole('tab', { name: 'Liquidity' }).click()
  await page.locator('.aside').evaluate((el) => (el.scrollTop = 420))
  await page.screenshot({ path: shot('03-liquidity-clmm') })

  // Live insert while scrolled: the pill appears and the rows do not move.
  await page.evaluate(() => window.scrollTo(0, 0))
  await harness.push({
    t: 'pools.new',
    seq: 101,
    ts: Date.now(),
    d: {
      chainId: 'base', address: '0x' + '9'.repeat(40), pair: 'NEWT / WETH', dex: 'Uniswap v2', model: 'v2', state: 'unconfirmed',
      createdAt: new Date().toISOString(), createdBlock: 1, createdTx: '0x' + '2'.repeat(64),
      token0: { address: '0x' + 'f'.repeat(40), symbol: 'NEWT', decimals: 18 }, token1: { address: '0x' + 'e'.repeat(40), symbol: 'WETH', decimals: 18 },
      feeTierBps: 30, liquidityUsd: 18200, volume1hUsd: 900, priceChange1h: null, buyers1h: 4, sellers1h: 0, risk: null,
    },
  })
  await expect(page.getByText('NEWT / WETH')).toBeVisible()
  await page.screenshot({ path: shot('04-live-insert') })

  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()
  // Navigate in-app: a reload would reset the mock wallet's authorisation.
  await page.getByRole('link', { name: 'Portfolio' }).click()
  await expect(page.getByText('Tokens bought')).toBeVisible()
  await page.screenshot({ path: shot('05-portfolio'), fullPage: true })

  await page.getByRole('link', { name: 'Watchlist' }).click()
  await page.getByPlaceholder('Transfer(address,address,uint256) or 0x topic').fill('PairCreated(address,address,address,uint256)')
  await page.screenshot({ path: shot('06-watchlist') })
})

test('narrow screens', async ({ page, harness }) => {
  void harness
  await page.setViewportSize({ width: 1000, height: 900 })
  await page.goto('/pools')
  await expect(page.getByTestId('trade-summary')).toBeVisible()
  await page.screenshot({ path: shot('07-pools-1000'), fullPage: false })

  await page.setViewportSize({ width: 400, height: 860 })
  await page.goto('/pools?risk=danger')
  await expect(page.getByTestId('trade-summary')).toBeVisible()
  await page.screenshot({ path: shot('08-pools-400'), fullPage: true })
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
})

import { POOL_AERO } from './fixtures'
import { connectWallet, expect, test } from './harness'

test('a holding opens a trade panel on the portfolio to sell it or buy more', async ({ page, harness }) => {
  void harness // installs the mocked backend
  const aero = '0x' + 'b'.repeat(40)
  // Registered after the harness, so it answers holdings for this test.
  let holdingsRequests = 0
  await page.route('**/api/v1/portfolio/holdings', (route) => {
    holdingsRequests++
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        data: [
          {
            chainId: 'base', pool: POOL_AERO, token: { address: aero, symbol: 'AERO', decimals: 18 },
            amount: '5000000000000000000', avgEntryUsd: 1.1, priceUsd: 1.25, valueUsd: 6.25, pnlPct: 13.6, pnlUsd: 0.75, risk: null,
          },
        ],
      }),
    })
  })

  await page.goto('/pools')
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()
  // The wallet dialog's closing overlay would swallow the next click.
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await page.getByRole('link', { name: 'Portfolio' }).click()
  // The route is lazy: in dev its first visit waits for Vite to compile it,
  // which under parallel test load can outlast the default timeout.
  await expect(page).toHaveURL(/\/portfolio/, { timeout: 30_000 })
  await expect(page.getByRole('heading', { name: 'Tokens bought' })).toBeVisible()

  // The only holding is selected, and the panel opens ready to sell it.
  const summary = page.getByTestId('trade-summary')
  await expect(summary).toContainText('AERO / USDC')
  const action = page.getByTestId('swap-action')
  await expect(action).toHaveText('Sell AERO')
  await expect(page.getByTestId('swap-held')).toHaveText('You hold 5 AERO')
  await page.getByTestId('swap-max').click()
  await expect(page.getByTestId('swap-amount')).toHaveValue('5')

  const row = page.getByTestId(`holding-base:${aero}`)
  await row.getByTestId('holding-buy').click()
  await expect(action).toHaveText('Buy AERO')
  await row.getByTestId('holding-sell').click()
  await expect(action).toHaveText('Sell AERO')
  await expect(page).toHaveURL(new RegExp(`sel=base(%3A|:)${POOL_AERO}`))
  if (process.env.EP_SCREENSHOTS) await page.screenshot({ path: `${process.env.EP_SCREENSHOTS}/06-portfolio-trade.png` })

  // Tokens bought refreshes every 10 s while on screen, without disturbing the
  // trade panel: the amount typed stays put.
  const before = holdingsRequests
  await expect.poll(() => holdingsRequests, { timeout: 15_000 }).toBeGreaterThan(before)
  await expect(page.getByTestId('swap-amount')).toHaveValue('5')
  await expect(action).toHaveText('Sell AERO')
})

test('a token can be removed from Tokens bought', async ({ page, harness }) => {
  void harness // installs the mocked backend
  const aero = '0x' + 'b'.repeat(40)
  let hidden = false
  const removals: string[] = []
  await page.route('**/api/v1/portfolio/holdings', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        data: hidden
          ? []
          : [
              {
                chainId: 'base', pool: POOL_AERO, token: { address: aero, symbol: 'AERO', decimals: 18 },
                amount: '5000000000000000000', avgEntryUsd: 1.1, priceUsd: 1.25, valueUsd: 6.25, pnlPct: 13.6, pnlUsd: 0.75, risk: null,
              },
            ],
      }),
    }),
  )
  await page.route('**/api/v1/portfolio/holdings/*/*', (route) => {
    removals.push(`${route.request().method()} ${new URL(route.request().url()).pathname}`)
    hidden = true
    return route.fulfill({ status: 204 })
  })

  await page.goto('/pools')
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await page.getByRole('link', { name: 'Portfolio' }).click()
  await expect(page).toHaveURL(/\/portfolio/, { timeout: 30_000 })

  const row = page.getByTestId(`holding-base:${aero}`)
  await expect(row).toBeVisible()

  // The confirmation is inline, never a native dialog (a browser can suppress
  // those, and then the click silently does nothing). Cancelling keeps the token.
  page.on('dialog', (d) => {
    throw new Error(`unexpected native dialog: ${d.message()}`)
  })
  await row.getByTestId('holding-remove').click()
  await row.getByTestId('holding-remove-no').click()
  await expect(row.getByTestId('holding-remove')).toBeVisible()
  expect(removals).toHaveLength(0)

  await row.getByTestId('holding-remove').click()
  await row.getByTestId('holding-remove-yes').click()
  await expect(row).toHaveCount(0)
  await expect.poll(() => removals).toEqual([`DELETE /api/v1/portfolio/holdings/base/${aero}`])
  await expect(page.getByText('No tokens held.')).toBeVisible()
})

test('a holding can be approved ahead, so selling it later is one transaction', async ({ page, harness }) => {
  void harness // installs the mocked backend
  const aero = '0x' + 'b'.repeat(40)
  let approved = false
  const approveBodies: unknown[] = []
  const holding = () => ({
    chainId: 'base', pool: POOL_AERO, token: { address: aero, symbol: 'AERO', decimals: 18 },
    amount: '5000000000000000000', avgEntryUsd: 1.1, priceUsd: 1.25, valueUsd: 6.25, pnlPct: 13.6, pnlUsd: 0.75, risk: null,
    sellReady: approved,
  })
  await page.route('**/api/v1/portfolio/holdings', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ data: [holding()] }) }),
  )
  // Two approvals, as a Uniswap v4 pool needs: token → Permit2, Permit2 → router.
  await page.route('**/api/v1/tx/approve', (route) => {
    approveBodies.push(route.request().postDataJSON())
    approved = true
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        approved: false,
        steps: [
          { chainId: 'base', to: aero, data: '0x095ea7b3', value: '0', kind: 'approve', label: 'Approve AERO for Permit2' },
          { chainId: 'base', to: '0x000000000022d473030f116ddee9f6b43ac78ba3', data: '0x87517c45', value: '0', kind: 'approve', label: 'Allow the Uniswap router to spend AERO via Permit2' },
        ],
      }),
    })
  })

  await page.goto('/pools')
  await connectWallet(page)
  await page.getByRole('button', { name: 'Start trading' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await page.getByRole('link', { name: 'Portfolio' }).click()
  await expect(page).toHaveURL(/\/portfolio/, { timeout: 30_000 })

  const row = page.getByTestId(`holding-base:${aero}`)
  await expect(row.getByTestId('holding-approve')).toBeVisible()
  await row.getByTestId('holding-approve').click()

  // Both approvals are signed — and nothing else: no swap goes out.
  await expect.poll(async () => (await harness.sentTxs()).length).toBe(2)
  expect(approveBodies).toHaveLength(1)
  expect(approveBodies[0]).toMatchObject({ chainId: 'base', pool: POOL_AERO, side: 'sell' })

  // The row now reports it is ready, and the button is gone.
  await expect(row.getByTestId('holding-ready')).toBeVisible()
  await expect(row.getByTestId('holding-approve')).toHaveCount(0)
})

// Long amounts once made a row wider than its card, and the card clipped the
// overflow: the delete button — right-most — silently disappeared. Every
// action must stay inside the card at common desktop widths.
for (const width of [1100, 1280, 1440]) {
  test(`every row action stays on screen at ${width}px`, async ({ page, harness }) => {
    void harness // installs the mocked backend
    await page.setViewportSize({ width, height: 900 })
    const token = '0x' + 'e'.repeat(40)
    await page.route('**/api/v1/portfolio/holdings', (route) =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          data: [
            {
              chainId: 'base', pool: POOL_AERO, token: { address: token, symbol: 'ARCJET', decimals: 18 },
              amount: '593399902295100710579000', avgEntryUsd: 0.0000168523, priceUsd: 0.0000196341, valueUsd: 11.6512,
              pnlPct: -16.52, pnlUsd: -0.0123, risk: null, sellReady: false,
            },
          ],
        }),
      }),
    )
    await page.goto('/pools')
    await connectWallet(page)
    await page.getByRole('button', { name: 'Start trading' }).click()
    await expect(page.getByRole('dialog')).toHaveCount(0)
    await page.getByRole('link', { name: 'Portfolio' }).click()
    const row = page.getByTestId(`holding-base:${token}`)
    await expect(row).toBeVisible({ timeout: 30_000 })

    const card = (await page.locator('.card.holdings').boundingBox())!
    for (const id of ['holding-buy', 'holding-sell', 'holding-approve', 'holding-remove']) {
      const box = (await row.getByTestId(id).boundingBox())!
      expect(box.x, `${id} left edge`).toBeGreaterThanOrEqual(card.x)
      expect(box.x + box.width, `${id} right edge`).toBeLessThanOrEqual(card.x + card.width)
    }
  })
}

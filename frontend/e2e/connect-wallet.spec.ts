import { connectWallet, expect, test } from './harness'

test('connects an EIP-6963 wallet, signs in, and shows balances', async ({ page, harness }) => {
  await page.goto('/pools')

  await page.getByTestId('wallet-button').click()
  const chooser = page.getByRole('dialog', { name: 'Connect a wallet' })
  await expect(chooser).toBeVisible()
  // Badges come from provider discovery: the mock announces itself as MetaMask only.
  await expect(page.getByTestId('connector-metamask')).toContainText('DETECTED')
  await expect(page.getByTestId('connector-rabby')).toContainText('INSTALL')
  await expect(chooser).toContainText('EarlyPair never takes custody.')
  await page.keyboard.press('Escape')

  await connectWallet(page)
  const done = page.getByRole('dialog', { name: 'Wallet connected' })
  await expect(done.getByTestId('connected-address')).toHaveText('0x7A4F…2Be1')
  await expect(done).toContainText('LIVE')
  await expect(done).toContainText('4 chains')

  // SIWE: a nonce was requested and the signature verified for this address.
  expect(harness.bodies['/auth/nonce']).toHaveLength(1)
  expect(harness.bodies['/auth/verify']?.[0]).toMatchObject({ nonce: 'abc123' })

  await expect(done).toContainText('$11.3K')
  await done.getByRole('button', { name: 'Start trading' }).click()
  await expect(page.getByTestId('wallet-button')).toHaveText('0x7A4F…2Be1')
  await expect(page.locator('.funds')).toContainText('$22.2K')
  await expect(page.locator('.funds')).toContainText('Native + stables on 4 networks')
  // Zero balances are not listed, and a network holding nothing is not shown at all.
  await expect(page.getByTestId('funds-eth')).toContainText('USDC')
  await expect(page.getByTestId('funds-eth')).not.toContainText('DAI')
  await expect(page.getByTestId('funds-rbn')).toHaveCount(0)
})

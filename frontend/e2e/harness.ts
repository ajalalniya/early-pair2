import { test as base, expect, type Page, type WebSocketRoute } from '@playwright/test'
import { CHAINS, FUNDS, POOLS, WALLET, rpcHealth } from './fixtures'

interface Harness {
  /** Push a server frame onto the mocked stream. */
  push(frame: object): Promise<void>
  /** Transactions the mock wallet was asked to send. */
  sentTxs(): Promise<unknown[]>
  /** Request bodies seen per API path. */
  bodies: Record<string, unknown[]>
}

const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
const notImplemented = (message: string) => json({ error: { code: 'not_implemented', message } }, 501)

async function mockBackend(page: Page, bodies: Record<string, unknown[]>) {
  let favorites: Record<string, unknown>[] = []
  await page.route('**/api/v1/**', async (route) => {
    const req = route.request()
    const url = new URL(req.url())
    const path = url.pathname.replace(/^\/api\/v1/, '')
    const body = req.postData() ? JSON.parse(req.postData()!) : undefined
    ;(bodies[path] ??= []).push(body)

    if (req.method() === 'GET' && path === '/chains') return route.fulfill(json({ data: CHAINS }))
    if (req.method() === 'GET' && path === '/pools') return route.fulfill(json({ data: POOLS, nextCursor: null }))
    if (req.method() === 'GET' && path === '/pools/search') {
      const q = (url.searchParams.get('q') ?? '').toLowerCase()
      if (!/^0x[0-9a-f]{4,64}$/.test(q)) return route.fulfill(json({ error: { code: 'bad_query', message: 'bad query' } }, 400))
      return route.fulfill(json({ data: POOLS.filter((p) => p.address.startsWith(q)) }))
    }
    const poolMatch = path.match(/^\/pools\/([^/]+)\/([^/]+)$/)
    if (poolMatch) {
      const p = POOLS.find((x) => x.chainId === poolMatch[1] && x.address === poolMatch[2]!.toLowerCase())
      return route.fulfill(p ? json(p) : json({ error: { code: 'not_found', message: 'no such record' } }, 404))
    }
    if (path === '/favorites' && req.method() === 'GET') return route.fulfill(json({ data: favorites, limit: 50 }))
    const favMatch = path.match(/^\/favorites\/([^/]+)\/([^/]+)$/)
    if (favMatch) {
      const address = favMatch[2]!.toLowerCase()
      const p = POOLS.find((x) => x.chainId === favMatch[1] && x.address === address)
      if (!p) return route.fulfill(json({ error: { code: 'not_found', message: 'no such record' } }, 404))
      favorites = favorites.filter((x) => x.address !== address)
      if (req.method() === 'DELETE') return route.fulfill({ status: 204 })
      const starred = { ...p, favoritedAt: new Date().toISOString() }
      favorites.unshift(starred)
      return route.fulfill(json(starred))
    }
    if (path === '/rpc/health') return route.fulfill(json(rpcHealth()))
    if (path === '/auth/nonce') {
      return route.fulfill(json({ nonce: 'abc123', expiresAt: new Date(Date.now() + 300_000).toISOString(), message: 'earlypair wants you to sign in\n\nNonce: abc123' }))
    }
    if (path === '/auth/verify') {
      return route.fulfill(json({ token: 'test.session.token', expiresAt: new Date(Date.now() + 86_400_000).toISOString(), address: WALLET }))
    }
    if (path === '/portfolio/funds') return route.fulfill(json(FUNDS))
    if (path === '/portfolio/trades' && req.method() === 'POST') {
      return route.fulfill(json({ recorded: true, side: 'buy', token: '0x' + 'b'.repeat(40), tokenAmount: '1' }))
    }
    if (path === '/portfolio/holdings') return route.fulfill(json({ data: [] }))
    if (path === '/portfolio/positions') return route.fulfill(notImplemented('LP position tracking is not built yet'))
    if (path === '/watch/tokens' && req.method() === 'GET') return route.fulfill(json({ data: [] }))
    if (path === '/watch/topics' && req.method() === 'GET') return route.fulfill(json({ data: [], limit: 10 }))

    if (path === '/quote/swap') {
      const p = POOLS.find((x) => x.address === body.pool)!
      const gated = body.side === 'buy' && p.risk.score >= (body.riskThreshold ?? 60)
      return route.fulfill(
        json({
          legs: [{ step: '1', label: 'Swap', value: '' }],
          amountIn: body.amountIn,
          amountOut: (BigInt(body.amountIn) * 1000n).toString(),
          minReceived: ((BigInt(body.amountIn) * 1000n * 99n) / 100n).toString(),
          priceImpactBps: 42,
          rate: `1 ${p.token0.symbol} = 0.001 ${p.token1.symbol}`,
          estGas: 180000,
          gasPriceWei: '1000000000',
          estGasNative: '0.00018',
          estGasUsd: 0.62,
          expiresAt: new Date(Date.now() + 30_000).toISOString(),
          gated,
        }),
      )
    }
    if (path === '/quote/zap') {
      const p = POOLS.find((x) => x.address === body.pool)!
      const swap = (BigInt(body.amountIn) * BigInt(body.splitBps)) / 10000n
      return route.fulfill(
        json({
          legs: [
            { step: '1', label: `Swap ${p.token1.symbol} → ${p.token0.symbol}`, value: `${Number(swap) / 1e6} ${p.token1.symbol}` },
            { step: '2', label: 'Deposit pair', value: `412.5 ${p.token0.symbol} + ${Number(BigInt(body.amountIn) - swap) / 1e6} ${p.token1.symbol}` },
            { step: '3', label: 'Mint LP position', value: p.dex },
          ],
          amountIn: body.amountIn,
          amountOut: '412500000000000000000',
          minReceived: '408375000000000000000',
          priceImpactBps: 18,
          lpSlipBps: 11,
          feeTierBps: 25,
          estAprPct: 24.8,
          poolSharePctAfter: 0.41,
          estGasUsd: 1.84,
          expiresAt: new Date(Date.now() + 30_000).toISOString(),
          gated: false,
        }),
      )
    }
    if (path === '/tx/build') {
      return route.fulfill(
        json({
          steps: [{ chainId: body.chainId, to: '0x' + 'd'.repeat(40), data: '0x12345678', value: '0', gas: 250000, kind: body.kind ?? 'swap', label: 'Router call' }],
          expiresAt: new Date(Date.now() + 300_000).toISOString(),
        }),
      )
    }
    return route.fulfill(json({ error: { code: 'not_found', message: `unmocked ${req.method()} ${path}` } }, 404))
  })
}

/** Public JSON-RPC endpoints used for simulation and receipts. */
async function mockPublicRpc(page: Page) {
  await page.route(
    (url) => url.hostname !== 'localhost' && !url.hostname.includes('fonts.g'),
    async (route) => {
      const req = route.request()
      const raw = req.postData()
      if (req.method() !== 'POST' || !raw || !raw.includes('jsonrpc')) return route.abort()
      const parsed = JSON.parse(raw)
      const answer = (m: { id: number; method: string; params?: unknown[] }) => ({ jsonrpc: '2.0', id: m.id, result: rpcResult(m.method, m.params) })
      const out = Array.isArray(parsed) ? parsed.map(answer) : answer(parsed)
      return route.fulfill(json(out))
    },
  )
}

function rpcResult(method: string, params: unknown[] = []): unknown {
  switch (method) {
    case 'eth_chainId':
      return '0x2105'
    case 'eth_blockNumber':
      return '0x10'
    case 'eth_call':
      return '0x'
    case 'eth_getTransactionReceipt':
      return {
        transactionHash: params[0],
        blockHash: '0x' + 'e'.repeat(64),
        blockNumber: '0x10',
        transactionIndex: '0x0',
        from: WALLET,
        to: '0x' + 'd'.repeat(40),
        status: '0x1',
        logs: [],
        logsBloom: '0x' + '0'.repeat(512),
        gasUsed: '0x5208',
        cumulativeGasUsed: '0x5208',
        effectiveGasPrice: '0x1',
        contractAddress: null,
        type: '0x2',
      }
    default:
      return null
  }
}

async function mockStream(page: Page): Promise<() => WebSocketRoute | null> {
  let current: WebSocketRoute | null = null
  await page.routeWebSocket(/\/api\/v1\/stream/, (ws) => {
    current = ws
    ws.onMessage((msg) => {
      const f = JSON.parse(String(msg))
      if (f.op === 'sub') ws.send(JSON.stringify({ t: 'subscribed', topics: f.topics, chains: [], seq: 100 }))
      if (f.op === 'ping') ws.send(JSON.stringify({ t: 'pong' }))
    })
  })
  return () => current
}

/** An EIP-1193 wallet announced over EIP-6963 as MetaMask. */
async function mockWallet(page: Page) {
  await page.addInitScript((address: string) => {
    const w = window as unknown as { __sentTxs: unknown[] }
    w.__sentTxs = []
    let chainId = '0x2105'
    // Like a real extension, the site is not authorised until it asks.
    let authorised = false
    const listeners: Record<string, ((...a: unknown[]) => void)[]> = {}
    const emit = (ev: string, ...args: unknown[]) => (listeners[ev] ?? []).forEach((fn) => fn(...args))
    const provider = {
      isMetaMask: true,
      async request({ method, params }: { method: string; params?: unknown[] }) {
        switch (method) {
          case 'eth_requestAccounts':
            authorised = true
            return [address]
          case 'eth_accounts':
            return authorised ? [address] : []
          case 'eth_chainId':
            return chainId
          case 'net_version':
            return String(parseInt(chainId, 16))
          case 'wallet_requestPermissions':
            authorised = true
            return [{ parentCapability: 'eth_accounts' }]
          case 'wallet_getPermissions':
            return authorised ? [{ parentCapability: 'eth_accounts' }] : []
          case 'wallet_switchEthereumChain': {
            chainId = (params?.[0] as { chainId: string }).chainId
            emit('chainChanged', chainId)
            return null
          }
          case 'personal_sign':
            return '0x' + '11'.repeat(64) + '1b'
          case 'eth_sendTransaction':
            w.__sentTxs.push(params?.[0])
            return '0x' + 'ab'.repeat(32)
          case 'eth_estimateGas':
            return '0x30d40'
          case 'eth_getTransactionCount':
            return '0x1'
          case 'eth_gasPrice':
            return '0x3b9aca00'
          case 'eth_maxPriorityFeePerGas':
            return '0x1'
          case 'eth_blockNumber':
            return '0x10'
          case 'eth_getBlockByNumber':
            return { number: '0x10', hash: '0x' + 'e'.repeat(64), baseFeePerGas: '0x1', timestamp: '0x0', transactions: [] }
          case 'eth_call':
            return '0x'
          default: {
            const err = new Error(`mock wallet: unsupported ${method}`) as Error & { code: number }
            err.code = 4200
            throw err
          }
        }
      },
      on(ev: string, fn: (...a: unknown[]) => void) {
        ;(listeners[ev] ??= []).push(fn)
      },
      removeListener(ev: string, fn: (...a: unknown[]) => void) {
        listeners[ev] = (listeners[ev] ?? []).filter((f) => f !== fn)
      },
    }
    const detail = Object.freeze({
      info: { uuid: '6f7c2a50-0000-4000-8000-000000000001', name: 'MetaMask', icon: 'data:image/svg+xml,%3Csvg xmlns="http://www.w3.org/2000/svg"/%3E', rdns: 'io.metamask' },
      provider,
    })
    const announce = () => window.dispatchEvent(new CustomEvent('eip6963:announceProvider', { detail }))
    window.addEventListener('eip6963:requestProvider', announce)
    announce()
  }, WALLET)
}

export const test = base.extend<{ harness: Harness }>({
  harness: async ({ page }, use) => {
    const bodies: Record<string, unknown[]> = {}
    await mockPublicRpc(page)
    await mockBackend(page, bodies)
    const socket = await mockStream(page)
    await mockWallet(page)
    await use({
      async push(frame) {
        const ws = socket()
        if (!ws) throw new Error('stream not connected yet')
        ws.send(JSON.stringify(frame))
      },
      async sentTxs() {
        return page.evaluate(() => (window as unknown as { __sentTxs: unknown[] }).__sentTxs)
      },
      bodies,
    })
  },
})

export { expect }

export async function connectWallet(page: Page) {
  await page.getByTestId('wallet-button').click()
  await page.getByTestId('connector-metamask').click()
  await expect(page.getByRole('dialog', { name: 'Wallet connected' })).toBeVisible()
}
